package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (r ResolutionRequest) valid() bool {
	if !uuidPattern.MatchString(r.AccountID) || !uuidPattern.MatchString(r.OrderID) || !uuidPattern.MatchString(r.EventID) ||
		r.EventID == r.AccountID || r.EventID == r.OrderID {
		return false
	}
	switch r.Outcome {
	case "unknown", "cancelled":
		return r.GrossUSD == "" && r.FeeUSD == "" && r.ObservedAt.IsZero()
	case "filled":
		return moneyPattern.MatchString(r.GrossUSD) && moneyPattern.MatchString(r.FeeUSD) && compareAmounts(r.GrossUSD, "0") > 0 && !r.ObservedAt.IsZero()
	default:
		return false
	}
}

// Resolve records an observed paper outcome. Unknown keeps the reservation;
// only a final cancelled or full filled outcome releases it.
func (s *Store) Resolve(ctx context.Context, r ResolutionRequest) (Order, bool, error) {
	if !r.valid() {
		return Order{}, false, ErrInvalid
	}
	r.ObservedAt = r.ObservedAt.UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(r)
	if err != nil {
		return Order{}, false, fmt.Errorf("encode paper outcome: %w", err)
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
	order, _, err := readOrder(ctx, tx, r.AccountID, r.OrderID)
	if err != nil {
		return Order{}, false, err
	}
	var priorHash []byte
	err = tx.QueryRow(ctx, `SELECT request_hash FROM paper_events WHERE account_id=$1 AND event_id=$2`, r.AccountID, r.EventID).Scan(&priorHash)
	if err == nil {
		if !bytes.Equal(priorHash, hash[:]) {
			return Order{}, false, ErrConflict
		}
		return order, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, fmt.Errorf("look up prior paper outcome: %w", err)
	}
	if order.Status != "reserved" && (order.Status != "unknown" || r.Outcome == "unknown") {
		return Order{}, false, ErrConflict
	}
	if r.Outcome == "filled" {
		if err := validateFill(ctx, tx, order, r); err != nil {
			return Order{}, false, err
		}
	}
	if r.Outcome != "unknown" {
		if err := resolveFunds(ctx, tx, order, r); err != nil {
			return Order{}, false, err
		}
	}
	order.AccountVersion = version + 1
	if err := writeEvent(ctx, tx, r.AccountID, r.EventID, order.AccountVersion, "paper_order."+r.Outcome, hash[:], payload); err != nil {
		return Order{}, false, err
	}
	if r.Outcome == "filled" {
		order.Fill, err = writeFill(ctx, tx, r)
		if err != nil {
			return Order{}, false, err
		}
	}
	order.Status = r.Outcome
	if _, err := tx.Exec(ctx, `UPDATE paper_orders SET status=$3, account_version=$4 WHERE account_id=$1 AND order_id=$2`,
		r.AccountID, r.OrderID, order.Status, order.AccountVersion); err != nil {
		return Order{}, false, fmt.Errorf("update paper order: %w", err)
	}
	if err := advanceAccount(ctx, tx, r.AccountID, order.AccountVersion); err != nil {
		return Order{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, fmt.Errorf("commit paper outcome: %w", err)
	}
	return order, true, nil
}

func validateFill(ctx context.Context, tx pgx.Tx, order Order, r ResolutionRequest) error {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read paper accounting clock: %w", err)
	}
	if r.ObservedAt.Before(order.CreatedAt) || r.ObservedAt.After(now) || !r.ObservedAt.Before(order.ExpiresAt) ||
		compareAmounts(r.FeeUSD, order.MaxFeeUSD) > 0 ||
		(order.Side == "BUY" && compareAmounts(r.GrossUSD, order.GrossUSD) > 0) ||
		(order.Side == "SELL" && compareAmounts(r.GrossUSD, order.GrossUSD) < 0) {
		return ErrInvalid
	}
	return nil
}

func resolveFunds(ctx context.Context, tx pgx.Tx, order Order, r ResolutionRequest) error {
	if r.Outcome == "cancelled" {
		r.GrossUSD, r.FeeUSD = "0", "0"
	}
	if order.Side == "BUY" {
		if err := updateOne(ctx, tx, `UPDATE paper_accounts SET
			reserved_cash_usd=reserved_cash_usd-$2::text::numeric-$3::text::numeric,
			cash_usd=cash_usd-$4::text::numeric-$5::text::numeric WHERE account_id=$1`,
			r.AccountID, order.GrossUSD, order.MaxFeeUSD, r.GrossUSD, r.FeeUSD); err != nil {
			return err
		}
		if r.Outcome == "cancelled" {
			return nil
		}
		return updateOne(ctx, tx, `INSERT INTO paper_positions (account_id, instrument_id, registry_version, decimals, quantity_raw)
			VALUES ($1,$2,$3,$4,$5::text::numeric) ON CONFLICT (account_id,instrument_id) DO UPDATE
			SET quantity_raw=paper_positions.quantity_raw+EXCLUDED.quantity_raw
			WHERE paper_positions.registry_version=EXCLUDED.registry_version AND paper_positions.decimals=EXCLUDED.decimals`,
			r.AccountID, order.Instrument.ID, order.Instrument.RegistryVersion, order.Instrument.Decimals, order.QuantityRaw)
	}
	unitsDisposed := order.QuantityRaw
	if r.Outcome == "cancelled" {
		unitsDisposed = "0"
	}
	if err := updateOne(ctx, tx, `UPDATE paper_positions SET reserved_quantity_raw=reserved_quantity_raw-$3::text::numeric,
		quantity_raw=quantity_raw-$4::text::numeric WHERE account_id=$1 AND instrument_id=$2`,
		r.AccountID, order.Instrument.ID, order.QuantityRaw, unitsDisposed); err != nil {
		return err
	}
	return updateOne(ctx, tx, `UPDATE paper_accounts SET cash_usd=cash_usd+$2::text::numeric-$3::text::numeric WHERE account_id=$1`,
		r.AccountID, r.GrossUSD, r.FeeUSD)
}

func updateOne(ctx context.Context, tx pgx.Tx, query string, args ...any) error {
	command, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("apply paper accounting amounts: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func writeFill(ctx context.Context, tx pgx.Tx, r ResolutionRequest) (*Fill, error) {
	fill := &Fill{EventID: r.EventID, ObservedAt: r.ObservedAt}
	err := tx.QueryRow(ctx, `INSERT INTO paper_fills (account_id,order_id,event_id,gross_usd,fee_usd,observed_at)
		VALUES ($1,$2,$3,$4::text::numeric,$5::text::numeric,$6) RETURNING trim_scale(gross_usd)::text, trim_scale(fee_usd)::text`,
		r.AccountID, r.OrderID, r.EventID, r.GrossUSD, r.FeeUSD, r.ObservedAt).Scan(&fill.GrossUSD, &fill.FeeUSD)
	if err != nil {
		return nil, fmt.Errorf("store paper fill: %w", err)
	}
	return fill, nil
}

type Event struct {
	ID             string          `json:"event_id"`
	AccountVersion int64           `json:"account_version"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload"`
	RecordedAt     time.Time       `json:"recorded_at"`
}

func (s *Store) Events(ctx context.Context, accountID string, afterVersion int64, limit int) ([]Event, error) {
	if !uuidPattern.MatchString(accountID) || afterVersion < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT event_id::text,account_version,kind,payload,recorded_at FROM paper_events
		WHERE account_id=$1 AND account_version>$2 ORDER BY account_version LIMIT $3`, accountID, afterVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("query paper events: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.AccountVersion, &event.Kind, &event.Payload, &event.RecordedAt); err != nil {
			return nil, fmt.Errorf("read paper event: %w", err)
		}
		event.RecordedAt = event.RecordedAt.UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read paper events: %w", err)
	}
	return events, nil
}
