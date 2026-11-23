package accounts

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	ReconciliationVersion = "0.1"
	ReconciliationSource  = "synthetic_fixture"
)

// ReconciliationObservation is a local paper venue observation. Its ID becomes
// the corresponding ledger event ID and its original bytes are pinned by hash.
type ReconciliationObservation struct {
	Version       string    `json:"paper_observation_schema_version"`
	Source        string    `json:"source_mode"`
	AccountID     string    `json:"account_id"`
	OrderID       string    `json:"order_id"`
	QuoteID       string    `json:"quote_id"`
	ObservationID string    `json:"observation_id"`
	Outcome       string    `json:"outcome"`
	ObservedAt    time.Time `json:"observed_at"`
	ReceivedAt    time.Time `json:"received_at"`
	QuantityRaw   string    `json:"quantity_raw,omitempty"`
	GrossUSD      string    `json:"gross_usd,omitempty"`
	FeeUSD        string    `json:"fee_usd,omitempty"`
}

type reconciliationProof struct {
	Source     string    `json:"source_mode"`
	SHA256     string    `json:"observation_sha256"`
	QuoteID    string    `json:"quote_id"`
	ObservedAt time.Time `json:"observed_at"`
	ReceivedAt time.Time `json:"received_at"`
}

func (s *Store) Reconcile(ctx context.Context, observation ReconciliationObservation, digest [32]byte) (Order, bool, error) {
	if observation.Version != ReconciliationVersion || observation.Source != ReconciliationSource || digest == [32]byte{} ||
		!uuidPattern.MatchString(observation.QuoteID) || observation.ObservedAt.IsZero() || observation.ReceivedAt.IsZero() ||
		observation.ObservedAt.Nanosecond()%1000 != 0 || observation.ReceivedAt.Nanosecond()%1000 != 0 ||
		observation.ReceivedAt.Before(observation.ObservedAt) {
		return Order{}, false, ErrInvalid
	}
	switch observation.Outcome {
	case "filled", "partially_filled":
		if observation.QuantityRaw == "" {
			return Order{}, false, ErrInvalid
		}
	case "cancelled":
	default:
		return Order{}, false, ErrInvalid
	}
	proof := reconciliationProof{
		Source: observation.Source, SHA256: hex.EncodeToString(digest[:]), QuoteID: observation.QuoteID,
		ObservedAt: observation.ObservedAt.UTC(), ReceivedAt: observation.ReceivedAt.UTC(),
	}
	request := ResolutionRequest{
		AccountID: observation.AccountID, OrderID: observation.OrderID, EventID: observation.ObservationID,
		Outcome: observation.Outcome, QuantityRaw: observation.QuantityRaw, GrossUSD: observation.GrossUSD, FeeUSD: observation.FeeUSD,
	}
	if request.isFill() {
		request.ObservedAt = proof.ObservedAt
	}
	return s.resolve(ctx, request, &proof)
}

func (p reconciliationProof) check(ctx context.Context, tx pgx.Tx, order Order) error {
	if p.QuoteID != order.QuoteID || (order.Status != "unknown" && order.Status != "partially_filled") {
		return ErrConflict
	}
	if p.ObservedAt.Before(order.CreatedAt) {
		return ErrInvalid
	}
	for _, fill := range order.Fills {
		if p.ObservedAt.Before(fill.ObservedAt) {
			return ErrInvalid
		}
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read paper reconciliation clock: %w", err)
	}
	if p.ReceivedAt.After(now) {
		return ErrInvalid
	}
	return nil
}
