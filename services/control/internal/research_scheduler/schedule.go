package research_scheduler

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
)

const (
	maxScheduleBytes = 1024 * 1024
	maxJobBytes      = 256 * 1024
	maxOccurrences   = 128
)

type occurrenceSpec struct {
	At        time.Time        `json:"scheduled_at"`
	Event     json.RawMessage  `json:"event,omitempty"`
	Portfolio json.RawMessage  `json:"portfolio"`
	Symbols   []string         `json:"requested_symbols"`
	Manifest  ArchiveReference `json:"data_manifest"`
	Budget    int              `json:"max_llm_calls"`
}

type scheduleSpec struct {
	Version     string           `json:"schema_version"`
	ID          string           `json:"schedule_id"`
	Policy      string           `json:"policy_version"`
	Lateness    int              `json:"max_lateness_seconds"`
	Occurrences []occurrenceSpec `json:"occurrences"`
}

type occurrence struct {
	At       time.Time
	Deadline time.Time
	ID       string
	Body     []byte
	archive  archiveProof
	calendar *contract.ResearchCalendarOccurrence
}

type Schedule struct {
	occurrences []occurrence
}

type jobSubmission struct {
	Version   string             `json:"job_schema_version"`
	ID        string             `json:"job_id"`
	Portfolio contract.Portfolio `json:"portfolio"`
	Symbols   []string           `json:"requested_symbols"`
	Manifest  ArchiveReference   `json:"data_manifest"`
	Policy    string             `json:"policy_version"`
	Budget    int                `json:"max_llm_calls"`
}

// Load validates each occurrence and freezes the request and calendar evidence
// used by both the scheduler and the coverage report.
func Load(reader io.Reader) (Schedule, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxScheduleBytes+1))
	if err != nil {
		return Schedule{}, fmt.Errorf("read schedule: %w", err)
	}
	if len(data) > maxScheduleBytes {
		return Schedule{}, errors.New("schedule exceeds the file size limit")
	}
	var input scheduleSpec
	if err := contract.Decode(data, nil, &input); err != nil {
		return Schedule{}, fmt.Errorf("decode schedule: %w", err)
	}
	if (input.Version != "0.1" && input.Version != "0.2") || !identifier.MatchString(input.ID) || !identifier.MatchString(input.Policy) ||
		input.Lateness < 1 || input.Lateness > 3600 || len(input.Occurrences) < 1 || len(input.Occurrences) > maxOccurrences {
		return Schedule{}, errors.New("invalid schedule settings")
	}
	schedule := Schedule{occurrences: make([]occurrence, 0, len(input.Occurrences))}
	seen := make(map[string]bool, len(input.Occurrences))
	for index, item := range input.Occurrences {
		job, err := input.prepare(item)
		if err != nil {
			return Schedule{}, fmt.Errorf("occurrence %d: %w", index+1, err)
		}
		if seen[job.ID] {
			return Schedule{}, errors.New("duplicate scheduled occurrence")
		}
		seen[job.ID] = true
		schedule.occurrences = append(schedule.occurrences, job)
	}
	slices.SortFunc(schedule.occurrences, func(a, b occurrence) int { return a.At.Compare(b.At) })
	return schedule, nil
}

func (input scheduleSpec) prepare(item occurrenceSpec) (occurrence, error) {
	if item.At.IsZero() || item.Budget < 1 || item.Budget > 64 || contract.CheckSymbols(item.Symbols) != nil || !item.Manifest.valid() {
		return occurrence{}, errors.New("invalid trigger or local archive reference")
	}
	portfolio, err := contract.ParsePortfolio(item.Portfolio)
	if err != nil || portfolio.AsOf.After(item.At) {
		return occurrence{}, errors.New("invalid portfolio or snapshot newer than the scheduled occurrence")
	}
	portfolio.ID = strings.ToLower(portfolio.ID)
	portfolio.AsOf = portfolio.AsOf.UTC()
	job := occurrence{
		At: item.At.UTC(), Deadline: item.At.UTC().Add(time.Duration(input.Lateness) * time.Second),
		archive: archiveProof{reference: item.Manifest},
	}
	if len(item.Event) == 0 {
		job.ID = contract.ResearchCalendarJobID(input.ID, job.At)
	} else {
		var reference EventReference
		if input.Version != "0.2" || contract.Decode(item.Event, nil, &reference) != nil || !reference.valid() {
			return occurrence{}, errors.New("invalid event reference for schedule version")
		}
		job.ID = contract.DerivedResearchJobID("tradingbot/research-event-v1\n" + input.ID + "\n" + reference.Provider + "\n" + reference.RecordID)
		job.archive.event = &eventLookup{reference: reference, cutoff: portfolio.AsOf, symbols: item.Symbols}
	}
	job.Body, err = json.Marshal(jobSubmission{
		Version: "0.1", ID: job.ID, Portfolio: portfolio, Symbols: item.Symbols,
		Manifest: item.Manifest, Policy: input.Policy, Budget: item.Budget,
	})
	if err != nil {
		return occurrence{}, fmt.Errorf("encode research job: %w", err)
	}
	if len(job.Body) > maxJobBytes {
		return occurrence{}, errors.New("research job exceeds the request size limit")
	}
	if job.archive.event == nil {
		job.calendar = &contract.ResearchCalendarOccurrence{
			Version: "0.1", ScheduleID: input.ID, ScheduledAt: job.At,
			MaxLatenessSeconds: input.Lateness, RequestSHA256: fmt.Sprintf("%x", sha256.Sum256(job.Body)),
		}
	}
	return job, nil
}

// PlannedCalendar returns the expected job digest for each non-event window.
// Event-triggered occurrences have separate identities and no calendar journal.
func (s Schedule) PlannedCalendar() ([]contract.ResearchCalendarOccurrence, int) {
	calendar := make([]contract.ResearchCalendarOccurrence, 0, len(s.occurrences))
	for _, job := range s.occurrences {
		if job.calendar != nil {
			calendar = append(calendar, *job.calendar)
		}
	}
	return calendar, len(s.occurrences) - len(calendar)
}

// PaperEntryOccurrenceID binds a calendar trigger to its owner account.
func PaperEntryOccurrenceID(accountID, scheduleID string, at time.Time) string {
	return contract.DerivedResearchJobID("tradingbot/paper-entry-calendar-v1\n" + accountID + "\n" + scheduleID + "\n" + at.UTC().Format(time.RFC3339Nano))
}

// VerifyEvent resolves an event from its exact frozen schedule and archive.
func (s Schedule) VerifyEvent(archiveRoot, provider, recordID string) (VerifiedEvent, error) {
	for _, job := range s.occurrences {
		event := job.archive.event
		if event != nil && event.reference.Provider == provider && event.reference.RecordID == recordID {
			return job.archive.verifyLocal(archiveRoot)
		}
	}
	return VerifiedEvent{}, errors.New("event is not present in the frozen schedule")
}
