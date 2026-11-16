package researchworker_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/researchworker"
)

const (
	jobPath    = "/v1/research/jobs"
	jobID      = "00000000-0000-0000-0000-000000000101"
	snapshotID = "00000000-0000-0000-0000-000000000102"
)

func TestWorkerRecoversLostClaimAndCompletionResponsesInPostgres(t *testing.T) {
	ctx, dsn, db := workerTestDatabase(t)
	store := openWorkerTestStore(t, ctx, dsn)
	controlHandler := control.NewHandler(time.Hour, time.Now, store)

	claimPath, completePath := jobPath+"/claim", jobPath+"/"+jobID+"/complete"
	heartbeatPath := jobPath + "/" + jobID + "/heartbeat"
	renewedLease := make(chan struct{})
	replayedCompletion := make(chan struct{})
	var observationsMu sync.Mutex
	observations := make(map[string][]recordedRequest)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.URL.Path == claimPath || r.URL.Path == completePath {
			var err error
			body, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read worker request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		response := httptest.NewRecorder()
		controlHandler.ServeHTTP(response, r)
		observationsMu.Lock()
		previous := observations[r.URL.Path]
		observations[r.URL.Path] = append(previous, recordedRequest{body, response.Code, response.Body.Bytes()})
		observationsMu.Unlock()
		first := len(previous) == 0
		switch {
		case r.URL.Path == claimPath && first && response.Code == http.StatusOK:
			dropWorkerResponse(t, w)
			return
		case r.URL.Path == completePath && first && response.Code == http.StatusCreated:
			dropWorkerResponse(t, w)
			return
		case r.URL.Path == heartbeatPath && len(previous) == 1 && response.Code == http.StatusOK:
			close(renewedLease)
		}
		for name, values := range response.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
		if r.URL.Path == completePath && len(previous) == 1 && response.Code == http.StatusOK {
			close(replayedCompletion)
		}
	}))
	defer server.Close()
	createWorkerTestJob(t, ctx, server.URL)

	analyze := func(ctx context.Context, assignment researchworker.Assignment) (json.RawMessage, error) {
		select {
		case <-renewedLease:
			return researchworker.AnalyzeSynthetic(ctx, assignment)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	stopWorker, workerDone := runWorkerForTest(ctx, server.URL, analyze)
	defer stopWorker()
	select {
	case <-replayedCompletion:
	case err := <-workerDone:
		t.Fatalf("worker stopped before replaying completion: %v", err)
	case <-ctx.Done():
		t.Fatal("worker did not replay the lost completion response")
	}
	stopWorker()
	waitForWorker(t, ctx, workerDone)

	observationsMu.Lock()
	claims := append([]recordedRequest(nil), observations[claimPath]...)
	completions := append([]recordedRequest(nil), observations[completePath]...)
	heartbeats := append([]recordedRequest(nil), observations[heartbeatPath]...)
	observationsMu.Unlock()
	if len(claims) < 2 || claims[0].status != http.StatusOK || claims[1].status != http.StatusOK || !bytes.Equal(claims[0].body, claims[1].body) {
		t.Fatal("the claim was not recovered with the original claim ID")
	}
	if len(heartbeats) < 2 || heartbeats[0].status != http.StatusOK || heartbeats[1].status != http.StatusOK ||
		!leaseExpiration(t, heartbeats[1].response).After(leaseExpiration(t, heartbeats[0].response)) {
		t.Fatal("periodic heartbeat did not extend the lease during analysis")
	}
	if len(completions) < 2 || len(completions) > 3 || completions[0].status != http.StatusCreated {
		t.Fatalf("completion did not commit and replay within its retry bound: %d requests", len(completions))
	}
	for i, replay := range completions[1:] {
		// Under load a reply can miss the client timeout after a durable replay.
		// Each additional request must carry and return the same durable result.
		if replay.status != http.StatusOK || !bytes.Equal(replay.body, completions[0].body) || !bytes.Equal(replay.response, completions[0].response) {
			t.Fatalf("completion replay %d did not return the original result: HTTP %d", i+1, replay.status)
		}
	}

	assertStoredWorkerResult(t, ctx, db, 1)
	server.Close()
	store.Close()
	reopenedStore := openWorkerTestStore(t, ctx, dsn)
	reopenedServer := httptest.NewServer(control.NewHandler(time.Hour, time.Now, reopenedStore))
	defer reopenedServer.Close()
	status, body := workerTestRequest(t, ctx, http.MethodGet, reopenedServer.URL+jobPath+"/"+jobID+"/result", nil)
	if status != http.StatusOK || !bytes.Equal(body, completions[1].response) {
		t.Fatalf("result did not survive reopening control and its database pool: HTTP %d", status)
	}
	assertSyntheticWorkerResult(t, body)
}

func TestWorkerLeavesExpiredAttemptAndPublishesOnNextAttemptInPostgres(t *testing.T) {
	ctx, dsn, db := workerTestDatabase(t)
	store := openWorkerTestStore(t, ctx, dsn)
	controlHandler := control.NewHandler(time.Hour, time.Now, store)
	completion := make(chan int, 3)
	completePath := jobPath + "/" + jobID + "/complete"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != completePath {
			controlHandler.ServeHTTP(w, r)
			return
		}
		response := httptest.NewRecorder()
		controlHandler.ServeHTTP(response, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
		select {
		case completion <- response.Code:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	createWorkerTestJob(t, ctx, server.URL)

	expiredResult := make(chan json.RawMessage, 1)
	analyze := func(ctx context.Context, assignment researchworker.Assignment) (json.RawMessage, error) {
		result, err := researchworker.AnalyzeSynthetic(ctx, assignment)
		if err != nil || assignment.Attempt != 1 {
			return result, err
		}
		// Advance persisted attempt times, serialized with the heartbeat's job lock.
		command, err := db.Exec(ctx, `WITH locked AS (SELECT id FROM research_jobs WHERE id = $1 FOR UPDATE)
			UPDATE research_job_attempts AS a SET started_at = clock_timestamp() - interval '2 seconds',
			expires_at = clock_timestamp() - interval '1 second' FROM locked
			WHERE a.job_id = locked.id AND a.lease_token = $2`, assignment.JobID, assignment.LeaseToken)
		if err != nil || command.RowsAffected() != 1 {
			return nil, fmt.Errorf("expire first attempt: rows=%d, error=%v", command.RowsAffected(), err)
		}
		<-ctx.Done()
		expiredResult <- result
		// A result can reach the worker just as a lease is canceled.
		return result, nil
	}
	stopWorker, workerDone := runWorkerForTest(ctx, server.URL, analyze)
	defer stopWorker()
	select {
	case status := <-completion:
		if status != http.StatusCreated {
			t.Fatalf("worker submitted an unusable result: HTTP %d", status)
		}
	case err := <-workerDone:
		t.Fatalf("worker stopped before completing the second attempt: %v", err)
	case <-ctx.Done():
		t.Fatal("worker did not recover the expired lease")
	}
	stopWorker()
	waitForWorker(t, ctx, workerDone)

	select {
	case body := <-expiredResult:
		status, _ := workerTestRequest(t, ctx, http.MethodPost, server.URL+completePath, body)
		if status != http.StatusConflict {
			t.Fatalf("control accepted publication from the expired attempt: HTTP %d", status)
		}
	default:
		t.Fatal("the worker did not cancel the expired attempt")
	}
	assertStoredWorkerResult(t, ctx, db, 2)
}

type recordedRequest struct {
	body     []byte
	status   int
	response []byte
}

func dropWorkerResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("close connection before writing response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_ = conn.Close()
}

func runWorkerForTest(ctx context.Context, baseURL string, analyze researchworker.AnalyzeFunc) (context.CancelFunc, <-chan error) {
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- researchworker.Run(workerCtx, researchworker.Config{
			BaseURL: baseURL, WorkerID: "integration-worker",
			PollInterval: 25 * time.Millisecond, HeartbeatInterval: 100 * time.Millisecond, RequestTimeout: 2 * time.Second,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, analyze)
	}()
	return stop, done
}

func waitForWorker(t *testing.T, ctx context.Context, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop worker: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("worker did not stop on cancellation")
	}
}

func leaseExpiration(t *testing.T, body []byte) time.Time {
	t.Helper()
	var lease struct {
		ExpiresAt time.Time `json:"lease_expires_at"`
	}
	if err := json.Unmarshal(body, &lease); err != nil || lease.ExpiresAt.IsZero() {
		t.Fatal("control did not return a lease expiration")
	}
	return lease.ExpiresAt
}

func createWorkerTestJob(t *testing.T, ctx context.Context, baseURL string) {
	t.Helper()
	contents, err := os.ReadFile("../../../../contracts/valid-portfolio.json")
	if err != nil {
		t.Fatal(err)
	}
	var portfolio map[string]json.RawMessage
	if err := json.Unmarshal(contents, &portfolio); err != nil {
		t.Fatal(err)
	}
	portfolio["snapshot_id"] = json.RawMessage(fmt.Sprintf("%q", snapshotID))
	portfolio["as_of"] = json.RawMessage(fmt.Sprintf("%q", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)))
	payload, err := json.Marshal(map[string]any{
		"job_schema_version": "0.1", "job_id": jobID, "portfolio": portfolio,
		"requested_symbols": []string{"META", "NVDA"},
		"data_manifest":     map[string]string{"ref": "offline/worker.json", "sha256": strings.Repeat("a", 64)},
		"policy_version":    "integration", "max_llm_calls": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body := workerTestRequest(t, ctx, http.MethodPost, baseURL+jobPath, payload)
	if status != http.StatusCreated {
		t.Fatalf("create job: HTTP %d, %s", status, body)
	}
}

func workerTestRequest(t *testing.T, ctx context.Context, method, endpoint string, body []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, payload
}

func assertStoredWorkerResult(t *testing.T, ctx context.Context, db *pgx.Conn, attempts int) {
	t.Helper()
	var status string
	var attemptCount, resultCount, resultAttempt, eventCount int
	err := db.QueryRow(ctx, `SELECT status,
		(SELECT count(*) FROM research_job_attempts WHERE job_id = $1),
		(SELECT count(*) FROM research_job_results WHERE job_id = $1),
		(SELECT attempt FROM research_job_results WHERE job_id = $1),
		(SELECT count(*) FROM research_outbox WHERE job_id = $1 AND event_type = 'research_job.completed')
		FROM research_jobs WHERE id = $1`, jobID).Scan(&status, &attemptCount, &resultCount, &resultAttempt, &eventCount)
	if err != nil || status != "completed" || attemptCount != attempts || resultCount != 1 || resultAttempt != attempts || eventCount != 1 {
		t.Fatalf("stored job = %q, %d attempts, %d results from attempt %d, %d completion events (%v)",
			status, attemptCount, resultCount, resultAttempt, eventCount, err)
	}
}

func assertSyntheticWorkerResult(t *testing.T, body []byte) {
	t.Helper()
	var reply struct {
		Result struct {
			Batch struct {
				SnapshotID string `json:"portfolio_snapshot_id"`
				Intents    []struct {
					Action string `json:"action"`
					Symbol string `json:"symbol"`
				} `json:"intents"`
			} `json:"batch"`
			Metadata struct {
				SourceMode    string `json:"source_mode"`
				Calls         *int   `json:"llm_calls"`
				AlphaMeasured *bool  `json:"alpha_measured"`
			} `json:"metadata"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	metadata, batch := reply.Result.Metadata, reply.Result.Batch
	if metadata.SourceMode != "synthetic_fixture" || metadata.Calls == nil || *metadata.Calls != 0 || metadata.AlphaMeasured == nil || *metadata.AlphaMeasured ||
		batch.SnapshotID != snapshotID || len(batch.Intents) != 2 || batch.Intents[0].Symbol != "META" || batch.Intents[1].Symbol != "NVDA" ||
		batch.Intents[0].Action != "HOLD" || batch.Intents[1].Action != "HOLD" {
		t.Fatal("result did not use the claimed snapshot and symbols in synthetic mode")
	}
}

func openWorkerTestStore(t *testing.T, ctx context.Context, dsn string) *jobs.Store {
	t.Helper()
	store, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal("open control job store in isolated test schema")
	}
	t.Cleanup(store.Close)
	return store
}

func workerTestDatabase(t *testing.T) (context.Context, string, *pgx.Conn) {
	t.Helper()
	baseDSN := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	dsn, err := url.Parse(baseDSN)
	if err != nil || dsn.Scheme == "" {
		t.Fatal("test database URL must be a PostgreSQL URI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "research_worker_" + strings.ToLower(rand.Text())
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
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	db, err := pgx.Connect(ctx, dsn.String())
	if err != nil {
		t.Fatal("connect to isolated test schema failed")
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return ctx, dsn.String(), db
}
