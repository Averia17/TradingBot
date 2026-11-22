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

	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/paper_quotes"
	"tradingbot/control/internal/paper_risk"
	"tradingbot/control/internal/paper_workflow"
)

func dispatch(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper dispatch", flag.ContinueOnError)
	account := flags.String("account", "", "virtual account UUID")
	policyPath := flags.String("policy", "", "path to owner paper risk policy JSON")
	quoteRoot := flags.String("quote-root", "", "root of job-ID quote pins and synthetic quotes")
	archiveRoot := flags.String("archive-root", "", "read-only owner archive containing identity quote tape")
	registrySHA := flags.String("identity-registry-sha256", os.Getenv("TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256"), "active owner identity registry digest")
	tapeSHA := flags.String("identity-quote-tape-sha256", os.Getenv("TRADINGBOT_PAPER_QUOTE_TAPE_SHA256"), "active identity quote tape digest")
	poll := flags.Duration("poll-interval", 0, "process at this interval (1s to 1h); default is one page")
	fixtureURL := flags.String("jupiter-fixture-url", "", "numeric loopback origin for offline Jupiter fixture settlement")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *account == "" || *policyPath == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --account and --policy are required")
	}
	if !validPollInterval(*poll) {
		return errors.New("dispatch poll interval must be zero or between 1s and 1h")
	}
	var fixture *jupiterFixtureClient
	if *fixtureURL != "" {
		var err error
		fixture, err = newJupiterFixtureClient(*fixtureURL)
		if err != nil {
			return err
		}
		defer fixture.http.CloseIdleConnections()
	}
	research, paper, err := openPaperStores(ctx, dsn)
	if err != nil {
		return err
	}
	defer research.Close()
	defer paper.Close()
	var settlementStore paper_workflow.FollowStore = paper
	if fixture != nil {
		settlementStore = jupiterFixtureStore{Store: paper, fixture: fixture}
	}
	encoder := json.NewEncoder(output)
	quotes := localQuoteSource(*quoteRoot)
	bound := boundDispatchOptions{*archiveRoot, *registrySHA, *tapeSHA}
	after := ""
	for ctx.Err() == nil {
		policy, err := readOwnerPolicy(*policyPath)
		if err != nil {
			return err
		}
		after, err = dispatchPage(ctx, research, settlementStore, *account, after, policy, quotes, bound, encoder)
		if *poll == 0 {
			return err
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("paper dispatch page had refused jobs", "account_id", *account, "error", err)
		}
		if !waitForPoll(ctx, *poll) {
			return nil
		}
	}
	return ctx.Err()
}

func dispatchPage(ctx context.Context, research *jobs.Store, paper paper_workflow.FollowStore, accountID, after string,
	policy paper_risk.Policy, quotes paper_workflow.QuoteSource, bound boundDispatchOptions, encoder *json.Encoder) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, paperOperationTimeout)
	defer cancel()
	tasks, err := research.ReadyPaperDispatch(ctx, accountID, after)
	if err != nil {
		return after, err
	}
	var refused error
	for _, task := range tasks {
		after = task.JobID
		if err := ctx.Err(); err != nil {
			return after, err
		}
		result, err := dispatchTask(ctx, research, paper, task, policy, quotes, bound)
		if err != nil {
			refused = errors.Join(refused, fmt.Errorf("paper dispatch job %s: %w", task.JobID, err))
			continue
		}
		if err := encoder.Encode(result); err != nil {
			return after, err
		}
	}
	if len(tasks) < jobs.MaxPaperDispatchPage {
		after = ""
	}
	return after, refused
}

type boundDispatchOptions struct {
	archiveRoot string
	registrySHA string
	tapeSHA     string
}

func (opts boundDispatchOptions) verifier(req paper_workflow.Request) paper_workflow.BoundQuoteVerifier {
	return func(ctx context.Context, raw []byte, completedAt, now, deadline time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		identity := req.Identity
		if opts.archiveRoot == "" || opts.registrySHA != identity.Registry.SHA256 || opts.tapeSHA != identity.QuoteTape.SHA256 {
			return errors.New("paper request differs from the active owner registry or tape")
		}
		root, err := os.OpenRoot(opts.archiveRoot)
		if err != nil {
			return errors.New("owner paper archive is unavailable")
		}
		defer root.Close()
		tape, err := paper_quotes.OpenIdentityTape(root, identity.QuoteTape.Ref, opts.tapeSHA, req.SnapshotContext.AccountID, opts.registrySHA)
		if err != nil {
			return err
		}
		_, err = tape.Verify(raw, completedAt, now, deadline)
		return err
	}
}

func dispatchTask(ctx context.Context, research *jobs.Store, paper paper_workflow.FollowStore, task jobs.PaperDispatchTask,
	policy paper_risk.Policy, quotes paper_workflow.QuoteSource, bound boundDispatchOptions) (paper_workflow.FollowResult, error) {
	return paper_workflow.Dispatch(ctx, research, paper, task, policy, quotes, bound.verifier)
}
