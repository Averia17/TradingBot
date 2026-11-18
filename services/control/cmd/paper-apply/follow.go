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
	"io/fs"
	"log/slog"
	"os"
	"reflect"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperrisk"
)

type quotePin struct {
	Version string `json:"paper_quote_pin_schema_version"`
	JobID   string `json:"job_id"`
	Quote   struct {
		Ref    string `json:"ref"`
		SHA256 string `json:"sha256"`
	} `json:"quote"`
}

func follow(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-apply follow", flag.ContinueOnError)
	requestPath := flags.String("request", "", "path to a bound paper application request")
	policyPath := flags.String("policy", "", "path to owner paper risk policy JSON")
	quoteRoot := flags.String("quote-root", "", "root of job-ID quote pins and synthetic quotes")
	poll := flags.Duration("poll-interval", 0, "retry pending work at this interval (1s to 1h); default is one pass")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *requestPath == "" || *policyPath == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --request and --policy are required")
	}
	if *poll < 0 || *poll > time.Hour || (*poll > 0 && *poll < time.Second) {
		return errors.New("follow poll interval must be zero or between 1s and 1h")
	}
	var input paperapply.Request
	if err := decodeDocument(*requestPath, contract.PaperApplication, &input); err != nil {
		return fmt.Errorf("read paper application request: %w", err)
	}
	if input.Intent != nil {
		return errors.New("follow requires a request without a preselected intent")
	}
	if err := checkOwnerPolicy(*policyPath, input.SnapshotContext.Policy); err != nil {
		return err
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
		} else if *poll == 0 || errors.Is(err, ledger.ErrConflict) {
			return err
		} else if ctx.Err() == nil {
			slog.Warn("paper application pass refused", "job_id", input.JobID, "error", err)
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

func checkOwnerPolicy(path string, expected paperrisk.Policy) error {
	policy, err := readOwnerPolicy(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(policy, expected) {
		return errors.New("paper snapshot policy differs from the owner policy")
	}
	return nil
}

func readOwnerPolicy(path string) (paperrisk.Policy, error) {
	var policy paperrisk.Policy
	if err := decodeDocument(path, contract.PaperRiskPolicy, &policy); err != nil {
		return paperrisk.Policy{}, fmt.Errorf("read owner paper policy: %w", err)
	}
	return policy, policy.Validate()
}

func followPass(ctx context.Context, research *jobs.Store, paper *ledger.Store, input paperapply.Request, quotes paperapply.QuoteSource) (paperapply.FollowResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now, err := research.Now(ctx)
	if err != nil {
		return paperapply.FollowResult{}, err
	}
	return paperapply.Follow(ctx, research, paper, input, quotes, now)
}

func localQuoteSource(directory string) paperapply.QuoteSource {
	return func(ctx context.Context, jobID string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if directory == "" {
			return nil, paperapply.ErrQuoteMissing
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
		var pin quotePin
		if contract.Decode(raw, contract.PaperQuotePin, &pin) != nil || pin.JobID != jobID || pin.Quote.Ref == "." || !fs.ValidPath(pin.Quote.Ref) {
			return nil, errors.New("invalid local paper quote pin")
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
		return nil, paperapply.ErrQuoteMissing
	}
	if err != nil {
		return nil, fmt.Errorf("inspect local paper quote data: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("paper quote data must be a regular file")
	}
	file, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, paperapply.ErrQuoteMissing
	}
	if err != nil {
		return nil, fmt.Errorf("open local paper quote data: %w", err)
	}
	defer file.Close()
	return readPaperDocument(file)
}
