package researchworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tradingbot/control/internal/contract"
)

type syntheticJobRequest struct {
	Version          string          `json:"job_schema_version"`
	ID               string          `json:"job_id"`
	Portfolio        json.RawMessage `json:"portfolio"`
	RequestedSymbols []string        `json:"requested_symbols"`
	DataManifest     json.RawMessage `json:"data_manifest"`
	PolicyVersion    string          `json:"policy_version"`
	MaxLLMCalls      int             `json:"max_llm_calls"`
}

type syntheticMetadata struct {
	SourceMode    string `json:"source_mode"`
	LLMCalls      int    `json:"llm_calls"`
	AlphaMeasured bool   `json:"alpha_measured"`
}

type syntheticCompletion struct {
	Version    string                       `json:"result_schema_version"`
	WorkerID   string                       `json:"worker_id"`
	LeaseToken string                       `json:"lease_token"`
	Batch      contract.Batch               `json:"batch"`
	Reports    map[string]map[string]string `json:"reports"`
	Metadata   syntheticMetadata            `json:"metadata"`
}

// AnalyzeSynthetic produces a fixture bound to the leased portfolio and
// symbols. All decisions are explicit fixture HOLDs and use no model or data API.
func AnalyzeSynthetic(ctx context.Context, assignment Assignment) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var request syntheticJobRequest
	if err := contract.Decode(assignment.Request, nil, &request); err != nil {
		return nil, fmt.Errorf("decode leased job: %w", err)
	}
	if request.Version != "0.1" || request.ID == "" || request.ID != assignment.JobID ||
		assignment.WorkerID == "" || assignment.LeaseToken == "" {
		return nil, errors.New("invalid leased job identity or version")
	}
	if err := contract.CheckSymbols(request.RequestedSymbols); err != nil {
		return nil, err
	}
	portfolio, err := contract.ParsePortfolio(request.Portfolio)
	if err != nil {
		return nil, fmt.Errorf("parse leased portfolio: %w", err)
	}
	now := time.Now().UTC()
	if portfolio.AsOf.After(now) || !now.Before(assignment.LeaseExpiresAt) || !now.Before(assignment.AttemptDeadline) {
		return nil, errors.New("leased attempt or portfolio is outside its valid time window")
	}

	result := syntheticCompletion{
		Version:    "0.1",
		WorkerID:   assignment.WorkerID,
		LeaseToken: assignment.LeaseToken,
		Batch: contract.Batch{
			Version: contract.BatchVersion, Mode: "research", RunID: syntheticID(assignment.LeaseToken, "run"),
			SnapshotID: portfolio.ID, AsOf: portfolio.AsOf.UTC(), Created: now, Expires: assignment.AttemptDeadline.UTC(),
			Evidence: make(map[string]string, len(request.RequestedSymbols)),
			Intents:  make([]contract.Intent, 0, len(request.RequestedSymbols)),
		},
		Reports:  make(map[string]map[string]string, len(request.RequestedSymbols)),
		Metadata: syntheticMetadata{SourceMode: "synthetic_fixture"},
	}
	for _, symbol := range request.RequestedSymbols {
		const field = "final_trade_decision"
		const report = "HOLD: synthetic fixture for offline research job verification."
		evidence := symbol + ":" + field
		digest := sha256.Sum256([]byte(report))
		result.Reports[symbol] = map[string]string{field: report}
		result.Batch.Evidence[evidence] = hex.EncodeToString(digest[:])
		result.Batch.Intents = append(result.Batch.Intents, contract.Intent{
			Symbol: symbol, Action: "HOLD", Notional: "0", Horizon: contract.Horizon{Min: 24, Max: 720},
			Thesis:       []string{"Synthetic fixture without a market thesis"},
			Invalidation: []string{"Use a TradingAgents run for market research"},
			Evidence:     []string{evidence},
			ID:           syntheticID(assignment.LeaseToken, "decision:"+symbol),
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// The v8 UUID encodes a stable, domain-separated identifier for this fixture attempt.
func syntheticID(leaseToken, domain string) string {
	digest := sha256.Sum256([]byte("synthetic_fixture:" + leaseToken + ":" + domain))
	digest[6] = digest[6]&0x0f | 0x80
	digest[8] = digest[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}
