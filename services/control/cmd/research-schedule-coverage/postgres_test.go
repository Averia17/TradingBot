package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/researchscheduler"
)

func TestCoverageReportsDeclaredWindowsAndDurableWorkerEvidence(t *testing.T) {
	ctx, dsn, db := coverageDatabase(t)
	store, err := jobs.Open(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	now, err := store.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	archive := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	digest := fmt.Sprintf("%x", sha256.Sum256(archive))
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), archive, 0600); err != nil {
		t.Fatal(err)
	}
	old, missed, open, active, future := now.Add(-72*time.Hour), now.Add(-2*time.Hour), now.Add(-3*time.Second), now.Add(-time.Second), now.Add(24*time.Hour)
	raw := coverageSchedule(t, digest, active)
	schedule, err := researchscheduler.Load(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(control.NewHandler(time.Hour, time.Now, store))
	t.Cleanup(server.Close)
	if err := researchscheduler.Run(ctx, schedule, researchscheduler.Config{ControlURL: server.URL, ArchiveRoot: root, Once: true}); err != nil {
		t.Fatal(err)
	}
	id := func(at time.Time) string { return contract.ResearchCalendarJobID("fixture-paper-research", at) }
	const worker = "fixture-worker-private"
	const claim = "10000000-0000-0000-0000-000000000001"
	lease, err := store.Claim(ctx, worker, claim, time.Hour)
	if err != nil || lease.Job.ID != id(active) {
		t.Fatalf("claim scheduled job: %v", err)
	}
	const seen = "10000000-0000-0000-0000-000000000002"
	const missing = "10000000-0000-0000-0000-000000000003"
	for _, call := range []string{seen, missing} {
		if _, created, err := store.AuthorizeCall(ctx, lease.Job.ID, worker, lease.Token, call); err != nil || !created {
			t.Fatalf("authorize fixture call: created=%v %v", created, err)
		}
	}
	if _, created, err := store.RecordCallUsage(ctx, jobs.RecordResearchCallUsage{
		JobID: lease.Job.ID, WorkerID: worker, Token: lease.Token,
		Call: contract.ResearchLLMCall{
			RunID: seen, Role: "research", Status: "completed", StartedAt: time.Now().UTC(), UsageSource: "unavailable",
		},
	}); err != nil || !created {
		t.Fatalf("record fixture callback: created=%v %v", created, err)
	}
	raw = coverageSchedule(t, digest, future, active, open, missed, old)
	full, err := researchscheduler.Load(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	plans, _ := full.PlannedCalendar()
	for _, plan := range plans {
		if plan.ScheduledAt.Equal(missed) {
			if _, created, err := store.RecordCalendarOccurrence(ctx, plan); err != nil || !created {
				t.Fatalf("record expired calendar window: created=%v %v", created, err)
			}
		}
	}
	readonly := *dsn
	params := readonly.Query()
	params.Set("default_transaction_read_only", "on")
	readonly.RawQuery = params.Encode()
	readReport := func() (coverageReport, map[string]windowEvidence) {
		t.Helper()
		config := filepath.Join(root, "config.json")
		if err := os.WriteFile(config, raw, 0600); err != nil {
			t.Fatal(err)
		}
		var stdout bytes.Buffer
		if err := run(ctx, readonly.String(), []string{"--config", config, "--sha256", fmt.Sprintf("%x", sha256.Sum256(raw))}, &stdout); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stdout.String(), worker) || strings.Contains(stdout.String(), lease.Token) || strings.Contains(stdout.String(), seen) {
			t.Fatal("coverage disclosed internal worker identities or callback payload")
		}
		var report coverageReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		validateCoverageFixture(t, stdout.Bytes())
		rows := make(map[string]windowEvidence)
		var previous time.Time
		for _, day := range report.Days {
			for _, row := range day.Occurrences {
				if row.ScheduledAt.Format(time.DateOnly) != day.Date || !previous.IsZero() && !row.ScheduledAt.After(previous) {
					t.Fatalf("wrong date grouping or order: %+v", report.Days)
				}
				previous = row.ScheduledAt
				rows[row.JobID] = row
			}
		}
		return report, rows
	}
	initial, rows := readReport()
	if len(rows) != 5 || initial.Scope != "declared_research_calendar" || initial.ExcludedEventOccurrences != 0 {
		t.Fatalf("wrong coverage scope: %+v", initial)
	}
	for at, want := range map[time.Time]string{
		old: "missing_evidence", missed: "observed_miss", open: "window_open", active: "on_time", future: "not_due",
	} {
		if row := rows[id(at)]; row.SubmissionStatus != want {
			t.Fatalf("window %s status=%s, want %s", at, row.SubmissionStatus, want)
		}
	}
	actual := rows[id(active)].Job
	if actual == nil || actual.Status != "leased" || actual.Attempts != 1 || actual.LeaseExpiresAt == nil ||
		!actual.PermitHistoryKnown || actual.DurablePermits != 2 || actual.RecordedCallbacks != 1 || actual.MissingCallbacks != 1 {
		t.Fatalf("wrong durable worker evidence: %+v", actual)
	}
	if _, err := db.Exec(ctx, `UPDATE research_call_usage SET payload = '\x12' WHERE job_id=$1`, lease.Job.ID); err != nil {
		t.Fatal(err)
	}
	if _, corrupted := readReport(); corrupted[id(active)].Job.InvalidCallbackReceipts != 1 {
		t.Fatal("altered callback did not surface as invalid evidence")
	}
	// A newly pinned copy with different policy preserves the calendar IDs but
	// must never bind to durable evidence recorded against the original request.
	raw = bytes.Replace(raw, []byte(`"policy_version":"fixture-paper-v1"`), []byte(`"policy_version":"fixture-paper-v2"`), 1)
	if _, changed := readReport(); changed[id(active)].SubmissionStatus != "conflict" ||
		changed[id(active)].Conflict != "job_request_evidence" || changed[id(missed)].Conflict != "closed_window_evidence" {
		t.Fatal("changed archived policy was incorrectly bound to the original job or observation")
	}
}

func coverageSchedule(t *testing.T, digest string, stamps ...time.Time) []byte {
	t.Helper()
	fixture, err := os.ReadFile("../../../../contracts/valid-research-schedule.json")
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Occurrences []json.RawMessage `json:"occurrences"`
	}
	if err := json.Unmarshal(fixture, &source); err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(fixture, &doc); err != nil {
		t.Fatal(err)
	}
	marshal := func(value any) json.RawMessage {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	var occurrences []json.RawMessage
	for _, at := range stamps {
		var item, portfolio map[string]json.RawMessage
		if err := json.Unmarshal(source.Occurrences[0], &item); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(item["portfolio"], &portfolio); err != nil {
			t.Fatal(err)
		}
		portfolio["as_of"] = marshal(at)
		item["portfolio"] = marshal(portfolio)
		item["scheduled_at"] = marshal(at)
		item["data_manifest"] = marshal(map[string]string{"ref": "fixture.json", "sha256": digest})
		occurrences = append(occurrences, marshal(item))
	}
	doc["occurrences"] = marshal(occurrences)
	return marshal(doc)
}

func coverageDatabase(t *testing.T) (context.Context, *url.URL, *pgx.Conn) {
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
	schema := "schedule_coverage_" + strings.ToLower(rand.Text())
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
	db, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatal("connect to test schema failed")
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return ctx, parsed, db
}
