package trading_api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
)

var (
	errInvalidResult    = errors.New("invalid research result")
	errInvalidStoredJob = errors.New("invalid immutable research job")
)

type resultDocument struct {
	SchemaVersion string                       `json:"result_schema_version"`
	Batch         json.RawMessage              `json:"batch"`
	Reports       map[string]map[string]string `json:"reports"`
	Metadata      any                          `json:"metadata"`
	ExpiresAt     time.Time                    `json:"-"`

	checkedBatch contract.Batch
	batchValid   bool
}

func (h jobHTTP) complete(w http.ResponseWriter, r *http.Request, jobID string) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	var workerID, token string
	if contract.Decode(body, nil, &fields) != nil || !hasExactKeys(fields,
		"result_schema_version", "worker_id", "lease_token", "batch", "reports", "metadata") ||
		json.Unmarshal(fields["worker_id"], &workerID) != nil || !workerIDPattern.MatchString(workerID) ||
		json.Unmarshal(fields["lease_token"], &token) != nil || !jobUUID.MatchString(strings.ToLower(token)) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	document, err := parseResultDocument(fields)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_result")
		return
	}
	payload, err := json.Marshal(document)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_result")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	input := jobs.CompleteJob{
		JobID: jobID, WorkerID: workerID, Token: strings.ToLower(token), Payload: payload, ExpiresAt: document.ExpiresAt,
	}
	if metadata, ok := document.Metadata.(workerResultMetadata); ok {
		input.ExpectedAttempt = metadata.Provenance.Attempt
		input.ExpectedLLMCalls = metadata.LLMCalls
		if document.SchemaVersion == providerResultVersion {
			input.ExpectedProviderUsage = &jobs.ProviderUsageExpectation{
				Provider: metadata.Provenance.ModelProvider, Model: metadata.Provenance.ModelID,
				InputTokens: metadata.InputTokens, OutputTokens: metadata.OutputTokens,
				MissingUsage: metadata.MissingUsageCalls == 1,
			}
		}
	}
	result, created, err := h.store.Complete(ctx, input, func(job jobs.Job, dbNow time.Time) error {
		return document.validate(job, dbNow, h.maxResearchAge)
	})
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "job_not_found")
	case errors.Is(err, jobs.ErrLeaseConflict):
		writeError(w, http.StatusConflict, "lease_conflict")
	case errors.Is(err, jobs.ErrConflict):
		writeError(w, http.StatusConflict, "result_conflict")
	case errors.Is(err, errInvalidResult), errors.Is(err, jobs.ErrResultExpired), errors.Is(err, jobs.ErrCallPermitMismatch):
		writeError(w, http.StatusUnprocessableEntity, "invalid_result")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		w.Header().Set("Location", "/v1/research/jobs/"+result.JobID+"/result")
		writeResult(w, createStatus(created), result)
	}
}

func (h jobHTTP) getResult(w http.ResponseWriter, r *http.Request, jobID string) {
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	result, err := h.store.GetResult(ctx, jobID)
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "result_not_found")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		writeResult(w, http.StatusOK, result)
	}
}

func parseResultDocument(fields map[string]json.RawMessage) (resultDocument, error) {
	var document resultDocument
	if json.Unmarshal(fields["result_schema_version"], &document.SchemaVersion) != nil {
		return resultDocument{}, errInvalidResult
	}
	var err error
	document.Metadata, err = parseResultMetadata(document.SchemaVersion, fields["metadata"])
	if err != nil {
		return resultDocument{}, err
	}
	if json.Unmarshal(fields["reports"], &document.Reports) != nil || len(document.Reports) == 0 || len(document.Reports) > 100 {
		return resultDocument{}, errInvalidResult
	}
	for _, reports := range document.Reports {
		if document.SchemaVersion == providerResultVersion {
			if len(reports) != 3 || strings.TrimSpace(reports["market_report"]) == "" ||
				strings.TrimSpace(reports["fundamentals_report"]) == "" || strings.TrimSpace(reports["news_report"]) == "" {
				return resultDocument{}, errInvalidResult
			}
		} else if strings.TrimSpace(reports["final_trade_decision"]) == "" {
			return resultDocument{}, errInvalidResult
		}
		for field, text := range reports {
			switch field {
			case "market_report", "fundamentals_report", "news_report", "sentiment_report", "trader_investment_plan", "final_trade_decision":
			default:
				return resultDocument{}, errInvalidResult
			}
			if strings.TrimSpace(text) == "" {
				return resultDocument{}, errInvalidResult
			}
		}
	}
	// Sort object keys without converting JSON numbers to float64. Array order
	// and decimal string values are part of the persisted result identity.
	d := json.NewDecoder(bytes.NewReader(fields["batch"]))
	d.UseNumber()
	var batch map[string]any
	if err := d.Decode(&batch); err != nil || batch == nil {
		return resultDocument{}, errInvalidResult
	}
	// Read the receipt deadline from the exact batch verified against the job.
	expiry, _ := batch["expires_at"].(string)
	document.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiry)
	if err != nil || document.ExpiresAt.IsZero() {
		return resultDocument{}, errInvalidResult
	}
	document.Batch, err = json.Marshal(batch)
	if err != nil {
		return resultDocument{}, err
	}
	// Local checks do not need a job lock. Record their verdict for the
	// store callback, where lease authority and the durable clock are known.
	document.checkedBatch, err = contract.DecodeBatch(document.Batch)
	document.batchValid = err == nil && document.matchesReportHashes()
	return document, nil
}

