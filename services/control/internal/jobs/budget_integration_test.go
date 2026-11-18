package jobs

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestCallBudgetSurvivesFailedAttemptAndStoreRestart(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	job := queueJob(t, ctx, store, 1, 0)
	first, err := store.Claim(ctx, "first", testIdentity(100), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	callID := testIdentity(201)
	permit, created, err := store.AuthorizeCall(ctx, job.ID, first.WorkerID, first.Token, callID)
	if err != nil || !created || permit != (CallPermit{JobID: job.ID, CallID: callID, Attempt: 1, Remaining: 11}) {
		t.Fatalf("first permit = %+v, %t, %v", permit, created, err)
	}
	// No model receipt was recorded. Losing the permit response and restarting
	// must not refund the call or charge it a second time.
	store.Close()
	reopened := openTestStore(t, ctx, dsn)
	if _, created, err := reopened.AuthorizeCall(ctx, job.ID, first.WorkerID, first.Token, testIdentity(202)); err != nil || !created {
		t.Fatalf("second permit = %t, %v", created, err)
	}
	if replay, created, err := reopened.AuthorizeCall(ctx, job.ID, first.WorkerID, first.Token, callID); err != nil || created || replay != permit {
		t.Fatalf("permit replay = %+v, %t, %v", replay, created, err)
	}
	forceAttemptTimes(t, ctx, db, first.Token, -time.Second, time.Minute)
	second, err := reopened.Claim(ctx, "replacement", testIdentity(101), testSnapshotWindow)
	if err != nil || second.Attempt != 2 {
		t.Fatalf("replacement claim = %+v, %v", second, err)
	}
	if _, _, err := reopened.AuthorizeCall(ctx, job.ID, first.WorkerID, first.Token, callID); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("old lease replay = %v", err)
	}
	if _, _, err := reopened.AuthorizeCall(ctx, job.ID, second.WorkerID, second.Token, callID); !errors.Is(err, ErrConflict) {
		t.Fatalf("permit ID stolen by new attempt = %v", err)
	}
	for index := range 10 {
		got, created, err := reopened.AuthorizeCall(ctx, job.ID, second.WorkerID, second.Token, testIdentity(301+index))
		if err != nil || !created || got.Attempt != 2 || got.Remaining != 9-index {
			t.Fatalf("replacement permit %d = %+v, %t, %v", index, got, created, err)
		}
	}
	if _, created, err := reopened.AuthorizeCall(ctx, job.ID, second.WorkerID, second.Token, testIdentity(312)); !errors.Is(err, ErrBudgetExhausted) || created {
		t.Fatalf("over-budget request = %t, %v", created, err)
	}
	complete := attemptResult(second, "result")
	complete.ExpectedAttempt = 2
	complete.ExpectedLLMCalls = 12
	verify := func(Job, time.Time) error { return nil }
	if _, created, err := reopened.Complete(ctx, complete, verify); !errors.Is(err, ErrCallPermitMismatch) || created {
		t.Fatalf("completion using whole-job permits as this attempt's usage = %t, %v", created, err)
	}
	complete.ExpectedLLMCalls = 10
	if _, created, err := reopened.Complete(ctx, complete, verify); err != nil || !created {
		t.Fatalf("completion matching only the current attempt's permits = %t, %v", created, err)
	}
}

