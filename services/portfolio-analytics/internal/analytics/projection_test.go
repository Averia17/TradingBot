package analytics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const testAccount = "7c4d4963-94e7-498b-8869-4e46feacbdd0"

func TestProjectionKeepsReservesAndMatchesFIFO(t *testing.T) {
	state := newState()
	for _, event := range scenario() {
		if err := state.apply(event); err != nil {
			t.Fatalf("version %d: %v", event.AccountVersion, err)
		}
		if event.Kind == "paper_order.unknown" && state.ReservedCashUSD != "121" {
			t.Fatalf("UNKNOWN released cash: %+v", state)
		}
	}
	position := state.Positions["equity:ABC"]
	if state.Version != 7 || state.CashUSD != "947.5" || state.ReservedCashUSD != "0" ||
		state.RealizedUSD != "5.25" || position.QuantityRaw != "3" || amountString(position.basis()) != "57.75" {
		t.Fatalf("unexpected final portfolio: %+v, position %+v", state, position)
	}
}

func TestPartialSalesAllocateTheCumulativeFIFORemainder(t *testing.T) {
	position := holding{Lots: []lot{{Quantity: "3", Basis: "1"}}}
	for i, want := range []string{"0.33333333", "0.33333333", "0.33333334"} {
		got, err := position.sell(rawAmount("1"))
		if err != nil || amountString(got) != want {
			t.Fatalf("sale %d: want %s, got %v, err %v", i+1, want, got, err)
		}
	}
	if len(position.Lots) != 0 {
		t.Fatal("fully sold lot remains")
	}
}

func TestAccountEventRejectsInvalidShapes(t *testing.T) {
	event := scenario()[2]
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAccount(data); err != nil {
		t.Fatalf("valid event: %v", err)
	}
	for _, invalid := range []string{
		strings.Replace(string(data), `"decimals":0`, `"decimals":0,"decimals":1`, 1),
		strings.Replace(string(data), `"decimals":0,`, ``, 1),
		strings.Replace(string(data), `"fee_usd":"1"`, `"fee_usd":1`, 1),
		strings.Replace(string(data), `"side":"BUY"`, `"side":"BUY","extra":0`, 1),
		strings.Replace(string(data), `"reserved_cash_usd":"121"`, `"reserved_cash_usd":"924"`, 1),
	} {
		if _, err := DecodeAccount([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid event %s", invalid)
		}
	}
}

func scenario() []AccountEvent {
	start := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	ids := []string{
		testAccount, "7c4d4963-94e7-498b-8869-4e46feacbdd1", "7c4d4963-94e7-498b-8869-4e46feacbdd2",
		"7c4d4963-94e7-498b-8869-4e46feacbdd3", "7c4d4963-94e7-498b-8869-4e46feacbdd4",
		"7c4d4963-94e7-498b-8869-4e46feacbdd5", "7c4d4963-94e7-498b-8869-4e46feacbdd6",
	}
	instrument := Instrument{InstrumentID: "equity:ABC", RegistryVersion: "v1"}
	rows := []AccountEvent{
		{Kind: "paper_account.created", CashUSD: "1000", ReservedCashUSD: "0"},
		{Kind: "paper_order.reserved", CashUSD: "1000", ReservedCashUSD: "202", Position: &Balance{instrument, "0", "0"}},
		{Kind: "paper_order.partially_filled", CashUSD: "923", ReservedCashUSD: "121", Position: &Balance{instrument, "4", "0"},
			Fill: &Fill{Side: "BUY", QuantityRaw: "4", GrossUSD: "76", FeeUSD: "1", OrderID: ids[1], DecisionID: ids[4], QuoteID: ids[5]}},
		{Kind: "paper_order.unknown", CashUSD: "923", ReservedCashUSD: "121", Position: &Balance{instrument, "4", "0"}},
		{Kind: "paper_order.cancelled", CashUSD: "923", ReservedCashUSD: "0", Position: &Balance{instrument, "4", "0"}},
		{Kind: "paper_order.reserved", CashUSD: "923", ReservedCashUSD: "0", Position: &Balance{instrument, "4", "1"}},
		{Kind: "paper_order.filled", CashUSD: "947.5", ReservedCashUSD: "0", Position: &Balance{instrument, "3", "0"},
			Fill: &Fill{Side: "SELL", QuantityRaw: "1", GrossUSD: "25", FeeUSD: "0.5", OrderID: ids[5], DecisionID: ids[3], QuoteID: ids[4]}},
	}
	for i := range rows {
		row := &rows[i]
		row.Version, row.AccountID, row.EventID, row.AccountVersion = WireVersion, testAccount, ids[i], int64(i+1)
		row.RecordedAt = start.Add(time.Duration(i) * time.Hour)
		if row.Fill != nil {
			row.Fill.ObservedAt = row.RecordedAt.Add(-time.Minute)
		}
	}
	return rows
}