func (d resultDocument) matchesReportHashes() bool {
	count := 0
	for symbol, reports := range d.Reports {
		for field, text := range reports {
			digest := sha256.Sum256([]byte(text))
			if d.checkedBatch.Evidence[symbol+":"+field] != hex.EncodeToString(digest[:]) {
				return false
			}
			count++
		}
	}
	return count == len(d.checkedBatch.Evidence)
}

func (d resultDocument) validate(job jobs.Job, dbNow time.Time, maxAge time.Duration) error {
	var request jobRequest
	if err := contract.Decode(job.Request, nil, &request); err != nil || request.ID != job.ID ||
		contract.CheckSymbols(request.RequestedSymbols) != nil {
		return errInvalidStoredJob
	}
	switch request.Version {
	case providerJobSchemaVersion:
		if request.AnalysisMode != jobs.ProviderAnalysisMode || request.MaxLLMCalls != 1 {
			return errInvalidStoredJob
		}
		if d.SchemaVersion != providerResultVersion {
			return errInvalidResult
		}
	case identityJobSchemaVersion:
		if d.SchemaVersion != identityResultVersion {
			return errInvalidResult
		}
	case jobSchemaVersion:
		if d.SchemaVersion != syntheticResultVersion && d.SchemaVersion != offlineResultVersion {
			return errInvalidResult
		}
	default:
		return errInvalidStoredJob
	}
	bound := request.Version != jobSchemaVersion
	if bound {
		if !request.hasIdentityBindings() {
			return errInvalidStoredJob
		}
	} else if request.IdentityRegistry != nil || len(request.SecurityBindings) != 0 {
		return errInvalidStoredJob
	}
	if metadata, ok := d.Metadata.(workerResultMetadata); ok {
		provenance := metadata.Provenance
		if provenance.JobID != job.ID || provenance.PolicyVersion != request.PolicyVersion ||
			provenance.DataManifest != request.DataManifest || metadata.LLMCalls > request.MaxLLMCalls {
			return errInvalidResult
		}
		if bound && (*provenance.IdentityRegistry != *request.IdentityRegistry ||
			!slices.Equal(provenance.SecurityBindings, request.SecurityBindings)) {
			return errInvalidResult
		}
	}
	portfolio, err := contract.ParsePortfolio(request.Portfolio)
	if err != nil {
		return errInvalidStoredJob
	}
	if !d.batchValid || d.checkedBatch.CheckFreshness(dbNow, maxAge) != nil ||
		!d.checkedBatch.Expires.Equal(d.ExpiresAt) || contract.CheckAllocation(d.checkedBatch, portfolio, request.RequestedSymbols) != nil {
		return errInvalidResult
	}
	if len(d.Reports) != len(request.RequestedSymbols) {
		return errInvalidResult
	}
	for _, symbol := range request.RequestedSymbols {
		if _, exists := d.Reports[symbol]; !exists {
			return errInvalidResult
		}
	}
	return nil
}

// ValidateArchivedResearchResult reuses admission checks at the durable receipt
// time. Freshness policy was applied on admission and may have changed since.
func ValidateArchivedResearchResult(job jobs.Job, result jobs.Result) error {
	if result.JobID != job.ID || job.Status != "completed" || job.CreatedAt.IsZero() ||
		result.CompletedAt.Before(job.CreatedAt) || result.Attempt < 1 || result.Attempt > 3 {
		return errInvalidResult
	}
	var fields map[string]json.RawMessage
	if contract.Decode(result.Payload, nil, &fields) != nil ||
		!hasExactKeys(fields, "result_schema_version", "batch", "reports", "metadata") {
		return errInvalidResult
	}
	document, err := parseResultDocument(fields)
	if err != nil {
		return err
	}
	if metadata, ok := document.Metadata.(workerResultMetadata); ok && metadata.Provenance.Attempt != result.Attempt {
		return errInvalidResult
	}
	return document.validate(job, result.CompletedAt, time.Duration(1<<63-1))
}

func writeResult(w http.ResponseWriter, status int, result jobs.Result) {
	writeJSON(w, status, struct {
		JobID       string          `json:"job_id"`
		Status      string          `json:"status"`
		Attempt     int             `json:"attempt"`
		CompletedAt time.Time       `json:"completed_at"`
		Result      json.RawMessage `json:"result"`
	}{result.JobID, "completed", result.Attempt, result.CompletedAt.UTC(), result.Payload})
}
