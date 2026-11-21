package paper_thesis

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
	"tradingbot/control/internal/paper_workflow"
)

type shadowFixture struct {
	*paperFixture
	app ledger.ApplicationRecord
}

func (f shadowFixture) GetApplication(context.Context, string) (ledger.ApplicationRecord, error) {
	return f.app, nil
}

func rejectedOpening(t *testing.T) (jobs.CompletedResearch, shadowFixture, time.Time) {
	t.Helper()
	research, paper, rejectedAt := reviewedOpening(t)
	paper.events = paper.events[:2]
	paper.account.Version--
	paper.order.Status, paper.order.FilledQuantityRaw, paper.order.RemainingQuantityRaw = "cancelled", "0", paper.order.QuantityRaw
	resolution := ledger.ResolutionRequest{
		AccountID: paper.order.AccountID, OrderID: paper.order.OrderID,
		EventID: paper_sim.OutcomeEventID(paper.order.OrderID), Outcome: "cancelled",
	}
	paper.add(t, resolution.EventID, "paper_order.cancelled", rejectedAt, resolution)
	paper.order.AccountVersion = paper.account.Version
	quote := fixtureJSON(t, paper_sim.Quote{
		Version: paper_sim.Version, Source: paper_sim.Source, Instrument: paper.order.Instrument, Side: "BUY",
		QuantityRaw: paper.order.QuantityRaw, GrossUSD: "100", FeeBPS: new(100), FixedFeeUSD: "0",
		ObservedAt: paper.order.CreatedAt, ExpiresAt: paper.order.ExpiresAt, Execution: "reject",
	})
	app := ledger.ApplicationRecord{
		ApplicationInput: ledger.ApplicationInput{
			JobID: paper.order.ResearchJobID, AccountID: paper.order.AccountID, SnapshotVersion: paper.order.SnapshotVersion, Quote: quote,
		},
		OrderID: paper.order.OrderID,
		Result: fixtureJSON(t, paper_workflow.Result{Version: paper_workflow.Version, Status: "applied", JobID: paper.order.ResearchJobID,
			Execution: &paper_sim.Result{
				Version: paper_sim.Version, Source: paper_sim.Source, QuoteSHA256: shadowDigest(quote), SimulatedExecution: "reject",
				ReservationRecorded: true, OutcomeRecorded: true, Order: paper.order,
			},
		}),
	}
	return jobs.CompletedResearch{Job: research.job, Result: research.result}, shadowFixture{paper, app}, rejectedAt
}

func shadowDocuments(t *testing.T) (raw, entry, exit []byte) {
	t.Helper()
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", "valid-paper-shadow-"+name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	return read("outcome-input"), read("entry"), read("exit")
}

func TestShadowOutcomeUsesExplicitRejectionAndExactHypotheticalCosts(t *testing.T) {
	raw, entry, exit := shadowDocuments(t)
	input, err := ParseShadowInput(raw, shadowDigest(raw), entry, exit)
	if err != nil {
		t.Fatal(err)
	}
	research, fixture, rejectedAt := rejectedOpening(t)
	known := input.request.AsOf
	report, err := BuildShadowOutcome(t.Context(), research, fixture, input, known)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "labelled" || report.Basis != "counterfactual" || report.RejectionEventID != fixture.events[2].ID ||
		report.RejectionReason != "simulated_rejection" || !report.RejectedAt.Equal(rejectedAt) ||
		report.RejectedQuoteSHA256 != shadowDigest(fixture.app.Quote) || report.Entry.SHA256 != shadowDigest(entry) ||
		report.Exit == nil || report.Exit.SHA256 != shadowDigest(exit) || report.Label == nil ||
		report.Label.EntryCostUSD != "100" || report.Label.ExitProceedsUSD != "104.99999999" ||
		report.Label.NetPnLUSD != "4.99999999" || report.Label.ReturnBPS != "499.999999" || !report.Label.AvailableAt.Equal(known) {
		t.Fatalf("shadow report: %+v, exit %+v, label %+v", report, report.Exit, report.Label)
	}
	var shape any
	if err := contract.Decode(fixtureJSON(t, report), contract.PaperShadowOutcomeReport, &shape); err != nil {
		t.Fatalf("report violates its wire schema: %v", err)
	}

	for _, test := range []struct {
		name   string
		change func(*ShadowInput)
		status string
	}{
		{"before horizon", func(in *ShadowInput) { in.request.AsOf = in.request.AsOf.Add(-time.Nanosecond) }, "immature"},
		{"exit becomes known after horizon", func(in *ShadowInput) { in.exit.Observation.AvailableAt = known.Add(time.Nanosecond) }, "unpriced_at_horizon"},
		{"exit expires at horizon", func(in *ShadowInput) { in.exit.Observation.ExpiresAt = known }, "unpriced_at_horizon"},
		{"stale exit", func(in *ShadowInput) { in.request.MaxObservationAgeSeconds = 20 }, "unpriced_at_horizon"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := input
			test.change(&candidate)
			report, err := BuildShadowOutcome(t.Context(), research, fixture, candidate, known)
			if err != nil || report.Status != test.status || report.Exit != nil || report.Label != nil {
				t.Fatalf("unavailable observation leaked: %+v %v", report, err)
			}
			var shape any
			if err := contract.Decode(fixtureJSON(t, report), contract.PaperShadowOutcomeReport, &shape); err != nil {
				t.Fatalf("unlabelled report violates its wire schema: %v", err)
			}
		})
	}
}

