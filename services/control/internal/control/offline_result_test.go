package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/jobs"
)

func TestCompletionChecksOfflineProvenanceAndUsage(t *testing.T) {
	const valid = "valid-offline-result-request.json"
	for _, tc := range []struct {
		name, original, substitute string
		status                     int
	}{
		{name: "bound offline result", status: http.StatusCreated},
		{"wrong job", jobID, claimID, http.StatusUnprocessableEntity},
		{"wrong policy", "test-1", "other-1", http.StatusUnprocessableEntity},
		{"wrong manifest", "test/manifest.json", "other/manifest.json", http.StatusUnprocessableEntity},
		{"wrong manifest hash", strings.Repeat("a", 64), strings.Repeat("b", 64), http.StatusUnprocessableEntity},
		{"exceeds job budget", `"llm_calls": 12`, `"llm_calls": 33`, http.StatusUnprocessableEntity},
		{"fractional call count", `"llm_calls": 12`, `"llm_calls": 12.0`, http.StatusUnprocessableEntity},
		{"unobserved count over calls", `"missing_usage_calls": 12`, `"missing_usage_calls": 13`, http.StatusUnprocessableEntity},
		{"null usage", `"input_tokens": 0`, `"input_tokens": null`, http.StatusUnprocessableEntity},
		{"negative usage", `"output_tokens": 0`, `"output_tokens": -1`, http.StatusUnprocessableEntity},
		{"uncounted tokens", `"output_tokens": 0`, `"output_tokens": 1`, http.StatusUnprocessableEntity},
		{"claimed real billing", `"usd_cost": null`, `"usd_cost": "0"`, http.StatusUnprocessableEntity},
		{"invalid upstream commit", `"upstream_sha": "c`, `"upstream_sha": "x`, http.StatusUnprocessableEntity},
		{"unverified PIT data", `"intraday_pit_verified": false`, `"intraday_pit_verified": true`, http.StatusUnprocessableEntity},
		{"unknown provenance key", `"model_id":`, `"policy_bypass":false,"model_id":`, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fixture(t, valid)
			if tc.original != "" {
				if !bytes.Contains(payload, []byte(tc.original)) {
					t.Fatalf("fixture has no %s", tc.original)
				}
				payload = bytes.Replace(payload, []byte(tc.original), []byte(tc.substitute), 1)
			}
			now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
			var canonical []byte
			handler := NewHandler(time.Hour, time.Now, stubJobStore{complete: func(_ context.Context, input jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
				if input.ExpectedAttempt != 1 {
					t.Fatal("offline provenance attempt did not reach the store fence")
				}
				if err := verify(jobs.Job{ID: jobID, Status: "leased", Request: jobBody(t)}, now); err != nil {
					return jobs.Result{}, false, err
				}
				if input.ExpectedLLMCalls != 12 {
					t.Fatal("offline call count did not reach the store fence")
				}
				receipt := jobs.Result{JobID: jobID, Attempt: 1, CompletedAt: now, Payload: input.Payload}
				if canonical != nil && !bytes.Equal(canonical, input.Payload) {
					t.Fatal("formatting changed the durable idempotency payload")
				}
				created := canonical == nil
				canonical = input.Payload
				return receipt, created, nil
			}})
			response := callJobAPI(handler, http.MethodPost, completePath, payload)
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("got HTTP %d, want %d", response.StatusCode, tc.status)
			}
			if tc.status != http.StatusCreated {
				return
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, payload); err != nil {
				t.Fatal(err)
			}
			replay := callJobAPI(handler, http.MethodPost, completePath, compact.Bytes())
			defer replay.Body.Close()
			if replay.StatusCode != http.StatusOK {
				t.Fatalf("got HTTP %d on replay, want 200", replay.StatusCode)
			}
		})
	}
}
