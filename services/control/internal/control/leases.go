package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
)

var workerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (h jobHTTP) claim(w http.ResponseWriter, r *http.Request) {
	workerID, claimID, ok := readLeaseIdentity(w, r, "claim_id")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	lease, err := h.store.Claim(ctx, workerID, claimID, h.maxResearchAge)
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

func (h jobHTTP) heartbeat(w http.ResponseWriter, r *http.Request) {
	jobID := strings.ToLower(r.PathValue("id"))
	if !jobUUID.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_job_id")
		return
	}
	workerID, token, ok := readLeaseIdentity(w, r, "lease_token")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	lease, err := h.store.Heartbeat(ctx, jobID, workerID, token)
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

func (h jobHTTP) authorizeCall(w http.ResponseWriter, r *http.Request) {
	jobID := strings.ToLower(r.PathValue("id"))
	if !jobUUID.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_job_id")
		return
	}
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var fields map[string]string
	if contract.Decode(body, nil, &fields) != nil || len(fields) != 3 || !workerIDPattern.MatchString(fields["worker_id"]) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	workerID := fields["worker_id"]
	token, callID := strings.ToLower(fields["lease_token"]), strings.ToLower(fields["call_id"])
	if !jobUUID.MatchString(token) || !jobUUID.MatchString(callID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	permit, created, err := h.store.AuthorizeCall(ctx, jobID, workerID, token, callID)
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
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, struct {
			JobID     string `json:"job_id"`
			CallID    string `json:"call_id"`
			Attempt   int    `json:"attempt"`
			Remaining int    `json:"remaining_llm_calls"`
		}{JobID: permit.JobID, CallID: permit.CallID, Attempt: permit.Attempt, Remaining: permit.Remaining})
	}
}

func readLeaseIdentity(w http.ResponseWriter, r *http.Request, key string) (workerID, id string, ok bool) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return "", "", false
	}
	var fields map[string]string
	if contract.Decode(body, nil, &fields) != nil || len(fields) != 2 || !workerIDPattern.MatchString(fields["worker_id"]) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return "", "", false
	}
	id = strings.ToLower(fields[key])
	if !jobUUID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return "", "", false
	}
	return fields["worker_id"], id, true
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
