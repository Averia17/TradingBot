package paper_workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_risk"
)

var ErrQuoteMissing = errors.New("no pinned local paper quote is available")

type FollowStore interface {
	PaperStore
	GetApplication(context.Context, string) (ledger.ApplicationRecord, error)
	ApplicationAfterReservations(context.Context, string, string) (ledger.ApplicationRecord, error)
	ReserveApplication(context.Context, ledger.ReservationRequest, ledger.ApplicationInput) (ledger.Order, bool, error)
	RecordApplication(context.Context, ledger.ApplicationInput, string, []byte, time.Time) (ledger.ApplicationRecord, error)
}

type QuoteSource func(context.Context, string) ([]byte, error)

type BoundQuoteVerifier func(ctx context.Context, raw []byte, completedAt, now, deadline time.Time) error

type boundResearchStore interface {
	ResearchStore
	GetPaperDispatch(context.Context, string) (jobs.PaperDispatchTask, error)
}

type FollowResult struct {
	Version     string  `json:"paper_follow_schema_version"`
	JobID       string  `json:"job_id"`
	Status      string  `json:"status"`
	Application *Result `json:"application,omitempty"`
}

func (r FollowResult) Pending() bool {
	return r.Status == "awaiting_research" || r.Status == "awaiting_quote" || r.Status == "quote_expired" || r.Status == "awaiting_recovery"
}

// Follow applies an accepted result when its inputs are available. The ledger
// fixes the actionable quote atomically with the reservation, so recovery
// always resumes the same order even when the fixture file or TTL changed.
func Follow(ctx context.Context, research ResearchStore, paper FollowStore, req Request, quotes QuoteSource, now time.Time) (FollowResult, error) {
	if req.Version != Version {
		return FollowResult{}, ErrInvalid
	}
	follower, _, err := newApplicationFollower(paper, req, now)
	if err != nil {
		return FollowResult{}, err
	}
	return follower.run(ctx, research, quotes, nil, now)
}

// FollowBound applies only an immutable owner-enrolled dispatch. For a new
// order it verifies local tape membership; a reserved order reuses its bytes.
func FollowBound(ctx context.Context, research boundResearchStore, paper FollowStore, req Request,
	quotes QuoteSource, verify BoundQuoteVerifier, now time.Time) (FollowResult, error) {
	if req.Version != IdentityVersion || !req.validFollow() {
		return FollowResult{}, ErrInvalid
	}
	task, err := research.GetPaperDispatch(ctx, req.JobID)
	if err != nil {
		return FollowResult{}, err
	}
	follower, body, err := newApplicationFollower(paper, req, now)
	if err != nil {
		return FollowResult{}, err
	}
	if !follower.matchesDispatch(task, body) {
		return FollowResult{}, ledger.ErrConflict
	}
	return follower.run(ctx, research, quotes, verify, now)
}

type applicationFollower struct {
	request      Request
	paper        FollowStore
	input        ledger.ApplicationInput
	recoveryOnly bool
}

func (f applicationFollower) matchesDispatch(task jobs.PaperDispatchTask, body []byte) bool {
	return task.AccountID == f.input.AccountID && task.JobID == f.input.JobID &&
		bytes.Equal(task.Request, body) && task.RequestSHA256 == f.input.RequestSHA256
}

func newApplicationFollower(paper FollowStore, req Request, now time.Time) (applicationFollower, []byte, error) {
	if !req.validFollow() || now.IsZero() {
		return applicationFollower{}, nil, ErrInvalid
	}
	if err := req.SnapshotContext.Policy.Validate(); err != nil {
		return applicationFollower{}, nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return applicationFollower{}, nil, fmt.Errorf("encode paper application request: %w", err)
	}
	input := ledger.ApplicationInput{
		JobID: req.JobID, AccountID: req.SnapshotContext.AccountID, SnapshotVersion: req.SnapshotContext.SnapshotVersion,
		RequestSHA256: sha256.Sum256(body),
	}
	return applicationFollower{request: req, paper: paper, input: input}, body, nil
}

func (f applicationFollower) status(value string) FollowResult {
	return FollowResult{Version: f.request.Version, JobID: f.request.JobID, Status: value}
}

