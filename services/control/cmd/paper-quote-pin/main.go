package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/papersim"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("local paper quote pin failed", "error", err)
		os.Exit(2)
	}
}

type researchReader interface {
	paperapply.ResearchStore
	GetPaperDispatch(context.Context, string) (jobs.PaperDispatchTask, error)
	Now(context.Context) (time.Time, error)
}

type pinResult struct {
	Version     string     `json:"paper_quote_pin_result_schema_version"`
	Source      string     `json:"source_mode"`
	JobID       string     `json:"job_id"`
	TapeSHA256  string     `json:"tape_sha256"`
	Status      string     `json:"status"`
	Quote       *pinQuote  `json:"quote,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ObservedAt  *time.Time `json:"observed_at,omitempty"`
	AvailableAt *time.Time `json:"available_at,omitempty"`
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-quote-pin", flag.ContinueOnError)
	jobID := flags.String("job", "", "research job ID")
	tapePath := flags.String("tape", "", "owner-pinned synthetic quote tape file")
	digest := flags.String("sha256", "", "SHA-256 of the exact tape bytes")
	directory := flags.String("quote-root", "", "existing directory for quote and job pin")
	requestPath := flags.String("request", "", "bound paper application request if the job has no durable dispatch")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *jobID == "" || *tapePath == "" || *digest == "" || *directory == "" || dsn == "" {
		return errors.New("provide --job, --tape, --sha256, --quote-root and TRADINGBOT_DATABASE_URL")
	}
	raw, err := readPath(*tapePath, maxTapeBytes)
	if err != nil {
		return fmt.Errorf("read synthetic quote tape: %w", err)
	}
	tape, err := decodeTape(raw, *digest, *jobID)
	if err != nil {
		return err
	}
	var requestBytes []byte
	if *requestPath != "" {
		requestBytes, err = readPath(*requestPath, papersim.MaxInputBytes)
		if err != nil {
			return fmt.Errorf("read bound application request: %w", err)
		}
	}
	root, err := os.OpenRoot(*directory)
	if err != nil {
		return fmt.Errorf("open local paper quote root: %w", err)
	}
	defer root.Close()
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		return errors.New("research job storage startup failed; check PostgreSQL configuration")
	}
	defer research.Close()
	result, err := pinCompleted(ctx, research, root, tape, *digest, requestBytes)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func pinCompleted(ctx context.Context, research researchReader, root *os.Root, tape quoteTape, digest string, requestBytes []byte) (pinResult, error) {
	result := pinResult{Version: "0.1", Source: papersim.Source, JobID: tape.JobID, TapeSHA256: digest}
	job, err := research.Get(ctx, tape.JobID)
	if errors.Is(err, jobs.ErrNotFound) {
		result.Status = "awaiting_research"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	switch job.Status {
	case "queued", "leased":
		result.Status = "awaiting_research"
		return result, nil
	case "failed", "expired":
		result.Status = "research_" + job.Status
		return result, nil
	case "completed":
	default:
		return result, errors.New("unrecognized durable research status")
	}
	receipt, err := research.GetResult(ctx, job.ID)
	if err != nil {
		return result, err
	}
	req, err := applicationRequest(ctx, research, tape.JobID, requestBytes)
	if err != nil {
		return result, err
	}
	now, err := research.Now(ctx)
	if err != nil {
		return result, err
	}
	review, err := paperapply.ReviewQuotes(req, job, receipt, now)
	if err != nil {
		return result, err
	}
	result.CompletedAt, result.CheckedAt = &receipt.CompletedAt, &now
	existing, err := readPin(root, tape.JobID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if existing != nil {
		match, err := tape.replay(root, *existing, digest, review, receipt.CompletedAt, now)
		if err != nil {
			return result, err
		}
		return result.withQuote("replayed", *existing, match), nil
	}
	if review.IsHold() {
		result.Status = "research_hold"
		return result, nil
	}
	if !now.Before(review.ExpiresAt()) {
		result.Status = "application_expired"
		return result, nil
	}
	candidate, err := tape.selectQuote(review, receipt.CompletedAt, now)
	if err != nil {
		return result, err
	}
	if candidate == nil {
		result.Status = "awaiting_quote"
		return result, nil
	}
	// Recheck database time after validating candidates and before publication.
	checkedAt, err := research.Now(ctx)
	if err != nil {
		return result, err
	}
	result.CheckedAt = &checkedAt
	if !candidate.available.Before(checkedAt) || !checkedAt.Before(candidate.quote.ExpiresAt) || !checkedAt.Before(review.ExpiresAt()) {
		result.Status = "awaiting_quote"
		return result, nil
	}
	pin := quotePin{Version: "0.1", JobID: tape.JobID, Quote: candidate.reference(digest)}
	created, err := publishPin(ctx, root, pin, candidate.raw)
	if err != nil {
		return result, err
	}
	if !created {
		return result.withQuote("replayed", pin, *candidate), nil
	}
	return result.withQuote("pinned", pin, *candidate), nil
}

func applicationRequest(ctx context.Context, research researchReader, jobID string, raw []byte) (paperapply.Request, error) {
	task, err := research.GetPaperDispatch(ctx, jobID)
	switch {
	case err == nil:
		if task.JobID != jobID {
			return paperapply.Request{}, paperapply.ErrInvalid
		}
		// A scheduled job always uses its pre-bound durable request.
		if len(raw) == 0 {
			raw = task.Request
		}
	case errors.Is(err, jobs.ErrNotFound):
		if len(raw) == 0 {
			return paperapply.Request{}, errors.New("provide --request for a job without durable paper dispatch")
		}
	default:
		return paperapply.Request{}, err
	}
	var req paperapply.Request
	if len(raw) > papersim.MaxInputBytes || contract.Decode(raw, contract.PaperApplication, &req) != nil || req.JobID != jobID || req.Intent != nil {
		return paperapply.Request{}, paperapply.ErrInvalid
	}
	if err == nil {
		canonical, encodeErr := json.Marshal(req)
		if encodeErr != nil || sha256.Sum256(canonical) != task.RequestSHA256 || req.SnapshotContext.AccountID != task.AccountID {
			return paperapply.Request{}, errors.New("paper application request differs from durable dispatch")
		}
	}
	return req, nil
}

func (r pinResult) withQuote(status string, pin quotePin, candidate tapeCandidate) pinResult {
	r.Status, r.Quote = status, &pin.Quote
	observed, available := candidate.quote.ObservedAt.UTC(), candidate.available.UTC()
	r.ObservedAt, r.AvailableAt = &observed, &available
	return r
}
