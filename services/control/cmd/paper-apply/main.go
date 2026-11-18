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
	"os/signal"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
)

const maxDocumentBytes = 256 * 1024

type snapshotBundle struct {
	Context   paperapply.SnapshotContext `json:"snapshot_context"`
	Portfolio contract.Portfolio         `json:"portfolio"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout, time.Now)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("paper command failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer, clock func() time.Time) error {
	if len(args) == 0 {
		return errors.New("use paper-apply snapshot, apply, follow, dispatch, reconcile, review, schedule-entry or schedule-review")
	}
	if args[0] == "schedule-entry" {
		return scheduleEntry(ctx, dsn, args[1:], output)
	}
	if args[0] == "schedule-review" {
		return scheduleReview(ctx, dsn, args[1:], output)
	}
	if args[0] == "follow" {
		return follow(ctx, dsn, args[1:], output)
	}
	if args[0] == "dispatch" {
		return dispatch(ctx, dsn, args[1:], output)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch args[0] {
	case "snapshot":
		return snapshot(ctx, dsn, args[1:], output, clock().UTC())
	case "apply":
		return apply(ctx, dsn, args[1:], output, clock)
	case "reconcile":
		return reconcile(ctx, dsn, args[1:], output)
	case "review":
		return review(ctx, dsn, args[1:], output, clock().UTC())
	default:
		return errors.New("unknown paper-apply command")
	}
}

func snapshot(ctx context.Context, dsn string, args []string, output io.Writer, now time.Time) error {
	flags := flag.NewFlagSet("paper-apply snapshot", flag.ContinueOnError)
	accountID := flags.String("account", "", "virtual paper account UUID")
	policyPath := flags.String("policy", "", "path to owner paper risk policy JSON")
	marksPath := flags.String("marks", "", "path to the JSON array of observed marks")
	cutoff := flags.String("as-of", "", "snapshot cutoff in RFC3339, defaults to the current time")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *accountID == "" || *policyPath == "" || *marksPath == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --account, --policy and --marks are required")
	}
	asOf, err := parseCutoff(*cutoff, now)
	if err != nil {
		return err
	}
	// Keep the snapshot ID stable through the Python research datetime contract.
	asOf = asOf.Truncate(time.Microsecond)
	input := paperapply.SnapshotContext{AccountID: *accountID}
	if err := decodeDocument(*policyPath, contract.PaperRiskPolicy, &input.Policy); err != nil {
		return fmt.Errorf("read paper policy: %w", err)
	}
	if err := decodeDocument(*marksPath, contract.PaperMarks, &input.Marks); err != nil {
		return fmt.Errorf("read paper marks: %w", err)
	}
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		return errors.New("paper ledger startup failed; check PostgreSQL configuration")
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
	portfolio, err := paperapply.Snapshot(account, input, asOf.UTC())
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

func apply(ctx context.Context, dsn string, args []string, output io.Writer, clock func() time.Time) error {
	flags := flag.NewFlagSet("paper-apply apply", flag.ContinueOnError)
	policyPath := flags.String("policy", "", "path to owner paper risk policy JSON")
	requestPath := flags.String("request", "", "path to the research decision application request JSON")
	quotePath := flags.String("quote", "", "path to the exact synthetic quote bytes for a trade")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *policyPath == "" || *requestPath == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --policy and --request are required")
	}
	var input paperapply.Request
	if err := decodeDocument(*requestPath, contract.PaperApplication, &input); err != nil {
		return fmt.Errorf("read paper application request: %w", err)
	}
	if err := checkOwnerPolicy(*policyPath, input.SnapshotContext.Policy); err != nil {
		return err
	}
	var quote []byte
	if *quotePath != "" {
		var err error
		quote, err = readDocument(*quotePath)
		if err != nil {
			return fmt.Errorf("read paper quote: %w", err)
		}
	}
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		return errors.New("research job storage startup failed; check PostgreSQL configuration")
	}
	defer research.Close()
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		return errors.New("paper ledger startup failed; check PostgreSQL configuration")
	}
	defer store.Close()
	receipt, err := paperapply.Apply(ctx, research, store, input, quote, clock().UTC())
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(receipt)
}

func decodeDocument(path string, validator *jsonschema.Schema, target any) error {
	data, err := readDocument(path)
	if err != nil {
		return err
	}
	return contract.Decode(data, validator, target)
}

func readDocument(path string) ([]byte, error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	return readPaperDocument(input)
}

func readPaperDocument(input *os.File) ([]byte, error) {
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxDocumentBytes {
		return nil, errors.New("paper document must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(input, maxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDocumentBytes || !utf8.Valid(data) {
		return nil, errors.New("paper document exceeds the size limit or is not valid UTF-8")
	}
	return data, nil
}
