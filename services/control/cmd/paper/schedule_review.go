package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_thesis"
)

func scheduleReview(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper schedule-review", flag.ContinueOnError)
	config := flags.String("config", "", "path to the review schedule request")
	archiveRoot := flags.String("archive-root", "", "root of the digest-pinned local archive")
	registrySHA := flags.String("identity-registry-sha256", os.Getenv("TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256"), "owner-pinned registry SHA-256 for stable paper review")
	rollForward := flags.Bool("roll-forward", false, "acknowledge a matching durable review in a newer stable window")
	maxAge := flags.Duration("max-research-age", time.Hour, "maximum age of a new research snapshot, as configured for Control")
	poll := flags.Duration("poll-interval", 0, "reload the request at this interval (1s to 1h); default is one pass")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *config == "" || *archiveRoot == "" || *maxAge <= 0 {
		return errors.New("TRADINGBOT_DATABASE_URL, --config, --archive-root and a positive --max-research-age are required")
	}
	if !validPollInterval(*poll) {
		return errors.New("review poll interval must be zero or between 1s and 1h")
	}
	research, paper, err := openPaperStores(ctx, dsn)
	if err != nil {
		return err
	}
	defer research.Close()
	defer paper.Close()

	encoder := json.NewEncoder(output)
	for ctx.Err() == nil {
		receipt, err := scheduleReviewPass(ctx, research, paper, *config, *archiveRoot, *maxAge, *registrySHA, *rollForward)
		if err == nil {
			if err := encoder.Encode(receipt); err != nil {
				return err
			}
		} else if *poll == 0 {
			return err
		} else if ctx.Err() == nil {
			slog.Warn("paper review scheduling pass refused", "error", err)
		}
		if *poll == 0 || !waitForPoll(ctx, *poll) {
			return nil
		}
	}
	return ctx.Err()
}

func scheduleReviewPass(ctx context.Context, research *jobs.Store, paper *ledger.Store, config, root string, maxAge time.Duration, registrySHA string, rollForward bool) (paper_thesis.ScheduleReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, paperOperationTimeout)
	defer cancel()
	var request paper_thesis.ScheduleRequest
	if err := decodeDocument(config, contract.PaperReviewSchedule, &request); err != nil {
		return paper_thesis.ScheduleReceipt{}, fmt.Errorf("read paper review schedule request: %w", err)
	}
	now, err := research.Now(ctx)
	if err != nil {
		return paper_thesis.ScheduleReceipt{}, err
	}
	if rollForward {
		return paper_thesis.ScheduleReviewWindow(ctx, research, paper, request, root, now, maxAge, registrySHA)
	}
	return paper_thesis.ScheduleReviewWithRegistry(ctx, research, paper, request, root, now, maxAge, registrySHA)
}
