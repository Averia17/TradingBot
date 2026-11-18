package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type accountAnalyticsEvent struct {
	Version         string                `json:"version"`
	AccountID       string                `json:"account_id"`
	EventID         string                `json:"event_id"`
	AccountVersion  int64                 `json:"account_version"`
	Kind            string                `json:"kind"`
	RecordedAt      time.Time             `json:"recorded_at"`
	CashUSD         string                `json:"cash_usd"`
	ReservedCashUSD string                `json:"reserved_cash_usd"`
	Position        *Position             `json:"position,omitempty"`
	Fill            *accountAnalyticsFill `json:"fill,omitempty"`
}

type accountAnalyticsFill struct {
	Side          string    `json:"side"`
	QuantityRaw   string    `json:"quantity_raw"`
	GrossUSD      string    `json:"gross_usd"`
	FeeUSD        string    `json:"fee_usd"`
	ObservedAt    time.Time `json:"observed_at"`
	OrderID       string    `json:"order_id"`
	DecisionID    string    `json:"decision_id"`
	QuoteID       string    `json:"quote_id"`
	ResearchJobID string    `json:"research_job_id,omitempty"`
}

func accountFill(order Order, fill Fill) accountAnalyticsFill {
	return accountAnalyticsFill{
		Side: order.Side, QuantityRaw: fill.QuantityRaw, GrossUSD: fill.GrossUSD, FeeUSD: fill.FeeUSD,
		ObservedAt: fill.ObservedAt.UTC(), OrderID: order.OrderID, DecisionID: order.DecisionID,
		QuoteID: order.QuoteID, ResearchJobID: order.ResearchJobID,
	}
}

// Capture balances after the authoritative ledger update while the writer
// still owns the transaction. No Kafka dependency participates in the write.
func captureAccountOutbox(ctx context.Context, tx pgx.Tx, accountID, eventID string, version int64, instrument *Instrument, fill *accountAnalyticsFill) error {
	event := accountAnalyticsEvent{
		Version: "1.0", AccountID: accountID, EventID: eventID, AccountVersion: version, Fill: fill,
	}
	err := tx.QueryRow(ctx, `SELECT e.kind, e.recorded_at, trim_scale(a.cash_usd)::text, trim_scale(a.reserved_cash_usd)::text
		FROM paper_accounts a JOIN paper_events e ON e.account_id=a.account_id AND e.account_version=a.version
		WHERE a.account_id=$1 AND e.event_id=$2 AND a.version=$3`, accountID, eventID, version).
		Scan(&event.Kind, &event.RecordedAt, &event.CashUSD, &event.ReservedCashUSD)
	if err != nil {
		return fmt.Errorf("capture paper account version %d: %w", version, err)
	}
	if instrument != nil {
		position := Position{Instrument: *instrument}
		err := tx.QueryRow(ctx, `SELECT quantity_raw::text, reserved_quantity_raw::text FROM paper_positions
			WHERE account_id=$1 AND instrument_id=$2 AND registry_version=$3 AND decimals=$4`,
			accountID, instrument.ID, instrument.RegistryVersion, instrument.Decimals).
			Scan(&position.QuantityRaw, &position.ReservedQuantityRaw)
		if err != nil {
			return fmt.Errorf("capture paper position version %d: %w", version, err)
		}
		event.Position = &position
	}
	return writeAccountOutbox(ctx, tx, event)
}

func writeAccountOutbox(ctx context.Context, tx pgx.Tx, event accountAnalyticsEvent) error {
	event.RecordedAt = event.RecordedAt.UTC()
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode paper account analytics event: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO paper_account_outbox
		(account_id, account_version, event_id, payload, created_at) VALUES ($1,$2,$3,$4,$5)`,
		event.AccountID, event.AccountVersion, event.EventID, payload, event.RecordedAt)
	if err != nil {
		return fmt.Errorf("enqueue paper account analytics version %d: %w", event.AccountVersion, err)
	}
	return nil
}
