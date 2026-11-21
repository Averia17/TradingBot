package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_risk"
	"tradingbot/control/internal/paper_workflow"
)

func TestPaperCheckReportsBoundDispatchAndBudget(t *testing.T) {
	ctx, dsn, inspection := diagnosticDatabase(t)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(research.Close)
	paper, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paper.Close)
	const symbol, securityID = "TSLA", "fixture-current"
	accountID, jobID := diagnosticID(1), diagnosticID(2)
	account, _, err := paper.CreateAccount(ctx, ledger.CreateAccount{ID: accountID, InitialCashUSD: "1000"})
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time {
		at, err := research.Now(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	at := clock()
	instrument := ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2}
	context := paper_workflow.SnapshotContext{
		AccountID: accountID, SnapshotVersion: account.Version,
		Policy: paper_risk.Policy{Version: "fixture-policy", MaxOrderUSD: "200", MinFreeCashUSD: "10",
			MaxPositionBPS: 2500, MaxGroupBPS: 5000, MaxGrossBPS: 8000, MaxFeeBPS: 50, MaxMarkAgeSeconds: 600,
			Instruments: []paper_risk.InstrumentRule{{Symbol: symbol, Group: "stock", Instrument: instrument}}},
		Marks: []paper_risk.Mark{{Instrument: instrument, PriceUSD: "100", At: at, AvailableAt: at}},
	}
	portfolio, err := paper_workflow.Snapshot(account, context, at)
	if err != nil {
		t.Fatal(err)
	}
	pin := strings.Repeat("a", 64)
	ref := paper_workflow.ArchiveReference{Ref: "fixtures/registry.json", SHA256: pin}
	manifest := paper_workflow.ArchiveReference{Ref: "fixtures/market.json", SHA256: strings.Repeat("b", 64)}
	bindings := []paper_workflow.ResearchSecurityBinding{{Symbol: symbol, SecurityID: securityID}}
	marshal := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	dispatch := paper_workflow.Request{
		Version: paper_workflow.IdentityVersion, JobID: jobID, SnapshotContext: context,
		EntryScheduleSHA256: strings.Repeat("c", 64),
		Identity: &paper_workflow.IdentityBinding{
			OwnerIdentity: paper_workflow.OwnerIdentity{Registry: ref,
				QuoteTape: paper_workflow.ArchiveReference{Ref: "fixtures/tape.json", SHA256: strings.Repeat("d", 64)}},
			ProjectedSnapshotSHA256: strings.Repeat("e", 64),
			Securities:              []paper_workflow.BoundSecurity{{Symbol: symbol, SecurityID: securityID, Instrument: instrument}},
		},
	}
	job := jobs.CreateJob{ID: jobID, SnapshotID: portfolio.ID, Snapshot: marshal(portfolio),
		Request: marshal(map[string]any{
			"job_schema_version": "0.2", "job_id": jobID, "portfolio": portfolio, "max_llm_calls": 2,
			"requested_symbols": []string{symbol}, "data_manifest": manifest, "policy_version": context.Policy.Version,
			"identity_registry": ref, "security_bindings": bindings,
		}),
		PaperDispatch: &jobs.PaperDispatchInput{AccountID: accountID, Request: marshal(dispatch)},
	}
	if _, created, err := research.CreateBoundPaperJob(ctx, jobs.BoundPaperJob{Job: job, OwnerRegistrySHA256: pin}, nil); err != nil || !created {
		t.Fatalf("create stable job: %t %v", created, err)
	}
	check := func(want int, stage string) report {
		t.Helper()
		var out bytes.Buffer
		if status := run(ctx, dsn, []string{"--account", accountID, "--pipeline", "--job", jobID}, &out); status != want {
			t.Fatalf("paper-check exit=%d want=%d: %s", status, want, out.String())
		}
		checkPaperReportSchema(t, out.Bytes())
		var got report
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Pipeline.Jobs) != 1 || got.Pipeline.Jobs[0].Stage != stage {
			t.Fatalf("wrong pipeline stage %s: %s (%v)", stage, out.String(), err)
		}
		return got
	}
	queued := check(0, "queued")
	if queued.Research.GuaranteedRemaining == nil || *queued.Research.GuaranteedRemaining != 2 {
		t.Fatalf("missing queued budget: %+v", queued.Research)
	}
	lease, err := research.ClaimWithIdentityRegistry(ctx, "diagnostic-fixture", diagnosticID(3), time.Hour, pin)
	if err != nil || lease.Job.ID != jobID {
		t.Fatalf("claim stable job: %v", err)
	}
	check(0, "leased")
	if _, _, err := research.AuthorizeCall(ctx, jobID, lease.WorkerID, lease.Token, diagnosticID(4)); err != nil {
		t.Fatal(err)
	}
	reported := clock()
	evidenceKey, evidence := symbol+":final_trade_decision", "Offline hold"
	digest := sha256.Sum256([]byte(evidence))
	handler, err := control.NewHandlerWithResearchIdentity(time.Hour, clock, research, paper, pin)
	if err != nil {
		t.Fatal(err)
	}
	completion := map[string]any{
		"result_schema_version": "0.3", "worker_id": lease.WorkerID, "lease_token": lease.Token,
		"batch": contract.Batch{
			Version: contract.BatchVersion, Mode: "research", RunID: diagnosticID(5), SnapshotID: portfolio.ID,
			AsOf: portfolio.AsOf, Created: reported, Expires: reported.Add(time.Minute),
			Evidence: map[string]string{evidenceKey: hex.EncodeToString(digest[:])},
			Intents: []contract.Intent{{ID: diagnosticID(6), Symbol: symbol, Action: "HOLD", Notional: "0",
				Horizon: contract.Horizon{Min: 1, Max: 2}, Thesis: []string{"offline"},
				Invalidation: []string{"offline"}, Evidence: []string{evidenceKey}}},
		},
		"reports": map[string]map[string]string{symbol: {"final_trade_decision": evidence}},
		"metadata": map[string]any{
			"source_mode": "tradingagents_offline", "alpha_measured": false, "intraday_pit_verified": false,
			"llm_calls": 1, "input_tokens": 10, "output_tokens": 10, "missing_usage_calls": 0, "usd_cost": nil,
			"provenance": map[string]any{
				"job_id": jobID, "attempt": lease.Attempt, "policy_version": context.Policy.Version,
				"data_manifest": manifest, "worker_version": "fixture", "upstream_version": "fixture",
				"upstream_sha": strings.Repeat("a", 40), "config_sha256": strings.Repeat("a", 64), "model_id": "scripted-offline-v1",
				"identity_registry": ref, "security_bindings": bindings, "projected_snapshot_sha256": dispatch.Identity.ProjectedSnapshotSHA256,
			},
		},
	}
	recorder := httptest.NewRecorder()
	httpRequest := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/research/jobs/"+jobID+"/complete", bytes.NewReader(marshal(completion)))
	httpRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, httpRequest)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("complete stable job: %d %s", recorder.Code, recorder.Body.String())
	}
	complete := check(0, "awaiting_application")
	if complete.Research.GuaranteedRemaining == nil || *complete.Research.GuaranteedRemaining != 1 ||
		complete.Research.ReportedCalls == nil || *complete.Research.ReportedCalls != 1 || complete.Research.UnconfirmedPermits != 0 {
		t.Fatalf("wrong committed budget: %+v", complete.Research)
	}
	mutate := func(query string, raw []byte) {
		t.Helper()
		if _, err := inspection.Exec(ctx, query, jobID, raw); err != nil {
			t.Fatal(err)
		}
	}
	const dispatchSQL = `UPDATE research_paper_dispatch SET request=$2, request_sha256=sha256($2) WHERE job_id=$1`
	const resultSQL = `UPDATE research_job_results SET payload=$2, payload_sha256=sha256($2) WHERE job_id=$1`
	wrongRegistry := func(raw []byte) []byte {
		return bytes.Replace(raw, []byte(`"sha256":"`+pin+`"`), []byte(`"sha256":"`+strings.Repeat("f", 64)+`"`), 1)
	}
	mutate(dispatchSQL, wrongRegistry(job.PaperDispatch.Request))
	damaged := check(1, "inconsistent")
	if len(damaged.Issues) != 1 || damaged.Issues[0].Code != "paper_dispatch_inconsistent" ||
		damaged.Research.ReportedCalls == nil || *damaged.Research.ReportedCalls != 1 {
		t.Fatalf("registry mismatch conflated with intact budget: %+v", damaged)
	}
	mutate(dispatchSQL, job.PaperDispatch.Request)
	receipt, err := research.GetResult(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	mutate(resultSQL, wrongRegistry(receipt.Payload))
	damaged = check(1, "inconsistent")
	seen := make(map[string]bool)
	for _, problem := range damaged.Issues {
		seen[problem.Code] = true
	}
	if !seen["paper_dispatch_inconsistent"] || !seen["research_result_integrity_failed"] ||
		damaged.Research.ReportedCalls != nil || damaged.Research.UnconfirmedPermits != 1 ||
		damaged.Research.GuaranteedRemaining == nil || *damaged.Research.GuaranteedRemaining != 1 {
		t.Fatalf("changed result converted unconfirmed usage into evidence: %+v", damaged)
	}
	mutate(resultSQL, receipt.Payload)
	finished, err := paper_workflow.FollowBound(ctx, research, paper, dispatch, nil, nil, clock())
	if err != nil || finished.Status != "hold" {
		t.Fatalf("apply registry-bound HOLD: %s %v", finished.Status, err)
	}
	task, err := research.GetPaperDispatch(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := research.FinishPaperDispatch(ctx, task, finished.Status); err != nil {
		t.Fatal(err)
	}
	acknowledged := check(0, "finished").Pipeline.Jobs[0]
	if !acknowledged.ApplicationReceipt || acknowledged.FinishedStatus != "hold" {
		t.Fatalf("wrong durable acknowledgement: %+v", acknowledged)
	}
}
