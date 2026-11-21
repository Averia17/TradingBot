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
		return r.QuantityRaw == "" && r.GrossUSD == "" && r.FeeUSD == "" && r.ObservedAt.IsZero()
	case "filled", "partially_filled":
		return (rawPattern.MatchString(r.QuantityRaw) || (r.Outcome == "filled" && r.QuantityRaw == "")) &&
			moneyPattern.MatchString(r.GrossUSD) && moneyPattern.MatchString(r.FeeUSD) && compareAmounts(r.GrossUSD, "0") > 0 && !r.ObservedAt.IsZero()
	default:
		return false
	}
}

func (r ResolutionRequest) isFill() bool {
	return r.Outcome == "filled" || r.Outcome == "partially_filled"
}

// Resolve records an observed paper outcome. A partial fill accounts only for
// executed units. Unknown retains the remaining reservation until reconciled.
func (s *Store) Resolve(ctx context.Context, r ResolutionRequest) (Order, bool, error) {
	return s.resolve(ctx, r, nil)
}

func (s *Store) resolve(ctx context.Context, r ResolutionRequest, proof *reconciliationProof) (Order, bool, error) {
	if !r.valid() {
		return Order{}, false, ErrInvalid
	}
	r.ObservedAt = r.ObservedAt.UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(struct {
		ResolutionRequest
		Reconciliation *reconciliationProof `json:"reconciliation,omitempty"`
	}{r, proof})
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
	var priorVersion int64
	var priorKind string
	err = tx.QueryRow(ctx, `SELECT request_hash, account_version, kind FROM paper_events WHERE account_id=$1 AND event_id=$2`,
		r.AccountID, r.EventID).Scan(&priorHash, &priorVersion, &priorKind)
	if err == nil {
		if !bytes.Equal(priorHash, hash[:]) || priorKind != "paper_order."+r.Outcome {
			return Order{}, false, ErrConflict
		}
		prior, _, err := readOrderAtVersion(ctx, tx, r.AccountID, r.OrderID, priorVersion, r.Outcome)
		return prior, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, fmt.Errorf("look up prior paper outcome: %w", err)
	}
	if proof != nil {
		if err := proof.check(ctx, tx, order); err != nil {
			return Order{}, false, err
		}
	}
	var fill *Fill
	if r.isFill() {
		quantity := r.QuantityRaw
		if quantity == "" {
			quantity = order.RemainingQuantityRaw
		}
		// The fill carries the inferred quantity. The hashed request still
		// has the original quantity field for legacy full-fill retries.
		fill = &Fill{EventID: r.EventID, QuantityRaw: quantity, GrossUSD: r.GrossUSD, FeeUSD: r.FeeUSD, ObservedAt: r.ObservedAt}
	}
	next, change, err := restoreOrderAccounting(order).resolve(r.Outcome, fill)
	if err != nil {
		return Order{}, false, err
	}
	if fill != nil {
		if err := validateObservedFill(ctx, tx, order, *fill); err != nil {
			return Order{}, false, err
		}
	}
	if next.status != "unknown" {
		if err := writeSettlement(ctx, tx, next, change); err != nil {
			return Order{}, false, err
		}
	}
	order.AccountVersion = version + 1
	if err := writeEvent(ctx, tx, r.AccountID, r.EventID, order.AccountVersion, "paper_order."+r.Outcome, hash[:], payload); err != nil {
		return Order{}, false, err
	}
	order.Status = next.status
	var analyticsFill *accountAnalyticsFill
	if fill != nil {
		if err := writeFill(ctx, tx, order, fill); err != nil {
			return Order{}, false, err
		}
		if err := writeFillOutbox(ctx, tx, order, *fill); err != nil {
			return Order{}, false, err
		}
		booked := accountFill(order.ReservationRequest, *fill)
		analyticsFill = &booked
		order.appendSettledFill(*fill, next.remaining.quantity)
	}
	if _, err := tx.Exec(ctx, `UPDATE paper_orders SET status=$3, account_version=$4 WHERE account_id=$1 AND order_id=$2`,
		r.AccountID, r.OrderID, order.Status, order.AccountVersion); err != nil {
		return Order{}, false, fmt.Errorf("update paper order: %w", err)
	}
	if err := advanceAccount(ctx, tx, r.AccountID, order.AccountVersion); err != nil {
		return Order{}, false, err
	}
	if err := captureAccountOutbox(ctx, tx, r.AccountID, r.EventID, order.AccountVersion, &order.Instrument, analyticsFill); err != nil {
		return Order{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, fmt.Errorf("commit paper outcome: %w", err)
	}
	return order, true, nil
}

func validateObservedFill(ctx context.Context, tx pgx.Tx, order Order, fill Fill) error {
	priceComparison := amount(fill.GrossUSD).Cmp(proportionalGross(order.ReservationRequest, rawUnits(fill.QuantityRaw)))
	if (order.Side == "BUY" && priceComparison > 0) || (order.Side == "SELL" && priceComparison < 0) {
		return ErrInvalid
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read paper accounting clock: %w", err)
	}
	if fill.ObservedAt.Before(order.CreatedAt) || fill.ObservedAt.After(now) || !fill.ObservedAt.Before(order.ExpiresAt) {
		return ErrInvalid
	}
	return nil
}

func writeSettlement(ctx context.Context, tx pgx.Tx, next orderAccounting, change balanceChange) error {
	order := next.request
	if order.Side == "BUY" {
		if err := updateOne(ctx, tx, `UPDATE paper_accounts SET
			reserved_cash_usd=reserved_cash_usd+$2::text::numeric,
			cash_usd=cash_usd+$3::text::numeric WHERE account_id=$1`,
			order.AccountID, change.reservedCash.FloatString(8), change.cash.FloatString(8)); err != nil {
			return err
		}
		if next.status == "cancelled" {
			return nil
		}
		return updateOne(ctx, tx, `INSERT INTO paper_positions (account_id, instrument_id, registry_version, decimals, quantity_raw)
			VALUES ($1,$2,$3,$4,$5::text::numeric) ON CONFLICT (account_id,instrument_id) DO UPDATE
			SET quantity_raw=paper_positions.quantity_raw+EXCLUDED.quantity_raw
			WHERE paper_positions.registry_version=EXCLUDED.registry_version AND paper_positions.decimals=EXCLUDED.decimals`,
			order.AccountID, order.Instrument.ID, order.Instrument.RegistryVersion, order.Instrument.Decimals, change.quantity.String())
	}
	if err := updateOne(ctx, tx, `UPDATE paper_positions SET reserved_quantity_raw=reserved_quantity_raw+$3::text::numeric,
		quantity_raw=quantity_raw+$4::text::numeric WHERE account_id=$1 AND instrument_id=$2`,
		order.AccountID, order.Instrument.ID, change.reservedQuantity.String(), change.quantity.String()); err != nil {
		return err
	}
	return updateOne(ctx, tx, `UPDATE paper_accounts SET cash_usd=cash_usd+$2::text::numeric WHERE account_id=$1`,
		order.AccountID, change.cash.FloatString(8))
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

func writeFill(ctx context.Context, tx pgx.Tx, order Order, fill *Fill) error {
	err := tx.QueryRow(ctx, `INSERT INTO paper_fills (account_id,order_id,event_id,gross_usd,fee_usd,observed_at,quantity_raw)
		VALUES ($1,$2,$3,$4::text::numeric,$5::text::numeric,$6,$7::text::numeric)
		RETURNING trim_scale(gross_usd)::text, trim_scale(fee_usd)::text`,
		order.AccountID, order.OrderID, fill.EventID, fill.GrossUSD, fill.FeeUSD, fill.ObservedAt, fill.QuantityRaw).Scan(&fill.GrossUSD, &fill.FeeUSD)
	if err != nil {
		return fmt.Errorf("store paper fill: %w", err)
	}
	return nil
}

type Event struct {
	ID             string          `json:"event_id"`
	AccountVersion int64           `json:"account_version"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload"`
	RecordedAt     time.Time       `json:"recorded_at"`
}

func (s *Store) Events(ctx context.Context, accountID string, afterVersion int64, limit int) ([]Event, error) {
	return readEvents(ctx, s.pool, accountID, afterVersion, limit)
}

type eventQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readEvents(ctx context.Context, db eventQuerier, accountID string, afterVersion int64, limit int) ([]Event, error) {
	if !uuidPattern.MatchString(accountID) || afterVersion < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := db.Query(ctx, `SELECT event_id::text,account_version,kind,payload,recorded_at FROM paper_events
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
