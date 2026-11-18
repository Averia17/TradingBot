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
	ResearchJobID   string     `json:"research_job_id,omitempty"`
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
	AccountID   string    `json:"account_id"`
	OrderID     string    `json:"order_id"`
	EventID     string    `json:"event_id"`
	Outcome     string    `json:"outcome"`
	QuantityRaw string    `json:"quantity_raw,omitempty"`
	GrossUSD    string    `json:"gross_usd,omitempty"`
	FeeUSD      string    `json:"fee_usd,omitempty"`
	ObservedAt  time.Time `json:"observed_at,omitzero"`
}

type Fill struct {
	EventID     string    `json:"event_id"`
	QuantityRaw string    `json:"quantity_raw"`
	GrossUSD    string    `json:"gross_usd"`
	FeeUSD      string    `json:"fee_usd"`
	ObservedAt  time.Time `json:"observed_at"`
}

type Order struct {
	ReservationRequest
	Status               string    `json:"status"`
	AccountVersion       int64     `json:"account_version"`
	CreatedAt            time.Time `json:"created_at"`
	FilledQuantityRaw    string    `json:"filled_quantity_raw"`
	RemainingQuantityRaw string    `json:"remaining_quantity_raw"`
	Fill                 *Fill     `json:"fill,omitempty"`
	Fills                []Fill    `json:"fills"`
}

func (r ReservationRequest) valid() bool {
	return uuidPattern.MatchString(r.AccountID) && uuidPattern.MatchString(r.OrderID) && r.OrderID != r.AccountID &&
		uuidPattern.MatchString(r.DecisionID) && uuidPattern.MatchString(r.QuoteID) && r.SnapshotVersion >= 1 &&
		(r.ResearchJobID == "" || uuidPattern.MatchString(r.ResearchJobID)) &&
		instrumentPart.MatchString(r.Instrument.ID) && instrumentPart.MatchString(r.Instrument.RegistryVersion) &&
		r.Instrument.Decimals >= 0 && r.Instrument.Decimals <= 18 && (r.Side == "BUY" || r.Side == "SELL") &&
		rawPattern.MatchString(r.QuantityRaw) && moneyPattern.MatchString(r.GrossUSD) && moneyPattern.MatchString(r.MaxFeeUSD) &&
		compareAmounts(r.GrossUSD, "0") > 0 && (r.Side != "SELL" || compareAmounts(r.MaxFeeUSD, r.GrossUSD) <= 0) && !r.ExpiresAt.IsZero()
}

func compareAmounts(left, right string) int {
	return amount(left).Cmp(amount(right))
}

func amount(value string) *big.Rat {
	parsed, _ := new(big.Rat).SetString(value)
	return parsed
}

func rawUnits(value string) *big.Int {
	units, _ := new(big.Int).SetString(value, 10)
	return units
}

const orderQuery = `SELECT o.request, o.request_hash, o.status, o.account_version, o.created_at,
	COALESCE((SELECT jsonb_agg(jsonb_build_object(
		'event_id', f.event_id::text, 'quantity_raw', f.quantity_raw::text,
		'gross_usd', trim_scale(f.gross_usd)::text,
		'fee_usd', trim_scale(f.fee_usd)::text, 'observed_at', f.observed_at
	) ORDER BY e.account_version)
	FROM paper_fills f JOIN paper_events e USING (account_id, event_id)
	WHERE (f.account_id, f.order_id)=(o.account_id, o.order_id)
		AND ($3::bigint=0 OR e.account_version <= $3)), '[]'::jsonb)
	FROM paper_orders o
	WHERE o.account_id=$1 AND o.order_id=$2`

func (s *Store) GetOrder(ctx context.Context, accountID, orderID string) (Order, error) {
	if !uuidPattern.MatchString(accountID) || !uuidPattern.MatchString(orderID) {
		return Order{}, ErrInvalid
	}
	order, _, err := readOrder(ctx, s.pool, accountID, orderID)
	return order, err
}

func readOrder(ctx context.Context, db rowQuerier, accountID, orderID string) (Order, []byte, error) {
	return readOrderAt(ctx, db, accountID, orderID, 0, "")
}

func readOrderAt(ctx context.Context, db rowQuerier, accountID, orderID string, version int64, status string) (Order, []byte, error) {
	var order Order
	var request, hash, fills []byte
	err := db.QueryRow(ctx, orderQuery, accountID, orderID, version).Scan(&request, &hash, &order.Status, &order.AccountVersion, &order.CreatedAt, &fills)
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
	if err := json.Unmarshal(fills, &order.Fills); err != nil {
		return Order{}, nil, fmt.Errorf("decode stored paper fills: %w", err)
	}
	order.CreatedAt = order.CreatedAt.UTC()
	if version != 0 {
		order.AccountVersion, order.Status = version, status
	}
	if err := order.summarizeFills(); err != nil {
		return Order{}, nil, err
	}
	return order, hash, nil
}

