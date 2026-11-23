package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/research_scheduler"
)

type coverageReport struct {
	SchemaVersion            string        `json:"schema_version"`
	Scope                    string        `json:"scope"`
	CheckedAt                time.Time     `json:"checked_at"`
	ScheduleID               string        `json:"schedule_id"`
	ScheduleSHA256           string        `json:"schedule_sha256"`
	ExcludedEventOccurrences int           `json:"excluded_event_occurrences"`
	Days                     []calendarDay `json:"days"`
}

type calendarDay struct {
	Date        string           `json:"date_utc"`
	Occurrences []windowEvidence `json:"occurrences"`
}

type windowEvidence struct {
	JobID                  string       `json:"job_id"`
	ScheduledAt            time.Time    `json:"scheduled_at"`
	AcceptBefore           time.Time    `json:"accept_before"`
	ExpectedRequestSHA256  string       `json:"expected_request_sha256"`
	SubmissionStatus       string       `json:"submission_status"`
	ClosedWindowObservedAt *time.Time   `json:"closed_window_observed_at,omitempty"`
	Conflict               string       `json:"conflict,omitempty"`
	Job                    *jobEvidence `json:"job,omitempty"`
}

type jobEvidence struct {
	CreatedAt               time.Time  `json:"created_at"`
	Status                  string     `json:"status"`
	Attempts                int        `json:"attempts"`
	LeaseExpiresAt          *time.Time `json:"lease_expires_at,omitempty"`
	CompletedAt             *time.Time `json:"completed_at,omitempty"`
	PermitHistoryKnown      bool       `json:"permit_history_known"`
	DurablePermits          int        `json:"durable_permits"`
	RecordedCallbacks       int        `json:"recorded_callbacks"`
	MissingCallbacks        int        `json:"missing_callbacks"`
	InvalidCallbackReceipts int        `json:"invalid_callback_receipts"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("research schedule coverage unavailable", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("research-schedule-coverage", flag.ContinueOnError)
	path := flags.String("config", "", "archived research-scheduler configuration")
	pin := flags.String("sha256", "", "SHA-256 of the exact schedule file bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if sum, err := hex.DecodeString(*pin); err != nil || len(sum) != sha256.Size || strings.ToLower(*pin) != *pin ||
		flags.NArg() != 0 || *path == "" || strings.TrimSpace(dsn) == "" {
		return errors.New("provide --config, a lowercase --sha256 and TRADINGBOT_DATABASE_URL")
	}
	file, err := os.Open(*path)
	if err != nil {
		return errors.New("open schedule file failed")
	}
	defer file.Close()
	hash := sha256.New()
	schedule, err := research_scheduler.Load(io.TeeReader(file, hash))
	if err != nil {
		return err
	}
	if *pin != hex.EncodeToString(hash.Sum(nil)) {
		return errors.New("schedule file disagrees with the pinned SHA-256")
	}
	calendar, excluded := schedule.PlannedCalendar()
	if len(calendar) == 0 {
		return errors.New("schedule contains no declared calendar occurrences")
	}
	ids := make([]string, 0, len(calendar))
	for _, plan := range calendar {
		ids = append(ids, contract.ResearchCalendarJobID(plan.ScheduleID, plan.ScheduledAt))
	}
	now, evidence, err := jobs.ReadScheduleEvidence(ctx, dsn, ids)
	if err != nil {
		return errors.New("read PostgreSQL schedule evidence failed")
	}
	report := coverageReport{
		SchemaVersion: "0.1", Scope: "declared_research_calendar", CheckedAt: now,
		ScheduleID: calendar[0].ScheduleID, ScheduleSHA256: *pin, ExcludedEventOccurrences: excluded,
	}
	for i, plan := range calendar {
		day := plan.ScheduledAt.UTC().Format(time.DateOnly)
		if len(report.Days) == 0 || report.Days[len(report.Days)-1].Date != day {
			report.Days = append(report.Days, calendarDay{Date: day})
		}
		last := &report.Days[len(report.Days)-1]
		last.Occurrences = append(last.Occurrences, classifyOccurrence(plan, now, evidence[ids[i]]))
	}
	return json.NewEncoder(output).Encode(report)
}

func classifyOccurrence(plan contract.ResearchCalendarOccurrence, now time.Time, evidence jobs.ScheduleEvidence) windowEvidence {
	row := windowEvidence{
		JobID: contract.ResearchCalendarJobID(plan.ScheduleID, plan.ScheduledAt), ScheduledAt: plan.ScheduledAt,
		AcceptBefore: plan.Deadline(), ExpectedRequestSHA256: plan.RequestSHA256,
	}
	if observation := evidence.Observation; observation != nil {
		if observation.ScheduleID != plan.ScheduleID || observation.ScheduledAt != plan.ScheduledAt.Format(time.RFC3339Nano) ||
			observation.MaxLatenessSeconds != plan.MaxLatenessSeconds || observation.RequestSHA256 != plan.RequestSHA256 ||
			observation.RecordedAt.Before(row.AcceptBefore) || observation.RecordedAt.After(row.AcceptBefore.Add(contract.ResearchCalendarObservationLookback)) ||
			observation.RecordedAt.After(now) {
			row.SubmissionStatus, row.Conflict = "conflict", "closed_window_evidence"
			return row
		}
		row.ClosedWindowObservedAt = &observation.RecordedAt
	}
	if observed := evidence.Job; observed != nil {
		if observed.RequestSHA256 != plan.RequestSHA256 || !observed.RequestIntegrity || observed.CreatedAt.After(now) {
			row.SubmissionStatus, row.Conflict = "conflict", "job_request_evidence"
			return row
		}
		if !observed.ResultIntegrity || observed.Status == "leased" && observed.LeaseExpiresAt == nil || observed.CompletedAt != nil &&
			(observed.CompletedAt.Before(observed.CreatedAt) || observed.CompletedAt.After(now)) {
			row.SubmissionStatus, row.Conflict = "conflict", "job_state_evidence"
			return row
		}
		row.Job = &jobEvidence{
			CreatedAt: observed.CreatedAt, Status: observed.Status, Attempts: observed.AttemptCount,
			LeaseExpiresAt: observed.LeaseExpiresAt, CompletedAt: observed.CompletedAt,
			PermitHistoryKnown: observed.PermitHistoryKnown, DurablePermits: observed.DurablePermits,
			RecordedCallbacks: observed.RecordedCallbacks, MissingCallbacks: observed.DurablePermits - observed.RecordedCallbacks,
			InvalidCallbackReceipts: observed.InvalidCallbackReceipts,
		}
		switch {
		case observed.CreatedAt.Before(row.ScheduledAt):
			row.SubmissionStatus = "early"
		case !observed.CreatedAt.Before(row.AcceptBefore):
			row.SubmissionStatus = "late"
		default:
			row.SubmissionStatus = "on_time"
		}
		return row
	}
	switch {
	case now.Before(row.ScheduledAt):
		row.SubmissionStatus = "not_due"
	case now.Before(row.AcceptBefore):
		row.SubmissionStatus = "window_open"
	case row.ClosedWindowObservedAt != nil:
		row.SubmissionStatus = "observed_miss"
	default:
		row.SubmissionStatus = "missing_evidence"
	}
	return row
}
