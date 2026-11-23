package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/paper_risk"
	"tradingbot/trading-api/internal/paper_thesis"
	"tradingbot/trading-api/internal/paper_workflow"
	"tradingbot/trading-api/internal/research_scheduler"
	"tradingbot/trading-api/internal/trading_api"
)

func TestPublishedReviewScheduleFixtureUsesAutomaticSnapshot(t *testing.T) {
	var request paper_thesis.ScheduleRequest
	if err := decodeDocument("../../../../contracts/valid-paper-review-schedule.json", contract.PaperReviewSchedule, &request); err != nil {
		t.Fatal(err)
	}
	if request.Version != paper_thesis.Version || request.Portfolio != nil || request.Context.SnapshotVersion != 0 || request.Event == nil {
		t.Fatalf("expected a dynamic catalyst schedule: %+v", request)
	}
	request.Context.SnapshotVersion = 1
	if err := contract.Decode(encode(t, request), contract.PaperReviewSchedule, &paper_thesis.ScheduleRequest{}); err == nil {
		t.Fatal("accepted a frozen version without a portfolio")
	}
}

func TestScheduleReviewCreatesOneClaimableJobAcrossRestarts(t *testing.T) {
	const (
		accountID   = "11111111-1111-4111-8111-111111111111"
		openingID   = "22222222-2222-4222-8222-222222222222"
		researchID  = "33333333-3333-4333-8333-333333333333"
		decisionID  = "44444444-4444-4444-8444-444444444444"
		quoteID     = "55555555-5555-4555-8555-555555555555"
		fillID      = "66666666-6666-4666-8666-666666666666"
		claimID     = "77777777-7777-4777-8777-777777777777"
		reviewClaim = "88888888-8888-4888-8888-888888888888"
		runID       = "99999999-9999-4999-8999-999999999999"
		reportText  = "Synthetic opening"
	)
	ctx, dsn, clock := isolatedDatabase(t)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer research.Close()
	paper, err := accounts.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer paper.Close()
	handler := trading_api.NewHandlerWithPaper(time.Hour, clock, research, paper)
	post(t, handler, "/v1/paper/accounts", accounts.CreateAccount{ID: accountID, InitialCashUSD: "1000"})

	instrument := accounts.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2}
	policy := paper_risk.Policy{
		Version: "review-e2e", MaxOrderUSD: "120", MinFreeCashUSD: "100",
		MaxPositionBPS: 2500, MaxGroupBPS: 5000, MaxGrossBPS: 9000, MaxFeeBPS: 30, MaxMarkAgeSeconds: 60,
		Instruments: []paper_risk.InstrumentRule{{Instrument: instrument, Symbol: "TSLA", Group: "tech"}},
	}
	openingAt := clock()
	portfolio := contract.Portfolio{ID: openingID, AsOf: openingAt, Currency: "USD", CashAsset: "USD",
		Cash: "1000", Reserved: "0", MaxPosition: "0.25", Positions: []contract.Position{}}
	openingArchive := research_scheduler.ArchiveReference{Ref: "opening.json", SHA256: hex.EncodeToString(make([]byte, 32))}
	post(t, handler, "/v1/research/jobs", map[string]any{
		"job_schema_version": "0.1", "job_id": researchID, "portfolio": portfolio,
		"data_manifest": openingArchive, "policy_version": policy.Version,
		"requested_symbols": []string{"TSLA"}, "max_llm_calls": 1,
	})
	lease, err := research.Claim(ctx, "opening-fixture", claimID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expires := clock().Add(time.Minute)
	reportHash := sha256.Sum256([]byte(reportText))
	batch := contract.Batch{
		Version: contract.BatchVersion, Mode: "research", RunID: runID, SnapshotID: portfolio.ID,
		AsOf: openingAt, Created: clock(), Expires: expires,
		Evidence: map[string]string{"TSLA:final_trade_decision": hex.EncodeToString(reportHash[:])},
		Intents: []contract.Intent{{ID: decisionID, Symbol: "TSLA", Action: "BUY", Notional: "100", Confidence: new(0.75), Expected: new(2.0),
			Horizon: contract.Horizon{Min: 1, Max: 2}, Thesis: []string{"fixture"}, Invalidation: []string{"fixture"},
			Evidence: []string{"TSLA:final_trade_decision"}, CostBPS: 25}},
	}
	post(t, handler, "/v1/research/jobs/"+researchID+"/complete", map[string]any{
		"result_schema_version": "0.1", "worker_id": "opening-fixture", "lease_token": lease.Token, "batch": batch,
		"reports":  map[string]map[string]string{"TSLA": {"final_trade_decision": reportText}},
		"metadata": map[string]any{"source_mode": "synthetic_fixture", "alpha_measured": false, "llm_calls": 0},
	})
	opened, _, err := paper.Reserve(ctx, accounts.ReservationRequest{
		ResearchJobID: researchID, AccountID: accountID, OrderID: openingID, DecisionID: decisionID, QuoteID: quoteID,
		SnapshotVersion: 1, Instrument: instrument, Side: "BUY", QuantityRaw: "500", GrossUSD: "100", MaxFeeUSD: "0.25", ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = paper.Resolve(ctx, accounts.ResolutionRequest{AccountID: accountID, OrderID: openingID, EventID: fillID,
		Outcome: "partially_filled", QuantityRaw: "250", GrossUSD: "50", FeeUSD: "0.1", ObservedAt: opened.CreatedAt})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	observed := clock()
	archiveBytes := encode(t, map[string]any{"schema_version": "0.1", "source_mode": "synthetic_fixture", "records": []map[string]any{{
		"kind": "news", "provider": "local", "record_id": "review-1", "symbol": "TSLA", "revision": 1, "event_time": observed,
		"available_at": observed, "ingested_at": observed, "payload": map[string]string{"headline": "fixture"},
	}}})
	saveDocument(t, dir, "review-data", archiveBytes)
	dataDigest := sha256.Sum256(archiveBytes)
	config := paper_thesis.ScheduleRequest{
		Version: paper_thesis.Version, OpeningOrderID: openingID,
		Context:  paper_workflow.SnapshotContext{AccountID: accountID, Policy: policy, Marks: []paper_risk.Mark{{Instrument: instrument, PriceUSD: "20", At: observed, AvailableAt: observed}}},
		Manifest: research_scheduler.ArchiveReference{Ref: "review-data.json", SHA256: hex.EncodeToString(dataDigest[:])}, Budget: 3,
	}
	configPath := func() string {
		return saveDocument(t, dir, "review-config", encode(t, config))
	}
	execute := func(path string) (paper_thesis.ScheduleReceipt, error) {
		var stdout bytes.Buffer
		err := run(ctx, dsn, []string{"schedule-review", "--config", path, "--archive-root", dir}, &stdout, clock)
		if err != nil {
			return paper_thesis.ScheduleReceipt{}, err
		}
		var receipt paper_thesis.ScheduleReceipt
		if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
			return paper_thesis.ScheduleReceipt{}, err
		}
		return receipt, nil
	}
	early, err := execute(configPath())
	if err != nil || early.Status != "not_due" || early.JobID != "" {
		t.Fatalf("before deadline: %+v, %v", early, err)
	}
	config.Event = &research_scheduler.EventReference{Kind: "news", Provider: "local", RecordID: "review-1"}
	config.Manifest.SHA256 = openingArchive.SHA256
	if _, err := execute(configPath()); err == nil {
		t.Fatal("accepted unverified archive bytes")
	}
	config.Manifest.SHA256 = hex.EncodeToString(dataDigest[:])
	path := configPath()

	var dispatched [2]paper_thesis.ScheduleReceipt
	var dispatchErrs [2]error
	var attempts sync.WaitGroup
	for i := range dispatched {
		attempts.Go(func() { dispatched[i], dispatchErrs[i] = execute(path) })
	}
	attempts.Wait()
	queued := dispatched[0]
	if !queued.Created {
		queued = dispatched[1]
	}
	for i, receipt := range dispatched {
		if dispatchErrs[i] != nil || receipt.Status != "queued" || receipt.Trigger == nil ||
			receipt.JobID != receipt.Trigger.ID || receipt.JobID != queued.JobID ||
			receipt.Snapshot == nil || queued.Snapshot == nil || receipt.Snapshot.Portfolio.ID != queued.Snapshot.Portfolio.ID {
			t.Fatalf("concurrent review dispatch: %+v, %v", receipt, dispatchErrs[i])
		}
	}
	if dispatched[0].Created == dispatched[1].Created {
		t.Fatalf("expected exactly one durable creation: %+v", dispatched)
	}
	if p := queued.Snapshot.Portfolio; p.Cash != "949.9" || p.Reserved != "50.15" || len(p.Positions) != 1 || p.Positions[0].Quantity != "2.50000000" {
		t.Fatalf("did not bind the partial fill and reserve: %+v", p)
	}
	claimed, err := research.Claim(ctx, "review-fixture", reviewClaim, time.Hour)
	if err != nil || claimed.Job.ID != queued.JobID {
		t.Fatalf("review job not claimable: %s, %v", claimed.Job.ID, err)
	}
	if ready, err := research.ReadyPaperDispatch(ctx, accountID, ""); err != nil || len(ready) != 0 {
		t.Fatalf("dispatched raw completed research or an incomplete review: %+v, %v", ready, err)
	}
	reviewBatch := contract.Batch{
		Version: contract.BatchVersion, Mode: "research", RunID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		SnapshotID: queued.Snapshot.Portfolio.ID, AsOf: queued.Snapshot.Portfolio.AsOf, Created: clock(), Expires: expires,
		Evidence: map[string]string{"TSLA:final_trade_decision": hex.EncodeToString(reportHash[:])},
		Intents: []contract.Intent{{ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Symbol: "TSLA", Action: "HOLD", Notional: "0",
			Horizon: contract.Horizon{Min: 1, Max: 2}, Thesis: []string{"fixture"}, Invalidation: []string{"fixture"},
			Evidence: []string{"TSLA:final_trade_decision"}}},
	}
	post(t, handler, "/v1/research/jobs/"+queued.JobID+"/complete", map[string]any{
		"result_schema_version": "0.1", "worker_id": claimed.WorkerID, "lease_token": claimed.Token, "batch": reviewBatch,
		"reports":  map[string]map[string]string{"TSLA": {"final_trade_decision": reportText}},
		"metadata": map[string]any{"source_mode": "synthetic_fixture", "alpha_measured": false, "llm_calls": 0},
	})
	tasks, err := research.ReadyPaperDispatch(ctx, accountID, "")
	if err != nil || len(tasks) != 1 || tasks[0].JobID != queued.JobID {
		t.Fatalf("review did not have exactly one durable handoff: %+v, %v", tasks, err)
	}
	if foreign, err := research.ReadyPaperDispatch(ctx, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", ""); err != nil || len(foreign) != 0 {
		t.Fatalf("paper dispatch crossed account boundary: %+v, %v", foreign, err)
	}
	var bound paper_workflow.Request
	if err := contract.Decode(tasks[0].Request, contract.PaperApplication, &bound); err != nil || bound.JobID != queued.JobID ||
		bound.SnapshotContext.CheckPortfolio(queued.Snapshot.Portfolio) != nil {
		t.Fatalf("invalid persisted dispatch binding: %+v, %v", bound, err)
	}
	held, err := paper_workflow.Follow(ctx, research, paper, bound, nil, clock())
	if err != nil || held.Status != "hold" || held.Application == nil {
		t.Fatalf("HOLD did not commit before our simulated lost dispatch ACK: %+v, %v", held, err)
	}
	_, _, err = paper.Resolve(ctx, accounts.ResolutionRequest{AccountID: accountID, OrderID: openingID, EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Outcome: "cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := execute(path)
	if err != nil || replayed.Created || replayed.JobID != queued.JobID || replayed.Status != "completed" || replayed.Snapshot == nil ||
		replayed.Snapshot.Portfolio.ID != queued.Snapshot.Portfolio.ID || !replayed.Snapshot.Portfolio.AsOf.Equal(queued.Snapshot.Portfolio.AsOf) {
		t.Fatalf("restart and account advance changed accepted inputs: %+v, %v", replayed, err)
	}
	config.Budget++
	if _, err := execute(configPath()); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("changed budget should conflict: %v", err)
	}
	config.Budget--
	config.Context.Policy.MaxOrderUSD = "125"
	if _, err := execute(configPath()); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("reused policy version with changed limits should conflict: %v", err)
	}
	config.Context = queued.Snapshot.Context
	config.Portfolio = &queued.Snapshot.Portfolio
	replayed, err = execute(configPath())
	if err != nil || replayed.Created || replayed.JobID != queued.JobID {
		t.Fatalf("frozen historical retry: %+v, %v", replayed, err)
	}
	watch, stopWatch := context.WithTimeout(ctx, 5*time.Second)
	defer stopWatch()
	catalyst := config.Event
	config.Event = nil
	watched := 0
	writer := scheduleWriter(func(data []byte) (int, error) {
		var current paper_thesis.ScheduleReceipt
		if err := json.Unmarshal(data, &current); err != nil {
			return 0, err
		}
		watched++
		switch watched {
		case 1:
			if current.Status != "not_due" {
				return 0, errors.New("expected no clock trigger in first watch pass")
			}
			config.Event = catalyst
			configPath()
		case 2:
			if current.JobID != queued.JobID || current.Created {
				return 0, errors.New("watch did not reload the catalyst config and replay its existing job")
			}
			stopWatch()
		}
		return len(data), nil
	})
	if err := run(watch, dsn, []string{"schedule-review", "--config", configPath(), "--archive-root", dir, "--poll-interval", "1s"}, writer, clock); err != nil || watched != 2 {
		t.Fatalf("watch did not reload config and stop: %d passes, %v", watched, err)
	}
	if other, err := research.Claim(ctx, "review-extra", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", time.Hour); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("unexpected duplicate job %s: %v", other.Job.ID, err)
	}
	policyPath := saveDocument(t, dir, "policy", encode(t, policy))
	var applied bytes.Buffer
	dispatchArgs := []string{"dispatch", "--account", accountID, "--policy", policyPath}
	if err := run(ctx, dsn, dispatchArgs, &applied, clock); err != nil ||
		strings.TrimSpace(applied.String()) != string(encode(t, held)) {
		t.Fatalf("dispatch restart failed to acknowledge the same HOLD: %s, %v", applied.String(), err)
	}
	applied.Reset()
	if err := run(ctx, dsn, dispatchArgs, &applied, clock); err != nil || applied.Len() != 0 {
		t.Fatalf("acknowledged task was dispatched again: %s, %v", applied.String(), err)
	}
}

type scheduleWriter func([]byte) (int, error)

func (write scheduleWriter) Write(data []byte) (int, error) { return write(data) }
