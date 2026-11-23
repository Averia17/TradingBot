package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RiskActivity binds settled UTC-day gross and outstanding order reservations
// to one account version. Settlement uses the ledger's own recorded time.
type RiskActivity struct {
	AccountID       string            `json:"account_id"`
	AccountVersion  int64             `json:"account_version"`
	DayUTC          time.Time         `json:"day_utc"`
	SettledGrossUSD string            `json:"settled_gross_usd"`
	HasUnknown      bool              `json:"has_unknown"`
	Pending         []RiskReservation `json:"pending"`
}

type RiskReservation struct {
	Instrument           Instrument `json:"instrument"`
	QuantityRaw          string     `json:"quantity_raw"`
	RemainingQuantityRaw string     `json:"remaining_quantity_raw"`
	GrossUSD             string     `json:"gross_usd"`
}

const readPendingOrdersSQL = storedOrderProjection + `
	WHERE o.account_id=$1 AND o.status IN ('reserved','partially_filled','unknown') ORDER BY o.order_id`

// RiskActivity reads one PostgreSQL snapshot. Reserve checks the same version
// under lock so later fills and orders invalidate a stale admission.
func (s *Store) RiskActivity(ctx context.Context, accountID string, expectedVersion int64) (RiskActivity, error) {
	if !uuidPattern.MatchString(accountID) || expectedVersion < 1 {
		return RiskActivity{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RiskActivity{}, fmt.Errorf("open paper risk snapshot: %w", err)
	}
	defer rollback(tx)

	activity := RiskActivity{AccountID: accountID}
	var now time.Time
	err = tx.QueryRow(ctx, `SELECT version, clock_timestamp() FROM paper_accounts WHERE account_id=$1`, accountID).
		Scan(&activity.AccountVersion, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return RiskActivity{}, ErrNotFound
	}
	if err != nil {
		return RiskActivity{}, fmt.Errorf("read paper risk account version: %w", err)
	}
	if activity.AccountVersion != expectedVersion {
		return RiskActivity{}, ErrStale
	}
	activity.DayUTC = now.UTC().Truncate(24 * time.Hour)
	err = tx.QueryRow(ctx, `SELECT trim_scale(COALESCE(sum(f.gross_usd),0))::text
		FROM paper_fills f JOIN paper_events e USING (account_id,event_id)
		WHERE f.account_id=$1 AND e.recorded_at >= $2 AND e.recorded_at < $3`,
		accountID, activity.DayUTC, activity.DayUTC.AddDate(0, 0, 1)).Scan(&activity.SettledGrossUSD)
	if err != nil {
		return RiskActivity{}, fmt.Errorf("read paper settled daily gross: %w", err)
	}

	rows, err := tx.Query(ctx, readPendingOrdersSQL, accountID, int64(0))
	if err != nil {
		return RiskActivity{}, fmt.Errorf("query pending paper reservations: %w", err)
	}
	defer rows.Close()
	activity.Pending = make([]RiskReservation, 0)
	for rows.Next() {
		order, _, err := scanStoredOrder(rows, accountID, 0, "")
		if err != nil {
			return RiskActivity{}, err
		}
		activity.HasUnknown = activity.HasUnknown || order.Status == "unknown"
		activity.Pending = append(activity.Pending, RiskReservation{
			Instrument: order.Instrument, QuantityRaw: order.QuantityRaw,
			RemainingQuantityRaw: order.RemainingQuantityRaw, GrossUSD: order.GrossUSD,
		})
	}
	if err := rows.Err(); err != nil {
		return RiskActivity{}, fmt.Errorf("read pending paper reservations: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RiskActivity{}, fmt.Errorf("finish paper risk snapshot: %w", err)
	}
	return activity, nil
}
