package jobs_http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
)

type creationFixture struct {
	jobsFixture
	mutex   sync.Mutex
	created *jobs.CreateJob
}

func (f *creationFixture) Get(ctx context.Context, id string) (jobs.Job, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.jobsFixture.Get(ctx, id)
}

func (f *creationFixture) Create(_ context.Context, input jobs.CreateJob, admit func() error) (jobs.Job, bool, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.created != nil {
		if f.created.ID != input.ID || f.created.SnapshotID != input.SnapshotID ||
			f.created.PaperDispatch.AccountID != input.PaperDispatch.AccountID ||
			!bytes.Equal(f.created.Request, input.Request) || !bytes.Equal(f.created.Snapshot, input.Snapshot) ||
			!bytes.Equal(f.created.PaperDispatch.Request, input.PaperDispatch.Request) {
			return jobs.Job{}, false, jobs.ErrConflict
		}
		return f.job, false, nil
	}
	if err := admit(); err != nil {
		return jobs.Job{}, false, err
	}
	f.created = &input
	f.job = jobs.Job{ID: input.ID, Status: "queued", CreatedAt: f.now, Request: input.Request}
	return f.job, true, nil
}

func TestPaperJobHTTPValidatesPublishedFixtureBeforeCreating(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/valid-internal-paper-job.json")
	if err != nil {
		t.Fatal(err)
	}
	var exact createRequest
	if err := contract.Decode(raw, contract.InternalJobCreate, &exact); err != nil {
		t.Fatal(err)
	}
	validTime := time.Date(2026, 11, 18, 10, 0, 15, 0, time.UTC)
	store := &creationFixture{jobsFixture: jobsFixture{now: validTime}}
	handler, err := NewHandler(store, Config{Token: testToken, MaxResearchAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	send := func(body []byte) int {
		req := httptest.NewRequest(http.MethodPost, PathPrefix, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out.Code
	}
	changedAccount := exact
	changedAccount.AccountID = accountID
	tampered, err := json.Marshal(changedAccount)
	if err != nil {
		t.Fatal(err)
	}
	if status := send(tampered); status != http.StatusUnprocessableEntity || store.created != nil {
		t.Fatalf("tampered account status = %d with store write = %t", status, store.created != nil)
	}

	store.now = validTime.Add(time.Minute)
	if status := send(raw); status != http.StatusUnprocessableEntity || store.created != nil {
		t.Fatalf("expired marks status = %d with store write = %t", status, store.created != nil)
	}
	store.now = validTime

	replayOnly := exact
	replayOnly.ReplayOnly = true
	missingReplay, err := json.Marshal(replayOnly)
	if err != nil {
		t.Fatal(err)
	}
	if status := send(missingReplay); status != http.StatusNotFound || store.created != nil {
		t.Fatalf("replay-only missing job status = %d with store write = %t", status, store.created != nil)
	}

	if status := send(raw); status != http.StatusCreated || store.created == nil {
		t.Fatalf("valid admission status = %d with store write = %t", status, store.created != nil)
	}
	if store.created.ID != exact.ID || store.created.SnapshotID != exact.SnapshotID ||
		store.created.PaperDispatch.AccountID != exact.AccountID ||
		!bytes.Equal(store.created.Request, exact.Request) || !bytes.Equal(store.created.Snapshot, exact.Snapshot) ||
		!bytes.Equal(store.created.PaperDispatch.Request, exact.Application) {
		t.Fatal("internal admission changed archived bytes or binding")
	}
}

func TestRemoteAdmissionDeadlineDoesNotBlockExactStoredReplay(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/valid-internal-paper-job.json")
	if err != nil {
		t.Fatal(err)
	}
	var exact createRequest
	if err := contract.Decode(raw, contract.InternalJobCreate, &exact); err != nil {
		t.Fatal(err)
	}
	validTime := time.Date(2026, 11, 18, 10, 0, 15, 0, time.UTC)
	store := &creationFixture{jobsFixture: jobsFixture{now: validTime}}
	handler, err := NewHandler(store, Config{Token: testToken, MaxResearchAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := NewClient(server.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	input := jobs.CreateJob{
		ID: exact.ID, Request: exact.Request, SnapshotID: exact.SnapshotID, Snapshot: exact.Snapshot,
		PaperDispatch: &jobs.PaperDispatchInput{AccountID: exact.AccountID, Request: exact.Application},
		AdmitThrough:  validTime.Add(-time.Second),
	}
	if _, created, err := client.Create(t.Context(), input, nil); !errors.Is(err, jobs.ErrAdmissionDeadline) || created {
		t.Fatalf("late new job created = %t with error %v", created, err)
	}
	input.AdmitThrough = validTime.Add(time.Minute)
	if _, created, err := client.Create(t.Context(), input, nil); err != nil || !created {
		t.Fatalf("new in-time job created = %t with error %v", created, err)
	}
	// The owner simulation clock remains fixed. A shorter supplied expiry
	// exercises the same replay path without modifying a concurrent fixture.
	input.AdmitThrough = validTime.Add(-time.Second)
	rechecked := false
	if job, created, err := client.Create(t.Context(), input, func() error {
		rechecked = true
		return jobs.ErrAdmissionDeadline
	}); err != nil || created || job.ID != input.ID || rechecked {
		t.Fatalf("late replay job = %s, created = %t, rechecked = %t, err = %v", job.ID, created, rechecked, err)
	}
}
