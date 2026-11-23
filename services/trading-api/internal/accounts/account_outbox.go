package accounts

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

const insertAccountOutboxSQL = `INSERT INTO paper_account_outbox
	(account_id, account_version, event_id, payload, created_at) VALUES ($1,$2,$3,$4,$5)`

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

func accountFill(order ReservationRequest, fill Fill) accountAnalyticsFill {
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
	var position Position
	if instrument != nil {
		position.Instrument = *instrument
	}
	err := tx.QueryRow(ctx, `SELECT e.kind, e.recorded_at, trim_scale(a.cash_usd)::text, trim_scale(a.reserved_cash_usd)::text,
		COALESCE(p.quantity_raw::text,''), COALESCE(p.reserved_quantity_raw::text,'')
		FROM paper_accounts a JOIN paper_events e ON e.account_id=a.account_id AND e.account_version=a.version
		LEFT JOIN paper_positions p ON p.account_id=a.account_id AND p.instrument_id=$4 AND p.registry_version=$5 AND p.decimals=$6
		WHERE a.account_id=$1 AND e.event_id=$2 AND a.version=$3`, accountID, eventID, version,
		position.ID, position.RegistryVersion, position.Decimals).
		Scan(&event.Kind, &event.RecordedAt, &event.CashUSD, &event.ReservedCashUSD, &position.QuantityRaw, &position.ReservedQuantityRaw)
	if err != nil {
		return fmt.Errorf("capture paper account version %d: %w", version, err)
	}
	if instrument != nil {
		if position.QuantityRaw == "" || position.ReservedQuantityRaw == "" {
			return fmt.Errorf("capture paper position version %d: %w", version, pgx.ErrNoRows)
		}
		event.Position = &position
	}
	return writeAccountOutbox(ctx, tx, event)
}

func writeAccountOutbox(ctx context.Context, tx pgx.Tx, event accountAnalyticsEvent) error {
	payload, err := encodeAccountOutboxEvent(event)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, insertAccountOutboxSQL,
		event.AccountID, event.AccountVersion, event.EventID, payload, event.RecordedAt)
	if err != nil {
		return fmt.Errorf("enqueue paper account analytics version %d: %w", event.AccountVersion, err)
	}
	return nil
}

func encodeAccountOutboxEvent(event accountAnalyticsEvent) ([]byte, error) {
	event.RecordedAt = event.RecordedAt.UTC()
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("encode paper account analytics event: %w", err)
	}
	return payload, nil
}
