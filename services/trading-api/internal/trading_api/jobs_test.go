package trading_api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tradingbot/trading-api/internal/jobs"
)

const (
	jobID    = "f700cba3-c634-4204-8669-1acd78612431"
	jobsPath = "/v1/research/jobs"
)

type stubJobStore struct {
	create    func(context.Context, jobs.CreateJob, func() error) (jobs.Job, bool, error)
	get       func(context.Context, string) (jobs.Job, error)
	ping      func(context.Context) error
	claim     func(context.Context, string, string, time.Duration) (jobs.Lease, error)
	heartbeat func(context.Context, string, string, string) (jobs.Lease, error)
	authorize func(context.Context, string, string, string, string) (jobs.CallPermit, bool, error)
	complete  func(context.Context, jobs.CompleteJob, func(jobs.Job, time.Time) error) (jobs.Result, bool, error)
	getResult func(context.Context, string) (jobs.Result, error)
}

func (s stubJobStore) Create(ctx context.Context, job jobs.CreateJob, admit func() error) (jobs.Job, bool, error) {
	return s.create(ctx, job, admit)
}

func (s stubJobStore) Get(ctx context.Context, id string) (jobs.Job, error) {
	return s.get(ctx, id)
}

func (s stubJobStore) Ping(ctx context.Context) error {
	return s.ping(ctx)
}

func (s stubJobStore) Claim(ctx context.Context, workerID, claimID string, maxAge time.Duration) (jobs.Lease, error) {
	return s.claim(ctx, workerID, claimID, maxAge)
}

func (s stubJobStore) Heartbeat(ctx context.Context, jobID, workerID, token string) (jobs.Lease, error) {
	return s.heartbeat(ctx, jobID, workerID, token)
}

func (s stubJobStore) AuthorizeCall(ctx context.Context, jobID, workerID, token, callID string) (jobs.CallPermit, bool, error) {
	return s.authorize(ctx, jobID, workerID, token, callID)
}

func (s stubJobStore) Complete(ctx context.Context, input jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
	return s.complete(ctx, input, verify)
}

func (s stubJobStore) GetResult(ctx context.Context, id string) (jobs.Result, error) {
	return s.getResult(ctx, id)
}

func jobBody(t *testing.T) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"job_schema_version": "0.1",
		"job_id":             jobID,
		"portfolio":          fixture(t, "valid-portfolio.json"),
		"requested_symbols":  []string{"TSLA", "NVDA"},
		"data_manifest":      map[string]string{"ref": "test/manifest.json", "sha256": strings.Repeat("a", 64)},
		"policy_version":     "test-1",
		"max_llm_calls":      32,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func callJobAPI(handler http.Handler, method, path string, body []byte) *http.Response {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Result()
}

func TestCreatesResearchJobWithNormalizedID(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	payload := jobBody(t)
	store := stubJobStore{create: func(ctx context.Context, job jobs.CreateJob, admit func() error) (jobs.Job, bool, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("create request needs a storage deadline")
		}
		if job.ID != jobID || job.SnapshotID != "00000000-0000-0000-0000-000000000001" || !json.Valid(job.Snapshot) {
			t.Fatalf("unexpected create payload: %+v", job)
		}
		if err := admit(); err != nil {
			t.Fatal(err)
		}
		return jobs.Job{ID: job.ID, Status: "queued", Request: job.Request, CreatedAt: now}, true, nil
	}}
	handler := NewHandler(time.Hour, func() time.Time { return now }, store)
	uppercase := bytes.Replace(payload, []byte(jobID), []byte(strings.ToUpper(jobID)), 1)
	response := callJobAPI(handler, http.MethodPost, jobsPath, uppercase)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated || response.Header.Get("Location") != jobsPath+"/"+jobID {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("got %d and %s: %s", response.StatusCode, response.Header.Get("Location"), body)
	}
	var value struct {
		ID      string                     `json:"job_id"`
		Status  string                     `json:"status"`
		Request map[string]json.RawMessage `json:"request"`
	}
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if value.ID != jobID || value.Status != "queued" || string(value.Request["job_id"]) != `"`+jobID+`"` {
		t.Fatalf("unexpected response: %+v", value)
	}
}

func TestFreshnessAppliesToAdmissionAndNotReplay(t *testing.T) {
	now := time.Date(2026, 10, 4, 20, 0, 0, 0, time.UTC)
	payload := jobBody(t)
	for _, test := range []struct {
		name       string
		create     func(context.Context, jobs.CreateJob, func() error) (jobs.Job, bool, error)
		wantStatus int
	}{
		{"new", func(_ context.Context, _ jobs.CreateJob, admit func() error) (jobs.Job, bool, error) {
			return jobs.Job{}, false, admit()
		}, http.StatusUnprocessableEntity},
		{"replay", func(_ context.Context, job jobs.CreateJob, _ func() error) (jobs.Job, bool, error) {
			return jobs.Job{ID: job.ID, Status: "queued", Request: job.Request, CreatedAt: now.Add(-8 * time.Hour)}, false, nil
		}, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(time.Hour, func() time.Time { return now }, stubJobStore{create: test.create})
			response := callJobAPI(handler, http.MethodPost, jobsPath, payload)
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("got %d, want %d: %s", response.StatusCode, test.wantStatus, body)
			}
		})
	}
}

