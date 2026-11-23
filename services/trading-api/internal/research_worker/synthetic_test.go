package research_worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"tradingbot/trading-api/internal/contract"
)

func TestAnalyzeSyntheticUsesClaimedPortfolioAndSymbols(t *testing.T) {
	assignment, portfolio := syntheticTestAssignment(t, []string{"BRK.B", "NVDA"})
	body, err := AnalyzeSynthetic(t.Context(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Version    string                       `json:"result_schema_version"`
		WorkerID   string                       `json:"worker_id"`
		LeaseToken string                       `json:"lease_token"`
		Batch      json.RawMessage              `json:"batch"`
		Reports    map[string]map[string]string `json:"reports"`
		Metadata   json.RawMessage              `json:"metadata"`
	}
	if err := contract.Decode(body, nil, &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != "0.1" || result.WorkerID != assignment.WorkerID || result.LeaseToken != assignment.LeaseToken {
		t.Fatal("completion does not identify the leased attempt")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(result.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 3 || string(metadata["source_mode"]) != `"synthetic_fixture"` ||
		string(metadata["llm_calls"]) != "0" || string(metadata["alpha_measured"]) != "false" {
		t.Fatalf("unexpected metadata: %s", result.Metadata)
	}
	batch, err := contract.ParseBatch(result.Batch, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.CheckAllocation(batch, portfolio, []string{"BRK.B", "NVDA"}); err != nil {
		t.Fatal(err)
	}
	if batch.Expires.After(assignment.AttemptDeadline) {
		t.Fatal("batch outlives the attempt")
	}
	if len(result.Reports) != len(batch.Intents) {
		t.Fatal("reports do not cover the claimed universe")
	}
	evidenceCount := 0
	for _, intent := range batch.Intents {
		if intent.Action != "HOLD" || intent.Notional != "0" || intent.Confidence != nil || intent.Expected != nil {
			t.Fatalf("unexpected non-fixture intent: %+v", intent)
		}
		report := result.Reports[intent.Symbol]
		if report["final_trade_decision"] == "" {
			t.Fatalf("missing decision for %s", intent.Symbol)
		}
		for name, content := range report {
			digest := sha256.Sum256([]byte(content))
			if batch.Evidence[intent.Symbol+":"+name] != hex.EncodeToString(digest[:]) {
				t.Fatalf("report %s for %s has no matching evidence digest", name, intent.Symbol)
			}
			evidenceCount++
		}
	}
	if len(batch.Evidence) != evidenceCount {
		t.Fatal("batch claims additional evidence")
	}
}

func TestAnalyzeSyntheticRejectsIneligibleAssignment(t *testing.T) {
	t.Run("duplicate universe", func(t *testing.T) {
		assignment, _ := syntheticTestAssignment(t, []string{"NVDA", "NVDA"})
		if body, err := AnalyzeSynthetic(t.Context(), assignment); err == nil || body != nil {
			t.Fatal("invalid universe yielded a fixture")
		}
	})
	t.Run("expired lease", func(t *testing.T) {
		assignment, _ := syntheticTestAssignment(t, []string{"NVDA"})
		assignment.LeaseExpiresAt = time.Now().Add(-time.Second)
		if body, err := AnalyzeSynthetic(t.Context(), assignment); err == nil || body != nil {
			t.Fatal("expired lease yielded a fixture")
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		assignment, _ := syntheticTestAssignment(t, []string{"NVDA"})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if body, err := AnalyzeSynthetic(ctx, assignment); !errors.Is(err, context.Canceled) || body != nil {
			t.Fatal("cancelled attempt yielded a fixture")
		}
	})
}

func syntheticTestAssignment(t *testing.T, symbols []string) (Assignment, contract.Portfolio) {
	t.Helper()
	now := time.Now().UTC()
	portfolio := contract.Portfolio{
		ID: "f0afebe1-b74f-4d37-9f9f-8e5946f92421", AsOf: now.Add(-time.Minute),
		Currency: "USD", CashAsset: "USDC", Cash: "150.01", Reserved: "3.10",
		MaxPosition: "0.25", Positions: []contract.Position{},
	}
	assignment := Assignment{
		JobID: "796ad879-9fe1-45e7-b388-9fc32986eebe", WorkerID: "fixture-worker",
		ClaimID: "9f5043a4-74dd-4be8-a5bd-66cdc98dcfc1", LeaseToken: "8e599a00-3da4-4972-b269-90a449f0b230",
		Attempt: 1, LeaseExpiresAt: now.Add(time.Minute), AttemptDeadline: now.Add(10 * time.Minute),
	}
	request, err := json.Marshal(map[string]any{
		"job_schema_version": "0.1", "job_id": assignment.JobID, "portfolio": portfolio,
		"requested_symbols": symbols, "policy_version": "test-v1", "max_llm_calls": 5,
		"data_manifest": map[string]string{
			"ref": "test/snapshot", "sha256": "c4ad9055f9e6b98ec1235a115fafc393735c0d7c34daefeae13224063139fbc4",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment.Request = request
	return assignment, portfolio
}
