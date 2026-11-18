// Package researchscheduler queues frozen local research occurrences through Control.
package researchscheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
)

const (
	maxScheduleBytes = 1024 * 1024
	maxArchiveBytes  = 16 * 1024 * 1024
	maxJobBytes      = 256 * 1024
	maxOccurrences   = 128
)

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	localRef   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	digest     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Config struct {
	ControlURL   string
	ArchiveRoot  string
	PollInterval time.Duration
	Once         bool
}

type ArchiveReference struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

func (ref ArchiveReference) valid() bool {
	return localRef.MatchString(ref.Ref) && fs.ValidPath(ref.Ref) && digest.MatchString(ref.SHA256)
}

type trigger struct {
	At        time.Time        `json:"scheduled_at"`
	Event     json.RawMessage  `json:"event"`
	Portfolio json.RawMessage  `json:"portfolio"`
	Symbols   []string         `json:"requested_symbols"`
	Manifest  ArchiveReference `json:"data_manifest"`
	Budget    int              `json:"max_llm_calls"`
}

type definition struct {
	Version    string    `json:"schema_version"`
	ID         string    `json:"schedule_id"`
	Policy     string    `json:"policy_version"`
	Lateness   int       `json:"max_lateness_seconds"`
	Occurrence []trigger `json:"occurrences"`
}

type occurrence struct {
	At       time.Time
	ID       string
	Body     []byte
	Manifest ArchiveReference
	Event    *eventAnchor
}

type Schedule struct {
	id     string
	window time.Duration
	jobs   []occurrence
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

// Load validates and freezes the payload for every scheduled occurrence.
func Load(reader io.Reader) (Schedule, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxScheduleBytes+1))
	if err != nil {
		return Schedule{}, fmt.Errorf("read schedule: %w", err)
	}
	if len(data) > maxScheduleBytes {
		return Schedule{}, errors.New("schedule exceeds the file size limit")
	}
	var input definition
	if err := contract.Decode(data, nil, &input); err != nil {
		return Schedule{}, fmt.Errorf("decode schedule: %w", err)
	}
	if (input.Version != "0.1" && input.Version != "0.2") || !identifier.MatchString(input.ID) || !identifier.MatchString(input.Policy) ||
		input.Lateness < 1 || input.Lateness > 3600 || len(input.Occurrence) < 1 || len(input.Occurrence) > maxOccurrences {
		return Schedule{}, errors.New("invalid schedule settings")
	}
	schedule := Schedule{id: input.ID, window: time.Duration(input.Lateness) * time.Second}
	seen := make(map[string]bool, len(input.Occurrence))
	for index, item := range input.Occurrence {
		job, err := prepare(input, item)
		if err != nil {
			return Schedule{}, fmt.Errorf("occurrence %d: %w", index+1, err)
		}
		if seen[job.ID] {
			return Schedule{}, errors.New("duplicate scheduled occurrence")
		}
		seen[job.ID] = true
		schedule.jobs = append(schedule.jobs, job)
	}
	slices.SortFunc(schedule.jobs, func(a, b occurrence) int { return a.At.Compare(b.At) })
	return schedule, nil
}

