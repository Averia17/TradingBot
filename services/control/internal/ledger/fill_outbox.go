package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type paperFillNotification struct {
	Version         string    `json:"version"`
	ID              string    `json:"id"`
	Mode            string    `json:"mode"`
	Source          string    `json:"source"`
	AccountID       string    `json:"account_id"`
	OrderID         string    `json:"order_id"`
	DecisionID      string    `json:"decision_id"`
	QuoteID         string    `json:"quote_id"`
	ResearchJobID   string    `json:"research_job_id,omitempty"`
	FillEventID     string    `json:"fill_event_id"`
	InstrumentID    string    `json:"instrument_id"`
	RegistryVersion string    `json:"registry_version"`
	Decimals        int       `json:"decimals"`
	Side            string    `json:"side"`
	QuantityRaw     string    `json:"quantity_raw"`
	GrossUSD        string    `json:"gross_usd"`
	FeeUSD          string    `json:"fee_usd"`
	Currency        string    `json:"currency"`
	FilledAt        time.Time `json:"filled_at"`
	RecordedAt      time.Time `json:"recorded_at"`
}

// The notification stores the booked gross and raw quantity, so a repeating
// unit price can never require rounding or block an otherwise valid fill.
func writeFillOutbox(ctx context.Context, tx pgx.Tx, order Order, fill Fill) error {
	var recordedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT recorded_at FROM paper_events WHERE account_id=$1 AND event_id=$2`,
		order.AccountID, fill.EventID).Scan(&recordedAt); err != nil {
		return fmt.Errorf("read paper fill recording time: %w", err)
	}
	payload, err := json.Marshal(paperFillNotification{
		Version: "1.1", ID: "paper:" + order.AccountID + ":" + fill.EventID, Mode: "paper", Source: "paper_ledger",
		AccountID: order.AccountID, OrderID: order.OrderID, DecisionID: order.DecisionID, QuoteID: order.QuoteID,
		ResearchJobID: order.ResearchJobID, FillEventID: fill.EventID,
		InstrumentID: order.Instrument.ID, RegistryVersion: order.Instrument.RegistryVersion, Decimals: order.Instrument.Decimals,
		Side: order.Side, QuantityRaw: fill.QuantityRaw, GrossUSD: fill.GrossUSD, FeeUSD: fill.FeeUSD, Currency: "USD",
		FilledAt: fill.ObservedAt.UTC(), RecordedAt: recordedAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("encode paper fill notification: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_fill_outbox (account_id,event_id,payload,created_at) VALUES ($1,$2,$3,$4)`,
		order.AccountID, fill.EventID, payload, recordedAt); err != nil {
		return fmt.Errorf("enqueue paper fill notification: %w", err)
	}
	return nil
}
