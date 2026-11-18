package papermemory

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"tradingbot/control/internal/ledger"
)

const testAccount = "00000000-0000-0000-0000-000000000001"

var (
	testTime       = time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	testInstrument = ledger.Instrument{ID: "stock:ABC", RegistryVersion: "fixture-v1", Decimals: 2}
)

type eventHistory []ledger.Event

func (history eventHistory) Events(ctx context.Context, accountID string, after int64, limit int) ([]ledger.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accountID != testAccount || after >= int64(len(history)) {
		return []ledger.Event{}, nil
	}
	// Short pages model APIs that return fewer rows than the caller requested.
	end := min(int(after)+min(limit, 2), len(history))
	return history[int(after):end], nil
}

func eventID(id int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", id)
}

func event(t *testing.T, id int, version int64, kind string, payload any) ledger.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Event{ID: eventID(id), AccountVersion: version, Kind: kind, Payload: raw, RecordedAt: testTime.Add(time.Duration(version) * time.Minute)}
}

func account(t *testing.T) ledger.Event {
	t.Helper()
	return event(t, 1, 1, "paper_account.created", ledger.CreateAccount{ID: testAccount, InitialCashUSD: "1000"})
}

func reserve(t *testing.T, id int, version int64, side, quantity, gross, fee string) ledger.Event {
	t.Helper()
	return event(t, id, version, "paper_order.reserved", ledger.ReservationRequest{
		AccountID: testAccount, OrderID: eventID(id), DecisionID: eventID(id + 100), QuoteID: eventID(id + 200),
		SnapshotVersion: version - 1, Instrument: testInstrument, Side: side, QuantityRaw: quantity,
		GrossUSD: gross, MaxFeeUSD: fee, ExpiresAt: testTime.Add(time.Duration(version+5) * time.Minute),
	})
}

func fill(t *testing.T, id, orderID int, version int64, gross, fee string, observed time.Time) ledger.Event {
	t.Helper()
	return event(t, id, version, "paper_order.filled", ledger.ResolutionRequest{
		AccountID: testAccount, OrderID: eventID(orderID), EventID: eventID(id), Outcome: "filled",
		GrossUSD: gross, FeeUSD: fee, ObservedAt: observed,
	})
}

