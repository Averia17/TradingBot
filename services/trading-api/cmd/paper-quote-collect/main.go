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
	"path/filepath"
	"syscall"
	"time"

	"tradingbot/trading-api/internal/paper_quotes"
	"tradingbot/trading-api/internal/quote_collect"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("synthetic paper quote collection failed", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-quote-collect", flag.ContinueOnError)
	request := flags.String("request", "", "local synthetic quote collection request")
	source := flags.String("source", "", "loopback HTTP fixture endpoint")
	out := flags.String("out", "", "new paper quote tape file")
	replayTime := flags.String("as-of", "", "fixture clock in RFC3339 format (defaults to current time)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *request == "" || *source == "" || *out == "" {
		return errors.New("provide --request, --source and --out")
	}
	var asOf time.Time
	if *replayTime != "" {
		var err error
		asOf, err = time.Parse(time.RFC3339Nano, *replayTime)
		if err != nil || asOf.IsZero() {
			return errors.New("--as-of must be a nonzero RFC3339 timestamp")
		}
	}
	raw, err := readConfig(*request)
	if err != nil {
		return fmt.Errorf("read collection request: %w", err)
	}
	config, err := quote_collect.ParseConfig(raw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tape, err := quote_collect.Collect(ctx, *source, config, asOf)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishTape(*out, tape.Raw); err != nil {
		return fmt.Errorf("publish synthetic tape: %w", err)
	}
	return json.NewEncoder(output).Encode(struct {
		TapeSHA256 string `json:"tape_sha256"`
	}{TapeSHA256: tape.SHA256})
}

func readConfig(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > quote_collect.MaxConfigBytes {
		return nil, errors.New("collection request must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, quote_collect.MaxConfigBytes+1))
}

func publishTape(path string, raw []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	return paper_quotes.PublishNewFile(root, filepath.Base(path), raw)
}
