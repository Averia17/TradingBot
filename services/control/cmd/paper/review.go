package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"tradingbot/control/internal/paper_thesis"
	"tradingbot/control/internal/paper_workflow"
	"tradingbot/control/internal/research_scheduler"
)

type reviewReceipt struct {
	paper_thesis.View
	Catalyst *paper_thesis.Trigger `json:"catalyst_review,omitempty"`
}

func review(ctx context.Context, dsn string, args []string, output io.Writer, now time.Time) error {
	flags := flag.NewFlagSet("paper review", flag.ContinueOnError)
	account := flags.String("account", "", "virtual paper account UUID")
	order := flags.String("order", "", "opening order UUID from paper")
	cutoff := flags.String("as-of", "", "review cutoff in RFC3339, defaults to the current time")
	schedule := flags.String("schedule", "", "frozen schedule containing a catalyst event")
	archiveRoot := flags.String("archive-root", "", "root of the digest-pinned local archive")
	provider := flags.String("event-provider", "", "archive event provider")
	record := flags.String("event-record-id", "", "archive event record ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *account == "" || *order == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --account and --order are required")
	}
	withCatalyst := *schedule != "" || *archiveRoot != "" || *provider != "" || *record != ""
	if withCatalyst && (*schedule == "" || *archiveRoot == "" || *provider == "" || *record == "") {
		return errors.New("catalyst review requires --schedule, --archive-root, --event-provider and --event-record-id")
	}
	asOf, err := parseCutoff(*cutoff, now)
	if err != nil {
		return err
	}
	research, store, err := openPaperStores(ctx, dsn)
	if err != nil {
		return err
	}
	defer research.Close()
	defer store.Close()
	view, err := paper_thesis.Load(ctx, research, store, *account, *order, asOf)
	if err != nil {
		return err
	}
	receipt := reviewReceipt{View: view}
	if withCatalyst {
		input, err := os.Open(*schedule)
		if err != nil {
			return err
		}
		defer input.Close()
		snapshot, err := research_scheduler.Load(input)
		if err != nil {
			return err
		}
		event, err := snapshot.VerifyEvent(*archiveRoot, *provider, *record)
		if err != nil {
			return err
		}
		if opening := view.OpeningIdentity; opening != nil {
			binding := paper_workflow.ResearchSecurityBinding{Symbol: opening.Symbol, SecurityID: opening.SecurityID}
			if err := paper_workflow.VerifyIdentityEvent(*archiveRoot, opening.Registry, binding, opening.Instrument, view.AsOf, event); err != nil {
				return err
			}
		}
		trigger, err := paper_thesis.CatalystReview(view, event)
		if err != nil {
			return err
		}
		receipt.Catalyst = &trigger
	}
	return json.NewEncoder(output).Encode(receipt)
}
