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

type manifest struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

type trigger struct {
	At        time.Time       `json:"scheduled_at"`
	Portfolio json.RawMessage `json:"portfolio"`
	Symbols   []string        `json:"requested_symbols"`
	Manifest  manifest        `json:"data_manifest"`
	Budget    int             `json:"max_llm_calls"`
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
	Manifest manifest
}

type Schedule struct {
	window time.Duration
	jobs   []occurrence
}

type jobSubmission struct {
	Version   string             `json:"job_schema_version"`
	ID        string             `json:"job_id"`
	Portfolio contract.Portfolio `json:"portfolio"`
	Symbols   []string           `json:"requested_symbols"`
	Manifest  manifest           `json:"data_manifest"`
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
	if input.Version != "0.1" || !identifier.MatchString(input.ID) || !identifier.MatchString(input.Policy) ||
		input.Lateness < 1 || input.Lateness > 3600 || len(input.Occurrence) < 1 || len(input.Occurrence) > maxOccurrences {
		return Schedule{}, errors.New("invalid schedule settings")
	}
	schedule := Schedule{window: time.Duration(input.Lateness) * time.Second}
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
	if item.At.IsZero() || item.Budget < 1 || item.Budget > 64 || contract.CheckSymbols(item.Symbols) != nil ||
		!localRef.MatchString(item.Manifest.Ref) || !fs.ValidPath(item.Manifest.Ref) || !digest.MatchString(item.Manifest.SHA256) {
		return occurrence{}, errors.New("invalid trigger or local archive reference")
	}
	portfolio, err := contract.ParsePortfolio(item.Portfolio)
	if err != nil || portfolio.AsOf.After(item.At) {
		return occurrence{}, errors.New("invalid portfolio or snapshot newer than the scheduled occurrence")
	}
	portfolio.ID = strings.ToLower(portfolio.ID)
	portfolio.AsOf = portfolio.AsOf.UTC()
	at := item.At.UTC()
	jobID := occurrenceID(input.ID, at)
	body, err := json.Marshal(jobSubmission{"0.1", jobID, portfolio, item.Symbols, item.Manifest, input.Policy, item.Budget})
	if err != nil {
		return occurrence{}, fmt.Errorf("encode research job: %w", err)
	}
	if len(body) > maxJobBytes {
		return occurrence{}, errors.New("research job exceeds the request size limit")
	}
	return occurrence{At: at, ID: jobID, Body: body, Manifest: item.Manifest}, nil
}

func occurrenceID(id string, at time.Time) string {
	// Input changes keep the same ID and must conflict with an accepted job.
	hash := sha256.Sum256([]byte("tradingbot/research-calendar-v1\n" + id + "\n" + at.Format(time.RFC3339Nano)))
	hash[6] = hash[6]&0x0f | 0x80 // UUID version 8
	hash[8] = hash[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
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
		for ctx.Err() == nil {
			now := time.Now()
			if now.Before(job.At) {
				if config.Once || !wait(ctx, min(config.PollInterval, job.At.Sub(now))) {
					return nil
				}
				continue
			}
			if now.Sub(job.At) > schedule.window {
				slog.Info("research occurrence outside scheduling window", "job_id", job.ID, "scheduled_at", job.At)
				break
			}
			if err := verifyArchive(archive, job.Manifest); err != nil {
				return fmt.Errorf("job %s: %w", job.ID, err)
			}
			if time.Since(job.At) > schedule.window {
				break
			}
			submitCtx, cancel := context.WithDeadline(ctx, job.At.Add(schedule.window))
			retry, err := submit(submitCtx, client, endpoint, job)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				break
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

func verifyArchive(root *os.Root, reference manifest) error {
	info, err := root.Stat(reference.Ref)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxArchiveBytes {
		return errors.New("archive is missing, oversized or outside the configured root")
	}
	file, err := root.Open(reference.Ref)
	if err != nil {
		return errors.New("cannot open local archive")
	}
	defer file.Close()
	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(file, maxArchiveBytes+1))
	if err != nil || n > maxArchiveBytes || fmt.Sprintf("%x", hasher.Sum(nil)) != reference.SHA256 {
		return errors.New("cannot verify local archive SHA-256")
	}
	return nil
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
	slog.Info("research occurrence confirmed", "job_id", job.ID, "scheduled_at", job.At, "http_status", response.StatusCode)
	return false, nil
}
