package paperthesis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/paperapply"
)

func priorStableReview(ctx context.Context, research ScheduleStore, proposed jobs.CreateJob, request paperapply.Request,
	trigger *Trigger, portfolio contract.Portfolio, rollForward bool) (ScheduleReceipt, error) {
	prior, err := research.Get(ctx, proposed.ID)
	if errors.Is(err, jobs.ErrNotFound) {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	if err != nil {
		return ScheduleReceipt{}, err
	}
	sameJob := bytes.Equal(prior.Request, proposed.Request)
	if !sameJob && !rollForward {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	task, err := research.GetPaperDispatch(ctx, proposed.ID)
	if errors.Is(err, jobs.ErrNotFound) {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	if err != nil {
		return ScheduleReceipt{}, err
	}
	var approved paperapply.Request
	if prior.ID != proposed.ID || prior.CreatedAt.IsZero() || task.JobID != prior.ID || task.AccountID != request.SnapshotContext.AccountID ||
		sha256.Sum256(task.Request) != task.RequestSHA256 || contract.Decode(task.Request, contract.PaperApplication, &approved) != nil ||
		approved.Version != paperapply.IdentityVersion || approved.JobID != prior.ID || approved.Identity == nil ||
		approved.SnapshotContext.AccountID != task.AccountID || !reviewDigest.MatchString(approved.ReviewScheduleSHA256) {
		return ScheduleReceipt{}, jobs.ErrConflict
	}

	// An older deployment can have accepted exactly this request before it
	// duplicated the opening proof alongside the review schedule digest.
	if sameJob && approved.ReviewOrigin == nil {
		request.ReviewOrigin = nil
		legacy, err := json.Marshal(request)
		if err != nil {
			return ScheduleReceipt{}, err
		}
		if bytes.Equal(task.Request, legacy) {
			return acceptedReview(paperapply.IdentityVersion, prior, false, trigger, request.SnapshotContext, portfolio), nil
		}
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	if !rollForward || approved.ReviewOrigin == nil || *approved.ReviewOrigin != *request.ReviewOrigin {
		return ScheduleReceipt{}, jobs.ErrConflict
	}

	var stored scheduledJob
	if contract.Decode(prior.Request, contract.ResearchIdentityJob, &stored) != nil || stored.ID != prior.ID ||
		stored.IdentityRegistry == nil || *stored.IdentityRegistry != approved.Identity.Registry ||
		stored.Policy != approved.SnapshotContext.Policy.Version || len(stored.RequestedSymbols) != 1 ||
		len(approved.Identity.Securities) != 1 || len(stored.SecurityBindings) != 1 {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	original, err := contract.ParsePortfolio(stored.Portfolio)
	if err != nil || !original.AsOf.Before(portfolio.AsOf) || original.AsOf.After(prior.CreatedAt) ||
		approved.SnapshotContext.CheckPortfolio(original) != nil {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	expected, recorded := request.Identity.Securities[0], approved.Identity.Securities[0]
	priorPolicy := ScheduleRequest{Context: approved.SnapshotContext}
	if recorded.Symbol != expected.Symbol || recorded.SecurityID != expected.SecurityID || recorded.Instrument != expected.Instrument ||
		stored.RequestedSymbols[0] != recorded.Symbol ||
		stored.SecurityBindings[0] != (paperapply.ResearchSecurityBinding{Symbol: recorded.Symbol, SecurityID: recorded.SecurityID}) ||
		!priorPolicy.allows(View{Symbol: recorded.Symbol, Instrument: recorded.Instrument}) ||
		(recorded.ExpiresAt != nil && !recorded.ExpiresAt.After(original.AsOf)) {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	return ScheduleReceipt{Version: paperapply.IdentityVersion, Status: "already_scheduled", JobID: prior.ID, CreatedAt: &prior.CreatedAt}, nil
}
