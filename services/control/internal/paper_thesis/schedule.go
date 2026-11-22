package paper_thesis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_risk"
	"tradingbot/control/internal/paper_workflow"
	"tradingbot/control/internal/research_scheduler"
)

var (
	scheduleUUID          = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	jobPolicyID           = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	reviewDigest          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errReviewWindowClosed = errors.New("frozen review window closed before admission")
)

type ScheduleRequest struct {
	Version        string                              `json:"review_schedule_schema_version"`
	OpeningOrderID string                              `json:"opening_order_id"`
	Context        paper_workflow.SnapshotContext      `json:"snapshot_context"`
	Portfolio      *contract.Portfolio                 `json:"portfolio,omitempty"`
	Manifest       research_scheduler.ArchiveReference `json:"data_manifest"`
	Budget         int                                 `json:"max_llm_calls"`
	Event          *research_scheduler.EventReference  `json:"event,omitempty"`
	Identity       *ReviewIdentity                     `json:"identity_binding,omitempty"`
}

type ReviewIdentity struct {
	paper_workflow.OwnerIdentity
	ProjectedSnapshotSHA256 string `json:"projected_snapshot_sha256"`
}

type ScheduleSnapshot struct {
	Context   paper_workflow.SnapshotContext `json:"snapshot_context"`
	Portfolio contract.Portfolio             `json:"portfolio"`
}

type ScheduleReceipt struct {
	Version   string            `json:"review_schedule_schema_version"`
	Status    string            `json:"status"`
	Created   bool              `json:"created"`
	Trigger   *Trigger          `json:"trigger,omitempty"`
	JobID     string            `json:"job_id,omitempty"`
	CreatedAt *time.Time        `json:"created_at,omitempty"`
	Snapshot  *ScheduleSnapshot `json:"snapshot,omitempty"`
}

type ScheduleStore interface {
	ResearchSource
	Create(context.Context, jobs.CreateJob, func() error) (jobs.Job, bool, error)
	CreateBoundPaperJob(context.Context, jobs.BoundPaperJob, func() error) (jobs.Job, bool, error)
	GetPaperDispatch(context.Context, string) (jobs.PaperDispatchTask, error)
}

// The field order matches Control's canonical job request.
type scheduledJob struct {
	Version          string                                   `json:"job_schema_version"`
	ID               string                                   `json:"job_id"`
	Portfolio        json.RawMessage                          `json:"portfolio"`
	RequestedSymbols []string                                 `json:"requested_symbols"`
	Manifest         research_scheduler.ArchiveReference      `json:"data_manifest"`
	Policy           string                                   `json:"policy_version"`
	Budget           int                                      `json:"max_llm_calls"`
	IdentityRegistry *paper_workflow.ArchiveReference         `json:"identity_registry,omitempty"`
	SecurityBindings []paper_workflow.ResearchSecurityBinding `json:"security_bindings,omitempty"`
	AnalysisMode     string                                   `json:"analysis_mode,omitempty"`
}

// ScheduleReview queues one due opening thesis review. The trigger is the
// job identity. A schedule with a frozen portfolio replays only the exact
// payload, while an automatic schedule reuses its first accepted snapshot.
func ScheduleReview(ctx context.Context, research ScheduleStore, source LedgerSource, req ScheduleRequest, archiveRoot string, now time.Time, maxAge time.Duration) (ScheduleReceipt, error) {
	return ScheduleReviewWithRegistry(ctx, research, source, req, archiveRoot, now, maxAge, "")
}

// ScheduleReviewWithRegistry enrolls a stable review only when the owner
// separately authorizes its frozen registry, snapshot and quote tape.
func ScheduleReviewWithRegistry(ctx context.Context, research ScheduleStore, source LedgerSource, req ScheduleRequest,
	archiveRoot string, now time.Time, maxAge time.Duration, ownerRegistrySHA256 string) (ScheduleReceipt, error) {
	return scheduleReview(ctx, research, source, req, archiveRoot, now, maxAge, ownerRegistrySHA256, false)
}

