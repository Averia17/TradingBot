package researchscheduler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
)

func TestOccurrenceSurvivesControlAndSchedulerRestartInPostgres(t *testing.T) {
	ctx, base, schema, dsn := scheduleDatabase(t)
	now := time.Now().UTC().Add(-2 * time.Second)
	archive := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), archive, 0600); err != nil {
		t.Fatal(err)
	}
	frozen := testSchedule(t, now, now.Add(-time.Second), archive)
	for _, content := range []string{frozen, frozen, strings.Replace(frozen, `"frozen-v1"`, `"frozen-v2"`, 1)} {
		err := runScheduleWithStore(t, ctx, dsn, root, content)
		if content == frozen && err != nil {
			t.Fatal(err)
		}
		if content != frozen && (err == nil || !strings.Contains(err.Error(), "HTTP 409")) {
			t.Fatalf("changed policy for the same occurrence must conflict, got %v", err)
		}
	}
	assertSingleQueuedJob(t, ctx, base, schema)
}

func TestClosedCalendarWindowPersistsOnlyBoundedObservationsAcrossRestart(t *testing.T) {
	ctx, base, schema, dsn := scheduleDatabase(t)
	now := time.Now().UTC()
	missedAt := now.Add(-2 * time.Minute)
	acceptedAt := now.Add(-3 * time.Minute)
	oldAt := now.Add(-48 * time.Hour)
	archive := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	root := t.TempDir() // Missed observations need no archive or executable request.
	missed := testSchedule(t, missedAt, missedAt.Add(-time.Second), archive)
	missed = strings.Replace(missed, "000000000098", "000000000099", 1)
	accepted := testSchedule(t, acceptedAt, acceptedAt.Add(-time.Second), archive)
	schedule, err := Load(strings.NewReader(accepted))
	if err != nil {
		t.Fatal(err)
	}
	// A durable write can precede the first scheduler audit after restart.
	store, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	commitWithoutSchedulerReceipt(t, ctx, store, schedule.jobs[0])
	store.Close()

	for _, content := range []string{missed, accepted, testSchedule(t, oldAt, oldAt.Add(-time.Second), archive),
		eventSchedule(t, "report", "recent-event", missedAt, missedAt.Add(-time.Second), archive)} {
		if err := runScheduleWithStore(t, ctx, dsn, root, content); err != nil {
			t.Fatal(err)
		}
	}
	store, err = jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(control.NewHandler(time.Hour, time.Now, store))
	defer server.Close()
	path := server.URL + "/v1/research/schedule-occurrences/"
	for at, want := range map[time.Time]string{missedAt: "missed", acceptedAt: "job_recorded", oldAt: ""} {
		response, err := server.Client().Get(path + contract.ResearchCalendarJobID("offline-paper", at))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Status      string    `json:"status"`
			ScheduledAt time.Time `json:"scheduled_at"`
			LateSeconds int       `json:"max_lateness_seconds"`
			RecordedAt  time.Time `json:"recorded_at"`
		}
		err = json.NewDecoder(response.Body).Decode(&document)
		response.Body.Close()
		if want == "" {
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("historical date unexpectedly recorded: %d", response.StatusCode)
			}
			continue
		}
		if err != nil || response.StatusCode != http.StatusOK || document.Status != want ||
			!document.ScheduledAt.Equal(at) || document.LateSeconds != 60 || document.RecordedAt.Before(at.Add(time.Minute)) {
			t.Fatalf("record %s: %d %+v %v", want, response.StatusCode, document, err)
		}
	}
	missedSchedule, err := Load(strings.NewReader(missed))
	if err != nil {
		t.Fatal(err)
	}
	var retries sync.WaitGroup
	for range 4 {
		retries.Go(func() {
			if err := Run(ctx, missedSchedule, Config{ControlURL: server.URL, ArchiveRoot: root, Once: true}); err != nil {
				t.Error(err)
			}
		})
	}
	retries.Wait()
	changed := strings.Replace(missed, `"frozen-v1"`, `"frozen-v2"`, 1)
	if err := runScheduleWithStore(t, ctx, dsn, root, changed); err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("changed expired occurrence should conflict, got %v", err)
	}
	var count int
	table := pgx.Identifier{schema, "research_schedule_occurrences"}.Sanitize()
	if err := base.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 2 {
		t.Fatalf("bounded occurrence journal: %d rows: %v", count, err)
	}
	assertSingleQueuedJob(t, ctx, base, schema)

	// A previously unknown submission can become visible after the observation.
	job := missedSchedule.jobs[0]
	initial, err := store.GetCalendarOccurrence(ctx, job.ID)
	if err != nil || initial.Status != "missed" || initial.JobCreatedAt != nil {
		t.Fatalf("initial observation changed: %+v %v", initial, err)
	}
	commitWithoutSchedulerReceipt(t, ctx, store, job)
	reconciled, err := store.GetCalendarOccurrence(ctx, job.ID)
	if err != nil || reconciled.Status != "job_recorded" || reconciled.JobCreatedAt == nil ||
		!reconciled.RecordedAt.Equal(initial.RecordedAt) || reconciled.RequestSHA256 != initial.RequestSHA256 {
		t.Fatalf("observation did not reconcile with the later durable job: %+v %v", reconciled, err)
	}
}

