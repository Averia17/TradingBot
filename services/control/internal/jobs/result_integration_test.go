package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func attemptResult(lease Lease, label string) CompleteJob {
	return CompleteJob{
		JobID:     lease.Job.ID,
		WorkerID:  lease.WorkerID,
		Token:     lease.Token,
		Payload:   json.RawMessage(fmt.Sprintf(`{"result_schema_version":"0.1","fixture":%q}`, label)),
		ExpiresAt: lease.Deadline,
	}
}

func TestCompletionMigratesExistingLeaseAndSurvivesReopenAfterExpiry(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	if _, err := db.Exec(ctx, researchJobsMigration+researchLeasesMigration+`
		CREATE TABLE research_schema_migrations (version integer PRIMARY KEY);
		INSERT INTO research_schema_migrations (version) VALUES (1),(2)`); err != nil {
		t.Fatal(err)
	}
	input := queueInput(t, 1, time.Now())
	claimID, token := testIdentity(600), testIdentity(601)
	if _, err := db.Exec(ctx, `INSERT INTO research_snapshots (id, payload, payload_sha256) VALUES ($1, $2, sha256($2));
		INSERT INTO research_jobs (id, snapshot_id, request, request_sha256, status, attempt_count)
		VALUES ($3, $1, $4, sha256($4), 'leased', 1);
		INSERT INTO research_job_attempts (job_id, attempt, worker_id, claim_id, lease_token, started_at, expires_at, deadline)
		VALUES ($3, 1, 'worker', $5, $6, clock_timestamp(), clock_timestamp() + interval '1 minute', clock_timestamp() + interval '15 minutes');
		INSERT INTO research_outbox (job_id, event_type, created_at) VALUES ($3, 'research_job.queued', clock_timestamp())`,
		pgx.QueryExecModeSimpleProtocol, input.SnapshotID, []byte(input.Snapshot), input.ID, []byte(input.Request), claimID, token); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, ctx, dsn)
	if _, err := store.GetResult(ctx, input.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("result before completion = %v", err)
	}
	lease, err := store.Claim(ctx, "worker", claimID, testSnapshotWindow)
	if err != nil || lease.Token != token {
		t.Fatalf("migrated lease = %+v, %v", lease, err)
	}
	submission := attemptResult(lease, "exact stored receipt")
	var validatedAt time.Time
	receipt, created, err := store.Complete(ctx, submission, func(job Job, dbNow time.Time) error {
		if !bytes.Equal(job.Request, input.Request) || job.ID != input.ID || job.Status != "leased" {
			return errors.New("validation did not receive immutable job")
		}
		validatedAt = dbNow
		return nil
	})
	if err != nil || !created || receipt.JobID != input.ID || receipt.Attempt != 1 || receipt.CompletedAt.Before(validatedAt) ||
		validatedAt.IsZero() || !receipt.CompletedAt.Before(lease.ExpiresAt) || !bytes.Equal(receipt.Payload, submission.Payload) {
		t.Fatalf("complete = %+v, %t, %v", receipt, created, err)
	}
	forceAttemptTimes(t, ctx, db, token, -time.Second, time.Minute)
	store.Close()
	reopened := openTestStore(t, ctx, dsn)
	duplicate, created, err := reopened.Complete(ctx, submission, func(Job, time.Time) error {
		return errors.New("result must not be revalidated after durable commit")
	})
	if err != nil || created || !duplicate.CompletedAt.Equal(receipt.CompletedAt) {
		t.Fatalf("duplicate receipt = %+v, %t, %v", duplicate, created, err)
	}
	fetched, err := reopened.GetResult(ctx, input.ID)
	if err != nil || fetched.Attempt != receipt.Attempt || fetched.JobID != receipt.JobID ||
		!fetched.CompletedAt.Equal(receipt.CompletedAt) || !bytes.Equal(fetched.Payload, receipt.Payload) {
		t.Fatalf("fetched receipt = %+v, %v", fetched, err)
	}
	job, err := reopened.Get(ctx, input.ID)
	if err != nil || job.Status != "completed" {
		t.Fatalf("completed job = %+v, %v", job, err)
	}
	if _, err := reopened.Heartbeat(ctx, input.ID, "worker", token); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("heartbeat after completion = %v", err)
	}
	if _, err := reopened.Claim(ctx, "worker", testIdentity(602), testSnapshotWindow); !errors.Is(err, ErrNoJob) {
		t.Fatalf("claim after completion = %v", err)
	}
	assertCompletionRows(t, ctx, db, input.ID, 1)
}

