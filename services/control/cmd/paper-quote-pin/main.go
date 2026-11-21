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
	"tradingbot/control/internal/paper_quotes"
	"tradingbot/control/internal/paper_sim"
	"tradingbot/control/internal/paper_workflow"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("local paper quote pin failed", "error", err)
		os.Exit(2)
	}
}

type researchReader interface {
	paper_workflow.ResearchStore
	GetPaperDispatch(context.Context, string) (jobs.PaperDispatchTask, error)
	Now(context.Context) (time.Time, error)
}

type pinResult struct {
	Version     string                       `json:"paper_quote_pin_result_schema_version"`
	Source      string                       `json:"source_mode"`
	JobID       string                       `json:"job_id"`
	TapeSHA256  string                       `json:"tape_sha256"`
	Status      string                       `json:"status"`
	Quote       *paper_quotes.QuoteReference `json:"quote,omitempty"`
	CompletedAt *time.Time                   `json:"completed_at,omitempty"`
	ObservedAt  *time.Time                   `json:"observed_at,omitempty"`
	AvailableAt *time.Time                   `json:"available_at,omitempty"`
	CheckedAt   *time.Time                   `json:"checked_at,omitempty"`
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-quote-pin", flag.ContinueOnError)
	jobID := flags.String("job", "", "research job ID")
	accountID := flags.String("account", "", "virtual account UUID for durable paper dispatch")
	tapePath := flags.String("tape", "", "owner-pinned synthetic quote tape file")
	digest := flags.String("sha256", "", "SHA-256 of the exact tape bytes")
	directory := flags.String("quote-root", "", "existing directory for quote and job pin")
	requestPath := flags.String("request", "", "bound paper application request if the job has no durable dispatch")
	poll := flags.Duration("poll-interval", 0, "account polling interval (1s to 1h); default is one page")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*jobID == "") == (*accountID == "") || *tapePath == "" || *digest == "" || *directory == "" || dsn == "" {
		return errors.New("provide one of --job or --account with --tape, --sha256, --quote-root and TRADINGBOT_DATABASE_URL")
	}
	if *accountID != "" && *requestPath != "" || *jobID != "" && *poll != 0 {
		return errors.New("--request requires --job and --poll-interval requires --account")
	}
	if *poll != 0 && (*poll < time.Second || *poll > time.Hour) {
		return errors.New("poll interval must be zero or between 1s and 1h")
	}
	tape, err := paper_quotes.OpenTape(*tapePath, *digest, *jobID, *accountID)
	if err != nil {
		return fmt.Errorf("open synthetic quote tape: %w", err)
	}
	var requestBytes []byte
	if *requestPath != "" {
		requestBytes, err = readPath(*requestPath, paper_sim.MaxInputBytes)
		if err != nil {
			return fmt.Errorf("read bound application request: %w", err)
		}
	}
	root, err := os.OpenRoot(*directory)
	if err != nil {
		return fmt.Errorf("open local paper quote root: %w", err)
	}
	defer root.Close()
	startup, stopStartup := context.WithTimeout(ctx, 30*time.Second)
	research, err := jobs.Open(startup, dsn)
	stopStartup()
	if err != nil {
		return errors.New("research job storage startup failed; check PostgreSQL configuration")
	}
	defer research.Close()
	encoder := json.NewEncoder(output)
	if *accountID != "" {
		return runAccount(ctx, research, root, tape, *poll, encoder)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := pinCompleted(ctx, research, root, tape, *jobID, requestBytes)
	if err != nil {
		return err
	}
	return encoder.Encode(result)
}