func prepare(input definition, item trigger) (occurrence, error) {
	if item.At.IsZero() || item.Budget < 1 || item.Budget > 64 || contract.CheckSymbols(item.Symbols) != nil || !item.Manifest.valid() {
		return occurrence{}, errors.New("invalid trigger or local archive reference")
	}
	portfolio, err := contract.ParsePortfolio(item.Portfolio)
	if err != nil || portfolio.AsOf.After(item.At) {
		return occurrence{}, errors.New("invalid portfolio or snapshot newer than the scheduled occurrence")
	}
	portfolio.ID = strings.ToLower(portfolio.ID)
	portfolio.AsOf = portfolio.AsOf.UTC()
	at := item.At.UTC()
	jobID := contract.ResearchCalendarJobID(input.ID, at)
	var event *eventAnchor
	if len(item.Event) != 0 {
		var reference EventReference
		if input.Version != "0.2" || contract.Decode(item.Event, nil, &reference) != nil || !reference.valid() {
			return occurrence{}, errors.New("invalid event reference for schedule version")
		}
		jobID = contract.DerivedResearchJobID("tradingbot/research-event-v1\n" + input.ID + "\n" + reference.Provider + "\n" + reference.RecordID)
		event = &eventAnchor{reference: reference, cutoff: portfolio.AsOf, symbols: item.Symbols}
	}
	body, err := json.Marshal(jobSubmission{"0.1", jobID, portfolio, item.Symbols, item.Manifest, input.Policy, item.Budget})
	if err != nil {
		return occurrence{}, fmt.Errorf("encode research job: %w", err)
	}
	if len(body) > maxJobBytes {
		return occurrence{}, errors.New("research job exceeds the request size limit")
	}
	return occurrence{At: at, ID: jobID, Body: body, Manifest: item.Manifest, Event: event}, nil
}

// VerifyArchive proves the bytes of a pinned local research archive. An event
// additionally requires its archived record to be visible at the cutoff.
func VerifyArchive(archiveRoot string, reference ArchiveReference, cutoff time.Time, symbols []string, event *EventReference) (VerifiedEvent, error) {
	if !reference.valid() || cutoff.IsZero() || contract.CheckSymbols(symbols) != nil {
		return VerifiedEvent{}, errors.New("invalid research archive reference or cutoff")
	}
	job := occurrence{Manifest: reference}
	if event != nil {
		if !event.valid() {
			return VerifiedEvent{}, errors.New("invalid archive event reference")
		}
		job.Event = &eventAnchor{reference: *event, cutoff: cutoff, symbols: symbols}
	}
	root, err := os.OpenRoot(archiveRoot)
	if err != nil {
		return VerifiedEvent{}, fmt.Errorf("open local archive root: %w", err)
	}
	defer root.Close()
	return verifyArchive(root, job)
}

// PaperEntryOccurrenceID binds a calendar trigger to its owner account.
func PaperEntryOccurrenceID(accountID, scheduleID string, at time.Time) string {
	return contract.DerivedResearchJobID("tradingbot/paper-entry-calendar-v1\n" + accountID + "\n" + scheduleID + "\n" + at.UTC().Format(time.RFC3339Nano))
}

// VerifyEvent validates and resolves one event already defined in the schedule.
// It neither submits a research job nor changes schedule state.
func (schedule Schedule) VerifyEvent(archiveRoot, provider, recordID string) (VerifiedEvent, error) {
	for _, occurrence := range schedule.jobs {
		if occurrence.Event == nil || occurrence.Event.reference.Provider != provider || occurrence.Event.reference.RecordID != recordID {
			continue
		}
		root, err := os.OpenRoot(archiveRoot)
		if err != nil {
			return VerifiedEvent{}, fmt.Errorf("open local archive root: %w", err)
		}
		defer root.Close()
		return verifyArchive(root, occurrence)
	}
	return VerifiedEvent{}, errors.New("event is not present in the frozen schedule")
}