func TestCompletionRejectsAuthorityAndRechecksDatabaseExpiry(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, time.Now())
	first, err := store.Claim(ctx, "worker", testIdentity(610), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	request := attemptResult(first, "valid result")
	for _, test := range []struct {
		job, worker, token string
		err                error
	}{
		{testIdentity(900), request.WorkerID, request.Token, ErrNotFound},
		{request.JobID, "another-worker", request.Token, ErrLeaseConflict},
		{request.JobID, request.WorkerID, testIdentity(901), ErrLeaseConflict},
	} {
		wrong := CompleteJob{JobID: test.job, WorkerID: test.worker, Token: test.token, Payload: request.Payload}
		if _, created, err := store.Complete(ctx, wrong, func(Job, time.Time) error {
			return errors.New("fenced request reached result validation")
		}); !errors.Is(err, test.err) || created {
			t.Fatalf("wrong authority = %t, %v", created, err)
		}
	}
	forceAttemptTimes(t, ctx, db, first.Token, time.Minute, 2*time.Minute)
	_, created, err := store.Complete(ctx, request, func(Job, time.Time) error {
		// Simulate an elapsed lease during local result verification using the
		// database clock. Only this test bypasses the owning job lock.
		forceAttemptTimes(t, ctx, db, first.Token, -time.Second, 2*time.Minute)
		return nil
	})
	if !errors.Is(err, ErrLeaseConflict) || created {
		t.Fatalf("lease elapsed during verification = %t, %v", created, err)
	}
	if _, created, err := store.Complete(ctx, request, nil); !errors.Is(err, ErrLeaseConflict) || created {
		t.Fatalf("expired completion = %t, %v", created, err)
	}
	second, err := store.Claim(ctx, "worker", testIdentity(611), testSnapshotWindow)
	if err != nil || second.Attempt != 2 {
		t.Fatalf("second claim = %+v, %v", second, err)
	}
	if _, created, err := store.Complete(ctx, request, nil); !errors.Is(err, ErrLeaseConflict) || created {
		t.Fatalf("superseded completion = %t, %v", created, err)
	}
	assertCompletionRows(t, ctx, db, input.ID, 0)
}

func TestCompletionRechecksBatchExpiryAfterVerification(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, time.Now())
	lease, err := store.Claim(ctx, "worker", testIdentity(615), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	request := attemptResult(lease, "result expiring during verification")
	if err := db.QueryRow(ctx, `SELECT clock_timestamp() + interval '2 seconds'`).Scan(&request.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if result, created, err := store.Complete(ctx, request, func(_ Job, verifyTime time.Time) error {
		if !verifyTime.Before(request.ExpiresAt) {
			return errors.New("test batch expired before verification began")
		}
		_, err := db.Exec(ctx, `SELECT pg_sleep_until($1)`, request.ExpiresAt)
		return err
	}); !errors.Is(err, ErrResultExpired) || created || result.JobID != "" {
		t.Fatalf("batch elapsed during verification = %+v, %t, %v", result, created, err)
	}
	assertCompletionRows(t, ctx, db, input.ID, 0)
	if job, err := store.Get(ctx, input.ID); err != nil || job.Status != "leased" {
		t.Fatalf("attempt after batch expiry = %+v, %v", job, err)
	}
}

func TestCompletionBindsExpectedAttemptAcrossReplay(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, time.Now())
	lease, err := store.Claim(ctx, "worker", testIdentity(616), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	request := attemptResult(lease, "attempt-bound result")
	request.ExpectedAttempt = lease.Attempt + 1
	if _, created, err := store.Complete(ctx, request, func(Job, time.Time) error {
		return errors.New("mismatched attempt reached validation")
	}); !errors.Is(err, ErrLeaseConflict) || created {
		t.Fatalf("completion for a mismatched attempt = %t, %v", created, err)
	}
	assertCompletionRows(t, ctx, db, input.ID, 0)

	request.ExpectedAttempt = lease.Attempt
	receipt, created, err := store.Complete(ctx, request, func(Job, time.Time) error { return nil })
	if err != nil || !created || receipt.Attempt != lease.Attempt {
		t.Fatalf("matching completion = %+v, %t, %v", receipt, created, err)
	}
	replay, created, err := store.Complete(ctx, request, nil)
	if err != nil || created || !replay.CompletedAt.Equal(receipt.CompletedAt) {
		t.Fatalf("matching replay = %+v, %t, %v", replay, created, err)
	}
	request.ExpectedAttempt++
	if _, created, err := store.Complete(ctx, request, nil); !errors.Is(err, ErrLeaseConflict) || created {
		t.Fatalf("replay for a mismatched attempt = %t, %v", created, err)
	}
	assertCompletionRows(t, ctx, db, input.ID, 1)
}

func TestConcurrentCompletionsCommitOneReceiptAndPreserveAuthority(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, time.Now())
	lease, err := store.Claim(ctx, "worker", testIdentity(620), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		Result
		created bool
		err     error
	}
	responses := make(chan answer, 8)
	var verifying atomic.Int32
	var pending sync.WaitGroup
	for index := range cap(responses) {
		pending.Go(func() {
			request := attemptResult(lease, fmt.Sprintf("choice %d", index%2))
			result, created, err := store.Complete(ctx, request, func(Job, time.Time) error {
				verifying.Add(1)
				return nil
			})
			responses <- answer{result, created, err}
		})
	}
	pending.Wait()
	close(responses)
	var written, replayed, conflicted int
	var winner Result
	for got := range responses {
		switch {
		case errors.Is(got.err, ErrConflict):
			conflicted++
		case got.err != nil:
			t.Fatal(got.err)
		case got.created:
			written++
			winner = got.Result
		default:
			replayed++
		}
	}
	if written != 1 || replayed != 3 || conflicted != 4 || verifying.Load() != 1 {
		t.Fatalf("written=%d replayed=%d conflicted=%d verified=%d", written, replayed, conflicted, verifying.Load())
	}
	wrong := CompleteJob{JobID: input.ID, WorkerID: "another-worker", Token: lease.Token, Payload: winner.Payload}
	if _, created, err := store.Complete(ctx, wrong, nil); !errors.Is(err, ErrLeaseConflict) || created {
		t.Fatalf("foreign replay with identical bytes = %t, %v", created, err)
	}
	assertCompletionRows(t, ctx, db, input.ID, 1)
}