func (o *Order) summarizeFills() error {
	var total big.Int
	for i := range o.Fills {
		fill := &o.Fills[i]
		if !rawPattern.MatchString(fill.QuantityRaw) || !moneyPattern.MatchString(fill.GrossUSD) ||
			compareAmounts(fill.GrossUSD, "0") <= 0 || !moneyPattern.MatchString(fill.FeeUSD) || fill.ObservedAt.IsZero() {
			return errors.New("invalid stored paper fill")
		}
		total.Add(&total, rawUnits(fill.QuantityRaw))
		fill.ObservedAt = fill.ObservedAt.UTC()
	}
	remaining := new(big.Int).Sub(rawUnits(o.QuantityRaw), &total)
	if remaining.Sign() < 0 || (o.Status == "filled") != (remaining.Sign() == 0) ||
		(o.Status == "reserved" && total.Sign() != 0) || (o.Status == "partially_filled" && total.Sign() == 0) {
		return errors.New("paper fill quantity does not match its order state")
	}
	o.FilledQuantityRaw, o.RemainingQuantityRaw = total.String(), remaining.String()
	if len(o.Fills) > 0 {
		o.Fill = &o.Fills[len(o.Fills)-1]
	}
	return nil
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
	return s.reserve(ctx, r, nil)
}

// ReserveApplication fixes the exact automatic quote in the same transaction
// that admits the order. An uncertain acknowledgement can be replayed from it.
func (s *Store) ReserveApplication(ctx context.Context, r ReservationRequest, input ApplicationInput) (Order, bool, error) {
	if !validApplication(input) || len(input.Quote) == 0 || input.ReserveBefore.IsZero() || r.AccountID != input.AccountID ||
		r.ResearchJobID != input.JobID || r.SnapshotVersion != input.SnapshotVersion {
		return Order{}, false, ErrInvalid
	}
	return s.reserve(ctx, r, &input)
}

func (s *Store) reserve(ctx context.Context, r ReservationRequest, application *ApplicationInput) (Order, bool, error) {
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
		if err != nil {
			return Order{}, false, err
		}
		if !bytes.Equal(previousHash, hash[:]) {
			return Order{}, false, ErrConflict
		}
		if application != nil {
			if _, err := bindApplication(ctx, tx, *application, r.OrderID, nil); err != nil {
				return Order{}, false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return Order{}, false, fmt.Errorf("commit paper application binding: %w", err)
			}
		}
		return previous, false, nil
	}
	if version != r.SnapshotVersion {
		return Order{}, false, ErrStale
	}
	if err := reserveFunds(ctx, tx, r); err != nil {
		return Order{}, false, err
	}
	order := Order{ReservationRequest: r, Status: "reserved", AccountVersion: version + 1,
		FilledQuantityRaw: "0", RemainingQuantityRaw: r.QuantityRaw, Fills: []Fill{}}
	var orderInserted bool
	deadline := r.ExpiresAt
	if application != nil && application.ReserveBefore.Before(deadline) {
		deadline = application.ReserveBefore.UTC()
	}
	// The deadline is checked when recording the order, after all waits for locks.
	err = tx.QueryRow(ctx, `WITH measured AS MATERIALIZED (SELECT clock_timestamp() AS t),
		stored AS (INSERT INTO paper_orders (account_id, order_id, decision_id, quote_id, request, request_hash, status, account_version, created_at)
		SELECT $1,$2,$3,$4,$5::jsonb,$6,'reserved',$7,t FROM measured WHERE t < $8::timestamptz
		ON CONFLICT DO NOTHING RETURNING created_at)
		SELECT t, EXISTS (SELECT 1 FROM stored) FROM measured`, r.AccountID, r.OrderID, r.DecisionID, r.QuoteID,
		payload, hash[:], order.AccountVersion, deadline).Scan(&order.CreatedAt, &orderInserted)
	if err != nil {
		return Order{}, false, fmt.Errorf("record paper order: %w", err)
	}
	if !order.CreatedAt.Before(deadline) {
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
	if application != nil {
		if _, err := bindApplication(ctx, tx, *application, r.OrderID, nil); err != nil {
			return Order{}, false, err
		}
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
