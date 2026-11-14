package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"tradingbot/internal/contract"
	"tradingbot/internal/jobs"
)

const (
	resultSchemaVersion = "0.1"
	syntheticSourceMode = "synthetic_fixture"
)

var (
	errInvalidResult    = errors.New("invalid research result")
	errInvalidStoredJob = errors.New("invalid immutable research job")
)

type resultMetadata struct {
	SourceMode    string `json:"source_mode"`
	LLMCalls      int    `json:"llm_calls"`
	AlphaMeasured bool   `json:"alpha_measured"`
}

type resultDocument struct {
	SchemaVersion string                       `json:"result_schema_version"`
	Batch         json.RawMessage              `json:"batch"`
	Reports       map[string]map[string]string `json:"reports"`
	Metadata      resultMetadata               `json:"metadata"`
	ExpiresAt     time.Time                    `json:"-"`
}

func (h jobHTTP) complete(w http.ResponseWriter, r *http.Request) {
	jobID := strings.ToLower(r.PathValue("id"))
	if !jobUUID.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_job_id")
		return
	}
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
	result, created, err := h.store.Complete(ctx, jobs.CompleteJob{
		JobID: jobID, WorkerID: workerID, Token: strings.ToLower(token), Payload: payload, ExpiresAt: document.ExpiresAt,
	}, func(job jobs.Job, dbNow time.Time) error {
		return document.validate(job, dbNow, h.maxResearchAge)
	})
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "job_not_found")
	case errors.Is(err, jobs.ErrLeaseConflict):
		writeError(w, http.StatusConflict, "lease_conflict")
	case errors.Is(err, jobs.ErrConflict):
		writeError(w, http.StatusConflict, "result_conflict")
	case errors.Is(err, errInvalidResult), errors.Is(err, jobs.ErrResultExpired):
		writeError(w, http.StatusUnprocessableEntity, "invalid_result")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		w.Header().Set("Location", "/v1/research/jobs/"+result.JobID+"/result")
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeResult(w, status, result)
	}
}

func (h jobHTTP) getResult(w http.ResponseWriter, r *http.Request) {
	jobID := strings.ToLower(r.PathValue("id"))
	if !jobUUID.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_job_id")
		return
	}
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
	var metadata map[string]json.RawMessage
	if json.Unmarshal(fields["result_schema_version"], &document.SchemaVersion) != nil || document.SchemaVersion != resultSchemaVersion ||
		json.Unmarshal(fields["metadata"], &metadata) != nil || !hasExactKeys(metadata, "source_mode", "llm_calls", "alpha_measured") ||
		json.Unmarshal(metadata["source_mode"], &document.Metadata.SourceMode) != nil || document.Metadata.SourceMode != syntheticSourceMode ||
		string(bytes.TrimSpace(metadata["llm_calls"])) != "0" || string(bytes.TrimSpace(metadata["alpha_measured"])) != "false" {
		return resultDocument{}, errInvalidResult
	}
	if json.Unmarshal(fields["reports"], &document.Reports) != nil || len(document.Reports) == 0 || len(document.Reports) > 100 {
		return resultDocument{}, errInvalidResult
	}
	for _, reports := range document.Reports {
		if strings.TrimSpace(reports["final_trade_decision"]) == "" {
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
	var err error
	document.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiry)
	if err != nil || document.ExpiresAt.IsZero() {
		return resultDocument{}, errInvalidResult
	}
	document.Batch, err = json.Marshal(batch)
	return document, err
}

func (d resultDocument) validate(job jobs.Job, dbNow time.Time, maxAge time.Duration) error {
	var request jobRequest
	if err := contract.Decode(job.Request, nil, &request); err != nil || request.ID != job.ID || request.Version != jobSchemaVersion ||
		contract.CheckSymbols(request.RequestedSymbols) != nil {
		return errInvalidStoredJob
	}
	portfolio, err := contract.ParsePortfolio(request.Portfolio)
	if err != nil {
		return errInvalidStoredJob
	}
	batch, err := contract.ParseBatch(d.Batch, dbNow, maxAge)
	if err != nil || !batch.Expires.Equal(d.ExpiresAt) || contract.CheckAllocation(batch, portfolio, request.RequestedSymbols) != nil {
		return errInvalidResult
	}
	if len(d.Reports) != len(request.RequestedSymbols) {
		return errInvalidResult
	}
	count := 0
	for _, symbol := range request.RequestedSymbols {
		reports, exists := d.Reports[symbol]
		if !exists {
			return errInvalidResult
		}
		for field, text := range reports {
			digest := sha256.Sum256([]byte(text))
			if batch.Evidence[symbol+":"+field] != hex.EncodeToString(digest[:]) {
				return errInvalidResult
			}
			count++
		}
	}
	if count != len(batch.Evidence) {
		return errInvalidResult
	}
	return nil
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