func TestCompletionVerificationAndOutboxRollbackAndCorruptReads(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, time.Now())
	lease, err := store.Claim(ctx, "worker", testIdentity(630), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	request := attemptResult(lease, "receipt to roll back")
	validationFailure := errors.New("rejected contract")
	if _, created, err := store.Complete(ctx, request, func(Job, time.Time) error { return validationFailure }); !errors.Is(err, validationFailure) || created {
		t.Fatalf("validation failure = %t, %v", created, err)
	}
	if _, err := db.Exec(ctx, `ALTER TABLE research_outbox ADD CONSTRAINT refuse_completed CHECK (event_type <> 'research_job.completed')`); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.Complete(ctx, request, func(Job, time.Time) error { return nil }); err == nil || created {
		t.Fatalf("failed outbox commit = %t, %v", created, err)
	}
	job, err := store.Get(ctx, input.ID)
	if err != nil || job.Status != "leased" {
		t.Fatalf("after rollback, job = %+v, %v", job, err)
	}
	assertCompletionRows(t, ctx, db, input.ID, 0)
	if _, err := db.Exec(ctx, `ALTER TABLE research_outbox DROP CONSTRAINT refuse_completed`); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.Complete(ctx, request, func(Job, time.Time) error { return nil }); err != nil || !created {
		t.Fatalf("retry completion = %t, %v", created, err)
	}
	if _, err := db.Exec(ctx, `UPDATE research_job_results SET payload = convert_to('{}', 'UTF8')`); err != nil {
		t.Fatal(err)
	}
	if result, err := store.GetResult(ctx, input.ID); err == nil || errors.Is(err, ErrNotFound) || len(result.Payload) != 0 {
		t.Fatalf("corrupt result read = %+v, %v", result, err)
	}
	if result, created, err := store.Complete(ctx, request, nil); err == nil || errors.Is(err, ErrConflict) || created || len(result.Payload) != 0 {
		t.Fatalf("corrupt replay = %+v, %t, %v", result, created, err)
	}
}

func assertCompletionRows(t *testing.T, ctx context.Context, db *pgx.Conn, jobID string, want int) {
	t.Helper()
	var results, events int
	err := db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM research_job_results WHERE job_id = $1),
		(SELECT count(*) FROM research_outbox WHERE job_id = $1 AND event_type = 'research_job.completed')`,
		jobID).Scan(&results, &events)
	if err != nil || results != want || events != want {
		t.Fatalf("result rows=%d completion events=%d err=%v", results, events, err)
	}
}
