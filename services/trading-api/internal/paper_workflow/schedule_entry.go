package paper_workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/research_scheduler"
)

var (
	scheduleName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	entryDigest    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errEntryMissed = errors.New("paper entry occurrence missed its scheduling window")
)

type EntrySchedule struct {
	Version         string          `json:"entry_schedule_schema_version"`
	ID              string          `json:"schedule_id"`
	At              time.Time       `json:"scheduled_at"`
	MaxLateness     int             `json:"max_lateness_seconds"`
	Budget          int             `json:"max_job_llm_calls"`
	Context         SnapshotContext `json:"snapshot_context"`
	SelectionSHA256 string          `json:"selection_sha256"`
	Identity        *OwnerIdentity  `json:"identity_binding,omitempty"`
}

type EntryReceipt struct {
	Version   string     `json:"entry_schedule_schema_version"`
	Status    string     `json:"status"`
	Created   bool       `json:"created"`
	JobID     string     `json:"job_id,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

type screenedSelection struct {
	Version    string            `json:"selection_schema_version"`
	Source     string            `json:"source_mode"`
	Snapshot   string            `json:"snapshot_sha256"`
	StockStats string            `json:"stockstats_version"`
	Job        *storedJob        `json:"job"`
	Candidates []json.RawMessage `json:"candidates"`
}

type EntryJobStore interface {
	Get(context.Context, string) (jobs.Job, error)
	Create(context.Context, jobs.CreateJob, func() error) (jobs.Job, bool, error)
	CreateBoundPaperJob(context.Context, jobs.BoundPaperJob, func() error) (jobs.Job, bool, error)
	GetPaperDispatch(context.Context, string) (jobs.PaperDispatchTask, error)
}

type EntryLedger interface {
	GetAccount(context.Context, string) (accounts.Account, error)
	Events(context.Context, string, int64, int) ([]accounts.Event, error)
}

// ScheduleEntry queues the screened candidates and their owner-side application
// binding in one durable transaction. The occurrence, not the suggested job ID,
// determines the key. Retrying committed input does not re-admit the snapshot.
func ScheduleEntry(ctx context.Context, research EntryJobStore, paper EntryLedger, config EntrySchedule, rawSelection []byte, archiveRoot string, now time.Time, maxAge time.Duration) (EntryReceipt, error) {
	return ScheduleEntryWithRegistry(ctx, research, paper, config, rawSelection, archiveRoot, now, maxAge, "")
}

// ScheduleEntryWithRegistry requires an independent owner pin for stable entry
// and keeps the schedule API usable for unchanged legacy configurations.
func ScheduleEntryWithRegistry(ctx context.Context, research EntryJobStore, paper EntryLedger, config EntrySchedule,
	rawSelection []byte, archiveRoot string, now time.Time, maxAge time.Duration, ownerRegistrySHA256 string) (EntryReceipt, error) {
	status := EntryReceipt{Version: config.Version}
	started := time.Now()
	if !config.valid() || archiveRoot == "" || now.IsZero() || maxAge <= 0 {
		return EntryReceipt{}, ErrInvalid
	}
	stable := config.Version == IdentityVersion
	if stable && (!entryDigest.MatchString(ownerRegistrySHA256) || config.Identity.Registry.SHA256 != ownerRegistrySHA256) {
		return EntryReceipt{}, fmt.Errorf("%w: stable paper entry requires its independent owner registry pin", ErrInvalid)
	}
	config.At = config.At.UTC()
	if !entryDigest.MatchString(config.SelectionSHA256) || fmt.Sprintf("%x", sha256.Sum256(rawSelection)) != config.SelectionSHA256 {
		return EntryReceipt{}, fmt.Errorf("%w: screened selection does not match its pinned SHA-256", ErrInvalid)
	}
	var selection screenedSelection
	if contract.Decode(rawSelection, contract.PaperEntrySelection, &selection) != nil || selection.Version != config.Version {
		return EntryReceipt{}, fmt.Errorf("%w: invalid screened selection", ErrInvalid)
	}
	if config.At.After(now) {
		status.Status = "not_due"
		return status, nil
	}
	jobID := research_scheduler.PaperEntryOccurrenceID(config.Context.AccountID, config.ID, config.At)
	if selection.Job == nil {
		if _, err := research.Get(ctx, jobID); err == nil {
			return EntryReceipt{}, jobs.ErrConflict
		} else if !errors.Is(err, jobs.ErrNotFound) {
			return EntryReceipt{}, err
		}
		status.Status = "not_selected"
		return status, nil
	}
	job := *selection.Job
	if job.PolicyVersion != config.Context.Policy.Version || job.MaxLLMCalls > config.Budget ||
		!uuidPattern.MatchString(strings.ToLower(job.ID)) || contract.CheckSymbols(job.RequestedSymbols) != nil {
		return EntryReceipt{}, fmt.Errorf("%w: screened job exceeds its owner binding", ErrInvalid)
	}
	if !config.matchesJobIdentity(job, selection.Snapshot) {
		return EntryReceipt{}, fmt.Errorf("%w: screened job identity does not match its owner binding", ErrInvalid)
	}
	portfolio, err := contract.ParsePortfolio(job.Portfolio)
	if err != nil || portfolio.AsOf.After(config.At) || stable && !portfolio.AsOf.Equal(config.At) ||
		config.Context.CheckPortfolio(portfolio) != nil {
		return EntryReceipt{}, fmt.Errorf("%w: screened portfolio does not match the owner snapshot", ErrInvalid)
	}
	if err := config.checkRequestedMarks(job.RequestedSymbols, portfolio.AsOf); err != nil {
		return EntryReceipt{}, err
	}
	job.ID = jobID
	deadline := config.At.Add(time.Duration(config.MaxLateness) * time.Second)
	_, lookupErr := research.Get(ctx, job.ID)
	if errors.Is(lookupErr, jobs.ErrNotFound) {
		if stable && now.After(deadline) {
			status.Status = "missed"
			return status, nil
		}
		if !now.After(deadline) {
			if _, err := research_scheduler.VerifyArchive(archiveRoot, job.DataManifest, portfolio.AsOf, job.RequestedSymbols, nil); err != nil {
				return EntryReceipt{}, err
			}
		}
	} else if lookupErr != nil {
		return EntryReceipt{}, lookupErr
	}
	portfolio.AsOf = portfolio.AsOf.UTC()
	snapshot, err := json.Marshal(portfolio)
	if err != nil {
		return EntryReceipt{}, fmt.Errorf("encode screened portfolio: %w", err)
	}
	job.Portfolio = snapshot
	body, err := json.Marshal(job)
	if err != nil {
		return EntryReceipt{}, fmt.Errorf("encode screened research request: %w", err)
	}
	owner, err := json.Marshal(config)
	if err != nil {
		return EntryReceipt{}, fmt.Errorf("encode owner entry schedule: %w", err)
	}
	fingerprint := sha256.Sum256(owner)
	entryHash := hex.EncodeToString(fingerprint[:])
	var binding *IdentityBinding
	if stable {
		if lookupErr == nil {
			binding, err = existingEntryIdentity(ctx, research, job, config, selection.Snapshot, entryHash)
		} else {
			binding, err = BindOwnerIdentity(archiveRoot, config.Context, *config.Identity, job.SecurityBindings,
				selection.Snapshot, portfolio.AsOf, nil)
		}
		if err != nil {
			return EntryReceipt{}, err
		}
	}
	dispatch, err := json.Marshal(Request{Version: config.Version, JobID: job.ID, SnapshotContext: config.Context,
		EntryScheduleSHA256: entryHash, Identity: binding})
	if err != nil {
		return EntryReceipt{}, fmt.Errorf("encode paper entry binding: %w", err)
	}
	input := jobs.CreateJob{
		ID: job.ID, Request: body, SnapshotID: portfolio.ID, Snapshot: snapshot,
		PaperDispatch: &jobs.PaperDispatchInput{AccountID: config.Context.AccountID, Request: dispatch},
		AdmitThrough:  deadline,
	}
	admit := func() error {
		admissionAt := now.Add(time.Since(started))
		if admissionAt.After(deadline) {
			return errEntryMissed
		}
		if admissionAt.Sub(portfolio.AsOf) > maxAge {
			return accounts.ErrStale
		}
		if binding != nil {
			for _, security := range binding.Securities {
				if security.ExpiresAt != nil && !security.ExpiresAt.After(admissionAt) {
					return accounts.ErrStale
				}
			}
		}
		if err := config.Context.CheckMarks(admissionAt); err != nil {
			return err
		}
		return currentEntrySnapshot(ctx, paper, config.Context, portfolio)
	}
	var accepted jobs.Job
	var created bool
	if stable {
		accepted, created, err = research.CreateBoundPaperJob(ctx,
			jobs.BoundPaperJob{Job: input, OwnerRegistrySHA256: ownerRegistrySHA256}, admit)
	} else {
		accepted, created, err = research.Create(ctx, input, admit)
	}
	if errors.Is(err, errEntryMissed) || errors.Is(err, jobs.ErrAdmissionDeadline) {
		status.Status = "missed"
		return status, nil
	}
	if err != nil {
		return EntryReceipt{}, err
	}
	if !created && !stable {
		task, err := research.GetPaperDispatch(ctx, accepted.ID)
		if errors.Is(err, jobs.ErrNotFound) || err == nil &&
			(task.AccountID != config.Context.AccountID || !bytes.Equal(task.Request, dispatch)) {
			return EntryReceipt{}, jobs.ErrConflict
		}
		if err != nil {
			return EntryReceipt{}, err
		}
	}
	return EntryReceipt{Version: config.Version, Status: accepted.Status, Created: created, JobID: accepted.ID, CreatedAt: &accepted.CreatedAt}, nil
}

func (c EntrySchedule) valid() bool {
	identity := c.Version == IdentityVersion && c.Identity != nil && validArchiveReference(c.Identity.Registry) &&
		validArchiveReference(c.Identity.QuoteTape) && independentEntryRefs(c.Identity.Registry, c.Identity.QuoteTape)
	return (c.Version == Version && c.Identity == nil || identity) && scheduleName.MatchString(c.ID) && !c.At.IsZero() &&
		c.MaxLateness >= 1 && c.MaxLateness <= 3600 && c.Budget >= 1 && c.Budget <= 64 &&
		uuidPattern.MatchString(c.Context.AccountID) && c.Context.SnapshotVersion >= 1 &&
		scheduleName.MatchString(c.Context.Policy.Version) && c.Context.Policy.Validate() == nil
}

func independentEntryRefs(a, b ArchiveReference) bool {
	return a.Ref != b.Ref && a.SHA256 != b.SHA256
}

func (c EntrySchedule) matchesJobIdentity(job storedJob, selectedSHA256 string) bool {
	if c.Version == Version {
		return job.Version == Version && job.IdentityRegistry == nil && len(job.SecurityBindings) == 0
	}
	if (job.Version != IdentityVersion && job.Version != "0.3") ||
		!entryDigest.MatchString(selectedSHA256) || job.IdentityRegistry == nil ||
		*job.IdentityRegistry != c.Identity.Registry || len(job.SecurityBindings) != len(job.RequestedSymbols) ||
		!independentEntryRefs(c.Identity.Registry, job.DataManifest) || !independentEntryRefs(c.Identity.QuoteTape, job.DataManifest) {
		return false
	}
	seen := make(map[string]bool, len(job.SecurityBindings))
	for i, binding := range job.SecurityBindings {
		if binding.Symbol != job.RequestedSymbols[i] || !scheduleName.MatchString(binding.SecurityID) || seen[binding.SecurityID] {
			return false
		}
		seen[binding.SecurityID] = true
	}
	return true
}

func (c EntrySchedule) checkRequestedMarks(symbols []string, asOf time.Time) error {
	rules, marks, err := c.Context.inputs(asOf)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(marks))
	for instrument := range marks {
		allowed[rules[instrument].Symbol] = true
	}
	for _, symbol := range symbols {
		if !allowed[symbol] {
			return fmt.Errorf("%w: selected symbol lacks owner eligibility and an observed mark", ErrInvalid)
		}
	}
	return nil
}

func currentEntrySnapshot(ctx context.Context, paper EntryLedger, config SnapshotContext, portfolio contract.Portfolio) error {
	account, err := paper.GetAccount(ctx, config.AccountID)
	if err != nil {
		return err
	}
	actual, err := Snapshot(account, config, portfolio.AsOf)
	if err != nil {
		return err
	}
	if actual.ID != portfolio.ID {
		return accounts.ErrStale
	}
	events, err := paper.Events(ctx, account.ID, account.Version-1, 1)
	if err != nil {
		return err
	}
	if len(events) != 1 || events[0].AccountVersion != account.Version || events[0].RecordedAt.After(portfolio.AsOf) {
		return accounts.ErrStale
	}
	return nil
}
