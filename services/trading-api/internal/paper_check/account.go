package paper_check

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/paper_memory"
)

var errReadEvents = errors.New("read paper events")

// A transaction-backed reader gives the projection the same MVCC snapshot as
// the materialized account and avoids opening a Store that runs migrations.
type paperEvents struct{ tx pgx.Tx }

func (p paperEvents) Events(ctx context.Context, accountID string, afterVersion int64, limit int) ([]accounts.Event, error) {
	rows, err := p.tx.Query(ctx, `SELECT event_id::text, account_version, kind, payload, recorded_at FROM paper_events
		WHERE account_id=$1 AND account_version>$2 ORDER BY account_version LIMIT $3`, accountID, afterVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errReadEvents, err)
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByPos[accounts.Event])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errReadEvents, err)
	}
	return events, nil
}

func checkAccount(ctx context.Context, tx pgx.Tx, unknownAfter time.Duration, r *report) error {
	var cash string
	err := tx.QueryRow(ctx, `SELECT version, trim_scale(cash_usd)::text FROM paper_accounts WHERE account_id=$1`, r.AccountID).
		Scan(&r.AccountVersion, &cash)
	if errors.Is(err, pgx.ErrNoRows) {
		r.add("paper_account_not_found")
		return nil
	}
	if err != nil {
		return err
	}
	projection, err := paper_memory.Load(ctx, paperEvents{tx: tx}, r.AccountID, r.CheckedAt)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	switch {
	case errors.Is(err, errReadEvents):
		return err
	case err != nil:
		r.add("ledger_projection_failed")
	default:
		r.ProjectionVersion = projection.AccountVersion
		if projection.AccountVersion != r.AccountVersion {
			r.add("ledger_version_mismatch")
		} else {
			if projection.CashUSD != cash {
				r.add("ledger_cash_mismatch")
			}
			if err := checkPositions(ctx, tx, projection.OpenLots, r); err != nil {
				return err
			}
		}
	}
	return checkOrders(ctx, tx, unknownAfter, r)
}

func checkPositions(ctx context.Context, tx pgx.Tx, lots []paper_memory.OpenLot, r *report) error {
	projected := make(map[accounts.Instrument]*big.Int)
	for _, lot := range lots {
		quantity, ok := new(big.Int).SetString(lot.QuantityRaw, 10)
		if !ok || quantity.Sign() <= 0 {
			r.add("ledger_projection_failed")
			return nil
		}
		if previous := projected[lot.Instrument]; previous != nil {
			quantity.Add(quantity, previous)
		}
		projected[lot.Instrument] = quantity
	}
	rows, err := tx.Query(ctx, `SELECT instrument_id, registry_version, decimals, quantity_raw::text FROM paper_positions
		WHERE account_id=$1 AND quantity_raw>0 ORDER BY instrument_id`, r.AccountID)
	if err != nil {
		return err
	}
	defer rows.Close()
	mismatches := make(map[string]bool)
	for rows.Next() {
		var instrument accounts.Instrument
		var quantity string
		if err := rows.Scan(&instrument.ID, &instrument.RegistryVersion, &instrument.Decimals, &quantity); err != nil {
			return err
		}
		expected := projected[instrument]
		if expected == nil || expected.String() != quantity {
			mismatches[instrument.ID] = true
		}
		delete(projected, instrument)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for instrument := range projected {
		mismatches[instrument.ID] = true
	}
	ids := make([]string, 0, len(mismatches))
	for id := range mismatches {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		r.Issues = append(r.Issues, issue{Code: "ledger_position_mismatch", InstrumentID: id})
	}
	return nil
}

func checkOrders(ctx context.Context, tx pgx.Tx, unknownAfter time.Duration, r *report) error {
	rows, err := tx.Query(ctx, `SELECT o.order_id::text, o.status, o.request->>'expires_at', e.recorded_at
		FROM paper_orders o LEFT JOIN paper_events e
		ON e.account_id=o.account_id AND e.account_version=o.account_version
		AND e.kind='paper_order.'||o.status AND e.payload->>'order_id'=o.order_id::text
		WHERE o.account_id=$1 AND o.status IN ('reserved', 'partially_filled', 'unknown') ORDER BY o.order_id`, r.AccountID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		var expires *string
		var recorded *time.Time
		if err := rows.Scan(&id, &status, &expires, &recorded); err != nil {
			return err
		}
		if expires == nil || recorded == nil || recorded.After(r.CheckedAt) {
			r.Issues = append(r.Issues, issue{Code: "paper_open_order_inconsistent", OrderID: id})
			continue
		}
		deadline, err := time.Parse(time.RFC3339Nano, *expires)
		if err != nil {
			r.Issues = append(r.Issues, issue{Code: "paper_open_order_inconsistent", OrderID: id})
			continue
		}
		if !r.CheckedAt.Before(deadline) {
			r.Issues = append(r.Issues, issue{Code: "paper_order_overdue", OrderID: id})
		}
		if status == "unknown" && r.CheckedAt.Sub(*recorded) >= unknownAfter {
			r.Issues = append(r.Issues, issue{Code: "paper_order_unknown_stuck", OrderID: id})
		}
	}
	return rows.Err()
}
