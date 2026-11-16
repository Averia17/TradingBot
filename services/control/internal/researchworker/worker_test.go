package researchworker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testJobID = "00000000-0000-4000-8000-000000000011"
const testToken = "00000000-0000-4000-8000-000000000012"
const testWorker = "worker-test"
const testCompletion = `{"result_schema_version":"0.1","worker_id":"` + testWorker + `","lease_token":"` + testToken + `","batch":{"b":2,"a":1},"reports":{},"metadata":{}}`

func TestWorkerRetriesSameClaimAndCompletionAfterLostResponses(t *testing.T) {
	var claimID, sent atomic.Value
	var claims, completes, analyses atomic.Int32
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/claim"):
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			switch claims.Add(1) {
			case 1:
				claimID.Store(req["claim_id"])
				dropResponse(t, w)
			case 2:
				if req["worker_id"] != testWorker || req["claim_id"] == "" || req["claim_id"] != claimID.Load() {
					t.Error("a lost claim response must keep its worker and claim IDs")
				}
				writeLease(t, w, claimID.Load().(string), true)
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			writeLease(t, w, claimID.Load().(string), false)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			switch completes.Add(1) {
			case 1:
				sent.Store(string(body))
				dropResponse(t, w)
			case 2:
				if string(body) != sent.Load() {
					t.Error("a lost completion response must keep the exact original bytes")
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"job_id": testJobID, "status": "completed", "attempt": 1,
					"completed_at": time.Now().UTC(), "result": map[string]string{},
				})
			case 3:
				if string(body) != sent.Load() {
					t.Error("an incomplete receipt must be retried using the original bytes")
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"job_id": testJobID, "status": "completed", "attempt": 1,
					"completed_at": time.Now().UTC(),
					"result":       json.RawMessage(`{"result_schema_version":"0.1","reports":{},"metadata":{},"batch":{"a":1,"b":2}}`),
				})
				close(received)
			default:
				t.Error("unexpected completion after successful replay")
			}
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, testConfig(server), func(_ context.Context, a Assignment) (json.RawMessage, error) {
			analyses.Add(1)
			if a.JobID != testJobID || a.LeaseToken != testToken {
				t.Error("wrong assignment delivered to analyzer")
			}
			return json.RawMessage(testCompletion), nil
		})
	}()
	select {
	case <-received:
	case err := <-done:
		t.Fatalf("worker stopped before replay: %v", err)
	case <-ctx.Done():
		t.Fatal("completion was never replayed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if analyses.Load() != 1 || completes.Load() != 3 {
		t.Fatalf("analysis calls = %d, completion calls = %d", analyses.Load(), completes.Load())
	}
}

func TestHeartbeatIsNotDueWhenLeaseAlreadyCoversAttempt(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	w := worker{cfg: Config{HeartbeatInterval: time.Second}}
	if delay := w.untilHeartbeat(Assignment{LeaseExpiresAt: deadline, AttemptDeadline: deadline}); delay < 50*time.Second {
		t.Fatalf("heartbeat would repeat without extending the lease, delay = %v", delay)
	}
}

func TestWorkerCancelsAnalysisWhenHeartbeatLosesLease(t *testing.T) {
	var attempts, heartbeats, completes atomic.Int32
	started := make(chan struct{})
	finished := make(chan struct{})
	var claimID atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/claim"):
			if attempts.Add(1) > 1 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			claimID.Store(req["claim_id"])
			writeLease(t, w, req["claim_id"], true)
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			heartbeats.Add(1)
			select {
			case <-started:
				w.WriteHeader(http.StatusConflict)
			default:
				writeLease(t, w, claimID.Load().(string), false)
			}
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completes.Add(1)
			w.WriteHeader(http.StatusConflict)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, testConfig(server), func(ctx context.Context, _ Assignment) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(finished)
			return json.RawMessage(`{"test":"must not be published"}`), nil
		})
	}()
	select {
	case <-finished:
	case err := <-done:
		t.Fatalf("worker stopped before noticing lease loss: %v", err)
	case <-ctx.Done():
		t.Fatal("lost lease did not cancel the analyzer")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if heartbeats.Load() < 2 || completes.Load() != 0 {
		t.Fatalf("heartbeats = %d, completions = %d", heartbeats.Load(), completes.Load())
	}
}

func TestWorkerDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := Run(ctx, testConfig(server), func(context.Context, Assignment) (json.RawMessage, error) {
		t.Error("redirect must not reach the analyzer")
		return nil, nil
	})
	if !statusIs(err, http.StatusTemporaryRedirect) || redirected.Load() {
		t.Fatalf("redirect was not rejected: %v", err)
	}
}

func testConfig(server *httptest.Server) Config {
	return Config{
		BaseURL: server.URL, WorkerID: testWorker, PollInterval: 10 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond, RequestTimeout: time.Second,
	}
}

func writeLease(t *testing.T, w http.ResponseWriter, claimID string, includeRequest bool) {
	t.Helper()
	lease := map[string]any{
		"job_id": testJobID, "status": "leased", "worker_id": testWorker,
		"claim_id": claimID, "lease_token": testToken, "attempt": 1,
		"lease_expires_at": time.Now().UTC().Add(time.Minute),
		"attempt_deadline": time.Unix(4_000_000_000, 0).UTC(),
	}
	if includeRequest {
		lease["request"] = json.RawMessage(`{"job_schema_version":"0.1","job_id":"` + testJobID + `"}`)
	}
	if err := json.NewEncoder(w).Encode(lease); err != nil {
		t.Error(err)
	}
}

func dropResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = conn.Close()
}
