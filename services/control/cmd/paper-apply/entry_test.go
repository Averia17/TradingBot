package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperrisk"
	"tradingbot/control/internal/papersim"
	"tradingbot/control/internal/researchscheduler"
)

func TestScheduleEntryDurablyBindsScreenedResearchToPaperDispatch(t *testing.T) {
	const accountID = "11111111-1111-4111-8111-111111111111"
	const suggestionID = "22222222-2222-4222-8222-222222222222"
	const claimID = "33333333-3333-4333-8333-333333333333"
	const decisionID = "55555555-5555-4555-8555-555555555555"
	const runID = "66666666-6666-4666-8666-666666666666"
	ctx, dsn, clock := isolatedDatabase(t)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer research.Close()
	paper, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer paper.Close()
	handler := control.NewHandlerWithPaper(time.Hour, clock, research, paper)
	post(t, handler, "/v1/paper/accounts", ledger.CreateAccount{ID: accountID, InitialCashUSD: "1000"})

	instrument := ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2}
	policy := paperrisk.Policy{
		Version: "entry-e2e", MaxOrderUSD: "120", MinFreeCashUSD: "100",
		MaxPositionBPS: 2500, MaxGroupBPS: 5000, MaxGrossBPS: 9000, MaxFeeBPS: 30, MaxMarkAgeSeconds: 60,
		Instruments: []paperrisk.InstrumentRule{{Instrument: instrument, Symbol: "TSLA", Group: "tech"}},
	}
	account, err := paper.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	at, err := research.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := paperapply.SnapshotContext{AccountID: accountID, SnapshotVersion: account.Version, Policy: policy,
		Marks: []paperrisk.Mark{{Instrument: instrument, PriceUSD: "20", At: at, AvailableAt: at}}}
	portfolio, err := paperapply.Snapshot(account, snapshot, at)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	saveDocument(t, dir, "entry-data", archive)
	archiveDigest := sha256.Sum256(archive)
	selection := selectionFixture(t, suggestionID, portfolio, policy.Version, archiveDigest)
	selectionDigest := sha256.Sum256(selection)
	selectionPath := saveDocument(t, dir, "entry-selection", selection)
	request := paperapply.EntrySchedule{
		Version: paperapply.Version, ID: "new-entries", At: at,
		MaxLateness: 30, Budget: 20, Context: snapshot, SelectionSHA256: hex.EncodeToString(selectionDigest[:]),
	}
	configPath := func(name string, value paperapply.EntrySchedule) string {
		return saveDocument(t, dir, name, encode(t, value))
	}
	execute := func(config string) (paperapply.EntryReceipt, error) {
		var stdout bytes.Buffer
		err := run(ctx, dsn, []string{"schedule-entry", "--config", config, "--selection", selectionPath, "--archive-root", dir}, &stdout, clock)
		if err != nil {
			return paperapply.EntryReceipt{}, err
		}
		var receipt paperapply.EntryReceipt
		err = json.Unmarshal(stdout.Bytes(), &receipt)
		return receipt, err
	}
	notDue := request
	notDue.At = at.Add(time.Hour)
	if receipt, err := execute(configPath("future", notDue)); err != nil || receipt.Status != "not_due" || receipt.JobID != "" {
		t.Fatalf("future occurrence: %+v %v", receipt, err)
	}
	wrongPin := request
	wrongPin.SelectionSHA256 = hex.EncodeToString(make([]byte, 32))
	if _, err := execute(configPath("wrong-pin", wrongPin)); err == nil {
		t.Fatal("accepted unpinned selection")
	}
	lowBudget := request
	lowBudget.Budget = 10
	if _, err := execute(configPath("budget", lowBudget)); err == nil {
		t.Fatal("accepted selection beyond owner research budget")
	}
	config := configPath("entry", request)
	results := make(chan paperapply.EntryReceipt, 2)
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			value, err := execute(config)
			results <- value
			failures <- err
		})
	}
	workers.Wait()
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	first, second := <-results, <-results
	if first.Status != "queued" || second.Status != "queued" || first.Created == second.Created || first.JobID != second.JobID || first.JobID == suggestionID {
		t.Fatalf("concurrent admission: %+v %+v", first, second)
	}
	binding, err := research.GetPaperDispatch(ctx, first.JobID)
	if err != nil || binding.AccountID != accountID {
		t.Fatalf("read durable paper binding: %+v %v", binding, err)
	}
	var application paperapply.Request
	if contract.Decode(binding.Request, contract.PaperApplication, &application) != nil ||
		application.JobID != first.JobID || application.SnapshotContext.SnapshotVersion != 1 || application.EntryScheduleSHA256 == "" {
		t.Fatalf("invalid durable paper binding: %s", binding.Request)
	}
	lease, err := research.Claim(ctx, "entry-fixture", claimID, time.Hour)
	if err != nil || lease.Job.ID != first.JobID {
		t.Fatalf("claim initial paper research: %+v %v", lease, err)
	}
	const reportText = "Offline fixture BUY for a screened candidate"
	reportDigest := sha256.Sum256([]byte(reportText))
	created, err := research.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	batch := contract.Batch{
		Version: contract.BatchVersion, Mode: "research", RunID: runID, SnapshotID: portfolio.ID,
		AsOf: portfolio.AsOf, Created: created, Expires: created.Add(time.Minute),
		Evidence: map[string]string{"TSLA:final_trade_decision": hex.EncodeToString(reportDigest[:])},
		Intents: []contract.Intent{{ID: decisionID, Symbol: "TSLA", Action: "BUY", Notional: "100", Confidence: new(0.75), Expected: new(2.0), CostBPS: 30,
			Horizon: contract.Horizon{Min: 1, Max: 2}, Thesis: []string{"fixture"}, Invalidation: []string{"fixture"},
			Evidence: []string{"TSLA:final_trade_decision"}}},
	}
	post(t, handler, "/v1/research/jobs/"+first.JobID+"/complete", map[string]any{
		"result_schema_version": "0.1", "worker_id": "entry-fixture", "lease_token": lease.Token, "batch": batch,
		"reports":  map[string]map[string]string{"TSLA": {"final_trade_decision": reportText}},
		"metadata": map[string]any{"source_mode": "synthetic_fixture", "alpha_measured": false, "llm_calls": 0},
	})
	policyPath := saveDocument(t, dir, "entry-policy", encode(t, policy))
	completed, err := research.GetResult(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	quote := encode(t, papersim.Quote{
		Version: papersim.Version, Source: papersim.Source, Instrument: instrument, Side: "BUY", QuantityRaw: "500",
		GrossUSD: "100", FeeBPS: new(20), FixedFeeUSD: "0", ObservedAt: completed.CompletedAt.Add(time.Microsecond),
		ExpiresAt: batch.Expires, Execution: "fill",
	})
	quoteDigest := sha256.Sum256(quote)
	saveDocument(t, dir, "entry-quote", quote)
	saveDocument(t, dir, first.JobID, encode(t, map[string]any{
		"paper_quote_pin_schema_version": "0.1", "job_id": first.JobID,
		"quote": map[string]string{"ref": "entry-quote.json", "sha256": hex.EncodeToString(quoteDigest[:])},
	}))
	dispatchArgs := []string{"dispatch", "--account", accountID, "--policy", policyPath, "--quote-root", dir}
	var output bytes.Buffer
	if err := run(ctx, dsn, dispatchArgs, &output, clock); err != nil {
		t.Fatal(err)
	}
	var applied paperapply.FollowResult
	if json.Unmarshal(output.Bytes(), &applied) != nil || applied.Status != "applied" || applied.JobID != first.JobID ||
		applied.Application == nil || applied.Application.Execution == nil || applied.Application.Execution.Order.Status != "filled" ||
		applied.Application.Execution.Order.ResearchJobID != first.JobID {
		t.Fatalf("could not dispatch screened job: %s", output.Bytes())
	}
	output.Reset()
	if err := run(ctx, dsn, dispatchArgs, &output, clock); err != nil || output.Len() != 0 {
		t.Fatalf("acknowledged paper job was dispatched again: %q %v", output.String(), err)
	}
	book, err := paper.GetAccount(ctx, accountID)
	if err != nil || book.Version != 3 || book.CashUSD != "899.8" || book.ReservedCashUSD != "0" ||
		len(book.Positions) != 1 || book.Positions[0].QuantityRaw != "500" {
		t.Fatalf("unexpected virtual account: %+v %v", book, err)
	}
	changed := request
	changed.Budget = 21
	if _, err := execute(configPath("changed", changed)); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("accepted changed owner parameters for same occurrence: %v", err)
	}
	replay, err := execute(config)
	if err != nil || replay.Created || replay.JobID != first.JobID || replay.Status != "completed" {
		t.Fatalf("frozen replay after account advance: %+v %v", replay, err)
	}
	stale := request
	stale.ID = "newer-screen"
	if _, err := execute(configPath("stale", stale)); !errors.Is(err, ledger.ErrStale) {
		t.Fatalf("accepted unbound current book: %v", err)
	}
	missed, err := paperapply.ScheduleEntry(ctx, research, paper, stale, selection, dir, at.Add(time.Hour), time.Hour)
	if err != nil || missed.Status != "missed" || missed.JobID != "" {
		t.Fatalf("late occurrence: %+v %v", missed, err)
	}
	staleID := researchscheduler.PaperEntryOccurrenceID(accountID, stale.ID, stale.At)
	if _, err := research.Get(ctx, staleID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("stale/missed job was persisted: %v", err)
	}

	var selectionFields, plain map[string]json.RawMessage
	if err := json.Unmarshal(selection, &selectionFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(selectionFields["job"], &plain); err != nil {
		t.Fatal(err)
	}
	researchOnly := request
	researchOnly.ID = "research-only"
	plainID := researchscheduler.PaperEntryOccurrenceID(accountID, researchOnly.ID, at)
	plain["job_id"] = encode(t, plainID)
	post(t, handler, "/v1/research/jobs", plain)
	if _, err := execute(configPath("research-only", researchOnly)); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("ordinary research job gained automatic application on replay: %v", err)
	}
	if _, err := research.GetPaperDispatch(ctx, plainID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("ordinary research job acquired a dispatch binding: %v", err)
	}
}

