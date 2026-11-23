package trading_api

import (
	"context"
	"errors"
	"net/http"

	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
)

type researchCallUsageStore interface {
	RecordCallUsage(context.Context, jobs.RecordResearchCallUsage) (jobs.CallUsageReceipt, bool, error)
}

type callUsageHTTP struct {
	store researchCallUsageStore
}

func (h callUsageHTTP) record(w http.ResponseWriter, r *http.Request, jobID string) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var request struct {
		Version  string                   `json:"call_usage_schema_version"`
		WorkerID string                   `json:"worker_id"`
		Token    string                   `json:"lease_token"`
		Call     contract.ResearchLLMCall `json:"call"`
	}
	if contract.Decode(body, contract.ResearchCallUsageRequest, &request) != nil ||
		(request.Version == "0.1" && request.Call.ValidateOfflineCall() != nil) ||
		(request.Version == "0.2" && request.Call.ValidateProviderCall() != nil) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_call_usage")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	receipt, created, err := h.store.RecordCallUsage(ctx, jobs.RecordResearchCallUsage{
		JobID: jobID, WorkerID: request.WorkerID, Token: request.Token, Call: request.Call, Provider: request.Version == "0.2",
	})
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "job_not_found")
	case errors.Is(err, jobs.ErrCallPermitNotFound):
		writeError(w, http.StatusNotFound, "call_permit_not_found")
	case errors.Is(err, jobs.ErrLeaseConflict):
		writeError(w, http.StatusConflict, "lease_conflict")
	case errors.Is(err, jobs.ErrConflict):
		writeError(w, http.StatusConflict, "call_conflict")
	case errors.Is(err, jobs.ErrInvalidCallUsage):
		writeError(w, http.StatusUnprocessableEntity, "invalid_call_usage")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		writeJSON(w, createStatus(created), receipt)
	}
}
