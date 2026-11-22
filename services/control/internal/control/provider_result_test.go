package control

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"tradingbot/control/internal/jobs"
)

const preparedProviderResultFixture = "valid-prepared-provider-result-request.json"

func preparedProviderJob(t *testing.T) []byte {
	t.Helper()
	return fixture(t, "valid-research-provider-job.json")
}

func TestPreparedProviderResultAndLeaseProvenance(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	job := jobs.Job{ID: jobID, Status: "completed", Request: preparedProviderJob(t), CreatedAt: now.Add(-time.Minute)}
	body := fixture(t, preparedProviderResultFixture)
	handler := NewHandler(time.Hour, time.Now, stubJobStore{complete: func(_ context.Context, input jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
		if input.ExpectedAttempt != 1 || input.ExpectedLLMCalls != 1 {
			t.Fatal("provider result skipped the attempt and call count fence")
		}
		expected := jobs.ProviderUsageExpectation{Provider: "openai", Model: "gpt-6-sol", InputTokens: 480, OutputTokens: 92}
		if input.ExpectedProviderUsage == nil || *input.ExpectedProviderUsage != expected {
			t.Fatalf("provider usage expectation = %+v", input.ExpectedProviderUsage)
		}
		if err := verify(job, now); err != nil {
			return jobs.Result{}, false, err
		}
		receipt := jobs.Result{JobID: job.ID, Attempt: 1, CompletedAt: now, Payload: input.Payload}
		if err := ValidateArchivedResearchResult(job, receipt); err != nil {
			t.Fatal(err)
		}
		receipt.Attempt = 2
		if err := ValidateArchivedResearchResult(job, receipt); err == nil {
			t.Fatal("result from a different archived attempt accepted")
		}
		receipt.Attempt = 1
		return receipt, true, nil
	}})
	response := callJobAPI(handler, http.MethodPost, completePath, body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("got HTTP %d, want %d", response.StatusCode, http.StatusCreated)
	}
}

func TestPreparedProviderResultRejectsCrossModeAndUnpinnedEvidence(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	providerJob, identityJob := preparedProviderJob(t), fixture(t, identityJobFixture)
	providerResult := fixture(t, preparedProviderResultFixture)
	swap := func(old, next string) []byte {
		t.Helper()
		if !bytes.Contains(providerResult, []byte(old)) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return bytes.Replace(providerResult, []byte(old), []byte(next), 1)
	}
	for _, test := range []struct {
		name   string
		job    []byte
		result []byte
	}{
		{"offline lease", identityJob, providerResult},
		{"offline result", providerJob, fixture(t, identityResultFixture)},
		{"wrong job mode", bytes.Replace(providerJob, []byte("prepared_provider"), []byte("offline"), 1), providerResult},
		{"synthetic archive", providerJob, swap(`"archive_source_mode": "local_archive"`, `"archive_source_mode": "synthetic_fixture"`)},
		{"scripted model", providerJob, swap(`"model_id": "gpt-6-sol"`, `"model_id": "scripted-offline-v1"`)},
		{"offline provider", providerJob, swap(`"model_provider": "openai"`, `"model_provider": "offline"`)},
		{"other identity", providerJob, swap("fixture-tesla", "other-tesla")},
		{"missing required report", providerJob, swap(`"news_report":`, `"final_trade_decision":`)},
		{"additional report", providerJob, swap(`"news_report":`, `"final_trade_decision":"extra","news_report":`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(time.Hour, time.Now, stubJobStore{complete: func(_ context.Context, _ jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
				return jobs.Result{}, false, verify(jobs.Job{ID: jobID, Request: test.job}, now)
			}})
			response := callJobAPI(handler, http.MethodPost, completePath, test.result)
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnprocessableEntity && !(test.name == "wrong job mode" && response.StatusCode == http.StatusServiceUnavailable) {
				t.Fatalf("got HTTP %d, want a rejected result", response.StatusCode)
			}
		})
	}
}