func selectionFixture(t *testing.T, jobID string, portfolio contract.Portfolio, policy string, archiveDigest [32]byte) []byte {
	t.Helper()
	return encode(t, map[string]any{
		"selection_schema_version": "0.1", "source_mode": "synthetic_fixture",
		"snapshot_sha256": hex.EncodeToString(archiveDigest[:]), "stockstats_version": "0.6.9",
		"job": map[string]any{"job_schema_version": "0.1", "job_id": jobID, "portfolio": portfolio,
			"requested_symbols": []string{"TSLA"}, "policy_version": policy, "max_llm_calls": 17,
			"data_manifest": researchscheduler.ArchiveReference{Ref: "entry-data.json", SHA256: hex.EncodeToString(archiveDigest[:])}},
		"candidates": []map[string]any{{"symbol": "TSLA", "status": "selected"}},
	})
}

func TestEntryScheduleRejectsBadCLIConfiguration(t *testing.T) {
	if err := run(context.Background(), "", []string{"schedule-entry"}, &bytes.Buffer{}, time.Now); err == nil {
		t.Fatal("missing flags were accepted")
	}
}

func TestPublishedEntryFixturesBindPythonSelectionAndGoSnapshot(t *testing.T) {
	var request paperapply.EntrySchedule
	if err := decodeDocument("../../../../contracts/valid-paper-entry-schedule.json", contract.PaperEntrySchedule, &request); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../contracts/valid-paper-entry-selection.json")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	var selection map[string]json.RawMessage
	var job struct {
		Portfolio        json.RawMessage `json:"portfolio"`
		RequestedSymbols []string        `json:"requested_symbols"`
		Budget           int             `json:"max_llm_calls"`
	}
	if err := contract.Decode(raw, contract.PaperEntrySelection, &selection); err != nil ||
		request.SelectionSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("fixture selection does not match its schema and pin: %v", err)
	}
	if err := json.Unmarshal(selection["job"], &job); err != nil || job.Budget != 17 ||
		len(job.RequestedSymbols) != 1 || job.RequestedSymbols[0] != "NVDA" {
		t.Fatalf("unexpected fixture job: %+v %v", job, err)
	}
	portfolio, err := contract.ParsePortfolio(job.Portfolio)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Context.CheckPortfolio(portfolio); err != nil {
		t.Fatal(err)
	}
}
