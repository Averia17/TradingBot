package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testDatabase(t *testing.T) (context.Context, string, *pgx.Conn) {
	t.Helper()
	baseDSN := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "jobs_" + strings.ToLower(rand.Text())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		_ = base.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		defer base.Close(cleanupCtx)
		if _, err := base.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	dsn, err := url.Parse(baseDSN)
	if err != nil || dsn.Scheme == "" {
		t.Fatal("test database URL must be a PostgreSQL URI")
	}
	params := dsn.Query()
	params.Set("search_path", schema)
	dsn.RawQuery = params.Encode()
	inspection, err := pgx.Connect(ctx, dsn.String())
	if err != nil {
		t.Fatal("connect to test schema failed")
	}
	t.Cleanup(func() { _ = inspection.Close(context.Background()) })
	return ctx, dsn.String(), inspection
}

func testJob() CreateJob {
	return CreateJob{
		ID:         "00000000-0000-0000-0000-000000000001",
		Request:    json.RawMessage(`{"job_schema_version":"0.1","test":"immutable request"}`),
		SnapshotID: "00000000-0000-0000-0000-000000000002",
		Snapshot:   json.RawMessage(`{"test":"immutable snapshot"}`),
	}
}

func openTestStore(t *testing.T, ctx context.Context, dsn string) *Store {
	t.Helper()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func assertCounts(t *testing.T, ctx context.Context, conn *pgx.Conn, want int) {
	t.Helper()
	var snapshots, jobs, events int
	err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM research_snapshots),
		(SELECT count(*) FROM research_jobs), (SELECT count(*) FROM research_outbox)`).Scan(&snapshots, &jobs, &events)
	if err != nil {
		t.Fatal(err)
	}
	if snapshots != want || jobs != want || events != want {
		t.Fatalf("snapshots/jobs/events = %d/%d/%d, want each %d", snapshots, jobs, events, want)
	}
}

func TestResearchJobPersistsAndReplaysAcrossReconnect(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := testJob()
	if _, err := store.Get(ctx, input.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job error = %v", err)
	}
	first, created, err := store.Create(ctx, input, nil)
	if err != nil || !created || first.CreatedAt.IsZero() || first.Status != "queued" {
		t.Fatalf("create = %+v, %t, %v", first, created, err)
	}
	store.Close()
	reopened := openTestStore(t, ctx, dsn)
	got, err := reopened.Get(ctx, input.ID)
	if err != nil || got.ID != first.ID || got.Status != "queued" || !bytes.Equal(got.Request, input.Request) || !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("get after reopen = %+v, %v", got, err)
	}
	replay, created, err := reopened.Create(ctx, input, func() error {
		return errors.New("original snapshot no longer fresh")
	})
	if err != nil || created || !replay.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replayed = %+v, %t, %v", replay, created, err)
	}
	assertCounts(t, ctx, inspection, 1)
}

func TestResearchJobConflictsNeverChangeTheJobOrSnapshot(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	original := testJob()
	if _, created, err := store.Create(ctx, original, nil); err != nil || !created {
		t.Fatalf("create = %t, %v", created, err)
	}
	newSnapshot := original
	newSnapshot.SnapshotID = "00000000-0000-0000-0000-000000000003"
	newSnapshot.Request = json.RawMessage(`{"job_schema_version":"0.1","value":"different"}`)
	newJob := original
	newJob.ID = "00000000-0000-0000-0000-000000000004"
	newJob.Snapshot = json.RawMessage(`{"test":"mutated snapshot"}`)
	for _, request := range []CreateJob{newSnapshot, newJob} {
		if _, created, err := store.Create(ctx, request, nil); !errors.Is(err, ErrConflict) || created {
			t.Fatalf("conflicting create = %t, %v", created, err)
		}
	}
	var snapshot []byte
	if err := inspection.QueryRow(ctx, "SELECT payload FROM research_snapshots WHERE id = $1", original.SnapshotID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot, original.Snapshot) {
		t.Fatal("stored snapshot changed")
	}
	assertCounts(t, ctx, inspection, 1)
}

func TestConcurrentResearchJobRetriesCreateOneOutboxEvent(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := testJob()
	var attempts sync.WaitGroup
	var created, admissions atomic.Int32
	const retries = 12
	for range retries {
		attempts.Go(func() {
			job, fresh, err := store.Create(ctx, input, func() error {
				admissions.Add(1)
				return nil
			})
			if err != nil || job.ID != input.ID {
				t.Errorf("create = %+v, %t, %v", job, fresh, err)
			}
			if fresh {
				created.Add(1)
			}
		})
	}
	attempts.Wait()
	if created.Load() != 1 || admissions.Load() != 1 {
		t.Fatalf("created %d jobs after %d admission checks", created.Load(), admissions.Load())
	}
	assertCounts(t, ctx, inspection, 1)
}

func TestAdmissionAndOutboxFailuresRollBackTheWholeJob(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := testJob()
	stale := errors.New("stale snapshot")
	if _, created, err := store.Create(ctx, input, func() error { return stale }); !errors.Is(err, stale) || created {
		t.Fatalf("rejected admission = %t, %v", created, err)
	}
	assertCounts(t, ctx, inspection, 0)
	if _, err := inspection.Exec(ctx, "ALTER TABLE research_outbox ADD CONSTRAINT refuse_events CHECK (false)"); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.Create(ctx, input, nil); err == nil || created {
		t.Fatalf("failed outbox write = %t, %v", created, err)
	}
	assertCounts(t, ctx, inspection, 0)
	if _, err := store.Get(ctx, input.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back job error = %v", err)
	}
}

func TestIndependentJobsShareOneIdenticalSnapshot(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	first := testJob()
	second := first
	second.ID = "00000000-0000-0000-0000-000000000003"
	second.Request = json.RawMessage(`{"job_schema_version":"0.1","job":"different request"}`)
	for _, input := range []CreateJob{first, second} {
		if job, created, err := store.Create(ctx, input, nil); err != nil || !created || job.ID != input.ID {
			t.Fatalf("create = %+v, %t, %v", job, created, err)
		}
	}
	var snapshots, jobs, events int
	err := inspection.QueryRow(ctx, `SELECT (SELECT count(*) FROM research_snapshots),
		(SELECT count(*) FROM research_jobs), (SELECT count(*) FROM research_outbox)`).Scan(&snapshots, &jobs, &events)
	if err != nil || snapshots != 1 || jobs != 2 || events != 2 {
		t.Fatalf("snapshots/jobs/events = %d/%d/%d, err=%v", snapshots, jobs, events, err)
	}
}

func TestCommitFailureLeavesJobUnacknowledged(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	if _, err := inspection.Exec(ctx, `CREATE FUNCTION refuse_outbox_commit() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test: reject commit'; END; $$;
		CREATE CONSTRAINT TRIGGER refuse_outbox_commit AFTER INSERT ON research_outbox
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_outbox_commit()`); err != nil {
		t.Fatal(err)
	}
	input := testJob()
	if job, created, err := store.Create(ctx, input, nil); err == nil || created || job.ID != "" {
		t.Fatalf("failed commit = %+v, %t, %v", job, created, err)
	}
	assertCounts(t, ctx, inspection, 0)
}

func TestStoreWithoutDatabaseConnectionCannotAcknowledgeJobs(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	store.Close()
	if err := store.Ping(ctx); err == nil {
		t.Fatal("closed store reports ready")
	}
	input := testJob()
	if job, created, err := store.Create(ctx, input, nil); err == nil || created || job.ID != "" {
		t.Fatalf("create without connection = %+v, %t, %v", job, created, err)
	}
	assertCounts(t, ctx, inspection, 0)
}

func TestOpenRefusesNewerUnknownDatabaseVersion(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	_ = openTestStore(t, ctx, dsn)
	if _, err := inspection.Exec(ctx, "INSERT INTO research_schema_migrations (version) VALUES (999)"); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(ctx, dsn); err == nil {
		store.Close()
		t.Fatal("opened unsupported database version")
	}
}
