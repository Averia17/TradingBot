package jobs

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestDailyCallBudgetSharesPermitsAcrossJobsAndRestart(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	firstStore := openTestStore(t, ctx, dsn)
	secondStore := openTestStore(t, ctx, dsn)
	firstJob := queueJob(t, ctx, firstStore, 1, 0)
	secondJob := queueJob(t, ctx, secondStore, 2, 0)
	first, err := firstStore.Claim(ctx, "first", testIdentity(100), testSnapshotWindow)
	if err != nil || first.Job.ID != firstJob.ID {
		t.Fatalf("claim first = %+v, %v", first, err)
	}
	second, err := secondStore.Claim(ctx, "second", testIdentity(101), testSnapshotWindow)
	if err != nil || second.Job.ID != secondJob.ID {
		t.Fatalf("claim second = %+v, %v", second, err)
	}
	now, err := firstStore.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	today := now.Format(time.DateOnly)
	initialID := testIdentity(200)
	initial, created, err := firstStore.AuthorizeCall(ctx, first.Job.ID, first.WorkerID, first.Token, initialID)
	if err != nil || !created {
		t.Fatalf("legacy grant = %+v, %t, %v", initial, created, err)
	}
	before, err := ReadDailyCallBudget(ctx, dsn, today)
	if err != nil || before.Status != "unconfigured" || before.RecordedPermits != 1 || before.RemainingPermits != nil {
		t.Fatalf("pre-activation report = %+v, %v", before, err)
	}
	if err := firstStore.SetDailyCallBudget(ctx, today, 4); err != nil {
		t.Fatal(err)
	}

	const attempts = 20
	results := make(chan error, attempts)
	var pending sync.WaitGroup
	for i := range attempts {
		pending.Go(func() {
			store, lease := firstStore, first
			if i%2 == 1 {
				store, lease = secondStore, second
			}
			_, created, err := store.AuthorizeCall(ctx, lease.Job.ID, lease.WorkerID, lease.Token, testIdentity(300+i))
			if err == nil && !created {
				err = fmt.Errorf("first use of call %d was returned as replay", i)
			}
			results <- err
		})
	}
	pending.Wait()
	close(results)
	permitted := 0
	for err := range results {
		switch {
		case err == nil:
			permitted++
		case !errors.Is(err, ErrBudgetExhausted):
			t.Fatal(err)
		}
	}
	if permitted != 3 {
		t.Fatalf("shared cap allowed %d additional calls, want 3", permitted)
	}

	firstStore.Close()
	secondStore.Close()
	reopened := openTestStore(t, ctx, dsn)
	if err := reopened.SetDailyCallBudget(ctx, today, 4); err != nil {
		t.Fatalf("same limit re-application = %v", err)
	}
	if err := reopened.SetDailyCallBudget(ctx, today, 5); !errors.Is(err, ErrDailyBudgetConflict) {
		t.Fatalf("changed limit = %v", err)
	}
	replay, created, err := reopened.AuthorizeCall(ctx, first.Job.ID, first.WorkerID, first.Token, initialID)
	if err != nil || created || replay != initial {
		t.Fatalf("committed grant replay = %+v, %t, %v", replay, created, err)
	}
	forceAttemptTimes(t, ctx, db, first.Token, -time.Second, time.Minute)
	replacement, err := reopened.Claim(ctx, "replacement", testIdentity(102), testSnapshotWindow)
	if err != nil || replacement.Job.ID != first.Job.ID || replacement.Attempt != 2 {
		t.Fatalf("replacement claim = %+v, %v", replacement, err)
	}
	if _, created, err := reopened.AuthorizeCall(ctx, replacement.Job.ID, replacement.WorkerID, replacement.Token, testIdentity(400)); created || !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("replacement grant = %t, %v", created, err)
	}

	readDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := readDSN.Query()
	query.Set("default_transaction_read_only", "on")
	query.Set("timezone", "Pacific/Auckland")
	readDSN.RawQuery = query.Encode()
	report, err := ReadDailyCallBudget(ctx, readDSN.String(), "")
	if err != nil || report.Day != today || report.Status != "exhausted" || report.RecordedPermits != 4 ||
		report.MaxCalls == nil || *report.MaxCalls != 4 || report.RemainingPermits == nil || *report.RemainingPermits != 0 {
		t.Fatalf("read-only UTC budget report = %+v, %v", report, err)
	}
}

