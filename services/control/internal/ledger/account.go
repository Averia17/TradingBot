package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	uuidPattern  = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	moneyPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,29})(?:\.[0-9]{1,8})?$`)
)

type CreateAccount struct {
	ID             string `json:"account_id"`
	InitialCashUSD string `json:"initial_cash_usd"`
}

type Instrument struct {
	ID              string `json:"instrument_id"`
	RegistryVersion string `json:"registry_version"`
	Decimals        int    `json:"decimals"`
}

type Position struct {
	Instrument
	QuantityRaw         string `json:"quantity_raw"`
	ReservedQuantityRaw string `json:"reserved_quantity_raw"`
}

type Account struct {
	ID              string     `json:"account_id"`
	Version         int64      `json:"version"`
	CashUSD         string     `json:"cash_usd"`
	ReservedCashUSD string     `json:"reserved_cash_usd"`
	Positions       []Position `json:"positions"`
	CreatedAt       time.Time  `json:"created_at"`
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

const accountQuery = `SELECT a.account_id::text, a.version, trim_scale(a.cash_usd)::text,
	trim_scale(a.reserved_cash_usd)::text, a.created_at,
	COALESCE((SELECT jsonb_agg(jsonb_build_object(
		'instrument_id', p.instrument_id, 'registry_version', p.registry_version, 'decimals', p.decimals,
		'quantity_raw', p.quantity_raw::text, 'reserved_quantity_raw', p.reserved_quantity_raw::text
	) ORDER BY p.instrument_id) FROM paper_positions p WHERE p.account_id = a.account_id AND p.quantity_raw > 0), '[]'::jsonb)
	FROM paper_accounts a WHERE a.account_id = $1`

func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	if !uuidPattern.MatchString(id) {
		return Account{}, ErrInvalid
	}
	return readAccount(ctx, s.pool, id)
}

func readAccount(ctx context.Context, db rowQuerier, id string) (Account, error) {
	var account Account
	var positions []byte
	err := db.QueryRow(ctx, accountQuery, id).Scan(&account.ID, &account.Version, &account.CashUSD, &account.ReservedCashUSD, &account.CreatedAt, &positions)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("read paper account: %w", err)
	}
	if err := json.Unmarshal(positions, &account.Positions); err != nil {
		return Account{}, fmt.Errorf("read paper positions: %w", err)
	}
	account.CreatedAt = account.CreatedAt.UTC()
	return account, nil
}

// CreateAccount initializes a virtual USD balance and its funding event atomically.
// A matching retry returns the current account without introducing new funds.
func (s *Store) CreateAccount(ctx context.Context, input CreateAccount) (Account, bool, error) {
	if !uuidPattern.MatchString(input.ID) || !moneyPattern.MatchString(input.InitialCashUSD) {
		return Account{}, false, ErrInvalid
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return Account{}, false, err
	}
	hash := sha256.Sum256(payload)
	tx, err := s.write(ctx)
	if err != nil {
		return Account{}, false, err
	}
	defer rollback(tx)
	command, err := tx.Exec(ctx, `INSERT INTO paper_accounts (account_id, creation_hash, cash_usd)
		VALUES ($1, $2, $3::text::numeric) ON CONFLICT (account_id) DO NOTHING`, input.ID, hash[:], input.InitialCashUSD)
	if err != nil {
		return Account{}, false, fmt.Errorf("initialize paper account: %w", err)
	}
	created := command.RowsAffected() == 1
	if created {
		if err := writeEvent(ctx, tx, input.ID, input.ID, 1, "paper_account.created", hash[:], payload); err != nil {
			return Account{}, false, err
		}
		if err := captureAccountOutbox(ctx, tx, input.ID, input.ID, 1, nil, nil); err != nil {
			return Account{}, false, err
		}
	} else {
		var storedHash []byte
		if err := tx.QueryRow(ctx, `SELECT creation_hash FROM paper_accounts WHERE account_id=$1`, input.ID).Scan(&storedHash); err != nil {
			return Account{}, false, fmt.Errorf("read paper account creation hash: %w", err)
		}
		if !bytes.Equal(hash[:], storedHash) {
			return Account{}, false, ErrConflict
		}
	}
	account, err := readAccount(ctx, tx, input.ID)
	if err != nil || !created {
		return account, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, false, fmt.Errorf("commit paper account: %w", err)
	}
	return account, true, nil
}

func writeEvent(ctx context.Context, tx pgx.Tx, accountID, eventID string, version int64, kind string, hash, payload []byte) error {
	command, err := tx.Exec(ctx, `INSERT INTO paper_events (account_id, event_id, account_version, kind, request_hash, payload)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb) ON CONFLICT (account_id, event_id) DO NOTHING`, accountID, eventID, version, kind, hash, payload)
	if err != nil {
		return fmt.Errorf("write paper event: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
