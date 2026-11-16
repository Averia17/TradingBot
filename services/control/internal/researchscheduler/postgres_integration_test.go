package researchscheduler

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
)

func TestOccurrenceSurvivesControlAndSchedulerRestartInPostgres(t *testing.T) {
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "schedule_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error("could not clean test schema")
		}
	}()
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

	now := time.Now().UTC().Add(-2 * time.Second)
	archive := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), archive, 0600); err != nil {
		t.Fatal(err)
	}
	frozen := testSchedule(t, now, now.Add(-time.Second), archive)
	for _, content := range []string{frozen, frozen, strings.Replace(frozen, `"frozen-v1"`, `"frozen-v2"`, 1)} {
		// Each pass reopens PostgreSQL and reloads the scheduler input from bytes.
		store, err := jobs.Open(ctx, parsed.String())
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(control.NewHandler(time.Hour, time.Now, store))
		schedule, err := Load(strings.NewReader(content))
		if err == nil {
			err = Run(ctx, schedule, Config{ControlURL: server.URL, ArchiveRoot: root, Once: true})
		}
		server.Close()
		store.Close()
		if content == frozen && err != nil {
			t.Fatal(err)
		}
		if content != frozen && (err == nil || !strings.Contains(err.Error(), "HTTP 409")) {
			t.Fatalf("changed policy for the same occurrence must conflict, got %v", err)
		}
	}
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
