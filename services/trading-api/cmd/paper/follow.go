package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/paper_quotes"
	"tradingbot/trading-api/internal/paper_risk"
	"tradingbot/trading-api/internal/paper_workflow"
)

func follow(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper follow", flag.ContinueOnError)
	requestPath := flags.String("request", "", "path to a bound paper application request")
	policyPath := flags.String("policy", "", "path to owner paper risk policy JSON")
	quoteRoot := flags.String("quote-root", "", "root of job-ID quote pins and synthetic quotes")
	poll := flags.Duration("poll-interval", 0, "retry pending work at this interval (1s to 1h); default is one pass")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *requestPath == "" || *policyPath == "" {
		return errors.New("--request and --policy are required")
	}
	if !validPollInterval(*poll) {
		return errors.New("follow poll interval must be zero or between 1s and 1h")
	}
	var input paper_workflow.Request
	if err := decodeDocument(*requestPath, contract.PaperApplication, &input); err != nil {
		return fmt.Errorf("read paper application request: %w", err)
	}
	if input.Intent != nil {
		return errors.New("follow requires a request without a preselected intent")
	}
	if err := checkOwnerPolicy(*policyPath, input.SnapshotContext.Policy); err != nil {
		return err
	}
	research, paper, closeServices, err := openPaperServices(ctx, dsn)
	if err != nil {
		return err
	}
	defer closeServices()
	encoder := json.NewEncoder(output)
	quotes := localQuoteSource(*quoteRoot)
	for ctx.Err() == nil {
		if err := checkOwnerPolicy(*policyPath, input.SnapshotContext.Policy); err != nil {
			return err
		}
		result, err := followPass(ctx, research, paper, input, quotes)
		if err == nil {
			if err := encoder.Encode(result); err != nil {
				return err
			}
			if !result.Pending() {
				return nil
			}
		} else if *poll == 0 || errors.Is(err, accounts.ErrConflict) {
			return err
		} else if ctx.Err() == nil {
			slog.Warn("paper application pass refused", "job_id", input.JobID, "error", err)
		}
		if *poll == 0 || !waitForPoll(ctx, *poll) {
			return nil
		}
	}
	return ctx.Err()
}

func checkOwnerPolicy(path string, expected paper_risk.Policy) error {
	policy, err := readOwnerPolicy(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(policy, expected) {
		return errors.New("paper snapshot policy differs from the owner policy")
	}
	return nil
}

func readOwnerPolicy(path string) (paper_risk.Policy, error) {
	var policy paper_risk.Policy
	if err := decodeDocument(path, contract.PaperRiskPolicy, &policy); err != nil {
		return paper_risk.Policy{}, fmt.Errorf("read owner paper policy: %w", err)
	}
	return policy, policy.Validate()
}

func followPass(ctx context.Context, research researchService, paper accountService, input paper_workflow.Request, quotes paper_workflow.QuoteSource) (paper_workflow.FollowResult, error) {
	ctx, cancel := context.WithTimeout(ctx, paperOperationTimeout)
	defer cancel()
	now, err := research.Now(ctx)
	if err != nil {
		return paper_workflow.FollowResult{}, err
	}
	return paper_workflow.Follow(ctx, research, paper, input, quotes, now)
}

func localQuoteSource(directory string) paper_workflow.QuoteSource {
	return func(ctx context.Context, jobID string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if directory == "" {
			return nil, paper_workflow.ErrQuoteMissing
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			return nil, fmt.Errorf("open local paper quote root: %w", err)
		}
		defer root.Close()
		raw, err := readRootDocument(root, jobID+".json")
		if err != nil {
			return nil, err
		}
		pin, err := paper_quotes.DecodeQuotePin(raw, jobID)
		if err != nil {
			return nil, err
		}
		body, err := readRootDocument(root, pin.Quote.Ref)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != pin.Quote.SHA256 {
			return nil, errors.New("local paper quote differs from its pinned SHA-256")
		}
		return body, nil
	}
}

func readRootDocument(root *os.Root, name string) ([]byte, error) {
	info, err := root.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, paper_workflow.ErrQuoteMissing
	}
	if err != nil {
		return nil, fmt.Errorf("inspect local paper quote data: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("paper quote data must be a regular file")
	}
	file, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, paper_workflow.ErrQuoteMissing
	}
	if err != nil {
		return nil, fmt.Errorf("open local paper quote data: %w", err)
	}
	defer file.Close()
	return readPaperDocument(file)
}
