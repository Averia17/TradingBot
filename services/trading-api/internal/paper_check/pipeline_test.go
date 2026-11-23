package paper_check

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/jobs"
)

func TestPaperCheckReportsDurablePipelineStalls(t *testing.T) {
	ctx, dsn, inspection := diagnosticDatabase(t)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(research.Close)
	paper, err := accounts.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paper.Close)
	accountID := diagnosticID(1)
	if _, _, err := paper.CreateAccount(ctx, accounts.CreateAccount{ID: accountID, InitialCashUSD: "100"}); err != nil {
		t.Fatal(err)
	}

	readonly, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parameters := readonly.Query()
	parameters.Set("default_transaction_read_only", "on")
	readonly.RawQuery = parameters.Encode()
	queued := createDiagnosticJob(t, ctx, research, accountID, 10)
	args := []string{"--account", accountID, "--pipeline", "--stall-after", "2m"}
	var stdout bytes.Buffer
	if code := run(ctx, readonly.String(), args, &stdout); code != 0 ||
		!strings.Contains(stdout.String(), `"stage":"queued"`) {
		t.Fatalf("pending recent queue is expected, got exit %d and %s", code, stdout.String())
	}

	lease, err := research.Claim(ctx, "worker-only-secret-fixture", diagnosticID(90), time.Hour)
	if err != nil || lease.Job.ID != queued.ID {
		t.Fatalf("claim first job: %v", err)
	}
	ready := createDiagnosticJob(t, ctx, research, accountID, 20)
	recovering := createDiagnosticJob(t, ctx, research, accountID, 30)
	awaitingAck := createDiagnosticJob(t, ctx, research, accountID, 40)
	finished := createDiagnosticJob(t, ctx, research, accountID, 50)
	for i, job := range []jobs.CreateJob{ready, recovering, awaitingAck, finished} {
		completeDiagnosticJob(t, ctx, research, job.ID, i)
	}
	digest := sha256.Sum256(awaitingAck.PaperDispatch.Request)
	if _, err := paper.RecordApplication(ctx, accounts.ApplicationInput{
		JobID: awaitingAck.ID, AccountID: accountID, SnapshotVersion: 1, RequestSHA256: digest,
	}, "", []byte(`{"status":"hold"}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	task, err := research.GetPaperDispatch(ctx, finished.ID)
	if err != nil {
		t.Fatal(err)
	}
	finishedDigest := sha256.Sum256(finished.PaperDispatch.Request)
	if _, err := paper.RecordApplication(ctx, accounts.ApplicationInput{
		JobID: finished.ID, AccountID: accountID, SnapshotVersion: 1, RequestSHA256: finishedDigest,
	}, "", []byte(`{"status":"hold"}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := research.FinishPaperDispatch(ctx, task, "hold"); err != nil {
		t.Fatal(err)
	}
	orderID := diagnosticID(70)
	reservation := accounts.ReservationRequest{
		AccountID: accountID, OrderID: orderID, DecisionID: diagnosticID(71), QuoteID: diagnosticID(72),
		Instrument: accounts.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 0},
		Side:       "BUY", QuantityRaw: "1", GrossUSD: "20", MaxFeeUSD: "0.01",
		SnapshotVersion: 1, ExpiresAt: time.Now().Add(time.Hour), ResearchJobID: recovering.ID,
	}
	recoveryDigest := sha256.Sum256(recovering.PaperDispatch.Request)
	order, _, err := paper.ReserveApplication(ctx, reservation, accounts.ApplicationInput{
		JobID: recovering.ID, AccountID: accountID, SnapshotVersion: 1, RequestSHA256: recoveryDigest,
		Quote: []byte(`{"quote":"fixture"}`), ReserveBefore: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	oldQueue := createDiagnosticJob(t, ctx, research, accountID, 60)
	otherAccount := createDiagnosticJob(t, ctx, research, diagnosticID(80), 80)
	for _, update := range []struct {
		sql string
		id  string
	}{
		{`UPDATE research_job_attempts SET started_at=clock_timestamp()-interval '5 minutes',
			expires_at=clock_timestamp()-interval '3 minutes' WHERE job_id=$1`, queued.ID},
		{`UPDATE research_job_results SET completed_at=clock_timestamp()-interval '3 minutes' WHERE job_id=$1`, ready.ID},
		{`UPDATE paper_applications SET created_at=clock_timestamp()-interval '3 minutes' WHERE job_id=$1`, recovering.ID},
		{`UPDATE paper_applications SET created_at=clock_timestamp()-interval '3 minutes' WHERE job_id=$1`, awaitingAck.ID},
		{`UPDATE research_jobs SET created_at=clock_timestamp()-interval '3 minutes' WHERE id=$1`, oldQueue.ID},
	} {
		if _, err := inspection.Exec(ctx, update.sql, update.id); err != nil {
			t.Fatal(err)
		}
	}
	stdout.Reset()
	if code := run(ctx, readonly.String(), append(args, "--job", finished.ID), &stdout); code != 1 {
		t.Fatalf("stalled pipeline got exit %d and %s", code, stdout.String())
	}
	var result struct {
		Pipeline struct {
			Jobs []struct {
				JobID   string `json:"job_id"`
				Stage   string `json:"stage"`
				OrderID string `json:"order_id"`
			} `json:"jobs"`
		} `json:"pipeline"`
		Issues []struct {
			Code    string `json:"code"`
			JobID   string `json:"job_id"`
			OrderID string `json:"order_id"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		queued.ID: "lease_expired", ready.ID: "awaiting_application", recovering.ID: "awaiting_recovery",
		awaitingAck.ID: "awaiting_ack", finished.ID: "finished", oldQueue.ID: "queued",
	}
	if len(result.Pipeline.Jobs) != len(expected) {
		t.Fatalf("wrong pending/selected job scope: %s", stdout.String())
	}
	for _, observed := range result.Pipeline.Jobs {
		if observed.Stage != expected[observed.JobID] || observed.JobID == recovering.ID && observed.OrderID != orderID {
			t.Fatalf("wrong pipeline evidence: %s", stdout.String())
		}
		delete(expected, observed.JobID)
	}
	expectedIssues := map[string]string{
		queued.ID: "research_lease_expired", ready.ID: "paper_dispatch_application_stalled",
		recovering.ID: "paper_dispatch_recovery_stalled", awaitingAck.ID: "paper_dispatch_ack_stalled",
		oldQueue.ID: "research_queue_stalled",
	}
	if len(result.Issues) != len(expectedIssues) {
		t.Fatalf("wrong attention scope: %s", stdout.String())
	}
	for _, observed := range result.Issues {
		if observed.Code != expectedIssues[observed.JobID] || observed.JobID == recovering.ID && observed.OrderID != orderID {
			t.Fatalf("wrong issue details: %s", stdout.String())
		}
		delete(expectedIssues, observed.JobID)
	}
	for _, hidden := range []string{lease.Token, lease.ClaimID, "worker-only-secret-fixture", "postgresql://", otherAccount.ID, "fixture-private"} {
		if strings.Contains(stdout.String(), hidden) {
			t.Fatalf("pipeline exposed out-of-scope content: %s", stdout.String())
		}
	}
	checkPaperReportSchema(t, stdout.Bytes())

	fill := accounts.ResolutionRequest{
		AccountID: accountID, OrderID: order.OrderID, EventID: diagnosticID(73),
		Outcome: "filled", GrossUSD: "20", FeeUSD: "0.01", ObservedAt: order.CreatedAt,
	}
	if _, _, err := paper.Resolve(ctx, fill); err != nil {
		t.Fatal(err)
	}
	if _, err := inspection.Exec(ctx, `UPDATE paper_fill_outbox
		SET created_at=clock_timestamp()-interval '3 minutes' WHERE event_id=$1`, fill.EventID); err != nil {
		t.Fatal(err)
	}
	if _, err := inspection.Exec(ctx, `UPDATE research_paper_dispatch
		SET request_sha256=sha256(convert_to('broken','UTF8')) WHERE job_id=$1`, ready.ID); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run(ctx, readonly.String(), args, &stdout); code != 1 {
		t.Fatalf("relay and digest faults got exit %d and %s", code, stdout.String())
	}
	var damaged report
	if err := json.Unmarshal(stdout.Bytes(), &damaged); err != nil {
		t.Fatal(err)
	}
	if damaged.Pipeline.FillRelay.PendingFills != 1 || damaged.Pipeline.FillRelay.OldestFillEventID != fill.EventID ||
		damaged.Pipeline.FillRelay.OldestCreatedAt == nil {
		t.Fatalf("wrong relay evidence: %s", stdout.String())
	}
	expectedFaults := map[issue]bool{
		{Code: "paper_fill_relay_stalled", FillEventID: fill.EventID}: true,
		{Code: "paper_dispatch_inconsistent", JobID: ready.ID}:        true,
	}
	for _, fault := range damaged.Issues {
		delete(expectedFaults, fault)
	}
	if len(expectedFaults) != 0 {
		t.Fatalf("missing added pipeline faults: %s", stdout.String())
	}
	if _, err := inspection.Exec(ctx, `UPDATE paper_fill_outbox SET queued_at=clock_timestamp() WHERE event_id=$1`, fill.EventID); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run(ctx, readonly.String(), args, &stdout); code != 1 ||
		!strings.Contains(stdout.String(), `"fill_relay":{"pending_fills":0}`) ||
		strings.Contains(stdout.String(), "paper_fill_relay_stalled") {
		t.Fatalf("relayed fill still shown pending, got exit %d and %s", code, stdout.String())
	}
}

func createDiagnosticJob(t *testing.T, ctx context.Context, store *jobs.Store, accountID string, sequence int) jobs.CreateJob {
	t.Helper()
	jobID, snapshotID := diagnosticID(sequence), diagnosticID(sequence+1)
	snapshot, err := json.Marshal(map[string]any{"as_of": time.Now().UTC().Add(-time.Minute), "snapshot_id": snapshotID})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]any{
		"job_schema_version": "0.1", "job_id": jobID, "portfolio": json.RawMessage(snapshot), "max_llm_calls": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := []byte(fmt.Sprintf(`{"paper_apply_schema_version":"0.1","job_id":%q,"snapshot_context":{"account_id":%q},"fixture":"fixture-private"}`, jobID, accountID))
	input := jobs.CreateJob{
		ID: jobID, SnapshotID: snapshotID, Request: request, Snapshot: snapshot,
		PaperDispatch: &jobs.PaperDispatchInput{AccountID: accountID, Request: dispatch},
	}
	if _, created, err := store.Create(ctx, input, nil); err != nil || !created {
		t.Fatalf("create diagnostic job: %t %v", created, err)
	}
	return input
}

func completeDiagnosticJob(t *testing.T, ctx context.Context, store *jobs.Store, jobID string, sequence int) {
	t.Helper()
	lease, err := store.Claim(ctx, "test-pipeline", diagnosticID(100+sequence), time.Hour)
	if err != nil || lease.Job.ID != jobID {
		t.Fatalf("claim diagnostic job: %+v %v", lease.Job, err)
	}
	_, _, err = store.Complete(ctx, jobs.CompleteJob{
		JobID: jobID, WorkerID: lease.WorkerID, Token: lease.Token, ExpiresAt: lease.Deadline,
		ExpectedAttempt: 1, Payload: []byte(`{"result_schema_version":"0.1","metadata":{"llm_calls":0}}`),
	}, func(jobs.Job, time.Time) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func diagnosticID(sequence int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", sequence)
}

func TestPublishedPaperCheckReport(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/valid-paper-check.json")
	if err != nil {
		t.Fatal(err)
	}
	checkPaperReportSchema(t, raw)
}

func checkPaperReportSchema(t *testing.T, raw []byte) {
	t.Helper()
	name := "paper-check.schema.json"
	published, err := os.ReadFile("../../../../contracts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(published))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(name, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(name)
	if err != nil {
		t.Fatal(err)
	}
	report, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(report); err != nil {
		t.Fatal(err)
	}
}
