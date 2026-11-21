package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
	"tradingbot/control/internal/paper_thesis"
	"tradingbot/control/internal/paper_workflow"
)

func checkStableHorizonOutcome(t *testing.T, fixture stableEntryFixture, order ledger.Order) {
	t.Helper()
	view, err := paper_thesis.Load(fixture.ctx, fixture.research, fixture.paper, fixture.accountID, order.OrderID, fixture.clock())
	if err != nil || view.ExpiresAt == nil || view.OpeningIdentity == nil {
		t.Fatalf("opening has no verified horizon: %+v %v", view, err)
	}
	horizon := *view.ExpiresAt
	request := paper_thesis.OutcomeRequest{
		Version: paper_workflow.IdentityVersion, Source: paper_sim.Source, AccountID: fixture.accountID, OpeningOrderID: order.OrderID,
		AsOf: horizon, MaxObservationAgeSeconds: 60, ExitObservations: []paper_thesis.ExitObservation{},
	}
	build := func(req paper_thesis.OutcomeRequest) (paper_thesis.OutcomeReport, error) {
		input, err := parseIdentityOutcome(t, req)
		if err != nil {
			return paper_thesis.OutcomeReport{}, err
		}
		return paper_thesis.BuildOutcomeFromArchive(fixture.ctx, fixture.research, fixture.paper, input, horizon, fixture.dir)
	}
	early := request
	early.AsOf = view.AsOf
	if report, err := build(early); err != nil || report.Status != "immature" || report.Horizon != nil {
		t.Fatalf("outcome exposed a future horizon: %+v %v", report, err)
	}
	if report, err := build(request); err != nil || report.Status != "open_unpriced_at_horizon" ||
		report.Horizon == nil || report.Horizon.NetPnLUSD != nil || report.Actual.OpenCostBasisUSD != "100.2" {
		t.Fatalf("unpriced position acquired a synthetic return: %+v %v", report, err)
	}
	legacy := request
	legacy.Version = paper_thesis.OutcomeVersion
	if _, err := build(legacy); !errors.Is(err, paper_thesis.ErrInvalid) {
		t.Fatalf("legacy request could drop the original identity: %v", err)
	}
	quote := paper_sim.Quote{
		Version: paper_sim.IdentityQuoteVersion, Source: paper_sim.Source, Instrument: view.Instrument,
		SecurityID: view.OpeningIdentity.SecurityID,
		Side:       "SELL", QuantityRaw: "500", GrossUSD: "115", FeeBPS: new(20), FixedFeeUSD: "0",
		ObservedAt: horizon.Add(-time.Second), ExpiresAt: horizon.Add(time.Second), Execution: "fill",
	}
	pinRegistry := func(name string, expires time.Time, prior []map[string]any) *paper_workflow.ArchiveReference {
		entries := append(prior, map[string]any{"symbol": view.Symbol, "security_id": quote.SecurityID, "instrument": view.Instrument,
			"effective_at": horizon.Add(-time.Minute), "available_at": horizon.Add(-time.Minute),
			"expires_at": expires, "record_providers": []string{"fixture-market"}})
		raw := encode(t, map[string]any{"schema_version": "0.1", "universe_provider": "fixture-roster", "entries": entries})
		return &paper_workflow.ArchiveReference{Ref: filepath.Base(saveDocument(t, fixture.dir, name, raw)), SHA256: digestHex(raw)}
	}
	request.ExitRegistry = pinRegistry("active-horizon-registry", horizon.Add(time.Minute), nil)
	quote.IdentityRegistrySHA256 = request.ExitRegistry.SHA256
	wrong := quote
	wrong.SecurityID, wrong.GrossUSD = "foreign-issuer", "1150"
	makeTape := func(registry string, quotes ...paper_sim.Quote) []byte {
		observations := make([]map[string]any, 0, len(quotes))
		for _, item := range quotes {
			item.IdentityRegistrySHA256 = registry
			observations = append(observations, map[string]any{"available_at": horizon, "firmness": "firm", "quote": item})
		}
		return encode(t, map[string]any{"paper_quote_tape_schema_version": "0.3", "source_mode": paper_sim.Source,
			"account_id": fixture.accountID, "identity_registry_sha256": registry, "observations": observations})
	}
	tape := makeTape(request.ExitRegistry.SHA256, wrong, quote)
	path := saveDocument(t, fixture.dir, "identity-horizon-tape", tape)
	request.ExitTape = &paper_workflow.ArchiveReference{Ref: filepath.Base(path), SHA256: digestHex(tape)}
	priced, err := build(request)
	if err != nil || priced.Status != "open_marked_at_horizon" || priced.OpeningIdentity == nil ||
		priced.OpeningIdentity.ApplicationSHA256 != view.OpeningIdentity.ApplicationSHA256 || priced.Horizon == nil ||
		priced.Horizon.TapeSHA256 != request.ExitTape.SHA256 || priced.Horizon.QuoteSHA256 != digestHex(encode(t, quote)) ||
		priced.Horizon.ExitRegistry == nil || *priced.Horizon.ExitRegistry != *request.ExitRegistry ||
		priced.Horizon.NetPnLUSD == nil || *priced.Horizon.NetPnLUSD != "14.57" {
		t.Fatalf("verified horizon used the wrong paper quote or amounts: %+v %v", priced, err)
	}
	var wire map[string]any
	if err := contract.Decode(encode(t, priced), contract.PaperThesisOutcomeReport, &wire); err != nil {
		t.Fatalf("stable outcome does not match its published contract: %v", err)
	}
	if err := os.WriteFile(path, append(tape, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := build(request); err == nil {
		t.Fatal("changed tape was accepted under the original horizon pin")
	}
	for name, registry := range map[string]*paper_workflow.ArchiveReference{
		"expired": pinRegistry("expired-horizon-registry", horizon, nil),
		"reused": pinRegistry("reused-horizon-registry", horizon.Add(time.Minute), []map[string]any{{
			"symbol": "OLD", "security_id": "foreign-issuer", "instrument": view.Instrument,
			"effective_at": horizon.Add(-time.Hour), "available_at": horizon.Add(-time.Hour),
			"expires_at": horizon.Add(-2 * time.Minute), "record_providers": []string{"fixture-market"},
		}}),
	} {
		invalid := request
		invalid.ExitRegistry = registry
		invalidTape := makeTape(registry.SHA256, quote)
		invalidTapePath := saveDocument(t, fixture.dir, name+"-horizon-tape", invalidTape)
		invalid.ExitTape = &paper_workflow.ArchiveReference{Ref: filepath.Base(invalidTapePath), SHA256: digestHex(invalidTape)}
		if report, err := build(invalid); err != nil || report.Status != "open_unpriced_at_horizon" ||
			report.Horizon == nil || report.Horizon.ExitObservation != nil {
			t.Fatalf("%s instrument was priced as executable: %+v %v", name, report, err)
		}
	}
	account, err := fixture.paper.GetAccount(fixture.ctx, fixture.accountID)
	if err != nil || account.Version != view.AccountVersion {
		t.Fatalf("horizon evaluation changed the account: %+v %v", account, err)
	}
}

func checkUnboundHorizonSale(t *testing.T, fixture stableEntryFixture, order ledger.Order) {
	t.Helper()
	account, err := fixture.paper.GetAccount(fixture.ctx, fixture.accountID)
	if err != nil {
		t.Fatal(err)
	}
	sale, _, err := fixture.paper.Reserve(fixture.ctx, ledger.ReservationRequest{
		AccountID: fixture.accountID, OrderID: "80000000-0000-4000-8000-000000000001",
		DecisionID: "80000000-0000-4000-8000-000000000002", QuoteID: "80000000-0000-4000-8000-000000000003",
		SnapshotVersion: account.Version, Instrument: order.Instrument, Side: "SELL", QuantityRaw: "500",
		GrossUSD: "110", MaxFeeUSD: "0", ExpiresAt: fixture.clock().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fixture.paper.Resolve(fixture.ctx, ledger.ResolutionRequest{
		AccountID: fixture.accountID, OrderID: sale.OrderID, EventID: "80000000-0000-4000-8000-000000000004",
		Outcome: "filled", GrossUSD: "110", FeeUSD: "0", ObservedAt: sale.CreatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := fixture.clock()
	input, err := parseIdentityOutcome(t, paper_thesis.OutcomeRequest{
		Version: paper_workflow.IdentityVersion, Source: paper_sim.Source, AccountID: fixture.accountID, OpeningOrderID: order.OrderID,
		AsOf: cutoff, MaxObservationAgeSeconds: 60, ExitObservations: []paper_thesis.ExitObservation{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := paper_thesis.BuildOutcome(fixture.ctx, fixture.research, fixture.paper, input, cutoff); !errors.Is(err, paper_thesis.ErrUnlinked) {
		t.Fatalf("reported realized P&L from a sale without the original identity: %v", err)
	}
}

func parseIdentityOutcome(t *testing.T, req paper_thesis.OutcomeRequest) (paper_thesis.OutcomeInput, error) {
	t.Helper()
	raw := encode(t, req)
	return paper_thesis.ParseOutcomeRequest(raw, digestHex(raw))
}
