package paperapply

import (
	"context"
	"errors"
	"testing"
	"time"

	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
)

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

func (s *followLedger) GetApplication(context.Context, string) (ledger.ApplicationRecord, error) {
	return ledger.ApplicationRecord{}, ledger.ErrNotFound
}

func (s *followLedger) ApplicationAfterReservations(context.Context, string, string) (ledger.ApplicationRecord, error) {
	return ledger.ApplicationRecord{}, ledger.ErrNotFound
}

func (s *followLedger) ReserveApplication(context.Context, ledger.ReservationRequest, ledger.ApplicationInput) (ledger.Order, bool, error) {
	panic("unexpected application reservation")
}

func (s *followLedger) RecordApplication(context.Context, ledger.ApplicationInput, string, []byte, time.Time) (ledger.ApplicationRecord, error) {
	panic("unexpected completed application")
}
