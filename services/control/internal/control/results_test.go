package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/jobs"
)

const completePath = jobsPath + "/" + jobID + "/complete"
const resultPath = jobsPath + "/" + jobID + "/result"

func TestCompletesFixtureAgainstDurableSnapshot(t *testing.T) {
	input := fixture(t, "valid-result-request.json")
	dbNow := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	var receipt jobs.Result
	store := stubJobStore{
		complete: func(ctx context.Context, req jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
			if _, ok := ctx.Deadline(); !ok || req.JobID != jobID || req.WorkerID != "fixture-worker" || req.Token != "00000000-0000-0000-0000-000000000020" {
				t.Fatal("completion requires normalized identity and a storage deadline")
			}
			if want := time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC); !req.ExpiresAt.Equal(want) {
				t.Fatalf("receipt fence %s does not match batch expiry %s", req.ExpiresAt, want)
			}
			if receipt.JobID != "" {
				if !bytes.Equal(req.Payload, receipt.Payload) {
					t.Fatal("formatting affected the canonical result")
				}
				return receipt, false, nil
			}
			if err := verify(jobs.Job{ID: jobID, Status: "leased", Request: jobBody(t)}, dbNow); err != nil {
				return jobs.Result{}, false, err
			}
			receipt = jobs.Result{JobID: req.JobID, Attempt: 1, CompletedAt: dbNow, Payload: req.Payload}
			return receipt, true, nil
		},
		getResult: func(ctx context.Context, id string) (jobs.Result, error) {
			if _, ok := ctx.Deadline(); !ok || id != jobID {
				t.Fatal("result read requires normalized job ID and a storage deadline")
			}
			return receipt, nil
		},
	}
	handler := NewHandler(time.Hour, func() time.Time {
		t.Fatal("result freshness must use the DB clock")
		return time.Time{}
	}, store)
	created := callJobAPI(handler, http.MethodPost, jobsPath+"/"+strings.ToUpper(jobID)+"/complete", input)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated || created.Header.Get("Location") != resultPath {
		body, _ := io.ReadAll(created.Body)
		t.Fatalf("got status %d, location %s, response %s", created.StatusCode, created.Header.Get("Location"), body)
	}
	body, err := io.ReadAll(created.Body)
	if err != nil {
		t.Fatal(err)
	}
	var received struct {
		JobID       string                     `json:"job_id"`
		Status      string                     `json:"status"`
		Attempt     int                        `json:"attempt"`
		CompletedAt time.Time                  `json:"completed_at"`
		Result      map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &received); err != nil {
		t.Fatal(err)
	}
	if received.JobID != jobID || received.Status != "completed" || received.Attempt != 1 || !received.CompletedAt.Equal(dbNow) ||
		!hasExactKeys(received.Result, "result_schema_version", "batch", "reports", "metadata") || bytes.Contains(body, []byte(`"lease_token"`)) || bytes.Contains(body, []byte(`"worker_id"`)) {
		t.Fatalf("unexpected completion response: %s", body)
	}
	var formatted bytes.Buffer
	if err := json.Compact(&formatted, input); err != nil {
		t.Fatal(err)
	}
	replayed := callJobAPI(handler, http.MethodPost, completePath, formatted.Bytes())
	defer replayed.Body.Close()
	assertResponse(t, replayed, http.StatusOK, strings.TrimSpace(string(body)))
	read := callJobAPI(handler, http.MethodGet, jobsPath+"/"+strings.ToUpper(jobID)+"/result", nil)
	defer read.Body.Close()
	assertResponse(t, read, http.StatusOK, strings.TrimSpace(string(body)))
}

