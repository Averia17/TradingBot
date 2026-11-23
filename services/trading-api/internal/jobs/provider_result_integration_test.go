package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"tradingbot/trading-api/internal/contract"
)

func TestPreparedCompletionRequiresExactCompletedProviderUsage(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	registry := strings.Repeat("a", 64)
	input := queueInput(t, 1, databaseNow(t, ctx, db).Add(-time.Minute))
	var wire map[string]any
	if err := json.Unmarshal(input.Request, &wire); err != nil {
		t.Fatal(err)
	}
	wire["job_schema_version"], wire["analysis_mode"], wire["max_llm_calls"] = "0.3", ProviderAnalysisMode, 1
	wire["identity_registry"] = map[string]string{"ref": "identity.json", "sha256": registry}
	input.Request, _ = json.Marshal(wire)
	if _, created, err := store.Create(ctx, input, nil); err != nil || !created {
		t.Fatalf("queue prepared job: created=%t error=%v", created, err)
	}
	lease, err := store.ClaimPreparedProvider(ctx, "prepared", testIdentity(100), testSnapshotWindow, registry)
	if err != nil {
		t.Fatal(err)
	}
	callID := testIdentity(101)
	if _, created, err := store.AuthorizeCall(ctx, input.ID, lease.WorkerID, lease.Token, callID); err != nil || !created {
		t.Fatalf("authorize call: created=%t error=%v", created, err)
	}
	request := attemptResult(lease, "prepared result")
	request.ExpectedAttempt, request.ExpectedLLMCalls = lease.Attempt, 1
	provider, model := "openai", "gpt-6-sol"
	expected := ProviderUsageExpectation{Provider: provider, Model: model, InputTokens: 420, OutputTokens: 92}
	check := func(Job, time.Time) error { return nil }
	reject := func(label string, expectation *ProviderUsageExpectation) {
		t.Helper()
		request.ExpectedProviderUsage = expectation
		if _, created, err := store.Complete(ctx, request, check); !errors.Is(err, ErrCallPermitMismatch) || created {
			t.Fatalf("%s: created=%t error=%v", label, created, err)
		}
	}
	reject("no usage expectation", nil)
	reject("no usage receipt", &expected)

	in, out := int(expected.InputTokens), int(expected.OutputTokens)
	observed := RecordResearchCallUsage{
		JobID: input.ID, WorkerID: lease.WorkerID, Token: lease.Token, Provider: true,
		Call: contract.ResearchLLMCall{RunID: callID, Provider: &provider, Model: &model,
			Role: "portfolio_allocator", Status: "completed", StartedAt: databaseNow(t, ctx, db).UTC(),
			InputTokens: &in, OutputTokens: &out, UsageSource: "langchain_usage_metadata"},
	}
	if _, created, err := store.RecordCallUsage(ctx, observed); err != nil || !created {
		t.Fatalf("record actual call: created=%t error=%v", created, err)
	}
	mismatch := expected
	mismatch.Model = "gpt-other"
	reject("different model", &mismatch)
	mismatch = expected
	mismatch.OutputTokens++
	reject("different token count", &mismatch)
	mismatch = expected
	mismatch.MissingUsage = true
	reject("different availability", &mismatch)
	assertCompletionRows(t, ctx, db, input.ID, 0)

	request.ExpectedProviderUsage = &expected
	result, created, err := store.Complete(ctx, request, check)
	if err != nil || !created || result.Attempt != lease.Attempt {
		t.Fatalf("valid completion: receipt=%+v created=%t error=%v", result, created, err)
	}
	assertCompletionRows(t, ctx, db, input.ID, 1)
	replay, created, err := store.Complete(ctx, request, func(Job, time.Time) error { return errors.New("revalidated stored result") })
	if err != nil || created || !replay.CompletedAt.Equal(result.CompletedAt) {
		t.Fatalf("exact completion replay: receipt=%+v created=%t error=%v", replay, created, err)
	}
	usage, err := ReadResearchCallUsage(ctx, dsn, input.ID)
	if err != nil || usage.AnalysisMode != ProviderAnalysisMode || !usage.PermitHistoryKnown ||
		len(usage.Records) != 1 || usage.Records[0].CallID != callID || usage.Records[0].PayloadSHA256 == "" {
		t.Fatalf("read prepared usage: mode=%s records=%d error=%v", usage.AnalysisMode, len(usage.Records), err)
	}
}

func TestPreparedUsageCountsPartialObservationAsMissing(t *testing.T) {
	provider, model := "openai", "gpt-6-sol"
	inputTokens := 12
	call := contract.ResearchLLMCall{
		RunID: testIdentity(1), Provider: &provider, Model: &model, Role: "portfolio_allocator",
		Status: "completed", StartedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		InputTokens: &inputTokens, UsageSource: "langchain_usage_metadata",
	}
	payload, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	missing := ProviderUsageExpectation{Provider: provider, Model: model, MissingUsage: true}
	complete := ProviderUsageExpectation{Provider: provider, Model: model, InputTokens: 12}
	if !missing.matchesCall(call.RunID, payload) || complete.matchesCall(call.RunID, payload) {
		t.Fatal("partial provider callback was treated as a complete total")
	}
	call.Status = "failed"
	payload, err = json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	if missing.matchesCall(call.RunID, payload) {
		t.Fatal("failed provider callback was allowed to complete a job")
	}
}
