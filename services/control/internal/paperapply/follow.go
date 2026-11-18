package paperapply

import (
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
	"tradingbot/control/internal/paperrisk"
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
	if req.Version != Version || req.Intent != nil || !uuidPattern.MatchString(req.JobID) || now.IsZero() {
		return FollowResult{}, ErrInvalid
	}
	if err := req.SnapshotContext.Policy.Validate(); err != nil {
		return FollowResult{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return FollowResult{}, fmt.Errorf("encode paper application request: %w", err)
	}
	input := ledger.ApplicationInput{
		JobID: req.JobID, AccountID: req.SnapshotContext.AccountID, SnapshotVersion: req.SnapshotContext.SnapshotVersion,
		RequestSHA256: sha256.Sum256(body),
	}
	status := func(value string) FollowResult {
		return FollowResult{Version: Version, JobID: req.JobID, Status: value}
	}
	pinned, err := paper.GetApplication(ctx, req.JobID)
	if err != nil && !errors.Is(err, ledger.ErrNotFound) {
		return FollowResult{}, err
	}
	hasPin := err == nil
	if hasPin {
		if pinned.AccountID != input.AccountID || pinned.SnapshotVersion != input.SnapshotVersion || pinned.RequestSHA256 != input.RequestSHA256 {
			return FollowResult{}, ledger.ErrConflict
		}
		if len(pinned.Result) != 0 {
			return completedFollow(pinned)
		}
		input.Quote = pinned.Quote
	}

	job, err := research.Get(ctx, req.JobID)
	if errors.Is(err, jobs.ErrNotFound) && !hasPin {
		return status("awaiting_research"), nil
	}
	if err != nil {
		return FollowResult{}, err
	}
	if job.ID != req.JobID || job.CreatedAt.IsZero() || (hasPin && job.Status != "completed") {
		return FollowResult{}, ErrInvalid
	}
	switch job.Status {
	case "queued", "leased":
		return status("awaiting_research"), nil
	case "failed", "expired":
		return status("research_" + job.Status), nil
	case "completed":
	default:
		return FollowResult{}, ErrInvalid
	}
	receipt, err := research.GetResult(ctx, req.JobID)
	if err != nil {
		return FollowResult{}, err
	}
	portfolio, batch, err := loadResearch(job, receipt, req.SnapshotContext, now)
	if err != nil {
		return FollowResult{}, err
	}
	if !hasPin && !batch.Expires.After(now) {
		return concurrentApplication(ctx, paper, input, status("expired"))
	}
	if !hasPin && req.SnapshotContext.CheckMarks(now) != nil {
		return concurrentApplication(ctx, paper, input, status("snapshot_stale"))
	}
	if !hasPin && !allHold(batch.Intents) {
		err = ErrQuoteMissing
		if quotes != nil {
			input.Quote, err = quotes(ctx, req.JobID)
		}
		if errors.Is(err, ErrQuoteMissing) {
			if _, stale := readSnapshot(ctx, paper, req.SnapshotContext, portfolio, now); stale != nil {
				if errors.Is(stale, ledger.ErrStale) {
					return concurrentApplication(ctx, paper, input, status("snapshot_stale"))
				}
				return FollowResult{}, stale
			}
			return status("awaiting_quote"), nil
		}
		if err != nil {
			return FollowResult{}, err
		}
		if len(input.Quote) == 0 {
			return FollowResult{}, ErrInvalid
		}
	}
	input.ReserveBefore = applicationDeadline(req.SnapshotContext, batch.Expires)
	application, err := applyCompleted(ctx, followedOrderStore{FollowStore: paper, input: input},
		req, input.Quote, portfolio, batch, receipt.CompletedAt, now)
	if errors.Is(err, ledger.ErrExpired) && !hasPin {
		return status("quote_expired"), nil
	}
	if errors.Is(err, ledger.ErrStale) && !hasPin {
		return concurrentApplication(ctx, paper, input, status("snapshot_stale"))
	}
	if err != nil {
		return FollowResult{}, err
	}
	body, err = json.Marshal(application)
	if err != nil {
		return FollowResult{}, fmt.Errorf("encode paper application receipt: %w", err)
	}
	var orderID string
	if application.Execution != nil {
		orderID = application.Execution.Order.OrderID
	}
	record, err := paper.RecordApplication(ctx, input, orderID, body, input.ReserveBefore)
	if errors.Is(err, ledger.ErrExpired) && orderID == "" {
		return concurrentApplication(ctx, paper, input, status("expired"))
	}
	if errors.Is(err, ledger.ErrStale) && orderID == "" {
		return concurrentApplication(ctx, paper, input, status("snapshot_stale"))
	}
	if err != nil {
		return FollowResult{}, err
	}
	return completedFollow(record)
}

func applicationDeadline(snapshot SnapshotContext, deadline time.Time) time.Time {
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
func concurrentApplication(ctx context.Context, paper FollowStore, input ledger.ApplicationInput, refused FollowResult) (FollowResult, error) {
	record, err := paper.ApplicationAfterReservations(ctx, input.AccountID, input.JobID)
	if errors.Is(err, ledger.ErrNotFound) {
		return refused, nil
	}
	if err != nil {
		return FollowResult{}, err
	}
	if record.AccountID != input.AccountID || record.SnapshotVersion != input.SnapshotVersion || record.RequestSHA256 != input.RequestSHA256 {
		return FollowResult{}, ledger.ErrConflict
	}
	if len(record.Result) == 0 {
		refused.Status = "awaiting_recovery"
		return refused, nil
	}
	return completedFollow(record)
}

type followedOrderStore struct {
	FollowStore
	input ledger.ApplicationInput
}

func (s followedOrderStore) Reserve(ctx context.Context, order ledger.ReservationRequest) (ledger.Order, bool, error) {
	return s.ReserveApplication(ctx, order, s.input)
}

func (s followedOrderStore) RiskActivity(ctx context.Context, accountID string, expectedVersion int64) (ledger.RiskActivity, error) {
	store, ok := s.FollowStore.(paperrisk.ActivityReader)
	if !ok {
		return ledger.RiskActivity{}, paperrisk.ErrSnapshot
	}
	return store.RiskActivity(ctx, accountID, expectedVersion)
}

func completedFollow(record ledger.ApplicationRecord) (FollowResult, error) {
	var result Result
	if err := contract.Decode(record.Result, nil, &result); err != nil || result.Version != Version || result.JobID != record.JobID {
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
	return FollowResult{Version: Version, JobID: result.JobID, Status: result.Status, Application: &result}, nil
}
