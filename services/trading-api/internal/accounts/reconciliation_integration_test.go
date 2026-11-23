package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestPinnedReconciliationSettlesOnlyObservedUnitsAcrossRestart(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "20")
	buy := reserveFixture(1, 1)
	buy.QuantityRaw, buy.GrossUSD, buy.MaxFeeUSD = "3", "12", "0.03"
	order, _, err := store.Reserve(ctx, buy)
	if err != nil {
		t.Fatal(err)
	}
	observation := ReconciliationObservation{
		Version: "0.1", Source: "synthetic_fixture",
		AccountID: testAccountID, OrderID: buy.OrderID, QuoteID: buy.QuoteID, ObservationID: identifier(51),
		Outcome: "partially_filled", QuantityRaw: "1", GrossUSD: "4", FeeUSD: "0.01",
		ObservedAt: order.CreatedAt, ReceivedAt: order.CreatedAt,
	}
	digest := sha256.Sum256([]byte("archived partial"))
	if _, _, err := store.Reconcile(ctx, observation, digest); !errors.Is(err, ErrConflict) {
		t.Fatalf("unobserved reservation accepted reconciliation: %v", err)
	}
	if _, _, err := store.Resolve(ctx, ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(50), Outcome: "unknown"}); err != nil {
		t.Fatal(err)
	}
	wrongQuote := observation
	wrongQuote.QuoteID = identifier(64)
	if _, _, err := store.Reconcile(ctx, wrongQuote, digest); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong quote accepted reconciliation: %v", err)
	}
	futureReceipt := observation
	futureReceipt.ReceivedAt = order.CreatedAt.Add(time.Hour)
	if _, _, err := store.Reconcile(ctx, futureReceipt, digest); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future observation receipt accepted: %v", err)
	}
	account, err := store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "20" || account.ReservedCashUSD != "12.03" {
		t.Fatalf("invalid observations changed reserves: %+v %v", account, err)
	}

	store.Close()
	store = openLedger(t, ctx, dsn)
	partial, created, err := store.Reconcile(ctx, observation, digest)
	if err != nil || !created || partial.Status != "partially_filled" || partial.RemainingQuantityRaw != "2" {
		t.Fatalf("partial reconciliation: %+v, %v, %v", partial, created, err)
	}
	cancellation := observation
	cancellation.Outcome, cancellation.ObservationID = "cancelled", identifier(52)
	cancellation.QuantityRaw, cancellation.GrossUSD, cancellation.FeeUSD = "", "", ""
	final, created, err := store.Reconcile(ctx, cancellation, sha256.Sum256([]byte("archived cancellation")))
	if err != nil || !created || final.Status != "cancelled" || len(final.Fills) != 1 {
		t.Fatalf("cancel residual: %+v, %v, %v", final, created, err)
	}
	replayed, created, err := store.Reconcile(ctx, observation, digest)
	if err != nil || created || !reflect.DeepEqual(replayed, partial) {
		t.Fatalf("changed partial receipt: %+v, %v, %v", replayed, created, err)
	}
	if _, _, err := store.Reconcile(ctx, observation, sha256.Sum256([]byte("changed bytes"))); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused observation ID with changed evidence: %v", err)
	}
	account, err = store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "15.99" || account.ReservedCashUSD != "0" || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "1" {
		t.Fatalf("account after cancellation: %+v, %v", account, err)
	}
	events, err := store.Events(ctx, testAccountID, 0, 10)
	if err != nil || len(events) != 5 {
		t.Fatalf("ledger event count: %d, %v", len(events), err)
	}
	var payload struct {
		Reconciliation *reconciliationProof `json:"reconciliation"`
	}
	if err := json.Unmarshal(events[3].Payload, &payload); err != nil || payload.Reconciliation == nil ||
		payload.Reconciliation.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("observation proof missing from fill event: %s (%v)", events[3].Payload, err)
	}
}
