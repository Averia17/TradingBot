package trading_api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
)

var workerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type providerClaimStore interface {
	ClaimPreparedProvider(context.Context, string, string, time.Duration, string) (jobs.Lease, error)
}

func (h jobHTTP) claim(w http.ResponseWriter, r *http.Request) {
	fields, ok := readWorkerFields(w, r)
	if !ok {
		return
	}
	registrySHA, scoped := fields["identity_registry_sha256"]
	mode, configuredMode := fields["analysis_mode"]
	fieldCount := 2
	if scoped {
		fieldCount++
	}
	if configuredMode {
		fieldCount++
	}
	workerID, claimID := fields["worker_id"], strings.ToLower(fields["claim_id"])
	if len(fields) != fieldCount || !jobUUID.MatchString(claimID) || scoped && !sha256Hex.MatchString(registrySHA) ||
		configuredMode && (!scoped || mode != jobs.ProviderAnalysisMode) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if scoped && registrySHA != h.identityRegistrySHA256 {
		writeError(w, http.StatusUnprocessableEntity, "identity_registry_mismatch")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	var lease jobs.Lease
	var err error
	if configuredMode {
		store, enabled := h.store.(providerClaimStore)
		if !enabled {
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
			return
		}
		lease, err = store.ClaimPreparedProvider(ctx, workerID, claimID, h.maxResearchAge, registrySHA)
	} else if scoped {
		store, enabled := h.store.(interface {
			ClaimWithIdentityRegistry(context.Context, string, string, time.Duration, string) (jobs.Lease, error)
		})
		if !enabled {
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
			return
		}
		lease, err = store.ClaimWithIdentityRegistry(ctx, workerID, claimID, h.maxResearchAge, registrySHA)
	} else {
		lease, err = h.store.Claim(ctx, workerID, claimID, h.maxResearchAge)
	}
	switch {
	case errors.Is(err, jobs.ErrNoJob):
		// No claim ID was bound. The worker can poll with the same ID.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, jobs.ErrLeaseConflict):
		writeError(w, http.StatusConflict, "lease_conflict")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		writeJSON(w, http.StatusOK, struct {
			leaseResponse
			Request json.RawMessage `json:"request"`
		}{leaseResponse: leasePayload(lease), Request: lease.Job.Request})
	}
}

func (h jobHTTP) heartbeat(w http.ResponseWriter, r *http.Request, jobID string) {
	fields, ok := readWorkerFields(w, r)
	if !ok {
		return
	}
	token := strings.ToLower(fields["lease_token"])
	if len(fields) != 2 || !jobUUID.MatchString(token) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	lease, err := h.store.Heartbeat(ctx, jobID, fields["worker_id"], token)
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "job_not_found")
	case errors.Is(err, jobs.ErrLeaseConflict):
		writeError(w, http.StatusConflict, "lease_conflict")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		writeJSON(w, http.StatusOK, leasePayload(lease))
	}
}

func (h jobHTTP) authorizeCall(w http.ResponseWriter, r *http.Request, jobID string) {
	fields, ok := readWorkerFields(w, r)
	if !ok {
		return
	}
	token, callID := strings.ToLower(fields["lease_token"]), strings.ToLower(fields["call_id"])
	if len(fields) != 3 || !jobUUID.MatchString(token) || !jobUUID.MatchString(callID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	permit, created, err := h.store.AuthorizeCall(ctx, jobID, fields["worker_id"], token, callID)
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "job_not_found")
	case errors.Is(err, jobs.ErrLeaseConflict):
		writeError(w, http.StatusConflict, "lease_conflict")
	case errors.Is(err, jobs.ErrConflict):
		writeError(w, http.StatusConflict, "call_conflict")
	case errors.Is(err, jobs.ErrBudgetExhausted):
		writeError(w, http.StatusUnprocessableEntity, "budget_exhausted")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		writeJSON(w, createStatus(created), struct {
			JobID     string `json:"job_id"`
			CallID    string `json:"call_id"`
			Attempt   int    `json:"attempt"`
			Remaining int    `json:"remaining_llm_calls"`
		}{JobID: permit.JobID, CallID: permit.CallID, Attempt: permit.Attempt, Remaining: permit.Remaining})
	}
}

func readWorkerFields(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return nil, false
	}
	var fields map[string]string
	if contract.Decode(body, nil, &fields) != nil || !workerIDPattern.MatchString(fields["worker_id"]) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return nil, false
	}
	return fields, true
}

type leaseResponse struct {
	JobID     string    `json:"job_id"`
	Status    string    `json:"status"`
	WorkerID  string    `json:"worker_id"`
	ClaimID   string    `json:"claim_id"`
	Token     string    `json:"lease_token"`
	Attempt   int       `json:"attempt"`
	ExpiresAt time.Time `json:"lease_expires_at"`
	Deadline  time.Time `json:"attempt_deadline"`
}

func leasePayload(lease jobs.Lease) leaseResponse {
	return leaseResponse{
		JobID: lease.Job.ID, Status: lease.Job.Status, WorkerID: lease.WorkerID,
		ClaimID: lease.ClaimID, Token: lease.Token, Attempt: lease.Attempt,
		ExpiresAt: lease.ExpiresAt.UTC(), Deadline: lease.Deadline.UTC(),
	}
}
