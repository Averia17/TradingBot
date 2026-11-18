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
	"reflect"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperquotes"
	"tradingbot/control/internal/paperrisk"
)

func dispatch(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-apply dispatch", flag.ContinueOnError)
	account := flags.String("account", "", "virtual account UUID")
	policyPath := flags.String("policy", "", "path to owner paper risk policy JSON")
	quoteRoot := flags.String("quote-root", "", "root of job-ID quote pins and synthetic quotes")
	archiveRoot := flags.String("archive-root", "", "read-only owner archive containing identity quote tape")
	registrySHA := flags.String("identity-registry-sha256", os.Getenv("TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256"), "active owner identity registry digest")
	tapeSHA := flags.String("identity-quote-tape-sha256", os.Getenv("TRADINGBOT_PAPER_QUOTE_TAPE_SHA256"), "active identity quote tape digest")
	poll := flags.Duration("poll-interval", 0, "process at this interval (1s to 1h); default is one page")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *account == "" || *policyPath == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --account and --policy are required")
	}
	if *poll < 0 || *poll > time.Hour || (*poll > 0 && *poll < time.Second) {
		return errors.New("dispatch poll interval must be zero or between 1s and 1h")
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
	quotes := localQuoteSource(*quoteRoot)
	bound := boundDispatchOptions{*archiveRoot, *registrySHA, *tapeSHA}
	after := ""
	for ctx.Err() == nil {
		policy, err := readOwnerPolicy(*policyPath)
		if err != nil {
			return err
		}
		after, err = dispatchPage(ctx, research, paper, *account, after, policy, quotes, bound, encoder)
		if *poll == 0 {
			return err
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("paper dispatch page had refused jobs", "account_id", *account, "error", err)
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

func dispatchPage(ctx context.Context, research *jobs.Store, paper *ledger.Store, accountID, after string,
	policy paperrisk.Policy, quotes paperapply.QuoteSource, bound boundDispatchOptions, encoder *json.Encoder) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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

func (opts boundDispatchOptions) verifier(req paperapply.Request) paperapply.BoundQuoteVerifier {
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
		tape, err := paperquotes.OpenIdentityTape(root, identity.QuoteTape.Ref, opts.tapeSHA, req.SnapshotContext.AccountID, opts.registrySHA)
		if err != nil {
			return err
		}
		_, err = tape.Verify(raw, completedAt, now, deadline)
		return err
	}
}

func dispatchTask(ctx context.Context, research *jobs.Store, paper *ledger.Store, task jobs.PaperDispatchTask,
	policy paperrisk.Policy, quotes paperapply.QuoteSource, bound boundDispatchOptions) (paperapply.FollowResult, error) {
	var req paperapply.Request
	if err := contract.Decode(task.Request, contract.PaperApplication, &req); err != nil ||
		req.JobID != task.JobID || req.SnapshotContext.AccountID != task.AccountID || req.Intent != nil {
		return paperapply.FollowResult{}, errors.New("invalid persisted paper dispatch binding")
	}
	if !reflect.DeepEqual(policy, req.SnapshotContext.Policy) {
		// A policy rotation affects new admissions. A durable application
		// still needs to settle or acknowledge its original binding.
		admitted, err := paper.GetApplication(ctx, task.JobID)
		if errors.Is(err, ledger.ErrNotFound) {
			return paperapply.FollowResult{}, errors.New("paper snapshot policy differs from the owner policy")
		}
		if err != nil {
			return paperapply.FollowResult{}, err
		}
		if admitted.AccountID != task.AccountID || admitted.SnapshotVersion != req.SnapshotContext.SnapshotVersion ||
			admitted.RequestSHA256 != task.RequestSHA256 {
			return paperapply.FollowResult{}, ledger.ErrConflict
		}
		quotes = nil
	}
	var result paperapply.FollowResult
	var err error
	if req.Identity == nil {
		result, err = followPass(ctx, research, paper, req, quotes)
	} else {
		now, clockErr := research.Now(ctx)
		if clockErr != nil {
			return paperapply.FollowResult{}, clockErr
		}
		result, err = paperapply.FollowBound(ctx, research, paper, req, quotes, bound.verifier(req), now)
	}
	if err != nil || result.Pending() {
		return result, err
	}
	if err := research.FinishPaperDispatch(ctx, task, result.Status); err != nil {
		return paperapply.FollowResult{}, err
	}
	return result, nil
}