func TestConcurrentCallAuthorizationsCannotExceedBudget(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	job := queueJob(t, ctx, store, 1, 0)
	lease, err := store.Claim(ctx, "worker", testIdentity(100), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	const requests = 20
	results := make(chan error, requests)
	var pending sync.WaitGroup
	for index := range requests {
		pending.Go(func() {
			_, _, err := store.AuthorizeCall(ctx, job.ID, lease.WorkerID, lease.Token, testIdentity(201+index))
			results <- err
		})
	}
	pending.Wait()
	close(results)
	var permitted, exhausted int
	for err := range results {
		switch {
		case err == nil:
			permitted++
		case errors.Is(err, ErrBudgetExhausted):
			exhausted++
		default:
			t.Fatal(err)
		}
	}
	var persisted int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM research_call_permits WHERE job_id = $1`, job.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if permitted != 12 || exhausted != requests-12 || persisted != 12 {
		t.Fatalf("permitted=%d exhausted=%d persisted=%d", permitted, exhausted, persisted)
	}
}

func TestBudgetMigrationDoesNotAssumeUnrecordedCallsWereFree(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	if _, err := db.Exec(ctx, researchJobsMigration+researchLeasesMigration+researchResultsMigration+`
		CREATE TABLE research_schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp());
		INSERT INTO research_schema_migrations (version) VALUES (1),(2),(3)`); err != nil {
		t.Fatal(err)
	}
	input := queueInput(t, 1, databaseNow(t, ctx, db))
	claimID, token := testIdentity(100), testIdentity(200)
	if _, err := db.Exec(ctx, `INSERT INTO research_snapshots (id, payload, payload_sha256) VALUES ($1, $2, sha256($2));
		INSERT INTO research_jobs (id, snapshot_id, request, request_sha256, status, attempt_count)
		VALUES ($3, $1, $4, sha256($4), 'leased', 1);
		INSERT INTO research_job_attempts (job_id, attempt, worker_id, claim_id, lease_token, started_at, expires_at, deadline)
		VALUES ($3, 1, 'legacy-worker', $5, $6, clock_timestamp() - interval '2 seconds', clock_timestamp() - interval '1 second', clock_timestamp() + interval '15 minutes')`,
		pgx.QueryExecModeSimpleProtocol, input.SnapshotID, []byte(input.Snapshot), input.ID, []byte(input.Request), claimID, token); err != nil {
		t.Fatal(err)
	}
	completed := queueInput(t, 2, databaseNow(t, ctx, db))
	completeClaim, completeToken := testIdentity(102), testIdentity(202)
	legacyPayload := []byte(`{"result_schema_version":"0.2","metadata":{"llm_calls":3}}`)
	if _, err := db.Exec(ctx, `INSERT INTO research_snapshots (id, payload, payload_sha256) VALUES ($1, $2, sha256($2));
		INSERT INTO research_jobs (id, snapshot_id, request, request_sha256, status, attempt_count)
		VALUES ($3, $1, $4, sha256($4), 'completed', 1);
		INSERT INTO research_job_attempts (job_id, attempt, worker_id, claim_id, lease_token, started_at, expires_at, deadline)
		VALUES ($3, 1, 'legacy-worker', $5, $6, clock_timestamp() - interval '2 seconds', clock_timestamp() - interval '1 second', clock_timestamp() + interval '15 minutes');
		INSERT INTO research_job_results (job_id, attempt, completed_at, payload, payload_sha256)
		VALUES ($3, 1, clock_timestamp() - interval '1500 milliseconds', $7, sha256($7))`,
		pgx.QueryExecModeSimpleProtocol, completed.SnapshotID, []byte(completed.Snapshot), completed.ID, []byte(completed.Request),
		completeClaim, completeToken, legacyPayload); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, ctx, dsn)
	lease, err := store.Claim(ctx, "new-worker", testIdentity(101), testSnapshotWindow)
	if err != nil || lease.Attempt != 2 {
		t.Fatalf("replacement lease = %+v, %v", lease, err)
	}
	if _, _, err := store.AuthorizeCall(ctx, input.ID, lease.WorkerID, lease.Token, testIdentity(201)); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("pre-migration unknown usage authorization = %v", err)
	}
	receipt, created, err := store.Complete(ctx, CompleteJob{
		JobID: completed.ID, WorkerID: "legacy-worker", Token: completeToken, Payload: legacyPayload,
		ExpectedAttempt: 1, ExpectedLLMCalls: 3,
	}, nil)
	if err != nil || created || string(receipt.Payload) != string(legacyPayload) {
		t.Fatalf("pre-migration result replay = %+v, %t, %v", receipt, created, err)
	}
	archive, err := ReadResearchCallUsage(ctx, dsn, completed.ID)
	if err != nil || archive.PermitHistoryKnown || archive.LegacyBudgetUnknown ||
		archive.Status != "completed" || archive.Attempts != 1 || len(archive.Records) != 0 {
		t.Fatalf("completed pre-permit history must stay unknown: %+v, %v", archive, err)
	}
}
