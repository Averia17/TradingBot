package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
	"tradingbot/control/internal/paper_thesis"
	"tradingbot/control/internal/paper_workflow"
	"tradingbot/control/internal/research_scheduler"
)

type stableReviewQuote struct {
	quote       paper_sim.Quote
	availableAt time.Time
}

func checkStableThesisReview(t *testing.T, fixture stableEntryFixture, order ledger.Order) {
	t.Helper()
	cutoff := fixture.clock()
	var stdout bytes.Buffer
	if err := run(fixture.ctx, fixture.dsn, []string{"review", "--account", fixture.accountID, "--order", order.OrderID}, &stdout, fixture.clock); err != nil {
		t.Fatal(err)
	}
	var view reviewReceipt
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil || view.Version != paper_workflow.IdentityVersion ||
		view.State != "open" || view.OpeningIdentity == nil || view.OpeningIdentity.Instrument != order.Instrument ||
		view.OpeningIdentity.SecurityID != fixture.securityID || view.OpenedAt == nil {
		t.Fatalf("stable opening not visible for review: %s %v", stdout.Bytes(), err)
	}
	due, err := paper_thesis.Load(fixture.ctx, fixture.research, fixture.paper, fixture.accountID, order.OrderID,
		view.OpenedAt.Add(time.Hour))
	if err != nil || due.OpeningIdentity == nil || due.OpeningIdentity.SecurityID != fixture.securityID ||
		due.ClockReview == nil || due.ClockReview.Reason != "cadence" {
		t.Fatalf("stable opening lost its scheduled cadence: %+v %v", due, err)
	}
	// The new owner registry has different bytes and pins, but the exact
	// original security and held instrument must still be eligible.
	pin := func(name string, record any) paper_workflow.ArchiveReference {
		raw := encode(t, record)
		return paper_workflow.ArchiveReference{Ref: filepath.Base(saveDocument(t, fixture.dir, name, raw)), SHA256: digestHex(raw)}
	}
	const provider = "review-archive"
	const previous = "previous-issuer"
	registry := func(name, securityID string) paper_workflow.ArchiveReference {
		entry := map[string]any{"symbol": view.Symbol, "security_id": securityID, "instrument": order.Instrument,
			"effective_at": cutoff.Add(-time.Hour), "available_at": cutoff.Add(-time.Hour), "expires_at": cutoff.Add(time.Hour),
			"record_providers": []string{provider}}
		return pin(name, map[string]any{"schema_version": "0.1", "universe_provider": "review-roster", "entries": []map[string]any{entry}})
	}
	owner := func(name string, ref paper_workflow.ArchiveReference, observations ...any) paper_workflow.OwnerIdentity {
		if observations == nil {
			observations = []any{}
		}
		return paper_workflow.OwnerIdentity{Registry: ref, QuoteTape: pin(name, map[string]any{
			"paper_quote_tape_schema_version": "0.3", "source_mode": "synthetic_fixture", "account_id": fixture.accountID,
			"identity_registry_sha256": ref.SHA256, "observations": observations,
		})}
	}
	news := func(recordID, securityID string) map[string]any {
		return map[string]any{"kind": "news", "provider": provider, "record_id": recordID, "symbol": view.Symbol, "revision": 1,
			"event_time": cutoff, "available_at": cutoff, "ingested_at": cutoff, "payload": map[string]any{"security_id": securityID}}
	}
	archive := pin("stable-review-archive", map[string]any{"schema_version": "0.1", "source_mode": "synthetic_fixture",
		"records": []map[string]any{news("held-security", fixture.securityID), news("reused-ticker", previous),
			news("standalone", fixture.securityID), news("late-window", fixture.securityID)}})
	account, err := fixture.paper.GetAccount(fixture.ctx, fixture.accountID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := fixture.config.Context
	snapshot.SnapshotVersion = account.Version
	portfolio, err := paper_workflow.Snapshot(account, snapshot, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	ref := registry("review-registry", fixture.securityID)
	exitQuote := paper_sim.Quote{Version: paper_sim.IdentityQuoteVersion, Source: paper_sim.Source, SecurityID: fixture.securityID,
		IdentityRegistrySHA256: ref.SHA256, Instrument: order.Instrument, Side: "SELL", QuantityRaw: "200",
		GrossUSD: "40", FixedFeeUSD: "0", FeeBPS: new(20), Execution: "fill", ExpiresAt: cutoff.Add(time.Minute)}
	var exits []stableReviewQuote
	var observations []any
	for seconds := 1; seconds <= 12; seconds++ {
		exitQuote.ObservedAt = cutoff.Add(time.Duration(seconds) * time.Second)
		available := exitQuote.ObservedAt.Add(time.Millisecond)
		exits = append(exits, stableReviewQuote{exitQuote, available})
		observations = append(observations, map[string]any{"available_at": available, "firmness": "firm", "quote": exitQuote})
	}
	identity := paper_thesis.ReviewIdentity{OwnerIdentity: owner("review-tape", ref, observations...),
		ProjectedSnapshotSHA256: strings.Repeat("a", 64)}
	config := paper_thesis.ScheduleRequest{Version: paper_workflow.IdentityVersion, OpeningOrderID: order.OrderID,
		Context: snapshot, Portfolio: &portfolio, Manifest: archive, Budget: 3, Identity: &identity}
	invoke := func(request paper_thesis.ScheduleRequest, trustedSHA string, flags ...string) (paper_thesis.ScheduleReceipt, error) {
		path := saveDocument(t, fixture.dir, "stable-review-config", encode(t, request))
		var output bytes.Buffer
		args := []string{"schedule-review", "--config", path, "--archive-root", fixture.dir, "--identity-registry-sha256", trustedSHA}
		if err := run(fixture.ctx, fixture.dsn, append(args, flags...), &output, fixture.clock); err != nil {
			return paper_thesis.ScheduleReceipt{}, err
		}
		var receipt paper_thesis.ScheduleReceipt
		err := json.Unmarshal(output.Bytes(), &receipt)
		return receipt, err
	}
	trusted := identity.Registry.SHA256
	if before, err := invoke(config, trusted); err != nil || before.Status != "not_due" || before.Version != paper_workflow.IdentityVersion {
		t.Fatalf("stable review enrolled before its deadline: %+v %v", before, err)
	}
	config.Event = &research_scheduler.EventReference{Kind: "news", Provider: provider, RecordID: "reused-ticker"}
	if _, err := invoke(config, trusted); !errors.Is(err, paper_workflow.ErrInvalid) {
		t.Fatalf("an old issuer's catalyst could schedule the new holding: %v", err)
	}
	config.Event.RecordID = "held-security"
	if _, err := invoke(config, strings.Repeat("c", 64)); !errors.Is(err, paper_thesis.ErrInvalid) {
		t.Fatalf("stable review accepted a substituted owner pin: %v", err)
	}
	wrong := config
	wrongIdentity := identity
	wrongIdentity.OwnerIdentity = owner("previous-issuer-tape", registry("previous-issuer-registry", previous))
	wrong.Identity = &wrongIdentity
	if _, err := invoke(wrong, wrongIdentity.Registry.SHA256); !errors.Is(err, paper_workflow.ErrInvalid) {
		t.Fatalf("stable review accepted a remapped holding: %v", err)
	}
	legacy := config
	legacy.Version, legacy.Identity = paper_thesis.Version, nil
	if _, err := invoke(legacy, ""); !errors.Is(err, paper_thesis.ErrInvalid) {
		t.Fatalf("stable opening could use ticker-only review: %v", err)
	}
	first, err := invoke(config, trusted)
	if err != nil || !first.Created || first.Version != paper_workflow.IdentityVersion || first.Trigger == nil || first.Trigger.Reason != "catalyst" {
		t.Fatalf("valid stable review not queued: %+v %v", first, err)
	}
	task, err := fixture.research.GetPaperDispatch(fixture.ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var dispatch paper_workflow.Request
	if err := contract.Decode(task.Request, contract.PaperApplication, &dispatch); err != nil || dispatch.Identity == nil ||
		dispatch.ReviewScheduleSHA256 == "" || dispatch.EntryScheduleSHA256 != "" || dispatch.Identity.OwnerIdentity != identity.OwnerIdentity ||
		dispatch.Identity.Registry == view.OpeningIdentity.Registry || dispatch.Identity.ProjectedSnapshotSHA256 != identity.ProjectedSnapshotSHA256 ||
		len(dispatch.Identity.Securities) != 1 || dispatch.Identity.Securities[0].SecurityID != fixture.securityID ||
		dispatch.Identity.Securities[0].Instrument != order.Instrument {
		t.Fatalf("queued review lost the held security proof: %+v %v", dispatch, err)
	}
	if again, err := invoke(config, trusted); err != nil || again.Created || again.JobID != first.JobID ||
		!again.CreatedAt.Equal(*first.CreatedAt) {
		t.Fatalf("frozen review replay changed its durable record: %+v %v", again, err)
	}
	if again, err := invoke(config, trusted, "--roll-forward", "--max-research-age", "1ns"); err != nil ||
		again.Created || again.Status != "queued" || again.JobID != first.JobID || !again.CreatedAt.Equal(*first.CreatedAt) {
		t.Fatalf("closed window lost an already committed acknowledgement: %+v %v", again, err)
	}
	changed := identity
	changed.ProjectedSnapshotSHA256 = strings.Repeat("b", 64)
	config.Identity = &changed
	if _, err := invoke(config, trusted); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("review occurrence accepted a changed owner projection: %v", err)
	}
	if _, err := invoke(config, trusted, "--roll-forward"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("roll-forward accepted changed proof at the same cutoff: %v", err)
	}
	newer, err := paper_workflow.Snapshot(account, snapshot, fixture.clock())
	if err != nil || !newer.AsOf.After(portfolio.AsOf) {
		t.Fatalf("could not construct the next frozen window: %+v %v", newer, err)
	}
	config.Portfolio = &newer
	if _, err := invoke(config, trusted); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("manual frozen scheduling stopped enforcing exact replay: %v", err)
	}
	if prior, err := invoke(config, trusted, "--roll-forward"); err != nil || prior.Status != "already_scheduled" || prior.Version != paper_workflow.IdentityVersion ||
		prior.Created || prior.JobID != first.JobID || !prior.CreatedAt.Equal(*first.CreatedAt) || prior.Trigger != nil || prior.Snapshot != nil {
		t.Fatalf("later window did not acknowledge the original durable occurrence: %+v %v", prior, err)
	}
	standalone := research_scheduler.EventReference{Kind: "news", Provider: provider, RecordID: "standalone"}
	verified, err := research_scheduler.VerifyArchive(fixture.dir, archive, view.AsOf, []string{view.Symbol}, &standalone)
	if err != nil {
		t.Fatal(err)
	}
	unboundTrigger, err := paper_thesis.CatalystReview(view.View, verified)
	if err != nil {
		t.Fatal(err)
	}
	priorJob, err := fixture.research.Get(fixture.ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := fixture.research.Create(fixture.ctx, jobs.CreateJob{
		ID: unboundTrigger.ID, Request: bytes.Replace(priorJob.Request, []byte(first.JobID), []byte(unboundTrigger.ID), 1),
		SnapshotID: portfolio.ID, Snapshot: encode(t, portfolio),
	}, nil); err != nil || !created {
		t.Fatalf("could not create the standalone research collision: %t %v", created, err)
	}
	config.Event = &standalone
	if _, err := invoke(config, trusted, "--roll-forward"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("standalone research was acknowledged as scheduled paper review: %v", err)
	}
	late := research_scheduler.EventReference{Kind: "news", Provider: provider, RecordID: "late-window"}
	config.Event = &late
	if result, err := invoke(config, trusted, "--roll-forward", "--max-research-age", "1ns"); err != nil ||
		result.Version != paper_workflow.IdentityVersion || result.Status != "missed" || result.Created || result.JobID != "" ||
		result.CreatedAt != nil || result.Trigger != nil || result.Snapshot != nil {
		t.Fatalf("closed uncommitted window produced a durable-looking receipt: %+v %v", result, err)
	}
	verifiedLate, err := research_scheduler.VerifyArchive(fixture.dir, archive, view.AsOf, []string{view.Symbol}, &late)
	if err != nil {
		t.Fatal(err)
	}
	lateTrigger, err := paper_thesis.CatalystReview(view.View, verifiedLate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.research.Get(fixture.ctx, lateTrigger.ID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("window that missed admission left a job: %v", err)
	}
	lease, err := fixture.research.ClaimWithIdentityRegistry(fixture.ctx, "stable-review-worker",
		"60000000-0000-4000-8000-000000000001", time.Hour, trusted)
	var wire struct {
		Version  string                                   `json:"job_schema_version"`
		Registry paper_workflow.ArchiveReference          `json:"identity_registry"`
		Bindings []paper_workflow.ResearchSecurityBinding `json:"security_bindings"`
	}
	if err != nil || lease.Job.ID != first.JobID || json.Unmarshal(lease.Job.Request, &wire) != nil || wire.Version != "0.2" ||
		wire.Registry != identity.Registry || len(wire.Bindings) != 1 || wire.Bindings[0].SecurityID != fixture.securityID {
		t.Fatalf("worker did not lease exact stable review: %+v %+v %v", lease.Job, wire, err)
	}
	checkStableReduction(t, fixture, config.OpeningOrderID, lease, task, dispatch, portfolio, archive, exits)
}

func checkStableReduction(t *testing.T, f stableEntryFixture, openingOrderID string, lease jobs.Lease, task jobs.PaperDispatchTask, dispatch paper_workflow.Request,
	portfolio contract.Portfolio, manifest paper_workflow.ArchiveReference, exits []stableReviewQuote) {
	t.Helper()
	const callID = "60000000-0000-4000-8000-000000000002"
	const runID = "60000000-0000-4000-8000-000000000003"
	const decisionID = "60000000-0000-4000-8000-000000000004"
	const report = "Synthetic review decision to reduce the original security"
	if _, _, err := f.research.AuthorizeCall(f.ctx, lease.Job.ID, lease.WorkerID, lease.Token, callID); err != nil {
		t.Fatal(err)
	}
	identity := dispatch.Identity
	symbol := identity.Securities[0].Symbol
	api, err := control.NewHandlerWithResearchIdentity(time.Hour, f.clock, f.research, f.paper, identity.Registry.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	done := f.clock()
	evidenceID := symbol + ":final_trade_decision"
	post(t, api, "/v1/research/jobs/"+lease.Job.ID+"/complete", map[string]any{
		"result_schema_version": "0.3", "worker_id": lease.WorkerID, "lease_token": lease.Token,
		"batch": contract.Batch{Version: contract.BatchVersion, Mode: "research", RunID: runID, SnapshotID: portfolio.ID,
			AsOf: portfolio.AsOf, Created: done, Expires: done.Add(time.Minute), Evidence: map[string]string{evidenceID: digestHex([]byte(report))},
			Intents: []contract.Intent{{ID: decisionID, Symbol: symbol, Action: "REDUCE", Notional: "40", Confidence: new(0.8), Expected: new(0.0),
				CostBPS: 30, Horizon: contract.Horizon{Min: 1, Max: 2}, Thesis: []string{"fixture"},
				Invalidation: []string{"fixture"}, Evidence: []string{evidenceID}}}},
		"reports": map[string]map[string]string{symbol: {"final_trade_decision": report}},
		"metadata": map[string]any{
			"source_mode": "tradingagents_offline", "alpha_measured": false, "intraday_pit_verified": false, "llm_calls": 1,
			"input_tokens": 0, "output_tokens": 0, "missing_usage_calls": 1, "usd_cost": nil,
			"provenance": map[string]any{"job_id": lease.Job.ID, "attempt": lease.Attempt, "policy_version": dispatch.SnapshotContext.Policy.Version,
				"data_manifest": manifest, "worker_version": "fixture", "upstream_version": "fixture", "upstream_sha": strings.Repeat("a", 40),
				"config_sha256": strings.Repeat("a", 64), "model_id": "scripted-offline-v1", "identity_registry": identity.Registry,
				"security_bindings":         []paper_workflow.ResearchSecurityBinding{{Symbol: symbol, SecurityID: identity.Securities[0].SecurityID}},
				"projected_snapshot_sha256": identity.ProjectedSnapshotSHA256},
		},
	})
	completed, err := f.research.GetResult(f.ctx, lease.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	idx := slices.IndexFunc(exits, func(exit stableReviewQuote) bool { return exit.quote.ObservedAt.After(completed.CompletedAt) })
	if idx < 0 {
		t.Fatalf("review completed after the last staged fixture quote at %s", completed.CompletedAt)
	}
	quote := exits[idx].quote
	if delay := exits[idx].availableAt.Sub(f.clock()); delay >= 0 {
		time.Sleep(delay + 20*time.Millisecond)
	}
	rawQuote := encode(t, quote)
	quotes := func(context.Context, string) ([]byte, error) { return rawQuote, nil }
	options := boundDispatchOptions{f.dir, identity.Registry.SHA256, identity.QuoteTape.SHA256}
	followed, err := dispatchTask(f.ctx, f.research, f.paper, task, dispatch.SnapshotContext.Policy, quotes, options)
	if err != nil || followed.Status != "applied" || followed.Application == nil || followed.Application.Execution == nil {
		t.Fatalf("stable review did not book its reduction: %+v %v", followed, err)
	}
	order := followed.Application.Execution.Order
	if order.Status != "filled" || order.Side != "SELL" || len(order.Fills) != 1 || order.Fills[0].QuantityRaw != "200" {
		t.Fatalf("review did not record the exact sell fill: %+v", order)
	}
	err = ledger.WithReadSnapshot(f.ctx, f.dsn, func(snapshot ledger.ReadSnapshot) error {
		persisted, err := snapshot.GetOrder(f.ctx, f.accountID, order.OrderID)
		if err != nil {
			return err
		}
		proof, err := paper_workflow.ReadExitIdentity(f.ctx, f.research, snapshot, persisted, snapshot.Cutoff())
		if err != nil || proof.SecurityID != quote.SecurityID || proof.Registry != identity.Registry || proof.Decision.Action != "REDUCE" ||
			proof.Decision.ID != decisionID || proof.Instrument != quote.Instrument || len(proof.ApplicationSHA256) != 64 {
			t.Fatalf("exit lost its exact paper identity provenance: %+v %v", proof, err)
		}
		if _, err := paper_workflow.ReadOpeningIdentity(f.ctx, f.research, snapshot, persisted, snapshot.Cutoff()); !errors.Is(err, paper_workflow.ErrInvalid) {
			t.Fatalf("a reduction could masquerade as a proved opening: %v", err)
		}
		input, err := parseIdentityOutcome(t, paper_thesis.OutcomeRequest{
			Version: paper_workflow.IdentityVersion, Source: paper_sim.Source, AccountID: f.accountID,
			OpeningOrderID: openingOrderID, AsOf: snapshot.Cutoff(), MaxObservationAgeSeconds: 60,
			ExitObservations: []paper_thesis.ExitObservation{},
		})
		if err != nil {
			return err
		}
		outcome, err := paper_thesis.BuildOutcome(f.ctx, f.research, snapshot, input, snapshot.Cutoff())
		if err != nil || outcome.Status != "immature" || outcome.Actual.RealizedNetCashUSD != "39.92" ||
			outcome.Actual.RealizedCostBasisUSD != "40.08" || outcome.Actual.RealizedPnLUSD != "-0.16" ||
			outcome.Actual.OpenQuantityRaw != "300" || outcome.Actual.OpenCostBasisUSD != "60.12" ||
			len(outcome.ExitApplications) != 1 || outcome.ExitApplications[0].FillEventID != order.Fills[0].EventID ||
			outcome.ExitApplications[0].OrderID != order.OrderID || outcome.ExitApplications[0].ApplicationSHA256 != proof.ApplicationSHA256 {
			t.Fatalf("stable FIFO outcome did not attest the verified reduction: %+v %v", outcome, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err := f.paper.GetAccount(f.ctx, f.accountID)
	if err != nil || account.CashUSD != "939.72" || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "300" {
		t.Fatalf("confirmed stable exit failed to reconcile virtual cash and quantities: %+v %v", account, err)
	}
}