func TestLoadRealizedFIFOAndKeepsRemainingBasis(t *testing.T) {
	history := eventHistory{
		account(t),
		reserve(t, 2, 2, "BUY", "3", "10.00000001", "0.00000002"),
		fill(t, 3, 2, 3, "10.00000001", "0.00000002", testTime.Add(2*time.Minute+time.Second)),
		reserve(t, 4, 4, "BUY", "5", "11", "0.00000001"),
		fill(t, 5, 4, 5, "11", "0.00000001", testTime.Add(4*time.Minute+time.Second)),
		reserve(t, 6, 6, "SELL", "2", "12", "0.00000001"),
		fill(t, 7, 6, 7, "12", "0.00000001", testTime.Add(6*time.Minute+time.Second)),
		reserve(t, 8, 8, "SELL", "4", "20", "0.00000002"),
		fill(t, 9, 8, 9, "20", "0.00000002", testTime.Add(8*time.Minute+time.Second)),
	}
	view, err := Load(context.Background(), history, testAccount, testTime.Add(9*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if view.AccountVersion != 9 || view.CashUSD != "1010.99999993" || view.RecordedFeesUSD != "0.00000006" || view.RealizedNetPnLUSD != "15.39999994" {
		t.Fatalf("unexpected paper totals: %+v", view)
	}
	if len(view.Realized) != 2 || len(view.OpenLots) != 1 {
		t.Fatalf("unexpected outcomes/lots: %+v", view)
	}
	first := view.Realized[0]
	if first.FillEventID != eventID(7) || first.DecisionID != eventID(106) || first.CostBasisUSD != "6.66666668" || first.NetPnLUSD != "5.33333331" || !first.AvailableAt.Equal(history[6].RecordedAt) {
		t.Fatalf("unexpected first realization: %+v", first)
	}
	second := view.Realized[1]
	if second.CostBasisUSD != "9.93333335" || second.NetPnLUSD != "10.06666663" || len(second.Lots) != 2 || second.Lots[0].BuyFillEventID != eventID(3) || second.Lots[0].QuantityRaw != "1" || second.Lots[1].BuyFillEventID != eventID(5) || second.Lots[1].QuantityRaw != "3" {
		t.Fatalf("unexpected second realization: %+v", second)
	}
	lot := view.OpenLots[0]
	if lot.BuyFillEventID != eventID(5) || lot.BuyDecisionID != eventID(104) || lot.QuantityRaw != "2" || lot.CostBasisUSD != "4.40000001" {
		t.Fatalf("unexpected open lot: %+v", lot)
	}
}

func TestLoadUsesRecordingTimeAndStopsAtFutureHistoryPrefix(t *testing.T) {
	history := eventHistory{
		account(t),
		reserve(t, 2, 2, "BUY", "1", "3.5", "0"),
		fill(t, 3, 2, 3, "3.5", "0", testTime.Add(2*time.Minute+time.Second)),
		reserve(t, 4, 4, "SELL", "1", "4.6", "0"),
		event(t, 5, 5, "paper_order.unknown", ledger.ResolutionRequest{AccountID: testAccount, OrderID: eventID(4), EventID: eventID(5), Outcome: "unknown"}),
		fill(t, 6, 4, 6, "4.6", "0", testTime.Add(4*time.Minute+time.Second)),
		reserve(t, 7, 7, "BUY", "1", "5", "0"),
	}
	history[6].RecordedAt = testTime.Add(time.Minute) // A database clock adjustment cannot make later versions available sooner.
	cutoff := testTime.Add(5*time.Minute + 30*time.Second)
	view, err := Load(context.Background(), history, testAccount, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if view.AccountVersion != 5 || len(view.Realized) != 0 || len(view.OpenLots) != 1 || view.CashUSD != "996.5" {
		t.Fatalf("future receipt leaked into snapshot: %+v", view)
	}
	view, err = Load(context.Background(), history, testAccount, history[5].RecordedAt)
	if err != nil {
		t.Fatal(err)
	}
	if view.AccountVersion != 7 || len(view.Realized) != 1 || len(view.OpenLots) != 0 || view.Realized[0].NetPnLUSD != "1.1" {
		t.Fatalf("expected first available realization: %+v", view)
	}
}

func TestLoadUsesExecutionQuantityForPartialsAndImplicitRemainingFill(t *testing.T) {
	partial := func(id, orderID int, version int64, quantity, gross, fee string) ledger.Event {
		return event(t, id, version, "paper_order.partially_filled", ledger.ResolutionRequest{
			AccountID: testAccount, OrderID: eventID(orderID), EventID: eventID(id), Outcome: "partially_filled",
			QuantityRaw: quantity, GrossUSD: gross, FeeUSD: fee, ObservedAt: testTime.Add(time.Duration(version-1) * time.Minute),
		})
	}
	history := eventHistory{
		account(t),
		reserve(t, 2, 2, "BUY", "3", "21", "0.03"),
		partial(3, 2, 3, "1", "6", "0.01"),
		event(t, 4, 4, "paper_order.unknown", ledger.ResolutionRequest{AccountID: testAccount, OrderID: eventID(2), EventID: eventID(4), Outcome: "unknown"}),
		fill(t, 5, 2, 5, "14", "0.02", testTime.Add(4*time.Minute)),
		reserve(t, 6, 6, "SELL", "3", "24", "0.03"),
		partial(7, 6, 7, "2", "16", "0.02"),
		event(t, 8, 8, "paper_order.cancelled", ledger.ResolutionRequest{AccountID: testAccount, OrderID: eventID(6), EventID: eventID(8), Outcome: "cancelled"}),
	}
	view, err := Load(context.Background(), history, testAccount, history[3].RecordedAt)
	if err != nil || view.CashUSD != "993.99" || len(view.OpenLots) != 1 || view.OpenLots[0].QuantityRaw != "1" {
		t.Fatalf("unexpected partial cutoff: %+v, %v", view, err)
	}
	view, err = Load(context.Background(), history, testAccount, history[7].RecordedAt)
	if err != nil || view.CashUSD != "995.95" || view.RecordedFeesUSD != "0.05" || view.RealizedNetPnLUSD != "2.96" || len(view.Realized) != 1 || len(view.OpenLots) != 1 {
		t.Fatalf("unexpected settled accounting: %+v, %v", view, err)
	}
	if view.Realized[0].QuantityRaw != "2" || view.Realized[0].CostBasisUSD != "13.02" || view.OpenLots[0].QuantityRaw != "1" || view.OpenLots[0].CostBasisUSD != "7.01" {
		t.Fatalf("incorrect FIFO allocation: %+v", view)
	}
	for _, invalid := range []ledger.Event{
		partial(3, 2, 3, "3", "21", "0.03"),
		partial(3, 2, 3, "", "6", "0.01"),
	} {
		if _, err := Load(context.Background(), eventHistory{history[0], history[1], invalid}, testAccount, invalid.RecordedAt); err == nil {
			t.Fatal("accepted an inconsistent partial fill")
		}
	}
}

func TestLoadRejectsInconsistentHistory(t *testing.T) {
	for name, history := range map[string]eventHistory{
		"absent account":           {},
		"version gap":              {account(t), reserve(t, 3, 3, "BUY", "1", "2", "0")},
		"fill without reservation": {account(t), fill(t, 2, 3, 2, "3.5", "0", testTime.Add(time.Minute))},
		"sell without a buy lot":   {account(t), reserve(t, 2, 2, "SELL", "1", "2", "0"), fill(t, 3, 2, 3, "2", "0", testTime.Add(2*time.Minute))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(context.Background(), history, testAccount, testTime.Add(time.Hour)); err == nil {
				t.Fatal("inconsistent ledger history was accepted")
			}
		})
	}
}
