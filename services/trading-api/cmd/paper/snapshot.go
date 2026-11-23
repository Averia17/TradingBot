package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/paper_workflow"
)

type snapshotBundle struct {
	Context   paper_workflow.SnapshotContext `json:"snapshot_context"`
	Portfolio contract.Portfolio             `json:"portfolio"`
}

func snapshot(ctx context.Context, dsn string, args []string, output io.Writer, now time.Time) error {
	flags := flag.NewFlagSet("paper snapshot", flag.ContinueOnError)
	accountID := flags.String("account", "", "virtual paper account UUID")
	policyPath := flags.String("policy", "", "path to owner paper risk policy JSON")
	marksPath := flags.String("marks", "", "path to the JSON array of observed marks")
	cutoff := flags.String("as-of", "", "snapshot cutoff in RFC3339, defaults to the current time")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *accountID == "" || *policyPath == "" || *marksPath == "" {
		return errors.New("--account, --policy and --marks are required")
	}
	asOf, err := parseCutoff(*cutoff, now)
	if err != nil {
		return err
	}
	// Keep the snapshot ID stable through the Python research datetime contract.
	asOf = asOf.Truncate(time.Microsecond)
	input := paper_workflow.SnapshotContext{AccountID: *accountID}
	if err := decodeDocument(*policyPath, contract.PaperRiskPolicy, &input.Policy); err != nil {
		return fmt.Errorf("read paper policy: %w", err)
	}
	if err := decodeDocument(*marksPath, contract.PaperMarks, &input.Marks); err != nil {
		return fmt.Errorf("read paper marks: %w", err)
	}
	store, err := openAccountService(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	account, err := store.GetAccount(ctx, input.AccountID)
	if err != nil {
		return err
	}
	events, err := store.Events(ctx, account.ID, account.Version-1, 1)
	if err != nil {
		return err
	}
	if len(events) != 1 || events[0].AccountVersion != account.Version || events[0].RecordedAt.After(asOf) {
		return errors.New("current paper account state was unavailable at the snapshot cutoff")
	}
	input.SnapshotVersion = account.Version
	portfolio, err := paper_workflow.Snapshot(account, input, asOf.UTC())
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(snapshotBundle{Context: input, Portfolio: portfolio})
}

func parseCutoff(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		return now, nil
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || at.IsZero() || at.After(now) {
		return time.Time{}, errors.New("invalid paper cutoff")
	}
	return at.UTC(), nil
}
