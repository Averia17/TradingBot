// Package research_scheduler queues frozen local research occurrences through Control.
package research_scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"tradingbot/control/internal/contract"
)

type Config struct {
	ControlURL   string
	ArchiveRoot  string
	PollInterval time.Duration
	Once         bool
}

// Run exits after the last occurrence, after one pass when Once is set, or on cancellation.
// PostgreSQL behind the local Control API provides deduplication across processes.
func Run(ctx context.Context, schedule Schedule, config Config) error {
	client := &http.Client{
		Timeout: 5 * time.Second, Transport: &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	return runSchedule(ctx, schedule, config, client)
}

func runSchedule(ctx context.Context, schedule Schedule, config Config, client *http.Client) error {
	origin, err := controlOrigin(config.ControlURL)
	if err != nil {
		return err
	}
	if !config.Once && (config.PollInterval < time.Second || config.PollInterval > time.Minute) {
		return errors.New("poll interval must be between one second and one minute")
	}
	if config.ArchiveRoot == "" {
		return errors.New("local archive root is required")
	}
	archive, err := os.OpenRoot(config.ArchiveRoot)
	if err != nil {
		return fmt.Errorf("open local archive root: %w", err)
	}
	defer archive.Close()
	control := controlClient{client: client, origin: origin}
	startedAt := time.Now()
	for _, job := range schedule.occurrences {
		if ctx.Err() != nil || config.Once && job.At.After(startedAt) {
			return nil
		}
		if err := job.run(ctx, archive, control, config); err != nil {
			return fmt.Errorf("job %s: %w", job.ID, err)
		}
	}
	return nil
}

func (job occurrence) run(ctx context.Context, archive *os.Root, control controlClient, config Config) error {
	archiveVerified := false
	for ctx.Err() == nil {
		now := time.Now()
		if delay := job.At.Sub(now); delay > 0 {
			if config.Once || !wait(ctx, min(config.PollInterval, delay)) {
				return nil
			}
			continue
		}
		if !now.Before(job.Deadline) {
			err := job.observeClosedWindow(ctx, control, now)
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if _, err := job.archive.verify(archive, !archiveVerified); err != nil {
			return err
		}
		archiveVerified = true
		if time.Until(job.Deadline) <= 0 {
			continue
		}
		submitCtx, cancel := context.WithDeadline(ctx, job.Deadline)
		retry, err := job.submit(submitCtx, control)
		cancel()
		if ctx.Err() != nil || err == nil {
			return nil
		}
		if retry && time.Until(job.Deadline) <= 0 {
			continue
		}
		if !retry || config.Once {
			return err
		}
		slog.Warn("could not confirm scheduled job, will repeat its ID", "job_id", job.ID, "error", err)
		if !wait(ctx, config.PollInterval) {
			return nil
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

func (job occurrence) submit(ctx context.Context, control controlClient) (bool, error) {
	var receipt struct {
		ID string `json:"job_id"`
	}
	status, retry, err := control.post(ctx, "/jobs", job.Body, maxJobBytes+1024, func(data []byte) bool {
		return json.Unmarshal(data, &receipt) == nil && receipt.ID == job.ID
	})
	if err != nil {
		return retry, err
	}
	logger := slog.Default()
	if event := job.archive.event; event != nil {
		ref := event.reference
		logger = logger.With("event_kind", ref.Kind, "event_provider", ref.Provider, "event_record_id", ref.RecordID)
	}
	logger.Info("research occurrence confirmed", "job_id", job.ID, "scheduled_at", job.At, "http_status", status)
	return false, nil
}

func (job occurrence) observeClosedWindow(ctx context.Context, control controlClient, now time.Time) error {
	logger := slog.With("job_id", job.ID, "scheduled_at", job.At)
	if job.calendar == nil || now.Sub(job.Deadline) > contract.ResearchCalendarObservationLookback {
		logger.Info("research occurrence outside scheduling window")
		return nil
	}
	body, err := json.Marshal(job.calendar)
	if err != nil {
		return fmt.Errorf("encode calendar observation: %w", err)
	}
	var receipt contract.ResearchCalendarReceipt
	_, _, err = control.post(ctx, "/schedule-occurrences/"+job.ID, body, 4*1024, func(data []byte) bool {
		return contract.Decode(data, contract.ResearchCalendarOccurrenceReceipt, &receipt) == nil &&
			receipt.JobID == job.ID && receipt.ResearchCalendarOccurrence == *job.calendar
	})
	if err != nil {
		return fmt.Errorf("observe closed calendar window: %w", err)
	}
	logger.Info("research calendar window recorded", "status", receipt.Status)
	return nil
}

type controlClient struct {
	client *http.Client
	origin string
}

func controlOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return "", errors.New("Control URL must be a loopback HTTP origin")
	}
	u.Path = ""
	return u.String(), nil
}

func (c controlClient) post(ctx context.Context, path string, body []byte, limit int, confirm func([]byte) bool) (status int, retry bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+"/v1/research"+path, bytes.NewReader(body))
	if err != nil {
		return 0, false, errors.New("cannot construct Control request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return 0, true, errors.New("Control request outcome unknown")
	}
	defer response.Body.Close()
	status = response.StatusCode
	if status != http.StatusCreated && status != http.StatusOK {
		return status, status == http.StatusTooManyRequests || status >= 500, fmt.Errorf("Control returned HTTP %d", status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil || len(data) > limit || !confirm(data) {
		return status, true, errors.New("Control receipt did not confirm the request")
	}
	return status, false, nil
}