func TestCompletionRejectsTamperedEvidenceAndMetadata(t *testing.T) {
	input := fixture(t, "valid-result-request.json")
	var valid map[string]json.RawMessage
	if err := json.Unmarshal(input, &valid); err != nil {
		t.Fatal(err)
	}
	dbNow := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		batch    func(map[string]any)
		reports  func(map[string]map[string]string)
		metadata json.RawMessage
		dbNow    time.Time
	}{
		{name: "changed text", reports: func(reports map[string]map[string]string) { reports["TSLA"]["market_report"] += " altered" }},
		{name: "missing decision evidence", reports: func(reports map[string]map[string]string) { delete(reports["TSLA"], "final_trade_decision") }},
		{name: "unknown report field", reports: func(reports map[string]map[string]string) { reports["NVDA"]["unknown_field"] = "extra" }},
		{name: "extra symbol", reports: func(reports map[string]map[string]string) { reports["AMD"] = reports["NVDA"] }},
		{name: "empty report", reports: func(reports map[string]map[string]string) { reports["TSLA"]["market_report"] = "   " }},
		{name: "extra hash", batch: func(batch map[string]any) {
			batch["evidence_hashes"].(map[string]any)["TSLA:news_report"] = strings.Repeat("a", 64)
		}},
		{name: "wrong snapshot", batch: func(batch map[string]any) { batch["portfolio_snapshot_id"] = claimID }},
		{name: "invalid expiry", batch: func(batch map[string]any) { batch["expires_at"] = "tomorrow" }},
		{name: "expired at database time", dbNow: dbNow.Add(time.Hour)},
		{name: "non-synthetic metadata", metadata: []byte(`{"source_mode":"upstream_date_research","llm_calls":8,"alpha_measured":false}`)},
		{name: "null llm calls", metadata: []byte(`{"source_mode":"synthetic_fixture","llm_calls":null,"alpha_measured":false}`)},
		{name: "null alpha measured", metadata: []byte(`{"source_mode":"synthetic_fixture","llm_calls":0,"alpha_measured":null}`)},
		{name: "extra metadata", metadata: []byte(`{"source_mode":"synthetic_fixture","llm_calls":0,"alpha_measured":false,"unexpected":true}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var batch map[string]any
			var reports map[string]map[string]string
			if err := json.Unmarshal(valid["batch"], &batch); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(valid["reports"], &reports); err != nil {
				t.Fatal(err)
			}
			if tc.batch != nil {
				tc.batch(batch)
			}
			if tc.reports != nil {
				tc.reports(reports)
			}
			metadata := tc.metadata
			if metadata == nil {
				metadata = valid["metadata"]
			}
			checkingTime := tc.dbNow
			if checkingTime.IsZero() {
				checkingTime = dbNow
			}
			mutated, err := json.Marshal(map[string]any{
				"result_schema_version": valid["result_schema_version"], "worker_id": valid["worker_id"], "lease_token": valid["lease_token"],
				"batch": batch, "reports": reports, "metadata": metadata,
			})
			if err != nil {
				t.Fatal(err)
			}
			store := stubJobStore{complete: func(_ context.Context, _ jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
				return jobs.Result{}, false, verify(jobs.Job{ID: jobID, Request: jobBody(t)}, checkingTime)
			}}
			response := callJobAPI(NewHandler(time.Hour, time.Now, store), http.MethodPost, completePath, mutated)
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnprocessableEntity {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("got %d, want 422: %s", response.StatusCode, body)
			}
		})
	}
}

func TestInvalidBatchDoesNotBypassLeaseAuthority(t *testing.T) {
	input := fixture(t, "valid-result-request.json")
	input = bytes.Replace(input, []byte(`"execution_mode": "research"`), []byte(`"execution_mode": "live"`), 1)
	if !bytes.Contains(input, []byte(`"execution_mode": "live"`)) {
		t.Fatal("batch mutation did not apply")
	}
	store := stubJobStore{complete: func(_ context.Context, _ jobs.CompleteJob, _ func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
		return jobs.Result{}, false, jobs.ErrLeaseConflict
	}}
	response := callJobAPI(NewHandler(time.Hour, time.Now, store), http.MethodPost, completePath, input)
	defer response.Body.Close()
	assertResponse(t, response, http.StatusConflict, `{"error":"lease_conflict"}`)
}

func TestResultTransportMapsStoreErrorsWithoutLeakingTokens(t *testing.T) {
	input := fixture(t, "valid-result-request.json")
	for _, test := range []struct {
		name   string
		method string
		path   string
		err    error
		status int
		body   string
	}{
		{"missing job", http.MethodPost, completePath, jobs.ErrNotFound, 404, `{"error":"job_not_found"}`},
		{"stale lease", http.MethodPost, completePath, jobs.ErrLeaseConflict, 409, `{"error":"lease_conflict"}`},
		{"changed result", http.MethodPost, completePath, jobs.ErrConflict, 409, `{"error":"result_conflict"}`},
		{"expired at receipt", http.MethodPost, completePath, jobs.ErrResultExpired, 422, `{"error":"invalid_result"}`},
		{"usage contradicts recorded permits", http.MethodPost, completePath, jobs.ErrCallPermitMismatch, 422, `{"error":"invalid_result"}`},
		{"failed write", http.MethodPost, completePath, errors.New(leaseToken), 503, `{"error":"storage_unavailable"}`},
		{"unavailable result", http.MethodGet, resultPath, jobs.ErrNotFound, 404, `{"error":"result_not_found"}`},
		{"failed read", http.MethodGet, resultPath, errors.New(leaseToken), 503, `{"error":"storage_unavailable"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := stubJobStore{
				complete: func(context.Context, jobs.CompleteJob, func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
					return jobs.Result{}, false, test.err
				},
				getResult: func(context.Context, string) (jobs.Result, error) { return jobs.Result{}, test.err },
			}
			response := callJobAPI(NewHandler(time.Hour, time.Now, store), test.method, test.path, input)
			defer response.Body.Close()
			assertResponse(t, response, test.status, test.body)
		})
	}
}
