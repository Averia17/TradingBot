package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrExpired      = errors.New("paper quote has expired")
	ErrStale        = errors.New("paper account version is stale")
	ErrInsufficient = errors.New("paper account has insufficient available cash or quantity")
	instrumentPart  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)
	rawPattern      = regexp.MustCompile(`^[1-9][0-9]{0,37}$`)
)

type ReservationRequest struct {
	AccountID       string     `json:"account_id"`
	OrderID         string     `json:"order_id"`
	DecisionID      string     `json:"decision_id"`
	QuoteID         string     `json:"quote_id"`
	SnapshotVersion int64      `json:"snapshot_version"`
	Instrument      Instrument `json:"instrument"`
	Side            string     `json:"side"`
	QuantityRaw     string     `json:"quantity_raw"`
	GrossUSD        string     `json:"gross_usd"`
	MaxFeeUSD       string     `json:"max_fee_usd"`
	ExpiresAt       time.Time  `json:"expires_at"`
}

type ResolutionRequest struct {
	AccountID  string    `json:"account_id"`
	OrderID    string    `json:"order_id"`
	EventID    string    `json:"event_id"`
	Outcome    string    `json:"outcome"`
	GrossUSD   string    `json:"gross_usd,omitempty"`
	FeeUSD     string    `json:"fee_usd,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

type Fill struct {
	EventID    string    `json:"event_id"`
	GrossUSD   string    `json:"gross_usd"`
	FeeUSD     string    `json:"fee_usd"`
	ObservedAt time.Time `json:"observed_at"`
}

type Order struct {
	ReservationRequest
	Status         string    `json:"status"`
	AccountVersion int64     `json:"account_version"`
	CreatedAt      time.Time `json:"created_at"`
	Fill           *Fill     `json:"fill,omitempty"`
}

func (r ReservationRequest) valid() bool {
	return uuidPattern.MatchString(r.AccountID) && uuidPattern.MatchString(r.OrderID) && r.OrderID != r.AccountID &&
		uuidPattern.MatchString(r.DecisionID) && uuidPattern.MatchString(r.QuoteID) && r.SnapshotVersion >= 1 &&
		instrumentPart.MatchString(r.Instrument.ID) && instrumentPart.MatchString(r.Instrument.RegistryVersion) &&
		r.Instrument.Decimals >= 0 && r.Instrument.Decimals <= 18 && (r.Side == "BUY" || r.Side == "SELL") &&
		rawPattern.MatchString(r.QuantityRaw) && moneyPattern.MatchString(r.GrossUSD) && moneyPattern.MatchString(r.MaxFeeUSD) &&
		compareAmounts(r.GrossUSD, "0") > 0 && (r.Side != "SELL" || compareAmounts(r.MaxFeeUSD, r.GrossUSD) <= 0) && !r.ExpiresAt.IsZero()
}

func compareAmounts(left, right string) int {
	a, _ := new(big.Rat).SetString(left)
	b, _ := new(big.Rat).SetString(right)
	return a.Cmp(b)
}

const orderQuery = `SELECT o.request, o.request_hash, o.status, o.account_version, o.created_at,
	CASE WHEN f.event_id IS NOT NULL THEN jsonb_build_object(
		'event_id', f.event_id::text, 'gross_usd', trim_scale(f.gross_usd)::text,
		'fee_usd', trim_scale(f.fee_usd)::text, 'observed_at', f.observed_at
	) END
	FROM paper_orders o
	LEFT JOIN paper_fills f USING (account_id, order_id)
	WHERE o.account_id=$1 AND o.order_id=$2`

func (s *Store) GetOrder(ctx context.Context, accountID, orderID string) (Order, error) {
	if !uuidPattern.MatchString(accountID) || !uuidPattern.MatchString(orderID) {
		return Order{}, ErrInvalid
	}
	order, _, err := readOrder(ctx, s.pool, accountID, orderID)
	return order, err
}

func readOrder(ctx context.Context, db rowQuerier, accountID, orderID string) (Order, []byte, error) {
	var order Order
	var request, hash, fill []byte
	err := db.QueryRow(ctx, orderQuery, accountID, orderID).Scan(&request, &hash, &order.Status, &order.AccountVersion, &order.CreatedAt, &fill)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, nil, ErrNotFound
	}
	if err != nil {
		return Order{}, nil, fmt.Errorf("read paper order: %w", err)
	}
	if err := json.Unmarshal(request, &order.ReservationRequest); err != nil {
		return Order{}, nil, fmt.Errorf("decode stored paper order: %w", err)
	}
	payload, err := json.Marshal(order.ReservationRequest)
	if err != nil {
		return Order{}, nil, fmt.Errorf("encode stored paper order: %w", err)
	}
	expected := sha256.Sum256(payload)
	if order.AccountID != accountID || order.OrderID != orderID || !bytes.Equal(expected[:], hash) || !order.ReservationRequest.valid() {
		return Order{}, nil, errors.New("stored paper order does not match its authorized request")
	}
	if len(fill) != 0 {
		if err := json.Unmarshal(fill, &order.Fill); err != nil {
			return Order{}, nil, fmt.Errorf("decode stored paper fill: %w", err)
		}
		order.Fill.ObservedAt = order.Fill.ObservedAt.UTC()
	}
	order.CreatedAt = order.CreatedAt.UTC()
	return order, hash, nil
}

func lockAccount(ctx context.Context, tx pgx.Tx, accountID string) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `SELECT version FROM paper_accounts WHERE account_id=$1 FOR UPDATE`, accountID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lock paper account: %w", err)
	}
	return version, nil
}

// Reserve admits an order for virtual cash or already held instrument units.
func (s *Store) Reserve(ctx context.Context, r ReservationRequest) (Order, bool, error) {
	if !r.valid() {
		return Order{}, false, ErrInvalid
	}
	r.ExpiresAt = r.ExpiresAt.UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(r)
	if err != nil {
		return Order{}, false, fmt.Errorf("encode paper reservation: %w", err)
	}
	hash := sha256.Sum256(payload)
	tx, err := s.write(ctx)
	if err != nil {
		return Order{}, false, err
	}
	defer rollback(tx)
	version, err := lockAccount(ctx, tx, r.AccountID)
	if err != nil {
		return Order{}, false, err
	}
	if previous, previousHash, err := readOrder(ctx, tx, r.AccountID, r.OrderID); !errors.Is(err, ErrNotFound) {
		if err == nil && !bytes.Equal(previousHash, hash[:]) {
			return Order{}, false, ErrConflict
		}
		return previous, false, err
	}
	if version != r.SnapshotVersion {
		return Order{}, false, ErrStale
	}
	if err := reserveFunds(ctx, tx, r); err != nil {
		return Order{}, false, err
	}
	order := Order{ReservationRequest: r, Status: "reserved", AccountVersion: version + 1}
	var orderInserted bool
	// The deadline is checked when recording the order, after all waits for locks.
	err = tx.QueryRow(ctx, `WITH measured AS MATERIALIZED (SELECT clock_timestamp() AS t),
		stored AS (INSERT INTO paper_orders (account_id, order_id, decision_id, quote_id, request, request_hash, status, account_version, created_at)
		SELECT $1,$2,$3,$4,$5::jsonb,$6,'reserved',$7,t FROM measured WHERE t < $8::timestamptz
		ON CONFLICT DO NOTHING RETURNING created_at)
		SELECT t, EXISTS (SELECT 1 FROM stored) FROM measured`, r.AccountID, r.OrderID, r.DecisionID, r.QuoteID,
		payload, hash[:], order.AccountVersion, r.ExpiresAt).Scan(&order.CreatedAt, &orderInserted)
	if err != nil {
		return Order{}, false, fmt.Errorf("record paper order: %w", err)
	}
	if !order.CreatedAt.Before(r.ExpiresAt) {
		return Order{}, false, ErrExpired
	}
	if !orderInserted {
		return Order{}, false, ErrConflict
	}
	if err := advanceAccount(ctx, tx, r.AccountID, order.AccountVersion); err != nil {
		return Order{}, false, err
	}
	if err := writeEvent(ctx, tx, r.AccountID, r.OrderID, order.AccountVersion, "paper_order.reserved", hash[:], payload); err != nil {
		return Order{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, fmt.Errorf("commit paper reservation: %w", err)
	}
	order.CreatedAt = order.CreatedAt.UTC()
	return order, true, nil
}

func reserveFunds(ctx context.Context, tx pgx.Tx, r ReservationRequest) error {
	var existingVersion string
	var existingDecimals int
	err := tx.QueryRow(ctx, `SELECT registry_version, decimals FROM paper_positions WHERE account_id=$1 AND instrument_id=$2`, r.AccountID, r.Instrument.ID).
		Scan(&existingVersion, &existingDecimals)
	if err == nil && (existingVersion != r.Instrument.RegistryVersion || existingDecimals != r.Instrument.Decimals) {
		return ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check held paper instrument: %w", err)
	}
	if r.Side == "BUY" && errors.Is(err, pgx.ErrNoRows) {
		// A pending buy binds its metadata before the first fill. Concurrent
		// orders cannot reserve the same instrument with different raw units.
		if _, err := tx.Exec(ctx, `INSERT INTO paper_positions (account_id, instrument_id, registry_version, decimals, quantity_raw)
			VALUES ($1, $2, $3, $4, 0)`, r.AccountID, r.Instrument.ID, r.Instrument.RegistryVersion, r.Instrument.Decimals); err != nil {
			return fmt.Errorf("bind paper instrument metadata: %w", err)
		}
	}
	var rows int64
	if r.Side == "BUY" {
		command, err := tx.Exec(ctx, `UPDATE paper_accounts SET reserved_cash_usd = reserved_cash_usd + $2::text::numeric + $3::text::numeric
			WHERE account_id=$1 AND cash_usd-reserved_cash_usd >= $2::text::numeric + $3::text::numeric`, r.AccountID, r.GrossUSD, r.MaxFeeUSD)
		if err != nil {
			return fmt.Errorf("reserve paper cash: %w", err)
		}
		rows = command.RowsAffected()
	} else {
		command, err := tx.Exec(ctx, `UPDATE paper_positions SET reserved_quantity_raw=reserved_quantity_raw+$3::text::numeric
			WHERE account_id=$1 AND instrument_id=$2 AND quantity_raw-reserved_quantity_raw >= $3::text::numeric`, r.AccountID, r.Instrument.ID, r.QuantityRaw)
		if err != nil {
			return fmt.Errorf("reserve paper quantity: %w", err)
		}
		rows = command.RowsAffected()
	}
	if rows == 0 {
		return ErrInsufficient
	}
	return nil
}

func advanceAccount(ctx context.Context, tx pgx.Tx, id string, version int64) error {
	if _, err := tx.Exec(ctx, `UPDATE paper_accounts SET version=$2 WHERE account_id=$1`, id, version); err != nil {
		return fmt.Errorf("update paper account version: %w", err)
	}
	return nil
}
