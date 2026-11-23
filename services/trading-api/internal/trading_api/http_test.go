package trading_api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testHandler(now time.Time) http.Handler {
	return NewHandler(time.Hour, func() time.Time { return now }, nil)
}

func postValidation(handler http.Handler, contentType string, body []byte) *http.Response {
	request := httptest.NewRequest(http.MethodPost, "/v1/research/validate", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func fixture(t *testing.T, filename string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("../../../../contracts/" + filename)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validationBody(t *testing.T) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"batch":             fixture(t, "valid-batch.json"),
		"portfolio":         fixture(t, "valid-portfolio.json"),
		"requested_symbols": []string{"TSLA", "NVDA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func assertResponse(t *testing.T, response *http.Response, wantStatus int, wantJSON string) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus || strings.TrimSpace(string(body)) != wantJSON {
		t.Fatalf("got status %d and %s, want status %d and %s", response.StatusCode, body, wantStatus, wantJSON)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("got content type %q, want application/json", got)
	}
}

func TestHealthAndActualCapabilities(t *testing.T) {
	handler := testHandler(time.Now())
	health := callJobAPI(handler, http.MethodGet, "/healthz", nil)
	defer health.Body.Close()
	assertResponse(t, health, http.StatusOK, `{"status":"ok"}`)

	capabilities := callJobAPI(handler, http.MethodGet, "/v1/capabilities", nil)
	defer capabilities.Body.Close()
	assertResponse(t, capabilities, http.StatusOK, `{"durable_jobs":false,"execution":false,"ledger":false,"research_batch_schema_version":"0.1","research_job_call_budget":false,"research_job_leases":false,"research_job_results":false,"research_validation":true}`)
}

func TestValidatesPythonResearchFixture(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	response := postValidation(testHandler(now), "application/json; charset=utf-8", validationBody(t))
	defer response.Body.Close()
	assertResponse(t, response, http.StatusOK, `{"valid":true}`)
}

func TestValidationRejectsInvalidInput(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	handler := testHandler(now)
	payload := validationBody(t)
	for _, test := range []struct {
		name        string
		body        []byte
		contentType string
		wantStatus  int
		wantError   string
	}{
		{"unsupported media type", payload, "text/plain", 415, "unsupported_content_type"},
		{"oversized body", bytes.Repeat([]byte{' '}, 256*1024+1), "application/json", 413, "body_too_large"},
		{"duplicate field", []byte(`{"batch":{},"batch":{}}`), "application/json", 400, "invalid_request"},
		{"unknown field", bytes.Replace(payload, []byte(`"requested_symbols"`), []byte(`"Requested_symbols"`), 1), "application/json", 400, "invalid_request"},
		{"trailing JSON", append(bytes.Clone(payload), []byte(` {}`)...), "application/json", 400, "invalid_request"},
		{"numeric money", bytes.Replace(payload, []byte(`"cash_usd":"1000"`), []byte(`"cash_usd":1000`), 1), "application/json", 422, "invalid_portfolio"},
		{"execution mode", bytes.Replace(payload, []byte(`"execution_mode":"research"`), []byte(`"execution_mode":"live"`), 1), "application/json", 422, "invalid_batch"},
		{"snapshot mismatch", bytes.Replace(payload, []byte(`"snapshot_id":"00000000-0000-0000-0000-000000000001"`), []byte(`"snapshot_id":"00000000-0000-0000-0000-000000000099"`), 1), "application/json", 422, "invalid_allocation"},
		{"duplicate symbols", bytes.Replace(payload, []byte(`["TSLA","NVDA"]`), []byte(`["TSLA","TSLA"]`), 1), "application/json", 422, "invalid_requested_symbols"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := postValidation(handler, test.contentType, test.body)
			defer response.Body.Close()
			assertResponse(t, response, test.wantStatus, `{"error":"`+test.wantError+`"}`)
		})
	}
}

func TestRejectsExpiredResearchFixture(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC)
	response := postValidation(testHandler(now), "application/json", validationBody(t))
	defer response.Body.Close()
	assertResponse(t, response, http.StatusUnprocessableEntity, `{"error":"invalid_batch"}`)
}

func TestValidationRequiresPost(t *testing.T) {
	response := callJobAPI(testHandler(time.Now()), http.MethodGet, "/v1/research/validate", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != "POST" {
		t.Fatalf("got status %d, Allow %q", response.StatusCode, response.Header.Get("Allow"))
	}
}

func TestCancelledRequestDoesNotValidateBatch(t *testing.T) {
	handler := NewHandler(time.Hour, func() time.Time {
		t.Fatal("validated a batch for a cancelled request")
		return time.Time{}
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/research/validate", bytes.NewReader(validationBody(t)))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), request)
}

func TestJSONRequestConcurrencyIsBounded(t *testing.T) {
	entered := make(chan struct{}, concurrentWrites+1)
	release := make(chan struct{})
	defer close(release)
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	handler := NewHandler(time.Hour, func() time.Time {
		entered <- struct{}{}
		<-release
		return now
	}, stubJobStore{})
	payload := validationBody(t)
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/research/validate", bytes.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		return r
	}
	for range concurrentWrites {
		go handler.ServeHTTP(httptest.NewRecorder(), request())
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range concurrentWrites {
		select {
		case <-entered:
		case <-timer.C:
			t.Fatal("active requests did not enter validation")
		}
	}
	response := httptest.NewRecorder()
	busy := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request())
		close(busy)
	}()
	select {
	case <-busy:
		assertResponse(t, response.Result(), http.StatusServiceUnavailable, `{"error":"validator_busy"}`)
	case <-entered:
		t.Fatal("over-capacity request entered validation")
	case <-timer.C:
		t.Fatal("over-capacity request did not return")
	}
	job := callJobAPI(handler, http.MethodPost, jobsPath, nil)
	defer job.Body.Close()
	assertResponse(t, job, http.StatusServiceUnavailable, `{"error":"job_api_busy"}`)
}
