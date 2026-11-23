package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/trading_api"
)

func TestExportCompletedResearchFromReadOnlyPostgresSnapshot(t *testing.T) {
	ctx, dsn, db := replayDatabase(t)
	store, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const firstID = "f700cba3-c634-4204-8669-1acd78612431"
	const secondID = "0700cba3-c634-4204-8669-1acd78612432"
	const queuedID = "f700cba3-c634-4204-8669-1acd78612433"
	first := persistResearchJob(t, ctx, store, firstID, true)
	second := persistResearchJob(t, ctx, store, secondID, true)
	persistResearchJob(t, ctx, store, queuedID, false)

	readonly, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parameters := readonly.Query()
	parameters.Set("default_transaction_read_only", "on")
	readonly.RawQuery = parameters.Encode()
	var out bytes.Buffer
	if err := run(ctx, readonly.String(), []string{"--job", secondID, "--job", firstID}, &out); err != nil {
		t.Fatal(err)
	}
	assertExportSchema(t, out.Bytes())
	var saved replayExport
	if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Version != "0.1" || len(saved.Jobs) != 2 {
		t.Fatalf("unexpected export version or count: %s/%d", saved.Version, len(saved.Jobs))
	}
	for i, wantResult := range []jobs.Result{first, second} {
		entry := saved.Jobs[i]
		job, err := store.Get(ctx, wantResult.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if entry.JobID != wantResult.JobID || entry.Attempt != wantResult.Attempt ||
			!bytes.Equal(entry.Request, job.Request) || !entry.CreatedAt.Equal(job.CreatedAt) ||
			!bytes.Equal(entry.Result, wantResult.Payload) || !entry.CompletedAt.Equal(wantResult.CompletedAt) {
			t.Fatalf("export did not preserve immutable bytes and receipt times for %s", wantResult.JobID)
		}
		if bytes.Contains(entry.Result, []byte("lease_token")) || bytes.Contains(entry.Result, []byte("worker_id")) {
			t.Fatal("result export contained worker lease authorization")
		}
	}

	var reordered bytes.Buffer
	if err := run(ctx, readonly.String(), []string{"--job", firstID, "--job", secondID}, &reordered); err != nil ||
		!bytes.Equal(out.Bytes(), reordered.Bytes()) {
		t.Fatalf("export bytes changed when job selection order changed: %v", err)
	}
	reordered.Reset()
	if err := run(ctx, readonly.String(), []string{"--job", secondID, "--job", queuedID}, &reordered); err == nil || reordered.Len() != 0 {
		t.Fatal("export selected an incomplete job or wrote a partial document")
	}
	if _, err := db.Exec(ctx, `UPDATE research_job_results SET payload_sha256 = decode(repeat('00', 32), 'hex') WHERE job_id = $1`, firstID); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, readonly.String(), []string{"--job", secondID, "--job", firstID}, &reordered); err == nil || reordered.Len() != 0 {
		t.Fatal("export accepted a result whose stored digest is incorrect")
	}
}

func persistResearchJob(t *testing.T, ctx context.Context, store *jobs.Store, id string, complete bool) jobs.Result {
	t.Helper()
	handler := trading_api.NewHandler(time.Hour, time.Now, store)
	var portfolio contract.Portfolio
	if err := json.Unmarshal(fixture(t, "valid-portfolio.json"), &portfolio); err != nil {
		t.Fatal(err)
	}
	now, err := store.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	portfolio.ID, portfolio.AsOf = id, now.Add(-time.Second)
	request := map[string]any{
		"job_schema_version": "0.1", "job_id": id, "portfolio": portfolio,
		"requested_symbols": []string{"TSLA", "NVDA"}, "policy_version": "fixture-1",
		"max_llm_calls": 4, "data_manifest": map[string]string{
			"ref": "fixtures/manifest.json", "sha256": strings.Repeat("a", 64),
		},
	}
	const path = "/v1/research/jobs"
	postResearch(t, handler, path, request)
	if !complete {
		return jobs.Result{}
	}
	lease, err := store.Claim(ctx, "replay-test-worker", id, time.Hour)
	if err != nil || lease.Job.ID != id {
		t.Fatalf("claim %s: %v", id, err)
	}
	var completion map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, "valid-result-request.json"), &completion); err != nil {
		t.Fatal(err)
	}
	var batch contract.Batch
	if err := json.Unmarshal(completion["batch"], &batch); err != nil {
		t.Fatal(err)
	}
	batch.AsOf, batch.Created, batch.Expires = portfolio.AsOf, now, now.Add(time.Minute)
	batch.SnapshotID = portfolio.ID
	postResearch(t, handler, path+"/"+id+"/complete", map[string]any{
		"result_schema_version": "0.1", "worker_id": lease.WorkerID, "lease_token": lease.Token,
		"batch": batch, "reports": completion["reports"], "metadata": completion["metadata"],
	})
	result, err := store.GetResult(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func postResearch(t *testing.T, handler http.Handler, path string, payload any) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST %s returned %d: %s", path, response.Code, response.Body)
	}
}

func replayDatabase(t *testing.T) (context.Context, string, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to research test database failed")
	}
	schema := "replay_export_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer db.Close(cleanup)
		if _, err := db.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(dsn)
	if err != nil || uri.Scheme == "" {
		t.Fatal("test PostgreSQL configuration must be a URI")
	}
	params := uri.Query()
	params.Set("search_path", schema)
	uri.RawQuery = params.Encode()
	return ctx, uri.String(), db
}
