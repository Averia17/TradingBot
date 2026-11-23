package paper_workflow

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/paper_risk"
)

var errOwnerPolicy = errors.New("paper snapshot policy differs from the owner policy")

type DispatchResearch interface {
	boundResearchStore
	Now(context.Context) (time.Time, error)
	FinishPaperDispatch(context.Context, jobs.PaperDispatchTask, string) error
}

// Dispatch runs a scheduled application and acknowledges it only after a
// terminal result. A rotated policy can recover a durable application.
func Dispatch(ctx context.Context, research DispatchResearch, paper FollowStore, task jobs.PaperDispatchTask,
	policy paper_risk.Policy, quotes QuoteSource, verifier func(Request) BoundQuoteVerifier) (FollowResult, error) {
	var req Request
	if sha256.Sum256(task.Request) != task.RequestSHA256 || contract.Decode(task.Request, contract.PaperApplication, &req) != nil ||
		req.JobID != task.JobID || req.SnapshotContext.AccountID != task.AccountID || !req.validFollow() {
		return FollowResult{}, errors.New("invalid persisted paper dispatch binding")
	}
	now, err := research.Now(ctx)
	if err != nil {
		return FollowResult{}, err
	}
	follower, body, err := newApplicationFollower(paper, req, now)
	if err != nil {
		return FollowResult{}, err
	}
	follower.recoveryOnly = !reflect.DeepEqual(policy, req.SnapshotContext.Policy)
	if follower.recoveryOnly && follower.input.RequestSHA256 != task.RequestSHA256 {
		return FollowResult{}, accounts.ErrConflict
	}
	var verify BoundQuoteVerifier
	if req.Identity != nil {
		durable, err := research.GetPaperDispatch(ctx, task.JobID)
		if err != nil {
			return FollowResult{}, err
		}
		if !follower.matchesDispatch(durable, body) || task.RequestSHA256 != durable.RequestSHA256 {
			return FollowResult{}, accounts.ErrConflict
		}
		if verifier != nil {
			verify = verifier(req)
		}
	}
	result, err := follower.run(ctx, research, quotes, verify, now)
	if err != nil || result.Pending() {
		return result, err
	}
	if err := research.FinishPaperDispatch(ctx, task, result.Status); err != nil {
		return FollowResult{}, err
	}
	return result, nil
}
