package control

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/jobs"
)

const identityResultFixture = "valid-research-identity-result-request.json"

func TestIdentityResultMustMatchDurableResearchIdentity(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	identityJob, legacyJob := fixture(t, identityJobFixture), jobBody(t)
	identityResult := fixture(t, identityResultFixture)
	withField := func(old, next string) []byte {
		t.Helper()
		if !bytes.Contains(identityResult, []byte(old)) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return bytes.Replace(identityResult, []byte(old), []byte(next), 1)
	}
	for _, test := range []struct {
		name   string
		job    []byte
		result []byte
	}{
		{"legacy job", legacyJob, identityResult},
		{"legacy offline result", identityJob, fixture(t, "valid-offline-result-request.json")},
		{"fixture result", identityJob, fixture(t, "valid-result-request.json")},
		{"different registry ref", identityJob, withField("test/research-identity.json", "test/other.json")},
		{"different registry SHA", identityJob, withField(strings.Repeat("e", 64), strings.Repeat("f", 64))},
		{"different security", identityJob, withField("fixture-tesla", "other-tesla")},
		{"different projected SHA grammar", identityJob, withField(strings.Repeat("d", 64), strings.Repeat("D", 64))},
		{"unexpected binding field", identityJob, withField(`"security_id": "fixture-tesla"`, `"security_id":"fixture-tesla","alias":"TSLA"`)},
		{"missing projected SHA", identityJob, withField(`"projected_snapshot_sha256":`, `"legacy_snapshot_sha256":`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := stubJobStore{complete: func(_ context.Context, _ jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
				return jobs.Result{}, false, verify(jobs.Job{ID: jobID, Request: test.job}, now)
			}}
			response := callJobAPI(NewHandler(time.Hour, time.Now, store), http.MethodPost, completePath, test.result)
			defer response.Body.Close()
			assertResponse(t, response, 422, `{"error":"invalid_result"}`)
		})
	}
}

func TestIdentityResultPreservesOriginalProjectionProvenance(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	identityJob := jobs.Job{ID: jobID, Status: "completed", Request: fixture(t, identityJobFixture), CreatedAt: now.Add(-time.Minute)}
	identityResult := fixture(t, identityResultFixture)
	var receipt jobs.Result
	handler := NewHandler(time.Hour, time.Now, stubJobStore{complete: func(_ context.Context, input jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
		if input.ExpectedAttempt != 1 || input.ExpectedLLMCalls != 12 {
			t.Fatal("bound result skipped the durable attempt and call-count fence")
		}
		if err := verify(identityJob, now); err != nil {
			t.Fatal(err)
		}
		created := receipt.Payload == nil
		if !created && !bytes.Equal(receipt.Payload, input.Payload) {
			return jobs.Result{}, false, jobs.ErrConflict
		}
		receipt = jobs.Result{JobID: jobID, Attempt: 1, CompletedAt: now, Payload: input.Payload}
		if err := ValidateArchivedResearchResult(identityJob, receipt); err != nil {
			t.Fatal(err)
		}
		return receipt, created, nil
	}})
	changedProjection := bytes.Replace(identityResult, []byte(strings.Repeat("d", 64)), []byte(strings.Repeat("f", 64)), 1)
	for _, test := range []struct {
		body   []byte
		status int
	}{
		{identityResult, 201},
		{append([]byte("\n "), identityResult...), 200},
		{changedProjection, 409},
	} {
		response := callJobAPI(handler, http.MethodPost, completePath, test.body)
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("got status %d, want %d", response.StatusCode, test.status)
		}
	}
}