func TestInvalidResearchJobDoesNotReachStorage(t *testing.T) {
	handler := NewHandler(time.Hour, time.Now, stubJobStore{})
	payload := jobBody(t)
	for _, test := range []struct {
		name       string
		old        string
		new        string
		wantStatus int
		wantError  string
	}{
		{"unknown key", `"policy_version"`, `"Policy_version"`, 400, "invalid_request"},
		{"duplicate field", `"max_llm_calls":32`, `"max_llm_calls":32,"max_llm_calls":64`, 400, "invalid_request"},
		{"unknown manifest key", `"ref"`, `"Ref"`, 400, "invalid_request"},
		{"URL in opaque manifest reference", `test/manifest.json`, `https://example.com/data`, 422, "invalid_job_request"},
		{"bad SHA-256", strings.Repeat("a", 64), strings.Repeat("B", 64), 422, "invalid_job_request"},
		{"unknown schema version", `"job_schema_version":"0.1"`, `"job_schema_version":"9.9"`, 422, "invalid_job_request"},
		{"invalid UUID", jobID, `bad-id`, 422, "invalid_job_request"},
		{"excess call budget", `"max_llm_calls":32`, `"max_llm_calls":65`, 422, "invalid_job_request"},
		{"fractional call budget", `"max_llm_calls":32`, `"max_llm_calls":32.0`, 400, "invalid_request"},
		{"numeric money", `"cash_usd":"1000"`, `"cash_usd":1000`, 422, "invalid_portfolio"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := bytes.Replace(payload, []byte(test.old), []byte(test.new), 1)
			if bytes.Equal(request, payload) {
				t.Fatal("test did not mutate payload")
			}
			response := callJobAPI(handler, http.MethodPost, jobsPath, request)
			defer response.Body.Close()
			assertResponse(t, response, test.wantStatus, `{"error":"`+test.wantError+`"}`)
		})
	}
}

func TestCanonicalJobRetainsMoneyAndIgnoresJSONFormatting(t *testing.T) {
	payload := jobBody(t)
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	formatted := &bytes.Buffer{}
	if err := json.Indent(formatted, payload, "", "  "); err != nil {
		t.Fatal(err)
	}
	var captured json.RawMessage
	store := stubJobStore{create: func(_ context.Context, job jobs.CreateJob, _ func() error) (jobs.Job, bool, error) {
		if captured == nil {
			captured = job.Request
		} else if !bytes.Equal(captured, job.Request) {
			t.Fatalf("canonical payload changed with whitespace: %s", job.Request)
		}
		if !bytes.Contains(job.Snapshot, []byte(`"cash_usd":"1000"`)) {
			t.Fatalf("snapshot did not retain decimal string: %s", job.Snapshot)
		}
		return jobs.Job{ID: job.ID, Request: job.Request, Status: "queued", CreatedAt: now}, true, nil
	}}
	handler := NewHandler(time.Hour, func() time.Time { return now }, store)
	for _, body := range [][]byte{payload, formatted.Bytes()} {
		response := callJobAPI(handler, http.MethodPost, jobsPath, body)
		response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("got %d, want 201", response.StatusCode)
		}
	}
}

func TestJobCreateDoesNotAcknowledgeStorageFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		status   int
		response string
	}{
		{"conflict", jobs.ErrConflict, 409, `{"error":"job_conflict"}`},
		{"failure", errors.New("private database error"), 503, `{"error":"storage_unavailable"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := stubJobStore{create: func(context.Context, jobs.CreateJob, func() error) (jobs.Job, bool, error) {
				return jobs.Job{}, false, test.err
			}}
			response := callJobAPI(NewHandler(time.Hour, time.Now, store), http.MethodPost, jobsPath, jobBody(t))
			defer response.Body.Close()
			assertResponse(t, response, test.status, test.response)
		})
	}
}

func TestJobReadAndReadiness(t *testing.T) {
	store := stubJobStore{
		get: func(_ context.Context, id string) (jobs.Job, error) {
			if id != jobID {
				t.Errorf("query used %q instead of normalized %q", id, jobID)
			}
			return jobs.Job{}, jobs.ErrNotFound
		},
		ping: func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("readiness needs a storage deadline")
			}
			return errors.New("private database error")
		},
	}
	handler := NewHandler(time.Hour, time.Now, store)
	for _, test := range []struct {
		path   string
		status int
		body   string
	}{
		{jobsPath + "/" + strings.ToUpper(jobID), 404, `{"error":"job_not_found"}`},
		{jobsPath + "/invalid", 400, `{"error":"invalid_job_id"}`},
		{"/readyz", 503, `{"error":"storage_unavailable"}`},
		{"/healthz", 200, `{"status":"ok"}`},
	} {
		response := callJobAPI(handler, http.MethodGet, test.path, nil)
		assertResponse(t, response, test.status, test.body)
		response.Body.Close()
	}
}
