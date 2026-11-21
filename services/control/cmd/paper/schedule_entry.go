package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_workflow"
)

func scheduleEntry(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper schedule-entry", flag.ContinueOnError)
	config := flags.String("config", "", "path to the owner entry schedule request")
	selection := flags.String("selection", "", "path to the screened research receipt")
	archiveRoot := flags.String("archive-root", "", "root of the digest-pinned local archive")
	registrySHA := flags.String("identity-registry-sha256", os.Getenv("TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256"), "owner-pinned registry SHA-256 for stable paper entry")
	maxAge := flags.Duration("max-research-age", time.Hour, "maximum age of a new research snapshot, as configured for Control")
	poll := flags.Duration("poll-interval", 0, "retry a future occurrence at this interval (1s to 1h); default is one pass")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *config == "" || *selection == "" || *archiveRoot == "" || *maxAge <= 0 {
		return errors.New("TRADINGBOT_DATABASE_URL, --config, --selection, --archive-root and a positive --max-research-age are required")
	}
	if !validPollInterval(*poll) {
		return errors.New("entry poll interval must be zero or between 1s and 1h")
	}
	var request paper_workflow.EntrySchedule
	if err := decodeDocument(*config, contract.PaperEntrySchedule, &request); err != nil {
		return fmt.Errorf("read paper entry schedule request: %w", err)
	}
	raw, err := readDocument(*selection)
	if err != nil {
		return fmt.Errorf("read screened research receipt: %w", err)
	}
	research, paper, err := openPaperStores(ctx, dsn)
	if err != nil {
		return err
	}
	defer research.Close()
	defer paper.Close()

	for ctx.Err() == nil {
		receipt, err := scheduleEntryPass(ctx, research, paper, request, raw, *archiveRoot, *maxAge, *registrySHA)
		if err != nil {
			return err
		}
		if *poll == 0 || receipt.Status != "not_due" {
			return json.NewEncoder(output).Encode(receipt)
		}
		if !waitForPoll(ctx, *poll) {
			return nil
		}
	}
	return ctx.Err()
}

func scheduleEntryPass(ctx context.Context, research *jobs.Store, paper *ledger.Store, request paper_workflow.EntrySchedule, raw []byte,
	archiveRoot string, maxAge time.Duration, registrySHA string) (paper_workflow.EntryReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, paperOperationTimeout)
	defer cancel()
	now, err := research.Now(ctx)
	if err != nil {
		return paper_workflow.EntryReceipt{}, err
	}
	return paper_workflow.ScheduleEntryWithRegistry(ctx, research, paper, request, raw, archiveRoot, now, maxAge, registrySHA)
}
