package analytics

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

const (
	batchSize     = 256
	maxReportRows = 100_000
)

var ErrConflict = errors.New("different contents under an existing portfolio record identity")

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || dsn == "" {
		return nil, errors.New("invalid analytics PostgreSQL configuration")
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("could not configure analytics PostgreSQL")
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var fsync string
		if err := tx.QueryRow(ctx, `SHOW fsync`).Scan(&fsync); err != nil || fsync != "on" {
			return errors.New("analytics PostgreSQL requires fsync")
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(790178013)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schemaSQL)
		return err
	})
	if err != nil {
		pool.Close()
		return nil, errors.New("could not initialize analytics PostgreSQL")
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ready(ctx context.Context) error { return s.pool.Ping(ctx) }

func ensureAccount(ctx context.Context, tx pgx.Tx, accountID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO analytics_accounts(account_id) VALUES ($1) ON CONFLICT DO NOTHING`, accountID)
	return err
}

func (s *Store) account(ctx context.Context, event AccountEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(body)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := ensureAccount(ctx, tx, event.AccountID); err != nil {
			return err
		}
		var seen int64
		var data []byte
		if err := tx.QueryRow(ctx, `SELECT seen_version, state FROM analytics_accounts WHERE account_id=$1 FOR UPDATE`,
			event.AccountID).Scan(&seen, &data); err != nil {
			return err
		}
		state := newState()
		if len(data) != 0 {
			if err := json.Unmarshal(data, &state); err != nil {
				return err
			}
		}
		inserted, err := tx.Exec(ctx, `INSERT INTO analytics_account_events
			(account_id, account_version, event_id, digest, payload) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT DO NOTHING`, event.AccountID, event.AccountVersion, event.EventID, hash[:], body)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() == 0 {
			var prior []byte
			if err := tx.QueryRow(ctx, `SELECT digest FROM analytics_account_events
				WHERE account_id=$1 AND (account_version=$2 OR event_id=$3) LIMIT 1`,
				event.AccountID, event.AccountVersion, event.EventID).Scan(&prior); err != nil {
				return err
			}
			if !bytes.Equal(prior, hash[:]) {
				return ErrConflict
			}
			return nil
		}
		if event.AccountVersion == state.Version+1 {
			// The next event is already decoded. Only previously staged successors
			// need to be read back after closing a gap.
			if err := state.apply(event); err != nil {
				return err
			}
		}
		seen = max(seen, event.AccountVersion)
		if err := applyAvailable(ctx, tx, event.AccountID, &state, seen); err != nil {
			return err
		}
		var projection []byte
		if state.Version > 0 {
			projection, err = json.Marshal(state)
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE analytics_accounts SET applied_version=$2, seen_version=$3, state=$4 WHERE account_id=$1`,
			event.AccountID, state.Version, seen, projection)
		return err
	})
}

func applyAvailable(ctx context.Context, tx pgx.Tx, accountID string, state *ledgerState, seen int64) error {
	for state.Version < seen {
		rows, err := tx.Query(ctx, `SELECT payload FROM analytics_account_events
			WHERE account_id=$1 AND account_version>$2 ORDER BY account_version LIMIT $3`, accountID, state.Version, batchSize)
		if err != nil {
			return err
		}
		events, err := collect[AccountEvent](rows)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return errors.New("seen account version is missing from inbox")
		}
		for _, event := range events {
			if event.AccountVersion != state.Version+1 {
				return nil
			}
			if err := state.apply(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) head(ctx context.Context, head AccountHead) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := ensureAccount(ctx, tx, head.AccountID); err != nil {
			return err
		}
		var priorVersion int64
		var priorAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT head_version, head_observed_at
			FROM analytics_accounts WHERE account_id=$1 FOR UPDATE`, head.AccountID).Scan(&priorVersion, &priorAt); err != nil {
			return err
		}
		if priorAt != nil && !head.ObservedAt.After(*priorAt) {
			if head.ObservedAt.Equal(*priorAt) && head.AccountVersion != priorVersion {
				return ErrConflict
			}
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE analytics_accounts SET head_version=$2, head_observed_at=$3 WHERE account_id=$1`,
			head.AccountID, head.AccountVersion, head.ObservedAt)
		return err
	})
}

func (s *Store) valuation(ctx context.Context, row Valuation) error {
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(body)
	result, err := s.pool.Exec(ctx, `INSERT INTO analytics_valuations (account_id,id,digest,payload) VALUES ($1,$2,$3,$4)
		ON CONFLICT (account_id,id) DO UPDATE SET digest=EXCLUDED.digest WHERE analytics_valuations.digest=EXCLUDED.digest`,
		row.AccountID, row.ID, hash[:], body)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) Report(ctx context.Context, accountID string, policy Policy, period string) (Report, error) {
	input, err := s.loadReport(ctx, accountID)
	if err != nil {
		return Report{}, err
	}
	return buildReport(input, policy, period)
}

func (s *Store) loadReport(ctx context.Context, accountID string) (reportInput, error) {
	input := reportInput{accountID: accountID}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var body []byte
		var headAt *time.Time
		var applied int64
		err := tx.QueryRow(ctx, `SELECT transaction_timestamp(), state, applied_version, seen_version, head_version, head_observed_at
			FROM analytics_accounts WHERE account_id=$1`, accountID).Scan(&input.now, &body, &applied, &input.seen, &input.head.AccountVersion, &headAt)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && applied == 0) {
			return ErrNoAccount
		}
		if err != nil {
			return err
		}
		input.now = input.now.UTC()
		if headAt != nil {
			input.head.ObservedAt = headAt.UTC()
		}
		if err := json.Unmarshal(body, &input.state); err != nil {
			return err
		}
		if applied > maxReportRows {
			return errors.New("account history exceeds portfolio report limit")
		}
		rows, err := tx.Query(ctx, `SELECT payload FROM analytics_account_events WHERE account_id=$1 AND account_version<=$2
			ORDER BY account_version`, accountID, applied)
		if err != nil {
			return err
		}
		input.events, err = collect[AccountEvent](rows)
		if err != nil {
			return err
		}
		if int64(len(input.events)) != applied || input.state.Version != applied {
			return errors.New("portfolio projection and inbox versions disagree")
		}
		rows, err = tx.Query(ctx, `SELECT payload FROM analytics_valuations WHERE account_id=$1 ORDER BY id LIMIT $2`, accountID, maxReportRows+1)
		if err != nil {
			return err
		}
		input.valuations, err = collect[Valuation](rows)
		if len(input.valuations) > maxReportRows {
			return errors.New("valuation history exceeds portfolio report limit")
		}
		return err
	})
	return input, err
}

func collect[T any](rows pgx.Rows) ([]T, error) {
	defer rows.Close()
	var result []T
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var row T
		if err := json.Unmarshal(payload, &row); err != nil {
			return nil, fmt.Errorf("decode stored portfolio record: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