func (f applicationFollower) run(ctx context.Context, research ResearchStore, quotes QuoteSource,
	verify BoundQuoteVerifier, now time.Time) (FollowResult, error) {
	pinned, err := f.paper.GetApplication(ctx, f.request.JobID)
	if err != nil && !errors.Is(err, ledger.ErrNotFound) {
		return FollowResult{}, err
	}
	resume := err == nil
	if !resume && f.recoveryOnly {
		return FollowResult{}, errOwnerPolicy
	}
	if resume {
		if !sameApplication(pinned, f.input) {
			return FollowResult{}, ledger.ErrConflict
		}
		if len(pinned.Result) != 0 {
			return completedFollow(pinned, f.request.Version)
		}
		f.input.Quote = pinned.Quote
	}

	job, err := research.Get(ctx, f.request.JobID)
	if errors.Is(err, jobs.ErrNotFound) && !resume {
		return f.status("awaiting_research"), nil
	}
	if err != nil {
		return FollowResult{}, err
	}
	if job.ID != f.request.JobID || job.CreatedAt.IsZero() || (resume && job.Status != "completed") {
		return FollowResult{}, ErrInvalid
	}
	switch job.Status {
	case "queued", "leased":
		return f.status("awaiting_research"), nil
	case "failed", "expired":
		return f.status("research_" + job.Status), nil
	case "completed":
	default:
		return FollowResult{}, ErrInvalid
	}
	receipt, err := research.GetResult(ctx, f.request.JobID)
	if err != nil {
		return FollowResult{}, err
	}
	application, err := validateCompletedResearch(job, receipt, f.request, now)
	if err != nil {
		return FollowResult{}, err
	}
	f.input.ReserveBefore = application.deadline()
	if resume {
		return f.execute(ctx, application, now, true)
	}
	return f.admit(ctx, application, quotes, verify, now)
}

func (f applicationFollower) admit(ctx context.Context, application applicationPlan, quotes QuoteSource,
	verify BoundQuoteVerifier, now time.Time) (FollowResult, error) {
	if !application.batch.Expires.After(now) {
		return f.refusalAfterRecheck(ctx, "expired")
	}
	if f.request.SnapshotContext.CheckMarks(now) != nil {
		return f.refusalAfterRecheck(ctx, "snapshot_stale")
	}
	if f.request.Identity != nil && !f.input.ReserveBefore.After(now) {
		return f.refusalAfterRecheck(ctx, "expired")
	}
	if application.isHold() {
		return f.execute(ctx, application, now, false)
	}
	var quote []byte
	err := ErrQuoteMissing
	if quotes != nil {
		quote, err = quotes(ctx, f.request.JobID)
	}
	if errors.Is(err, ErrQuoteMissing) {
		_, err := readSnapshot(ctx, f.paper, f.request.SnapshotContext, application.portfolio, now)
		if errors.Is(err, ledger.ErrStale) {
			return f.refusalAfterRecheck(ctx, "snapshot_stale")
		}
		if err != nil {
			return FollowResult{}, err
		}
		return f.status("awaiting_quote"), nil
	}
	if err != nil {
		return FollowResult{}, err
	}
	if len(quote) == 0 {
		return FollowResult{}, ErrInvalid
	}
	if f.request.Identity != nil {
		if verify == nil {
			return FollowResult{}, fmt.Errorf("%w: no owner tape verifier configured", ErrInvalid)
		}
		if err := verify(ctx, quote, application.completedAt, now, f.input.ReserveBefore); err != nil {
			return FollowResult{}, fmt.Errorf("verify pinned owner quote tape: %w", err)
		}
	}
	f.input.Quote = quote
	return f.execute(ctx, application, now, false)
}

