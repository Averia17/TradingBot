package jobs_http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tradingbot/trading-api/internal/jobs"
)

const (
	jobID     = "6dd88699-932d-4714-8e6f-20141cb5f976"
	accountID = "85603a11-a24d-4fdd-a388-5aa54dc23a2d"
	testToken = "paper-transport-test-token-at-least-32"
)

type jobsFixture struct {
	*jobs.Store
	job    jobs.Job
	result jobs.Result
	task   jobs.PaperDispatchTask
	now    time.Time
}

func (f jobsFixture) Now(context.Context) (time.Time, error) { return f.now, nil }

func (f jobsFixture) Get(_ context.Context, id string) (jobs.Job, error) {
	if id != f.job.ID {
		return jobs.Job{}, jobs.ErrNotFound
	}
	return f.job, nil
}

func (f jobsFixture) GetResult(_ context.Context, id string) (jobs.Result, error) {
	if id != f.result.JobID {
		return jobs.Result{}, jobs.ErrNotFound
	}
	return f.result, nil
}

func (f jobsFixture) ReadCompletedPaperResearch(_ context.Context, id string) (jobs.CompletedPaperResearch, error) {
	if id != f.job.ID {
		return jobs.CompletedPaperResearch{}, jobs.ErrNotFound
	}
	return jobs.CompletedPaperResearch{
		CompletedResearch: jobs.CompletedResearch{Job: f.job, Result: f.result}, Dispatch: &f.task,
	}, nil
}

func (f jobsFixture) GetPaperDispatch(_ context.Context, id string) (jobs.PaperDispatchTask, error) {
	if id != f.task.JobID {
		return jobs.PaperDispatchTask{}, jobs.ErrNotFound
	}
	return f.task, nil
}

func (f jobsFixture) ReadyPaperDispatch(_ context.Context, account, after string) ([]jobs.PaperDispatchTask, error) {
	if account != f.task.AccountID || after != "" {
		return nil, nil
	}
	return []jobs.PaperDispatchTask{f.task}, nil
}

func (f jobsFixture) FinishPaperDispatch(_ context.Context, task jobs.PaperDispatchTask, status string) error {
	if task.JobID != f.task.JobID || task.AccountID != f.task.AccountID || task.RequestSHA256 != f.task.RequestSHA256 || status != "hold" {
		return jobs.ErrConflict
	}
	return nil
}

func TestPrivateHTTPPreservesBytesAndRejectsUnauthenticatedRequests(t *testing.T) {
	raw := []byte("{ \"value\" : \"<abc>\" }\n")
	now := time.Unix(1800000000, 0).UTC()
	store := jobsFixture{
		now:    now,
		job:    jobs.Job{ID: jobID, Request: raw, Status: "completed", CreatedAt: now},
		result: jobs.Result{JobID: jobID, Payload: raw, Attempt: 1, CompletedAt: now},
		task:   jobs.PaperDispatchTask{PaperDispatchInput: jobs.PaperDispatchInput{AccountID: accountID, Request: raw}, JobID: jobID, RequestSHA256: sha256.Sum256(raw)},
	}
	handler, err := NewHandler(store, Config{Token: testToken, MaxResearchAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, PathPrefix+jobID, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := NewClient(server.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	job, err := client.Get(t.Context(), jobID)
	if err != nil || job.ID != jobID || !bytes.Equal(job.Request, raw) {
		t.Fatalf("job round trip = %+v, %v", job, err)
	}
	result, err := client.GetResult(t.Context(), jobID)
	if err != nil || result.JobID != jobID || !bytes.Equal(result.Payload, raw) {
		t.Fatalf("result round trip = %+v, %v", result, err)
	}
	completed, err := client.ReadCompletedPaperResearch(t.Context(), jobID)
	if err != nil || !bytes.Equal(completed.Job.Request, raw) || !bytes.Equal(completed.Result.Payload, raw) ||
		completed.Dispatch == nil || !bytes.Equal(completed.Dispatch.Request, raw) {
		t.Fatalf("completed paper round trip = %+v, %v", completed, err)
	}
	tasks, err := client.ReadyPaperDispatch(t.Context(), accountID, "")
	if err != nil || len(tasks) != 1 || !bytes.Equal(tasks[0].Request, raw) {
		t.Fatalf("dispatch page round trip = %+v, %v", tasks, err)
	}
	if wantDigest := sha256.Sum256(raw); tasks[0].RequestSHA256 != wantDigest {
		t.Fatalf("dispatch request SHA = %x, want %x", tasks[0].RequestSHA256, wantDigest)
	}
	if err := client.FinishPaperDispatch(t.Context(), tasks[0], "hold"); err != nil {
		t.Fatal(err)
	}
	if err := client.FinishPaperDispatch(t.Context(), tasks[0], "applied"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("conflicting acknowledgement = %v, want %v", err, jobs.ErrConflict)
	}
	if _, err := client.Get(t.Context(), accountID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("absent job = %v, want %v", err, jobs.ErrNotFound)
	}
	gotNow, err := client.Now(t.Context())
	if err != nil || !gotNow.Equal(now) {
		t.Fatalf("database time = %v, %v", gotNow, err)
	}
}