func pinCompleted(ctx context.Context, research researchReader, root *os.Root, tape *paper_quotes.Tape, jobID string, requestBytes []byte) (pinResult, error) {
	result := pinResult{Version: "0.1", Source: paper_sim.Source, JobID: jobID, TapeSHA256: tape.SHA256()}
	job, err := research.Get(ctx, jobID)
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
	req, err := applicationRequest(ctx, research, jobID, requestBytes)
	if err != nil {
		return result, err
	}
	if err := checkTapeBinding(tape, req); err != nil {
		return result, err
	}
	now, err := research.Now(ctx)
	if err != nil {
		return result, err
	}
	review, err := paper_workflow.ReviewQuotes(req, job, receipt, now)
	if err != nil {
		return result, err
	}
	result.CompletedAt, result.CheckedAt = &receipt.CompletedAt, &now
	existing, err := readQuotePin(root, jobID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if existing != nil {
		match, err := replayPublished(root, tape, *existing, review, receipt.CompletedAt, now)
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
	candidate := tape.Select(receipt.CompletedAt, now, review.ExpiresAt(), review.Authorize)
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
	if !candidate.AvailableAt.Before(checkedAt) || !checkedAt.Before(candidate.Quote.ExpiresAt) || !checkedAt.Before(review.ExpiresAt()) {
		result.Status = "awaiting_quote"
		return result, nil
	}
	pin := paper_quotes.QuotePin{Version: "0.1", JobID: jobID, Quote: quoteReference(tape, jobID, candidate.SHA256)}
	created, err := publishQuotePin(ctx, root, pin, candidate.Raw)
	if err != nil {
		return result, err
	}
	if !created {
		return result.withQuote("replayed", pin, *candidate), nil
	}
	return result.withQuote("pinned", pin, *candidate), nil
}

func checkTapeBinding(tape *paper_quotes.Tape, req paper_workflow.Request) error {
	if tape.AccountID() != "" && req.SnapshotContext.AccountID != tape.AccountID() {
		return errors.New("durable paper dispatch differs from the synthetic tape account")
	}
	if req.Identity == nil {
		if tape.IdentityRegistrySHA256() != "" {
			return errors.New("security-bound tape requires security-bound paper dispatch")
		}
	} else if tape.IdentityRegistrySHA256() != req.Identity.Registry.SHA256 || tape.SHA256() != req.Identity.QuoteTape.SHA256 {
		return errors.New("synthetic tape differs from the durable paper dispatch tape or registry pin")
	}
	return nil
}

func applicationRequest(ctx context.Context, research researchReader, jobID string, raw []byte) (paper_workflow.Request, error) {
	explicit := len(raw) != 0
	task, err := research.GetPaperDispatch(ctx, jobID)
	switch {
	case err == nil:
		if task.JobID != jobID {
			return paper_workflow.Request{}, paper_workflow.ErrInvalid
		}
		// A scheduled job always uses its pre-bound durable request.
		if len(raw) == 0 {
			raw = task.Request
		}
	case errors.Is(err, jobs.ErrNotFound):
		if len(raw) == 0 {
			return paper_workflow.Request{}, errors.New("provide --request for a job without durable paper dispatch")
		}
	default:
		return paper_workflow.Request{}, err
	}
	var req paper_workflow.Request
	if len(raw) > paper_sim.MaxInputBytes || contract.Decode(raw, contract.PaperApplication, &req) != nil || req.JobID != jobID || req.Intent != nil {
		return paper_workflow.Request{}, paper_workflow.ErrInvalid
	}
	if req.Identity != nil && (err != nil || explicit) {
		return paper_workflow.Request{}, errors.New("security-bound quote pin requires the durable paper dispatch request")
	}
	if err == nil {
		canonical, encodeErr := json.Marshal(req)
		if encodeErr != nil || sha256.Sum256(canonical) != task.RequestSHA256 || req.SnapshotContext.AccountID != task.AccountID {
			return paper_workflow.Request{}, errors.New("paper application request differs from durable dispatch")
		}
	}
	return req, nil
}

func (r pinResult) withQuote(status string, pin paper_quotes.QuotePin, candidate paper_quotes.Observation) pinResult {
	r.Status, r.Quote = status, &pin.Quote
	observed, available := candidate.Quote.ObservedAt.UTC(), candidate.AvailableAt.UTC()
	r.ObservedAt, r.AvailableAt = &observed, &available
	return r
}
