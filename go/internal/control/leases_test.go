package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"tradingbot/internal/jobs"
)

const (
	claimPath  = jobsPath + "/claim"
	claimID    = "c3bac624-3a0e-40a2-8d4f-1e55d844692a"
	leaseToken = "cadc0a32-6c82-4dec-b742-13d47c05b34a"
	workerID   = "Lab-worker.1"
)

func TestClaimAndHeartbeatReturnActiveLease(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	lease := jobs.Lease{
		Job:      jobs.Job{ID: jobID, Status: "leased", Request: json.RawMessage(`{"fixture":true}`)},
		WorkerID: workerID, ClaimID: claimID, Token: leaseToken, Attempt: 1,
		ExpiresAt: now.Add(time.Minute), Deadline: now.Add(15 * time.Minute),
	}
	store := stubJobStore{
		claim: func(ctx context.Context, worker, claim string, maxAge time.Duration) (jobs.Lease, error) {
			if _, ok := ctx.Deadline(); !ok || worker != workerID || claim != claimID || maxAge != time.Hour {
				t.Fatal("claim requires deadline, case-sensitive worker and canonical claim ID")
			}
			return lease, nil
		},
		heartbeat: func(ctx context.Context, id, worker, token string) (jobs.Lease, error) {
			if _, ok := ctx.Deadline(); !ok || id != jobID || worker != workerID || token != leaseToken {
				t.Fatal("heartbeat requires deadline and canonical job/token IDs")
			}
			updated := lease
			updated.ExpiresAt = updated.ExpiresAt.Add(15 * time.Second)
			return updated, nil
		},
		get: func(context.Context, string) (jobs.Job, error) { return lease.Job, nil },
	}
	handler := NewHandler(time.Hour, time.Now, store)
	claim := callJobAPI(handler, http.MethodPost, claimPath, []byte(`{"worker_id":"`+workerID+`","claim_id":"`+strings.ToUpper(claimID)+`"}`))
	defer claim.Body.Close()
	assertResponse(t, claim, 200, `{"job_id":"`+jobID+`","status":"leased","worker_id":"`+workerID+`","claim_id":"`+claimID+`","lease_token":"`+leaseToken+`","attempt":1,"lease_expires_at":"2026-10-04T12:02:00Z","attempt_deadline":"2026-10-04T12:16:00Z","request":{"fixture":true}}`)
	if claim.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("lease response may not be cached")
	}
	heartbeat := callJobAPI(handler, http.MethodPost, jobsPath+"/"+strings.ToUpper(jobID)+"/heartbeat", []byte(`{"worker_id":"`+workerID+`","lease_token":"`+strings.ToUpper(leaseToken)+`"}`))
	defer heartbeat.Body.Close()
	assertResponse(t, heartbeat, 200, `{"job_id":"`+jobID+`","status":"leased","worker_id":"`+workerID+`","claim_id":"`+claimID+`","lease_token":"`+leaseToken+`","attempt":1,"lease_expires_at":"2026-10-04T12:02:15Z","attempt_deadline":"2026-10-04T12:16:00Z"}`)
	status := callJobAPI(handler, http.MethodGet, jobsPath+"/"+jobID, nil)
	defer status.Body.Close()
	statusBody, err := io.ReadAll(status.Body)
	if err != nil || status.StatusCode != 200 || strings.Contains(string(statusBody), "lease_token") {
		t.Fatalf("unexpected GET body %q, status %d, err %v", statusBody, status.StatusCode, err)
	}
}

func TestLeaseCapabilityRequiresConfiguredStorage(t *testing.T) {
	configured := NewHandler(time.Hour, time.Now, stubJobStore{})
	capabilities := callJobAPI(configured, http.MethodGet, "/v1/capabilities", nil)
	defer capabilities.Body.Close()
	var value struct {
		Enabled bool `json:"research_job_leases"`
	}
	if err := json.NewDecoder(capabilities.Body).Decode(&value); err != nil || capabilities.StatusCode != http.StatusOK || !value.Enabled {
		t.Fatalf("configured storage capability missing: status %d, value %+v, error %v", capabilities.StatusCode, value, err)
	}
	unconfigured := NewHandler(time.Hour, time.Now, nil)
	response := callJobAPI(unconfigured, http.MethodPost, claimPath, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want missing lease route in diagnostics mode", response.StatusCode)
	}
}

func TestLeaseErrorsDoNotExposeAuthority(t *testing.T) {
	for _, tt := range []struct {
		name   string
		claim  bool
		err    error
		status int
		body   string
	}{
		{"empty queue", true, jobs.ErrNoJob, 204, ""},
		{"claimed identity conflict", true, jobs.ErrLeaseConflict, 409, `{"error":"lease_conflict"}`},
		{"claim storage unavailable", true, errors.New(leaseToken), 503, `{"error":"storage_unavailable"}`},
		{"unknown job", false, jobs.ErrNotFound, 404, `{"error":"job_not_found"}`},
		{"stale lease", false, jobs.ErrLeaseConflict, 409, `{"error":"lease_conflict"}`},
		{"heartbeat storage unavailable", false, errors.New(leaseToken), 503, `{"error":"storage_unavailable"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := stubJobStore{
				claim:     func(context.Context, string, string, time.Duration) (jobs.Lease, error) { return jobs.Lease{}, tt.err },
				heartbeat: func(context.Context, string, string, string) (jobs.Lease, error) { return jobs.Lease{}, tt.err },
			}
			handler := NewHandler(time.Hour, time.Now, store)
			path, body := claimPath, `{"worker_id":"`+workerID+`","claim_id":"`+claimID+`"}`
			if !tt.claim {
				path, body = jobsPath+"/"+jobID+"/heartbeat", `{"worker_id":"`+workerID+`","lease_token":"`+leaseToken+`"}`
			}
			response := callJobAPI(handler, http.MethodPost, path, []byte(body))
			defer response.Body.Close()
			if tt.status == http.StatusNoContent {
				data, err := io.ReadAll(response.Body)
				if err != nil || len(data) != 0 || response.StatusCode != tt.status || response.Header.Get("Cache-Control") != "no-store" {
					t.Fatalf("unexpected 204 response: %d %q %v", response.StatusCode, data, err)
				}
				return
			}
			assertResponse(t, response, tt.status, tt.body)
		})
	}
}

func TestInvalidLeaseInputDoesNotReachStorage(t *testing.T) {
	handler := NewHandler(time.Hour, time.Now, stubJobStore{})
	for _, tt := range []struct {
		name string
		path string
		body string
	}{
		{"unknown claim field", claimPath, `{"Worker_id":"` + workerID + `","claim_id":"` + claimID + `"}`},
		{"duplicate worker key", claimPath, `{"worker_id":"a","worker_id":"b","claim_id":"` + claimID + `"}`},
		{"invalid claim ID", claimPath, `{"worker_id":"` + workerID + `","claim_id":"not-uuid"}`},
		{"invalid worker ID", claimPath, `{"worker_id":"bad/worker","claim_id":"` + claimID + `"}`},
		{"missing heartbeat token", jobsPath + "/" + jobID + "/heartbeat", `{"worker_id":"` + workerID + `"}`},
		{"invalid heartbeat token", jobsPath + "/" + jobID + "/heartbeat", `{"worker_id":"` + workerID + `","lease_token":null}`},
		{"invalid heartbeat job ID", jobsPath + "/bad-id/heartbeat", `{"worker_id":"` + workerID + `","lease_token":"` + leaseToken + `"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := callJobAPI(handler, http.MethodPost, tt.path, []byte(tt.body))
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", response.StatusCode)
			}
		})
	}
}