func (f applicationFollower) execute(ctx context.Context, application applicationPlan, now time.Time, resume bool) (FollowResult, error) {
	result, err := application.execute(ctx, followedOrderStore{FollowStore: f.paper, input: f.input}, f.input.Quote, now)
	if !resume {
		switch {
		case errors.Is(err, ledger.ErrExpired):
			return f.status("quote_expired"), nil
		case errors.Is(err, ledger.ErrStale):
			return f.refusalAfterRecheck(ctx, "snapshot_stale")
		}
	}
	if err != nil {
		return FollowResult{}, err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return FollowResult{}, fmt.Errorf("encode paper application receipt: %w", err)
	}
	var orderID string
	if result.Execution != nil {
		orderID = result.Execution.Order.OrderID
	}
	record, err := f.paper.RecordApplication(ctx, f.input, orderID, body, f.input.ReserveBefore)
	if errors.Is(err, ledger.ErrExpired) && orderID == "" {
		return f.refusalAfterRecheck(ctx, "expired")
	}
	if errors.Is(err, ledger.ErrStale) && orderID == "" {
		return f.refusalAfterRecheck(ctx, "snapshot_stale")
	}
	if err != nil {
		return FollowResult{}, err
	}
	return completedFollow(record, f.request.Version)
}

func capDeadlineAtMarkExpiry(snapshot SnapshotContext, deadline time.Time) time.Time {
	for _, mark := range snapshot.Marks {
		until := mark.At.Add(time.Duration(snapshot.Policy.MaxMarkAgeSeconds) * time.Second)
		if until.Before(deadline) {
			deadline = until
		}
	}
	return deadline
}

// Commit may race the initial lookup. Check again before acknowledging a
// terminal refusal so a concurrently admitted order remains recoverable.
func (f applicationFollower) refusalAfterRecheck(ctx context.Context, status string) (FollowResult, error) {
	record, err := f.paper.ApplicationAfterReservations(ctx, f.input.AccountID, f.input.JobID)
	if errors.Is(err, ledger.ErrNotFound) {
		return f.status(status), nil
	}
	if err != nil {
		return FollowResult{}, err
	}
	if !sameApplication(record, f.input) {
		return FollowResult{}, ledger.ErrConflict
	}
	if len(record.Result) == 0 {
		return f.status("awaiting_recovery"), nil
	}
	return completedFollow(record, f.request.Version)
}

func sameApplication(record ledger.ApplicationRecord, input ledger.ApplicationInput) bool {
	return record.AccountID == input.AccountID && record.SnapshotVersion == input.SnapshotVersion &&
		record.RequestSHA256 == input.RequestSHA256
}

type followedOrderStore struct {
	FollowStore
	input ledger.ApplicationInput
}

func (s followedOrderStore) Reserve(ctx context.Context, order ledger.ReservationRequest) (ledger.Order, bool, error) {
	return s.ReserveApplication(ctx, order, s.input)
}

func (s followedOrderStore) RiskActivity(ctx context.Context, accountID string, expectedVersion int64) (ledger.RiskActivity, error) {
	store, ok := s.FollowStore.(paper_risk.ActivityReader)
	if !ok {
		return ledger.RiskActivity{}, paper_risk.ErrSnapshot
	}
	return store.RiskActivity(ctx, accountID, expectedVersion)
}

func completedFollow(record ledger.ApplicationRecord, version string) (FollowResult, error) {
	var result Result
	if err := contract.Decode(record.Result, nil, &result); err != nil || result.Version != version || result.JobID != record.JobID {
		return FollowResult{}, errors.New("invalid stored paper application receipt")
	}
	switch result.Status {
	case "hold":
		if result.Execution != nil || record.OrderID != "" || len(record.Quote) != 0 {
			return FollowResult{}, errors.New("stored HOLD conflicts with its application binding")
		}
	case "applied":
		digest := sha256.Sum256(record.Quote)
		if result.Execution == nil || result.Execution.Order.OrderID != record.OrderID || result.Execution.Order.AccountID != record.AccountID ||
			result.Execution.Order.ResearchJobID != record.JobID || result.Execution.QuoteSHA256 != hex.EncodeToString(digest[:]) {
			return FollowResult{}, errors.New("stored result conflicts with its application binding")
		}
	default:
		return FollowResult{}, errors.New("unknown stored paper application status")
	}
	return FollowResult{Version: version, JobID: result.JobID, Status: result.Status, Application: &result}, nil
}
