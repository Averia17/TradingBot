package jupiter_execution

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/quotes"
)

const receiptColumns = `owner, intent_id::text, reservation_id::text, provider_request_id, signature,
	approval_hash, last_valid_block_height, provider_status, provider_code, provider_slot, claimed_at, provider_observed_at`

type Postgres struct {
	pool *pgxpool.Pool
}

func OpenPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil || databaseURL == "" {
		return nil, ErrInvalid
	}
	config.MaxConns = 4
	config.ConnConfig.RuntimeParams["synchronous_commit"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, ErrUnavailable
	}
	if _, err := pool.Exec(ctx, "SELECT "+receiptColumns+" FROM jupiter_execution_attempts LIMIT 0"); err != nil {
		pool.Close()
		return nil, ErrUnavailable
	}
	if err := checkAttemptConstraints(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

func checkAttemptConstraints(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `SELECT array_agg(a.attname::text ORDER BY a.attname)
		FROM pg_constraint c
		JOIN pg_index i ON i.indexrelid = c.conindid
		CROSS JOIN LATERAL unnest(c.conkey) AS k(attnum)
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conrelid = 'jupiter_execution_attempts'::regclass AND c.contype IN ('p', 'u')
			AND c.convalidated AND NOT c.condeferrable AND i.indisvalid AND i.indisready
		GROUP BY c.oid`)
	if err != nil {
		return ErrUnavailable
	}
	defer rows.Close()
	needed := map[string]bool{"intent_id,owner": true, "reservation_id": true, "provider_request_id": true, "signature": true}
	for rows.Next() {
		var columns []string
		if err := rows.Scan(&columns); err != nil {
			return ErrUnavailable
		}
		delete(needed, strings.Join(columns, ","))
	}
	if rows.Err() != nil || len(needed) != 0 {
		return ErrUnavailable
	}
	return nil
}

func (db *Postgres) Close() {
	db.pool.Close()
}

func (db *Postgres) lookup(ctx context.Context, owner, intentID string) (record, error) {
	row := db.pool.QueryRow(ctx, "SELECT "+receiptColumns+" FROM jupiter_execution_attempts WHERE owner=$1 AND intent_id=$2",
		owner, intentID)
	return scanReceipt(row)
}

func (db *Postgres) claim(ctx context.Context, candidate record) (record, bool, error) {
	row := db.pool.QueryRow(ctx, `INSERT INTO jupiter_execution_attempts
		(owner, intent_id, reservation_id, provider_request_id, signature, approval_hash, last_valid_block_height, provider_status, claimed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'unknown', $8)
		ON CONFLICT (owner, intent_id) DO NOTHING RETURNING `+receiptColumns,
		candidate.Owner, candidate.IntentID, candidate.ReservationID, candidate.ProviderRequestID, candidate.Signature,
		candidate.approvalHash, candidate.LastValidBlockHeight, candidate.ClaimedAt)
	attempt, err := scanReceipt(row)
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
		return attempt, err == nil, err
	}
	attempt, err = db.lookup(ctx, candidate.Owner, candidate.IntentID)
	if errors.Is(err, ErrNotFound) {
		return record{}, false, ErrConflict
	}
	return attempt, false, err
}

func (db *Postgres) observe(ctx context.Context, attempt record, observed jupiter.ExecuteObservation, at time.Time) (record, error) {
	var slot *string
	if observed.Slot != nil {
		slot = new(strconv.FormatUint(*observed.Slot, 10))
	}
	row := db.pool.QueryRow(ctx, `UPDATE jupiter_execution_attempts
		SET provider_status=$1, provider_code=$2, provider_slot=$3, provider_observed_at=$4
		WHERE owner=$5 AND intent_id=$6 AND approval_hash=$7 AND provider_observed_at IS NULL
		RETURNING `+receiptColumns,
		observed.Status, observed.Code, slot, at, attempt.Owner, attempt.IntentID, attempt.approvalHash)
	return scanReceipt(row)
}

func scanReceipt(row pgx.Row) (record, error) {
	r := record{Receipt: Receipt{SchemaVersion: "1", Network: quotes.SolanaMainnet}}
	var slot *string
	err := row.Scan(&r.Owner, &r.IntentID, &r.ReservationID, &r.ProviderRequestID, &r.Signature,
		&r.approvalHash, &r.LastValidBlockHeight, &r.ProviderStatus, &r.ProviderCode,
		&slot, &r.ClaimedAt, &r.ProviderObservedAt)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return record{}, ErrNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return record{}, ErrConflict
	case err != nil:
		return record{}, ErrUnavailable
	}
	if slot != nil {
		value, err := strconv.ParseUint(*slot, 10, 64)
		if err != nil {
			return record{}, ErrUnavailable
		}
		r.ProviderSlot = &value
	}
	r.ClaimedAt = r.ClaimedAt.UTC()
	if r.ProviderObservedAt != nil {
		r.ProviderObservedAt = new(r.ProviderObservedAt.UTC())
	}
	return r, nil
}
