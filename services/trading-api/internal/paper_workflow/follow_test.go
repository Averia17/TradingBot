package paper_workflow

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/jobs"
)

func TestDispatchAfterPolicyRotationOnlyAcknowledgesRecordedApplication(t *testing.T) {
	research, paper, req, _, now := approveFixture(t, true)
	body := marshalFixture(t, req)
	task := jobs.PaperDispatchTask{JobID: req.JobID, RequestSHA256: sha256.Sum256(body),
		PaperDispatchInput: jobs.PaperDispatchInput{AccountID: req.SnapshotContext.AccountID, Request: body}}
	store := &dispatchResearchStub{researchStub: research, now: now}
	book := &dispatchPaperStub{followLedger: followLedger{ledgerStub: paper}}
	policy := req.SnapshotContext.Policy
	policy.Version = "rotated"
	quotes := func(context.Context, string) ([]byte, error) {
		t.Fatal("rotation must never request a new quote")
		return nil, nil
	}
	if _, err := Dispatch(t.Context(), store, book, task, policy, quotes, nil); !errors.Is(err, errOwnerPolicy) || store.finished != "" {
		t.Fatalf("unadmitted application under rotated policy: %v, ack %q", err, store.finished)
	}
	book.application = accounts.ApplicationRecord{
		ApplicationInput: accounts.ApplicationInput{JobID: task.JobID, AccountID: task.AccountID,
			SnapshotVersion: req.SnapshotContext.SnapshotVersion, RequestSHA256: task.RequestSHA256},
		Result: marshalFixture(t, Result{Version: Version, JobID: task.JobID, Status: "hold"}),
	}
	result, err := Dispatch(t.Context(), store, book, task, policy, quotes, nil)
	if err != nil || result.Status != "hold" || store.finished != "hold" || paper.reserves != 0 {
		t.Fatalf("acknowledge recorded application: %+v, %v, ack %q", result, err, store.finished)
	}
}

type dispatchResearchStub struct {
	researchStub
	now      time.Time
	finished string
}

func (s *dispatchResearchStub) Now(context.Context) (time.Time, error) { return s.now, nil }

func (s *dispatchResearchStub) GetPaperDispatch(context.Context, string) (jobs.PaperDispatchTask, error) {
	panic("unexpected legacy dispatch lookup")
}

func (s *dispatchResearchStub) FinishPaperDispatch(_ context.Context, _ jobs.PaperDispatchTask, status string) error {
	s.finished = status
	return nil
}

type dispatchPaperStub struct {
	followLedger
	application accounts.ApplicationRecord
}

func (s *dispatchPaperStub) GetApplication(context.Context, string) (accounts.ApplicationRecord, error) {
	if s.application.JobID == "" {
		return accounts.ApplicationRecord{}, accounts.ErrNotFound
	}
	return s.application, nil
}

func TestFollowDoesNotTreatPendingResearchOrMissingQuoteAsHold(t *testing.T) {
	ctx := t.Context()
	research, paper, req, _, now := approveFixture(t, false)
	req.Intent = nil
	store := &followLedger{ledgerStub: paper}
	research.job.Status = "leased"
	unavailable := func(context.Context, string) ([]byte, error) {
		t.Fatal("quote read unexpectedly")
		return nil, nil
	}
	result, err := Follow(ctx, research, store, req, unavailable, now)
	if err != nil || result.Status != "awaiting_research" || result.Application != nil || paper.reserves != 0 {
		t.Fatalf("pending research receipt: %+v, %v", result, err)
	}
	research.job.Status = "completed"
	result, err = Follow(ctx, research, store, req, func(context.Context, string) ([]byte, error) {
		return nil, ErrQuoteMissing
	}, now)
	if err != nil || result.Status != "awaiting_quote" || result.Application != nil || paper.reserves != 0 {
		t.Fatalf("missing quote receipt: %+v, %v", result, err)
	}
	result, err = Follow(ctx, research, store, req, unavailable, now.Add(2*time.Hour))
	if err != nil || result.Status != "expired" || result.Application != nil || paper.reserves != 0 {
		t.Fatalf("expired decision receipt: %+v, %v", result, err)
	}
	offline := errors.New("storage unavailable")
	_, err = Follow(ctx, unavailableResearch{err: offline}, store, req, unavailable, now)
	if !errors.Is(err, offline) || paper.reserves != 0 {
		t.Fatalf("outage was not exposed: %v", err)
	}
}

type unavailableResearch struct {
	err error
}

func (r unavailableResearch) Get(context.Context, string) (jobs.Job, error) {
	return jobs.Job{}, r.err
}

func (r unavailableResearch) GetResult(context.Context, string) (jobs.Result, error) {
	return jobs.Result{}, r.err
}

type followLedger struct {
	*ledgerStub
}

func (s *followLedger) GetApplication(context.Context, string) (accounts.ApplicationRecord, error) {
	return accounts.ApplicationRecord{}, accounts.ErrNotFound
}

func (s *followLedger) ApplicationAfterReservations(context.Context, string, string) (accounts.ApplicationRecord, error) {
	return accounts.ApplicationRecord{}, accounts.ErrNotFound
}

func (s *followLedger) ReserveApplication(context.Context, accounts.ReservationRequest, accounts.ApplicationInput) (accounts.Order, bool, error) {
	panic("unexpected application reservation")
}

func (s *followLedger) RecordApplication(context.Context, accounts.ApplicationInput, string, []byte, time.Time) (accounts.ApplicationRecord, error) {
	panic("unexpected completed application")
}