func TestDailyCallBudgetFutureActivationAndMissingActiveDay(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	now, err := store.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	today, tomorrow := now.Format(time.DateOnly), now.AddDate(0, 0, 1).Format(time.DateOnly)
	if err := store.SetDailyCallBudget(ctx, tomorrow, 5); err != nil {
		t.Fatal(err)
	}
	prior, err := ReadDailyCallBudget(ctx, dsn, today)
	if err != nil || prior.Status != "unconfigured" || prior.RemainingPermits != nil {
		t.Fatalf("day preceding activation = %+v, %v", prior, err)
	}
	job := queueJob(t, ctx, store, 1, 0)
	lease, err := store.Claim(ctx, "worker", testIdentity(100), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.AuthorizeCall(ctx, job.ID, lease.WorkerID, lease.Token, testIdentity(200)); !created || err != nil {
		t.Fatalf("grant before activation = %t, %v", created, err)
	}

	// Reproduce an already activated database where yesterday had a policy
	// and today's owner cap has yet to be registered.
	if _, err := db.Exec(ctx, `UPDATE research_daily_call_policy SET active_from = $1::date - 1`, today); err != nil {
		t.Fatal(err)
	}
	missing, err := ReadDailyCallBudget(ctx, dsn, today)
	if err != nil || missing.Status != "missing_limit" || missing.MaxCalls != nil ||
		missing.RemainingPermits == nil || *missing.RemainingPermits != 0 {
		t.Fatalf("active day without owner's limit = %+v, %v", missing, err)
	}
	if _, created, err := store.AuthorizeCall(ctx, job.ID, lease.WorkerID, lease.Token, testIdentity(201)); created || !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("missing day grant = %t, %v", created, err)
	}
}

func TestDailyCallBudgetActivationIsVisibleAfterLockWait(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	job := queueJob(t, ctx, store, 1, 0)
	lease, err := store.Claim(ctx, "worker", testIdentity(100), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	var day string
	if err := tx.QueryRow(ctx, `UPDATE research_daily_call_policy
		SET active_from = (clock_timestamp() AT TIME ZONE 'UTC')::date WHERE singleton
		RETURNING to_char(active_from, 'YYYY-MM-DD')`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO research_daily_call_limits (utc_day, max_calls) VALUES ($1::date, 0)`, day); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := store.AuthorizeCall(ctx, job.ID, lease.WorkerID, lease.Token, testIdentity(200))
		done <- err
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	wait := time.After(3 * time.Second)
	for {
		var blocked bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE wait_event_type='Lock' AND $1 = ANY(pg_blocking_pids(pid))
			)`, db.PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("grant did not wait for the policy transaction: %v", err)
		case <-wait:
			t.Fatal("grant never reached the policy lock")
		case <-ticker.C:
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("grant after lock wait = %v", err)
	}
	report, err := ReadDailyCallBudget(ctx, dsn, day)
	if err != nil || report.Status != "exhausted" || report.RecordedPermits != 0 {
		t.Fatalf("active zero-cap report = %+v, %v", report, err)
	}
}

func TestDailyCallBudgetReportsExactUTCDateBoundaries(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	job := queueJob(t, ctx, store, 1, 0)
	lease, err := store.Claim(ctx, "worker", testIdentity(100), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	for i, stamp := range []string{
		"2026-03-07T23:59:59.999999Z", "2026-03-08T00:00:00Z",
		"2026-03-08T23:59:59.999999Z", "2026-03-09T00:00:00Z",
	} {
		callID := testIdentity(200 + i)
		if _, created, err := store.AuthorizeCall(ctx, job.ID, lease.WorkerID, lease.Token, callID); !created || err != nil {
			t.Fatalf("fixture grant = %t, %v", created, err)
		}
		stamp, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `UPDATE research_call_permits SET granted_at=$2 WHERE call_id=$1`, callID, stamp); err != nil {
			t.Fatal(err)
		}
	}
	localDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	params := localDSN.Query()
	params.Set("timezone", "America/New_York")
	localDSN.RawQuery = params.Encode()
	for day, expected := range map[string]int64{"2026-03-07": 1, "2026-03-08": 2, "2026-03-09": 1} {
		report, err := ReadDailyCallBudget(ctx, localDSN.String(), day)
		if err != nil || report.Day != day || report.RecordedPermits != expected {
			t.Fatalf("UTC budget report for %s = %+v, %v", day, report, err)
		}
	}
}
