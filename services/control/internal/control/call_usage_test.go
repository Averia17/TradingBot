package control

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/jobs"
)

type usageTestStore struct {
	stubJobStore
	record func(context.Context, jobs.RecordResearchCallUsage) (jobs.CallUsageReceipt, bool, error)
}

func (store usageTestStore) RecordCallUsage(ctx context.Context, input jobs.RecordResearchCallUsage) (jobs.CallUsageReceipt, bool, error) {
	return store.record(ctx, input)
}

func TestCallUsageTransport(t *testing.T) {
	payload, err := os.ReadFile("../../../../contracts/valid-research-call-usage.json")
	if err != nil {
		t.Fatal(err)
	}
	const callID = "00000000-0000-4000-8000-abcdef000013"
	const receipt = `{"job_id":"` + jobID + `","call_id":"` + callID + `","attempt":2,"recorded_at":"2026-10-04T12:01:06Z"}`
	for _, tt := range []struct {
		name    string
		body    string
		created bool
		err     error
		status  int
		want    string
	}{
		{"new", string(payload), true, nil, 201, receipt},
		{"replay", string(payload), false, nil, 200, receipt},
		{"missing permit", string(payload), false, jobs.ErrCallPermitNotFound, 404, `{"error":"call_permit_not_found"}`},
		{"expired", string(payload), false, jobs.ErrLeaseConflict, 409, `{"error":"lease_conflict"}`},
		{"conflict", string(payload), false, jobs.ErrConflict, 409, `{"error":"call_conflict"}`},
		{"storage", string(payload), false, errors.New(leaseToken), 503, `{"error":"storage_unavailable"}`},
		{"unfinished", strings.Replace(string(payload), `"failed"`, `"unfinished"`, 1), false, nil, 422, `{"error":"invalid_call_usage"}`},
		{"foreign model", strings.Replace(string(payload), `"scripted-offline-v1"`, `"provider-model"`, 1), false, nil, 422, `{"error":"invalid_call_usage"}`},
		{"exact counts", strings.Replace(string(payload), `"cache_read_tokens": 300`, `"cache_read_tokens": 900`, 1), false, nil, 422, `{"error":"invalid_call_usage"}`},
		{"duplicate", strings.Replace(string(payload), `"cache_read_tokens": 300`, `"cache_read_tokens": 300, "cache_read_tokens": 0`, 1), false, nil, 422, `{"error":"invalid_call_usage"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(time.Hour, time.Now, usageTestStore{record: func(ctx context.Context, input jobs.RecordResearchCallUsage) (jobs.CallUsageReceipt, bool, error) {
				if tt.status == 422 {
					t.Fatal("unvalidated call reached PostgreSQL")
				}
				if _, ok := ctx.Deadline(); !ok || input.JobID != jobID ||
					input.Token != leaseToken || input.WorkerID != "research-worker" ||
					input.Call.RunID != callID || input.Call.Status != "failed" {
					t.Fatalf("unexpected observation %+v", input)
				}
				return jobs.CallUsageReceipt{JobID: jobID, CallID: callID, Attempt: 2,
					RecordedAt: time.Date(2026, 10, 4, 12, 1, 6, 0, time.UTC)}, tt.created, tt.err
			}})
			response := callJobAPI(handler, http.MethodPost, jobsPath+"/"+jobID+"/record-call", []byte(tt.body))
			defer response.Body.Close()
			assertResponse(t, response, tt.status, tt.want)
			if response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("callback receipt may not be cached")
			}
		})
	}
}