// Run exits after the last occurrence, after one pass when Once is set, or on cancellation.
// PostgreSQL behind the local Control API provides deduplication across processes.
func Run(ctx context.Context, schedule Schedule, config Config) error {
	endpoint, err := controlEndpoint(config.ControlURL)
	if err != nil {
		return err
	}
	if config.PollInterval < time.Second || config.PollInterval > time.Minute {
		if !config.Once {
			return errors.New("poll interval must be between one second and one minute")
		}
	}
	if config.ArchiveRoot == "" {
		return errors.New("local archive root is required")
	}
	archive, err := os.OpenRoot(config.ArchiveRoot)
	if err != nil {
		return fmt.Errorf("open local archive root: %w", err)
	}
	defer archive.Close()
	client := &http.Client{
		Timeout: 5 * time.Second, Transport: &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	startedAt := time.Now()
	for _, job := range schedule.jobs {
		if config.Once && job.At.After(startedAt) {
			return nil
		}
		deadline := job.At.Add(schedule.window)
		for ctx.Err() == nil {
			now := time.Now()
			if now.Before(job.At) {
				if config.Once || !wait(ctx, min(config.PollInterval, job.At.Sub(now))) {
					return nil
				}
				continue
			}
			if !now.Before(deadline) {
				err := schedule.observeClosedWindow(ctx, client, endpoint, job, now.Sub(deadline))
				if ctx.Err() != nil {
					return nil
				}
				if err != nil {
					return fmt.Errorf("observe closed window for job %s: %w", job.ID, err)
				}
				break
			}
			if _, err := verifyArchive(archive, job); err != nil {
				return fmt.Errorf("job %s: %w", job.ID, err)
			}
			if !time.Now().Before(deadline) {
				continue
			}
			submitCtx, cancel := context.WithDeadline(ctx, deadline)
			retry, err := submit(submitCtx, client, endpoint, job)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				break
			}
			if retry && !time.Now().Before(deadline) {
				continue
			}
			if !retry || config.Once {
				return fmt.Errorf("job %s: %w", job.ID, err)
			}
			slog.Warn("could not confirm scheduled job, will repeat its ID", "job_id", job.ID, "error", err)
			if !wait(ctx, config.PollInterval) {
				return nil
			}
		}
	}
	return nil
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func verifyArchive(root *os.Root, job occurrence) (VerifiedEvent, error) {
	reference := job.Manifest
	info, err := root.Stat(reference.Ref)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxArchiveBytes {
		return VerifiedEvent{}, errors.New("archive is missing, oversized or outside the configured root")
	}
	file, err := root.Open(reference.Ref)
	if err != nil {
		return VerifiedEvent{}, errors.New("cannot open local archive")
	}
	defer file.Close()
	hasher := sha256.New()
	var evidence bytes.Buffer
	var writer io.Writer = hasher
	if job.Event != nil {
		writer = io.MultiWriter(hasher, &evidence)
	}
	n, err := io.Copy(writer, io.LimitReader(file, maxArchiveBytes+1))
	if err != nil || n > maxArchiveBytes || fmt.Sprintf("%x", hasher.Sum(nil)) != reference.SHA256 {
		return VerifiedEvent{}, errors.New("cannot verify local archive SHA-256")
	}
	if job.Event != nil {
		return job.Event.verify(evidence.Bytes(), reference.SHA256)
	}
	return VerifiedEvent{}, nil
}

func controlEndpoint(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return "", errors.New("Control URL must be a loopback HTTP origin")
	}
	u.Path = "/v1/research/jobs"
	return u.String(), nil
}

func submit(ctx context.Context, client *http.Client, endpoint string, job occurrence) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(job.Body))
	if err != nil {
		return false, errors.New("cannot construct scheduled job request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return true, errors.New("Control request outcome unknown")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500, fmt.Errorf("Control returned HTTP %d", response.StatusCode)
	}
	var receipt struct {
		ID string `json:"job_id"`
	}
	const receiptLimit = maxJobBytes + 1024
	data, err := io.ReadAll(io.LimitReader(response.Body, receiptLimit+1))
	if err != nil || len(data) > receiptLimit || json.Unmarshal(data, &receipt) != nil || receipt.ID != job.ID {
		return true, errors.New("Control receipt did not confirm the scheduled job ID")
	}
	logger := slog.Default()
	if job.Event != nil {
		event := job.Event.reference
		logger = logger.With("event_kind", event.Kind, "event_provider", event.Provider, "event_record_id", event.RecordID)
	}
	logger.Info("research occurrence confirmed", "job_id", job.ID, "scheduled_at", job.At, "http_status", response.StatusCode)
	return false, nil
}