func TestShadowRejectsTamperedObservationsAndIneligibleOrders(t *testing.T) {
	raw, entry, exit := shadowDocuments(t)
	input, err := ParseShadowInput(raw, shadowDigest(raw), entry, exit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseShadowInput(raw, shadowDigest(raw), append(entry, '\n'), exit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted changed entry bytes: %v", err)
	}
	if _, err := ParseShadowInput(raw, shadowDigest(raw), entry, append(exit, '\n')); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted changed exit bytes: %v", err)
	}
	if _, err := ParseShadowInput(raw, shadowDigest(raw), exit, entry); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted swapped observations: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*shadowFixture, *ShadowInput)
	}{
		{"manual cancellation", func(f *shadowFixture, _ *ShadowInput) { f.events[2].ID = fixtureID(999) }},
		{"missing completed application", func(f *shadowFixture, _ *ShadowInput) { f.app.Result = nil }},
		{"unknown or timeout", func(f *shadowFixture, _ *ShadowInput) { f.order.Status = "unknown" }},
		{"partial execution", func(f *shadowFixture, _ *ShadowInput) { f.order.FilledQuantityRaw = "1" }},
		{"entry from before rejection", func(f *shadowFixture, in *ShadowInput) {
			in.entry.Observation.ObservedAt = f.events[2].RecordedAt.Add(-time.Nanosecond)
		}},
		{"entry at reservation expiry", func(f *shadowFixture, in *ShadowInput) { in.entry.Observation.AvailableAt = f.order.ExpiresAt }},
		{"entry over fee budget", func(_ *shadowFixture, in *ShadowInput) { in.entry.Observation.FeeUSD = "1.00000001" }},
		{"future cutoff", func(_ *shadowFixture, in *ShadowInput) { in.request.AsOf = in.request.AsOf.Add(time.Nanosecond) }},
		{"different exit size", func(_ *shadowFixture, in *ShadowInput) { in.exit.Observation.QuantityRaw = "3" }},
		{"different exit instrument", func(_ *shadowFixture, in *ShadowInput) { in.exit.Observation.Instrument.RegistryVersion = "wrong" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			research, fixture, _ := rejectedOpening(t)
			candidate := input
			test.change(&fixture, &candidate)
			if _, err := BuildShadowOutcome(t.Context(), research, fixture, candidate, input.request.AsOf); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted ineligible shadow case: %v", err)
			}
		})
	}
}

func TestShadowOutcomePreservesLossesAfterBothFees(t *testing.T) {
	raw, entry, exit := shadowDocuments(t)
	var request shadowRequest
	if err := contract.Decode(raw, contract.PaperShadowOutcomeRequest, &request); err != nil {
		t.Fatal(err)
	}
	var sell shadowObservation
	if err := contract.Decode(exit, contract.PaperShadowObservation, &sell); err != nil {
		t.Fatal(err)
	}
	sell.GrossUSD, sell.FeeUSD = "95.1", "0.10000001"
	exit = fixtureJSON(t, sell)
	request.ExitSHA256 = shadowDigest(exit)
	raw = fixtureJSON(t, request)
	input, err := ParseShadowInput(raw, shadowDigest(raw), entry, exit)
	if err != nil {
		t.Fatal(err)
	}
	research, fixture, _ := rejectedOpening(t)
	report, err := BuildShadowOutcome(t.Context(), research, fixture, input, input.request.AsOf)
	if err != nil || report.Label == nil || report.Label.NetPnLUSD != "-5.00000001" || report.Label.ReturnBPS != "-500.000001" {
		t.Fatalf("incorrect net loss: %+v, %v", report.Label, err)
	}
}