// ScheduleReviewWindow allows an explicitly newer frozen review window to
// report a prior accepted occurrence without changing its durable approval.
func ScheduleReviewWindow(ctx context.Context, research ScheduleStore, source LedgerSource, req ScheduleRequest,
	archiveRoot string, now time.Time, maxAge time.Duration, ownerRegistrySHA256 string) (ScheduleReceipt, error) {
	return scheduleReview(ctx, research, source, req, archiveRoot, now, maxAge, ownerRegistrySHA256, true)
}

func scheduleReview(ctx context.Context, research ScheduleStore, source LedgerSource, req ScheduleRequest,
	archiveRoot string, now time.Time, maxAge time.Duration, ownerRegistrySHA256 string, rollForward bool) (ScheduleReceipt, error) {
	if !req.valid() || now.IsZero() || maxAge <= 0 || archiveRoot == "" {
		return ScheduleReceipt{}, ErrInvalid
	}
	stable := req.Version == paper_workflow.IdentityVersion
	if rollForward && !stable {
		return ScheduleReceipt{}, fmt.Errorf("%w: roll-forward requires a stable paper review", ErrInvalid)
	}
	if stable && (!reviewDigest.MatchString(ownerRegistrySHA256) || req.Identity.Registry.SHA256 != ownerRegistrySHA256) {
		return ScheduleReceipt{}, fmt.Errorf("%w: stable review requires its independent owner registry pin", ErrInvalid)
	}
	started := time.Now()
	cutoff := now.UTC()
	dynamic := req.Portfolio == nil
	if !dynamic {
		cutoff = req.Portfolio.AsOf.UTC()
		if cutoff.After(now) {
			return ScheduleReceipt{}, fmt.Errorf("%w: future frozen review cutoff", ErrInvalid)
		}
	}
	view, err := projectOpening(ctx, research, source, req.Context.AccountID, req.OpeningOrderID, cutoff)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	if stable != (view.OpeningIdentity != nil) {
		return ScheduleReceipt{}, fmt.Errorf("%w: review schedule version differs from the opening identity", ErrInvalid)
	}
	trigger, event, err := req.trigger(view, archiveRoot)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	if trigger == nil {
		return ScheduleReceipt{Version: req.Version, Status: "not_due"}, nil
	}
	if !req.allows(view) {
		return ScheduleReceipt{}, fmt.Errorf("%w: opening instrument is not eligible for review", ErrInvalid)
	}
	if dynamic {
		prior, err := research.Get(ctx, trigger.ID)
		if !errors.Is(err, jobs.ErrNotFound) {
			if err != nil {
				return ScheduleReceipt{}, err
			}
			return replayReview(ctx, research, source, req, archiveRoot, prior, trigger.ID)
		}
	}

	config := req.Context
	var portfolio contract.Portfolio
	if dynamic {
		account, _, err := currentAccount(ctx, source, view)
		if err != nil {
			return ScheduleReceipt{}, err
		}
		config.SnapshotVersion = view.AccountVersion
		portfolio, err = paper_workflow.Snapshot(account, config, cutoff)
		if err != nil {
			return ScheduleReceipt{}, err
		}
	} else {
		portfolio = *req.Portfolio
		portfolio.AsOf = cutoff
		if err := config.CheckPortfolio(portfolio); err != nil {
			return ScheduleReceipt{}, err
		}
	}
	snapshot, err := json.Marshal(portfolio)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	wire := scheduledJob{
		Version: "0.1", ID: trigger.ID, Portfolio: snapshot, RequestedSymbols: []string{view.Symbol},
		Manifest: req.Manifest, Policy: config.Policy.Version, Budget: req.Budget,
	}
	dispatchRequest := paper_workflow.Request{Version: req.Version, JobID: trigger.ID, SnapshotContext: config}
	if stable {
		if err := req.enrollStable(archiveRoot, view, portfolio, event, &wire, &dispatchRequest); err != nil {
			return ScheduleReceipt{}, err
		}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	dispatch, err := json.Marshal(dispatchRequest)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	input := jobs.CreateJob{
		ID: trigger.ID, Request: body, SnapshotID: portfolio.ID, Snapshot: snapshot,
		PaperDispatch: &jobs.PaperDispatchInput{AccountID: config.AccountID, Request: dispatch},
	}
	admit := func() error {
		admissionAt := now.Add(time.Since(started))
		if admissionAt.Sub(cutoff) > maxAge {
			if rollForward {
				return errReviewWindowClosed
			}
			return fmt.Errorf("%w: stale research review snapshot", ledger.ErrStale)
		}
		if dispatchRequest.Identity != nil {
			for _, security := range dispatchRequest.Identity.Securities {
				if security.ExpiresAt != nil && !security.ExpiresAt.After(admissionAt) {
					return ledger.ErrStale
				}
			}
		}
		if err := config.CheckMarks(admissionAt); err != nil {
			return err
		}
		account, _, err := currentAccount(ctx, source, view)
		if err != nil {
			return err
		}
		actual, err := paper_workflow.Snapshot(account, config, cutoff)
		if err != nil {
			return err
		}
		if actual.ID != portfolio.ID {
			return ledger.ErrStale
		}
		return nil
	}
	var job jobs.Job
	var created bool
	if stable {
		job, created, err = research.CreateBoundPaperJob(ctx,
			jobs.BoundPaperJob{Job: input, OwnerRegistrySHA256: ownerRegistrySHA256}, admit)
	} else {
		job, created, err = research.Create(ctx, input, admit)
	}
	if dynamic && errors.Is(err, jobs.ErrConflict) {
		// Another scheduler can commit this trigger between our read and write.
		prior, lookupErr := research.Get(ctx, trigger.ID)
		if lookupErr != nil {
			return ScheduleReceipt{}, lookupErr
		}
		return replayReview(ctx, research, source, req, archiveRoot, prior, trigger.ID)
	}
	if stable && errors.Is(err, jobs.ErrConflict) {
		return priorStableReview(ctx, research, input, dispatchRequest, trigger, portfolio, rollForward)
	}
	if errors.Is(err, errReviewWindowClosed) {
		return ScheduleReceipt{Version: req.Version, Status: "missed"}, nil
	}
	if err != nil {
		return ScheduleReceipt{}, err
	}
	return acceptedReview(req.Version, job, created, trigger, config, portfolio), nil
}

func (r ScheduleRequest) valid() bool {
	mode := r.Version == Version && r.Identity == nil ||
		r.Version == paper_workflow.IdentityVersion && r.Identity != nil && r.Portfolio != nil &&
			reviewDigest.MatchString(r.Identity.ProjectedSnapshotSHA256)
	return mode && scheduleUUID.MatchString(r.OpeningOrderID) && scheduleUUID.MatchString(r.Context.AccountID) &&
		(r.Portfolio == nil && r.Context.SnapshotVersion == 0 || r.Portfolio != nil && r.Context.SnapshotVersion > 0) &&
		r.Budget >= 1 && r.Budget <= 64 && jobPolicyID.MatchString(r.Context.Policy.Version) && r.Context.Policy.Validate() == nil
}

func (r ScheduleRequest) allows(view View) bool {
	return slices.ContainsFunc(r.Context.Policy.Instruments, func(rule paper_risk.InstrumentRule) bool {
		return rule.Instrument == view.Instrument && rule.Symbol == view.Symbol
	})
}

func (r ScheduleRequest) trigger(view View, archiveRoot string) (*Trigger, *research_scheduler.VerifiedEvent, error) {
	if view.State != "open" || r.Event == nil && view.ClockReview == nil {
		return nil, nil, nil
	}
	event, err := research_scheduler.VerifyArchive(archiveRoot, r.Manifest, view.AsOf, []string{view.Symbol}, r.Event)
	if err != nil {
		return nil, nil, err
	}
	if r.Event == nil {
		return view.ClockReview, nil, nil
	}
	trigger, err := CatalystReview(view, event)
	if err != nil {
		return nil, nil, err
	}
	return &trigger, &event, nil
}

func (r ScheduleRequest) enrollStable(archiveRoot string, view View, portfolio contract.Portfolio,
	event *research_scheduler.VerifiedEvent, wire *scheduledJob, dispatch *paper_workflow.Request) error {
	opening := view.OpeningIdentity
	if opening.Symbol != view.Symbol || opening.Instrument != view.Instrument ||
		r.Manifest.Ref == r.Identity.Registry.Ref || r.Manifest.SHA256 == r.Identity.Registry.SHA256 ||
		r.Manifest.Ref == r.Identity.QuoteTape.Ref || r.Manifest.SHA256 == r.Identity.QuoteTape.SHA256 {
		return fmt.Errorf("%w: stable review cannot replace its opening security or share archive pins", ErrInvalid)
	}
	bindings := []paper_workflow.ResearchSecurityBinding{{Symbol: opening.Symbol, SecurityID: opening.SecurityID}}
	identity, err := paper_workflow.BindOwnerIdentity(archiveRoot, dispatch.SnapshotContext, r.Identity.OwnerIdentity, bindings,
		r.Identity.ProjectedSnapshotSHA256, portfolio.AsOf, event)
	if err != nil {
		return err
	}
	r.Portfolio = &portfolio
	proof, err := json.Marshal(struct {
		Schedule                 ScheduleRequest `json:"schedule"`
		OpeningOrderID           string          `json:"opening_order_id"`
		OpeningApplicationSHA256 string          `json:"opening_application_sha256"`
	}{r, view.OrderID, opening.ApplicationSHA256})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(proof)
	wire.Version, wire.IdentityRegistry, wire.SecurityBindings = paper_workflow.IdentityVersion, &identity.Registry, bindings
	if opening.AnalysisMode == jobs.ProviderAnalysisMode {
		if r.Budget != 1 {
			return fmt.Errorf("%w: prepared provider review requires one model call", ErrInvalid)
		}
		wire.Version, wire.AnalysisMode = "0.3", jobs.ProviderAnalysisMode
	}
	dispatch.Identity, dispatch.ReviewScheduleSHA256 = identity, hex.EncodeToString(digest[:])
	dispatch.ReviewOrigin = &paper_workflow.ReviewOrigin{OpeningOrderID: view.OrderID, OpeningApplicationSHA256: opening.ApplicationSHA256}
	return nil
}

func replayReview(ctx context.Context, research ResearchSource, source LedgerSource, req ScheduleRequest, archiveRoot string, job jobs.Job, triggerID string) (ScheduleReceipt, error) {
	var wire scheduledJob
	if contract.Decode(job.Request, nil, &wire) != nil || wire.ID != triggerID || job.ID != triggerID || wire.Version != "0.1" ||
		wire.Manifest != req.Manifest || wire.Policy != req.Context.Policy.Version || wire.Budget != req.Budget {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	portfolio, err := contract.ParsePortfolio(wire.Portfolio)
	// A concurrent scheduler can have persisted its snapshot after this request started.
	if err != nil || portfolio.AsOf.After(job.CreatedAt) {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	view, err := projectOpening(ctx, research, source, req.Context.AccountID, req.OpeningOrderID, portfolio.AsOf)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	config := req.Context
	config.SnapshotVersion = view.AccountVersion
	if portfolio.Cash != view.CashUSD || !slices.Equal(wire.RequestedSymbols, []string{view.Symbol}) || config.CheckPortfolio(portfolio) != nil {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	trigger, _, err := req.trigger(view, archiveRoot)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	if trigger == nil || trigger.ID != triggerID {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	return acceptedReview(req.Version, job, false, trigger, config, portfolio), nil
}

func acceptedReview(version string, job jobs.Job, created bool, trigger *Trigger, config paper_workflow.SnapshotContext, portfolio contract.Portfolio) ScheduleReceipt {
	return ScheduleReceipt{
		Version: version, Status: job.Status, Created: created, Trigger: trigger, JobID: job.ID, CreatedAt: &job.CreatedAt,
		Snapshot: &ScheduleSnapshot{Context: config, Portfolio: portfolio},
	}
}