func commitWithoutSchedulerReceipt(t *testing.T, ctx context.Context, store *jobs.Store, job occurrence) {
	t.Helper()
	var body jobSubmission
	if err := json.Unmarshal(job.Body, &body); err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(body.Portfolio)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.Create(ctx, jobs.CreateJob{
		ID: body.ID, Request: job.Body, SnapshotID: body.Portfolio.ID, Snapshot: snapshot,
	}, nil); err != nil || !created {
		t.Fatalf("create job without scheduler response: created=%v %v", created, err)
	}
}

func TestEventDeliverySurvivesRestartAndConflictsOnChangedEvidenceInPostgres(t *testing.T) {
	ctx, base, schema, dsn := scheduleDatabase(t)
	now := time.Now().UTC()
	cutoff, known := now.Add(-10*time.Second), now.Add(-20*time.Second)
	archive := eventArchive("filing", `{"reported_value":"2.50"}`, `"TSLA"`, known, known, known)
	changed := eventArchive("filing", `{"reported_value":"2.51"}`, `"TSLA"`, known.Add(time.Second), known.Add(time.Second), known.Add(time.Second))
	root := t.TempDir()
	for i, content := range [][]byte{archive, archive, changed} {
		if err := os.WriteFile(filepath.Join(root, "fixture.json"), content, 0600); err != nil {
			t.Fatal(err)
		}
		// Job identity is unchanged by new dispatch times after a restart.
		at := now.Add(time.Duration(i-3) * time.Second)
		frozen := eventSchedule(t, "report", "catalyst", at, cutoff, content)
		err := runScheduleWithStore(t, ctx, dsn, root, frozen)
		if i < 2 && err != nil {
			t.Fatalf("send or replay frozen event: %v", err)
		}
		if i == 2 && (err == nil || !strings.Contains(err.Error(), "HTTP 409")) {
			t.Fatalf("changed evidence under the same event ID must conflict, got %v", err)
		}
	}
	assertSingleQueuedJob(t, ctx, base, schema)
	var payload []byte
	if err := base.QueryRow(ctx, "SELECT request FROM "+pgx.Identifier{schema, "research_jobs"}.Sanitize()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	schedule, err := Load(strings.NewReader(eventSchedule(t, "report", "catalyst", now, cutoff, archive)))
	if err != nil || string(payload) != string(schedule.jobs[0].Body) {
		t.Fatalf("the durable original event request changed: %v", err)
	}
}

func scheduleDatabase(t *testing.T) (context.Context, *pgx.Conn, string, string) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "schedule_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error("could not clean test schema")
		}
	})
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return ctx, base, schema, parsed.String()
}

func runScheduleWithStore(t *testing.T, ctx context.Context, dsn, root, content string) error {
	t.Helper()
	// Each pass reopens PostgreSQL and reloads the scheduler input from bytes.
	store, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(control.NewHandler(time.Hour, time.Now, store))
	defer server.Close()
	schedule, err := Load(strings.NewReader(content))
	if err != nil {
		return err
	}
	return Run(ctx, schedule, Config{ControlURL: server.URL, ArchiveRoot: root, Once: true})
}

func assertSingleQueuedJob(t *testing.T, ctx context.Context, base *pgx.Conn, schema string) {
	t.Helper()
	for _, table := range []string{"research_jobs", "research_snapshots", "research_outbox"} {
		var count int
		if err := base.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, table}.Sanitize()).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s has %d rows after replay, want 1", table, count)
		}
	}
}
