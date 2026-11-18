package paperthesis

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
)

func TestOpeningOutcomeSeparatesActualExitsFromFrozenHorizon(t *testing.T) {
	research, paper, opened := reviewedOpening(t)
	var result map[string]json.RawMessage
	if err := json.Unmarshal(research.result.Payload, &result); err != nil {
		t.Fatal(err)
	}
	var batch contract.Batch
	if err := json.Unmarshal(result["batch"], &batch); err != nil {
		t.Fatal(err)
	}
	batch.Intents[0].Horizon = contract.Horizon{Min: 24, Max: 720}
	result["batch"] = fixtureJSON(t, batch)
	research.result.Payload = fixtureJSON(t, result)
	maturity := opened.Add(30 * 24 * time.Hour)
	closingFill := func(id int, at time.Time, quantity, gross string) {
		t.Helper()
		sale := ledger.ReservationRequest{
			AccountID: paper.account.ID, OrderID: fixtureID(id), DecisionID: fixtureID(id + 1), QuoteID: fixtureID(id + 2),
			SnapshotVersion: paper.account.Version, Instrument: paper.order.Instrument, Side: "SELL",
			QuantityRaw: quantity, GrossUSD: gross, MaxFeeUSD: "1", ExpiresAt: at.Add(time.Minute),
		}
		paper.add(t, sale.OrderID, "paper_order.reserved", at, sale)
		fill := ledger.ResolutionRequest{
			AccountID: sale.AccountID, OrderID: sale.OrderID, EventID: fixtureID(id + 3), Outcome: "filled",
			GrossUSD: gross, FeeUSD: "1", ObservedAt: at,
		}
		paper.add(t, fill.EventID, "paper_order.filled", at.Add(time.Second), fill)
	}
	closingFill(20, opened.Add(time.Hour), "1", "24")
	closingFill(30, maturity.Add(time.Minute), "2", "50")
	good := ExitObservation{
		ID: "firm-sale", Instrument: paper.order.Instrument, QuantityRaw: "2", GrossUSD: "44.12345678", FeeUSD: "0.12345678",
		ObservedAt: maturity.Add(-time.Minute), AvailableAt: maturity.Add(-30 * time.Second), ExpiresAt: maturity.Add(time.Minute),
	}
	future := good
	future.ID, future.GrossUSD, future.AvailableAt = "late-receipt", "99", maturity.Add(time.Second)
	wrongSize := good
	wrongSize.ID, wrongSize.QuantityRaw, wrongSize.ObservedAt = "wrong-size", "1", maturity.Add(-45*time.Second)
	expired := good
	expired.ID, expired.ObservedAt, expired.ExpiresAt = "expired", maturity.Add(-40*time.Second), maturity
	req := OutcomeRequest{
		Version: "0.1", AccountID: paper.account.ID, OpeningOrderID: paper.order.OrderID,
		Source: "synthetic_fixture", MaxObservationAgeSeconds: 120,
		ExitObservations: []ExitObservation{future, wrongSize, good, expired},
		Benchmark: &BenchmarkInput{ID: "fixture-index", Levels: []BenchmarkLevel{
			{Level: "100", ObservedAt: opened.Add(-time.Minute), AvailableAt: opened},
			{Level: "105", ObservedAt: maturity.Add(-time.Minute), AvailableAt: maturity},
			{Level: "199", ObservedAt: maturity, AvailableAt: maturity.Add(time.Second)},
		}},
	}
	reportAt := func(at time.Time) OutcomeReport {
		t.Helper()
		req.AsOf = at
		raw := fixtureJSON(t, req)
		input, err := ParseOutcomeRequest(raw, fmt.Sprintf("%x", sha256.Sum256(raw)))
		if err != nil {
			t.Fatal(err)
		}
		report, err := BuildOutcome(context.Background(), research, paper, input, maturity.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var wire any
		if err := contract.Decode(fixtureJSON(t, report), contract.PaperThesisOutcomeReport, &wire); err != nil {
			t.Fatalf("serialized paper outcome violates its contract: %v", err)
		}
		return report
	}
	early := reportAt(opened.Add(5 * 24 * time.Hour))
	if early.Status != "immature" || early.Horizon != nil || early.Benchmark != nil ||
		early.Actual.RealizedPnLUSD != "3" || early.Actual.OpenQuantityRaw != "2" ||
		len(early.Actual.ExitFillIDs) != 1 || early.Actual.ExitFillIDs[0] != fixtureID(23) {
		t.Fatalf("premature report revealed horizon evidence or dropped realized exit: %+v", early)
	}
	frozen := reportAt(maturity)
	if frozen.Status != "open_marked_at_horizon" || frozen.Horizon == nil || frozen.Horizon.ExitObservation.ID != "firm-sale" ||
		inexact(frozen.Horizon.NetPnLUSD, "7") || inexact(frozen.Horizon.EstimatedOpenNetUSD, "44") ||
		frozen.Benchmark == nil || inexact(frozen.Benchmark.ReturnBPS, "500") || !frozen.Horizon.AsOf.Equal(maturity) ||
		frozen.Horizon.AccountVersion != frozen.AccountVersion || frozen.Version != "0.1" {
		t.Fatalf("incorrect frozen horizon: %+v", frozen)
	}
	late := reportAt(maturity.Add(2 * time.Minute))
	if late.Actual.RealizedPnLUSD != "12" || late.Actual.OpenQuantityRaw != "0" ||
		!reflect.DeepEqual(late.Horizon, frozen.Horizon) || !reflect.DeepEqual(late.Benchmark, frozen.Benchmark) {
		t.Fatalf("later fills rewrote the fixed horizon: late %+v, frozen %+v", late, frozen)
	}
	req.ExitObservations = []ExitObservation{future, wrongSize, expired}
	req.Benchmark.Levels = req.Benchmark.Levels[:1]
	unpriced := reportAt(maturity.Add(2 * time.Minute))
	if unpriced.Status != "open_unpriced_at_horizon" || unpriced.Horizon.NetPnLUSD != nil ||
		unpriced.Horizon.ExitObservation != nil || unpriced.Benchmark.Status != "unpriced" ||
		unpriced.Benchmark.ReturnBPS != nil || unpriced.Horizon.Basis.RealizedPnLUSD != "3" {
		t.Fatalf("missing eligible observations must stay explicitly unpriced: %+v", unpriced)
	}
	paper.events, paper.account.Version = paper.events[:5], 5
	closingFill(30, maturity.Add(-time.Minute), "2", "50")
	settled := reportAt(maturity)
	if settled.Status != "settled_at_horizon" || inexact(settled.Horizon.NetPnLUSD, "12") || settled.Horizon.EstimatedOpenNetUSD != nil {
		t.Fatalf("zero residual acquired a hypothetical mark: %+v", settled)
	}
}

func inexact(value *string, expected string) bool { return value == nil || *value != expected }

func TestOutcomeRequestPublicFixtureIsPinned(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", "valid-paper-thesis-outcome-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	if _, err := ParseOutcomeRequest(raw, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseOutcomeRequest(append(raw, ' '), hash); err == nil {
		t.Fatal("accepted changed input under original hash")
	}
}
