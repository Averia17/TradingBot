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
	"tradingbot/control/internal/paperthesis"
)

func scheduleReview(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-apply schedule-review", flag.ContinueOnError)
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
	if *poll < 0 || *poll > time.Hour || *poll > 0 && *poll < time.Second {
		return errors.New("review poll interval must be zero or between 1s and 1h")
	}
	startup, stopStartup := context.WithTimeout(ctx, 30*time.Second)
	defer stopStartup()
	research, err := jobs.Open(startup, dsn)
	if err != nil {
		return errors.New("research job storage startup failed; check PostgreSQL configuration")
	}
	defer research.Close()
	paper, err := ledger.Open(startup, dsn)
	if err != nil {
		return errors.New("paper ledger startup failed; check PostgreSQL configuration")
	}
	defer paper.Close()
	stopStartup()

	encoder := json.NewEncoder(output)
	for ctx.Err() == nil {
		receipt, err := reviewSchedulePass(ctx, research, paper, *config, *archiveRoot, *maxAge, *registrySHA, *rollForward)
		if err == nil {
			if err := encoder.Encode(receipt); err != nil {
				return err
			}
		} else if *poll == 0 {
			return err
		} else if ctx.Err() == nil {
			slog.Warn("paper review scheduling pass refused", "error", err)
		}
		if *poll == 0 {
			return nil
		}
		timer := time.NewTimer(*poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func reviewSchedulePass(ctx context.Context, research *jobs.Store, paper *ledger.Store, config, root string, maxAge time.Duration, registrySHA string, rollForward bool) (paperthesis.ScheduleReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var request paperthesis.ScheduleRequest
	if err := decodeDocument(config, contract.PaperReviewSchedule, &request); err != nil {
		return paperthesis.ScheduleReceipt{}, fmt.Errorf("read paper review schedule request: %w", err)
	}
	now, err := research.Now(ctx)
	if err != nil {
		return paperthesis.ScheduleReceipt{}, err
	}
	if rollForward {
		return paperthesis.ScheduleReviewWindow(ctx, research, paper, request, root, now, maxAge, registrySHA)
	}
	return paperthesis.ScheduleReviewWithRegistry(ctx, research, paper, request, root, now, maxAge, registrySHA)
}
