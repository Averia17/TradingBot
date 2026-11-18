package paperthesis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
)

func openingDecision(ctx context.Context, research ResearchSource, order ledger.ReservationRequest, reservedAt time.Time) (contract.Intent, error) {
	job, err := research.Get(ctx, order.ResearchJobID)
	if err != nil {
		return contract.Intent{}, fmt.Errorf("read opening research job: %w", err)
	}
	receipt, err := research.GetResult(ctx, order.ResearchJobID)
	if err != nil {
		return contract.Intent{}, fmt.Errorf("read opening research result: %w", err)
	}
	if job.ID != order.ResearchJobID || job.Status != "completed" || job.CreatedAt.IsZero() || receipt.JobID != job.ID || receipt.Attempt < 1 ||
		receipt.CompletedAt.Before(job.CreatedAt) || receipt.CompletedAt.After(reservedAt) {
		return contract.Intent{}, fmt.Errorf("%w: opening receipt is not bound to the reservation", ErrInvalid)
	}
	var request struct {
		Version   string          `json:"job_schema_version"`
		ID        string          `json:"job_id"`
		Portfolio json.RawMessage `json:"portfolio"`
		Symbols   []string        `json:"requested_symbols"`
		Manifest  json.RawMessage `json:"data_manifest"`
		Policy    string          `json:"policy_version"`
		CallLimit int             `json:"max_llm_calls"`
	}
	var result struct {
		Version  string          `json:"result_schema_version"`
		Batch    json.RawMessage `json:"batch"`
		Reports  json.RawMessage `json:"reports"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if contract.Decode(job.Request, nil, &request) != nil || request.Version != "0.1" || request.ID != job.ID ||
		contract.CheckSymbols(request.Symbols) != nil || contract.Decode(receipt.Payload, nil, &result) != nil ||
		(result.Version != "0.1" && result.Version != "0.2") {
		return contract.Intent{}, fmt.Errorf("%w: unsupported opening research contract", ErrInvalid)
	}
	portfolio, err := contract.ParsePortfolio(request.Portfolio)
	if err != nil || portfolio.AsOf.After(job.CreatedAt) {
		return contract.Intent{}, fmt.Errorf("%w: opening research portfolio", ErrInvalid)
	}
	batch, err := contract.ParseBatch(result.Batch, receipt.CompletedAt, receipt.CompletedAt.Sub(portfolio.AsOf))
	if err != nil || contract.CheckAllocation(batch, portfolio, request.Symbols) != nil || order.ExpiresAt.After(batch.Expires) {
		return contract.Intent{}, fmt.Errorf("%w: opening decision does not match the research job", ErrInvalid)
	}
	for _, intent := range batch.Intents {
		if strings.EqualFold(intent.ID, order.DecisionID) && (intent.Action == "BUY" || intent.Action == "ADD") {
			// The research wire contract allows positive integers. Reject values
			// that would overflow when converted to hours for the review clock.
			if intent.Horizon.Min > 0 && int64(intent.Horizon.Max) <= int64((1<<63-1)/time.Hour) {
				return intent, nil
			}
			break
		}
	}
	return contract.Intent{}, fmt.Errorf("%w: no opening decision with a supported horizon", ErrInvalid)
}
