package paperthesis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperrisk"
	"tradingbot/control/internal/researchscheduler"
)

var (
	scheduleUUID = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	jobPolicyID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type ScheduleRequest struct {
	Version        string                             `json:"review_schedule_schema_version"`
	OpeningOrderID string                             `json:"opening_order_id"`
	Context        paperapply.SnapshotContext         `json:"snapshot_context"`
	Portfolio      *contract.Portfolio                `json:"portfolio,omitempty"`
	Manifest       researchscheduler.ArchiveReference `json:"data_manifest"`
	Budget         int                                `json:"max_llm_calls"`
	Event          *researchscheduler.EventReference  `json:"event,omitempty"`
}

type ScheduleSnapshot struct {
	Context   paperapply.SnapshotContext `json:"snapshot_context"`
	Portfolio contract.Portfolio         `json:"portfolio"`
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
}

// The field order matches Control's canonical job request.
type scheduledJob struct {
	Version          string                             `json:"job_schema_version"`
	ID               string                             `json:"job_id"`
	Portfolio        json.RawMessage                    `json:"portfolio"`
	RequestedSymbols []string                           `json:"requested_symbols"`
	Manifest         researchscheduler.ArchiveReference `json:"data_manifest"`
	Policy           string                             `json:"policy_version"`
	Budget           int                                `json:"max_llm_calls"`
}

// ScheduleReview queues one due opening thesis review. The trigger is the
// job identity. A schedule with a frozen portfolio replays only the exact
// payload, while an automatic schedule reuses its first accepted snapshot.
func ScheduleReview(ctx context.Context, research ScheduleStore, source LedgerSource, req ScheduleRequest, archiveRoot string, now time.Time, maxAge time.Duration) (ScheduleReceipt, error) {
	if !req.valid() || now.IsZero() || maxAge <= 0 || archiveRoot == "" {
		return ScheduleReceipt{}, ErrInvalid
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
	trigger, err := req.trigger(view, archiveRoot)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	if trigger == nil {
		return ScheduleReceipt{Version: Version, Status: "not_due"}, nil
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
		portfolio, err = paperapply.Snapshot(account, config, cutoff)
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
	body, err := json.Marshal(wire)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	dispatch, err := json.Marshal(paperapply.Request{Version: paperapply.Version, JobID: trigger.ID, SnapshotContext: config})
	if err != nil {
		return ScheduleReceipt{}, err
	}
	job, created, err := research.Create(ctx, jobs.CreateJob{
		ID: trigger.ID, Request: body, SnapshotID: portfolio.ID, Snapshot: snapshot,
		PaperDispatch: &jobs.PaperDispatchInput{AccountID: config.AccountID, Request: dispatch},
	}, func() error {
		admissionAt := now.Add(time.Since(started))
		if admissionAt.Sub(cutoff) > maxAge {
			return fmt.Errorf("%w: stale research review snapshot", ledger.ErrStale)
		}
		if err := config.CheckMarks(admissionAt); err != nil {
			return err
		}
		account, _, err := currentAccount(ctx, source, view)
		if err != nil {
			return err
		}
		actual, err := paperapply.Snapshot(account, config, cutoff)
		if err != nil {
			return err
		}
		if actual.ID != portfolio.ID {
			return ledger.ErrStale
		}
		return nil
	})
	if dynamic && errors.Is(err, jobs.ErrConflict) {
		// Another scheduler can commit this trigger between our read and write.
		prior, lookupErr := research.Get(ctx, trigger.ID)
		if lookupErr != nil {
			return ScheduleReceipt{}, lookupErr
		}
		return replayReview(ctx, research, source, req, archiveRoot, prior, trigger.ID)
	}
	if err != nil {
		return ScheduleReceipt{}, err
	}
	return acceptedReview(job, created, trigger, config, portfolio), nil
}

func (r ScheduleRequest) valid() bool {
	return r.Version == Version && scheduleUUID.MatchString(r.OpeningOrderID) && scheduleUUID.MatchString(r.Context.AccountID) &&
		(r.Portfolio == nil && r.Context.SnapshotVersion == 0 || r.Portfolio != nil && r.Context.SnapshotVersion > 0) &&
		r.Budget >= 1 && r.Budget <= 64 && jobPolicyID.MatchString(r.Context.Policy.Version) && r.Context.Policy.Validate() == nil
}

func (r ScheduleRequest) allows(view View) bool {
	return slices.ContainsFunc(r.Context.Policy.Instruments, func(rule paperrisk.InstrumentRule) bool {
		return rule.Instrument == view.Instrument && rule.Symbol == view.Symbol
	})
}

func (r ScheduleRequest) trigger(view View, archiveRoot string) (*Trigger, error) {
	if view.State != "open" || r.Event == nil && view.ClockReview == nil {
		return nil, nil
	}
	event, err := researchscheduler.VerifyArchive(archiveRoot, r.Manifest, view.AsOf, []string{view.Symbol}, r.Event)
	if err != nil {
		return nil, err
	}
	if r.Event == nil {
		return view.ClockReview, nil
	}
	trigger, err := CatalystReview(view, event)
	if err != nil {
		return nil, err
	}
	return &trigger, nil
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
	trigger, err := req.trigger(view, archiveRoot)
	if err != nil {
		return ScheduleReceipt{}, err
	}
	if trigger == nil || trigger.ID != triggerID {
		return ScheduleReceipt{}, jobs.ErrConflict
	}
	return acceptedReview(job, false, trigger, config, portfolio), nil
}

func acceptedReview(job jobs.Job, created bool, trigger *Trigger, config paperapply.SnapshotContext, portfolio contract.Portfolio) ScheduleReceipt {
	return ScheduleReceipt{
		Version: Version, Status: job.Status, Created: created, Trigger: trigger, JobID: job.ID, CreatedAt: &job.CreatedAt,
		Snapshot: &ScheduleSnapshot{Context: config, Portfolio: portfolio},
	}
}
