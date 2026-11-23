package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const testSnapshotWindow = time.Hour

func testIdentity(n int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012x", n)
}

func queueInput(t *testing.T, number int, asOf time.Time) CreateJob {
	t.Helper()
	input := CreateJob{ID: testIdentity(number), SnapshotID: testIdentity(number + 1000)}
	var err error
	input.Snapshot, err = json.Marshal(map[string]any{"as_of": asOf.UTC(), "snapshot_id": input.SnapshotID})
	if err != nil {
		t.Fatal(err)
	}
	input.Request, err = json.Marshal(map[string]any{
		"job_schema_version": "0.1", "job_id": input.ID,
		"portfolio": input.Snapshot, "max_llm_calls": 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func databaseNow(t *testing.T, ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) time.Time {
	t.Helper()
	var now time.Time
	if err := db.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func queueJob(t *testing.T, ctx context.Context, store *Store, number int, snapshotOffset time.Duration) CreateJob {
	t.Helper()
	input := queueInput(t, number, databaseNow(t, ctx, store.pool).Add(snapshotOffset))
	if _, created, err := store.Create(ctx, input, nil); err != nil || !created {
		t.Fatalf("queue job = %t, %v", created, err)
	}
	return input
}

func TestClaimKeepsFutureSnapshotQueued(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, time.Minute)
	if _, err := store.Claim(ctx, "worker", testIdentity(100), testSnapshotWindow); !errors.Is(err, ErrNoJob) {
		t.Fatalf("claim future snapshot: %v", err)
	}
	var status string
	var attempts int
	if err := db.QueryRow(ctx, "SELECT status, attempt_count FROM research_jobs WHERE id=$1", input.ID).Scan(&status, &attempts); err != nil ||
		status != "queued" || attempts != 0 {
		t.Fatalf("future snapshot state = %s/%d, %v", status, attempts, err)
	}
}

func forceAttemptTimes(t *testing.T, ctx context.Context, db *pgx.Conn, token string, ttl, deadline time.Duration) {
	t.Helper()
	// Advance the attempt's persisted times rather than waiting in a real database test.
	_, err := db.Exec(ctx, `UPDATE research_job_attempts SET started_at = clock_timestamp() - interval '20 minutes',
		expires_at = clock_timestamp() + $2 * interval '1 second',
		deadline = clock_timestamp() + $3 * interval '1 second' WHERE lease_token = $1`, token, ttl.Seconds(), deadline.Seconds())
	if err != nil {
		t.Fatal(err)
	}
}

func TestClaimMigratesExistingJobAndReplaysAfterReconnect(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	if _, err := inspection.Exec(ctx, researchJobsMigration+`
		CREATE TABLE research_schema_migrations (version integer PRIMARY KEY);
		INSERT INTO research_schema_migrations (version) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	input := queueInput(t, 1, databaseNow(t, ctx, inspection))
	if _, err := inspection.Exec(ctx, `INSERT INTO research_snapshots (id, payload, payload_sha256) VALUES ($1, $2, sha256($2));
		INSERT INTO research_jobs (id, snapshot_id, request, request_sha256) VALUES ($3, $1, $4, sha256($4))`,
		pgx.QueryExecModeSimpleProtocol, input.SnapshotID, []byte(input.Snapshot), input.ID, []byte(input.Request)); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, ctx, dsn)
	claimID := testIdentity(100)
	lease, err := store.Claim(ctx, "worker-1", claimID, testSnapshotWindow)
	if err != nil || lease.Attempt != 1 || lease.Token == "" || lease.Job.ID != input.ID || lease.Job.Status != "leased" ||
		!bytes.Equal(lease.Job.Request, input.Request) || !lease.ExpiresAt.Before(lease.Deadline) {
		t.Fatalf("claim = %+v, %v", lease, err)
	}
	store.Close()
	reopened := openTestStore(t, ctx, dsn)
	replay, err := reopened.Claim(ctx, "worker-1", claimID, testSnapshotWindow)
	if err != nil || replay.Token != lease.Token || !replay.ExpiresAt.Equal(lease.ExpiresAt) || replay.Attempt != 1 {
		t.Fatalf("claim replay = %+v, %v", replay, err)
	}
	if _, err := reopened.Claim(ctx, "worker-2", claimID, testSnapshotWindow); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("foreign claim replay = %v", err)
	}
}

func TestConcurrentWorkersAndClaimRetriesHaveOneAttempt(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, 0)
	type observation struct {
		lease Lease
		err   error
	}
	const workers = 12
	var pending sync.WaitGroup
	results := make(chan observation, workers)
	for worker := range workers {
		pending.Go(func() {
			// Duplicate traffic from half the workers uses the very same claim ID.
			identity := worker / 2
			lease, err := store.Claim(ctx, fmt.Sprintf("worker-%d", identity), testIdentity(100+identity), testSnapshotWindow)
			results <- observation{lease, err}
		})
	}
	pending.Wait()
	close(results)
	var winningToken string
	var successes int
	for got := range results {
		if errors.Is(got.err, ErrNoJob) {
			continue
		}
		if got.err != nil || got.lease.Job.ID != input.ID || got.lease.Attempt != 1 {
			t.Fatalf("claim observation = %+v, %v", got.lease, got.err)
		}
		if successes == 0 {
			winningToken = got.lease.Token
		}
		if got.lease.Token != winningToken {
			t.Fatal("more than one active lease was granted")
		}
		successes++
	}
	var attempts int
	if err := inspection.QueryRow(ctx, "SELECT count(*) FROM research_job_attempts").Scan(&attempts); err != nil || attempts != 1 || successes != 2 {
		t.Fatalf("attempts=%d successes=%d error=%v", attempts, successes, err)
	}
}

func TestLeaseHeartbeatRejectsStaleWorkerAndStopsAtDeadline(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, 0)
	claimID := testIdentity(202)
	first, err := store.Claim(ctx, "worker-1", claimID, testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Heartbeat(ctx, input.ID, "worker-2", first.Token); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("wrong worker heartbeat = %v", err)
	}
	if _, err := store.Heartbeat(ctx, input.ID, "worker-1", testIdentity(400)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("wrong token heartbeat = %v", err)
	}
	forceAttemptTimes(t, ctx, inspection, first.Token, 5*time.Second, 20*time.Second)
	renewed, err := store.Heartbeat(ctx, input.ID, "worker-1", first.Token)
	if err != nil || !renewed.ExpiresAt.Equal(renewed.Deadline) || renewed.Job.Request != nil {
		t.Fatalf("capped renewal = %+v, %v", renewed, err)
	}
	forceAttemptTimes(t, ctx, inspection, first.Token, -time.Second, 20*time.Second)
	if _, err := store.Heartbeat(ctx, input.ID, "worker-1", first.Token); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("expired heartbeat = %v", err)
	}
	if _, err := store.Claim(ctx, "worker-1", claimID, testSnapshotWindow); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("expired claim replay = %v", err)
	}
	second, err := store.Claim(ctx, "worker-2", testIdentity(203), testSnapshotWindow)
	if err != nil || second.Attempt != 2 || second.Token == first.Token {
		t.Fatalf("reclaim = %+v, %v", second, err)
	}
	if _, err := store.Heartbeat(ctx, input.ID, "worker-1", first.Token); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("superseded heartbeat = %v", err)
	}
}

func TestClaimRechecksLeaseAfterLockingAnExpiredCandidate(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := queueJob(t, ctx, store, 1, 0)
	first, err := store.Claim(ctx, "worker-1", testIdentity(205), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	forceAttemptTimes(t, ctx, inspection, first.Token, -time.Second, time.Minute)
	// Simulate a candidate scan seeing an older MVCC snapshot than the renewal
	// that committed immediately before the job lock became obtainable.
	forceAttemptTimes(t, ctx, inspection, first.Token, 30*time.Second, time.Minute)
	tx, err := inspection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	job, err := lockJob(ctx, tx, input.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := job.claim(ctx, "worker-2", testIdentity(206), testSnapshotWindow, "")
	if err != nil || lease.Token != "" {
		t.Fatalf("renewed candidate unexpectedly acquired = %+v, %v", lease, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Claim(ctx, "worker-1", first.ClaimID, testSnapshotWindow)
	if err != nil || replay.Token != first.Token || replay.Attempt != 1 {
		t.Fatalf("original attempt changed = %+v, %v", replay, err)
	}
}

func TestExhaustedAndStaleJobsStayTerminal(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	claimID := testIdentity(303)
	if _, err := store.Claim(ctx, "worker", claimID, testSnapshotWindow); !errors.Is(err, ErrNoJob) {
		t.Fatalf("empty claim = %v", err)
	}
	stale := queueJob(t, ctx, store, 1, -2*time.Hour)
	eligible := queueJob(t, ctx, store, 2, 0)
	for attempt := 1; attempt <= 3; attempt++ {
		lease, err := store.Claim(ctx, "worker", claimID, testSnapshotWindow)
		if err != nil || lease.Job.ID != eligible.ID || lease.Attempt != attempt {
			t.Fatalf("attempt %d = %+v, %v", attempt, lease, err)
		}
		forceAttemptTimes(t, ctx, inspection, lease.Token, -time.Second, time.Minute)
		claimID = testIdentity(303 + attempt)
	}
	if _, err := store.Claim(ctx, "worker", claimID, testSnapshotWindow); !errors.Is(err, ErrNoJob) {
		t.Fatalf("exhausted claim = %v", err)
	}
	for _, want := range []struct{ id, status string }{{stale.ID, "expired"}, {eligible.ID, "failed"}} {
		got, err := store.Get(ctx, want.id)
		if err != nil || got.Status != want.status {
			t.Fatalf("get %s = %s, %v", want.id, got.Status, err)
		}
	}
}

func TestClaimRefusesCorruptStoredInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		sql  string
	}{
		{"request", `UPDATE research_jobs SET request = convert_to('{}', 'UTF8')`},
		{"snapshot", `UPDATE research_snapshots SET payload = convert_to('{}', 'UTF8')`},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, dsn, inspection := testDatabase(t)
			store := openTestStore(t, ctx, dsn)
			input := queueJob(t, ctx, store, 1, 0)
			if _, err := inspection.Exec(ctx, test.sql); err != nil {
				t.Fatal(err)
			}
			if lease, err := store.Claim(ctx, "worker", testIdentity(404), testSnapshotWindow); err == nil ||
				errors.Is(err, ErrNoJob) || errors.Is(err, ErrLeaseConflict) || lease.Token != "" {
				t.Fatalf("corrupt claim = %+v, %v", lease, err)
			}
			// The public read rejects a corrupt request, so inspect the stored status directly.
			var status string
			err := inspection.QueryRow(ctx, `SELECT status FROM research_jobs WHERE id = $1`, input.ID).Scan(&status)
			if err != nil || status != "queued" {
				t.Fatalf("corrupt job mutated = %s, %v", status, err)
			}
		})
	}
}
