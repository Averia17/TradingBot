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

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papersim"
)

func main() {
	intent := flag.String("intent", "", "path to a versioned paper fixture intent JSON")
	quote := flag.String("quote", "", "path to the synthetic quote JSON pinned by the intent")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), *intent, *quote, os.Stdout); err != nil {
		slog.Error("paper fixture replay failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn, intentPath, quotePath string, output io.Writer) error {
	if dsn == "" || intentPath == "" || quotePath == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --intent and --quote are required")
	}
	data, err := readInput(intentPath)
	if err != nil {
		return fmt.Errorf("read paper intent: %w", err)
	}
	intent, err := papersim.ParseIntent(data)
	if err != nil {
		return err
	}
	quote, err := readInput(quotePath)
	if err != nil {
		return fmt.Errorf("read paper quote: %w", err)
	}
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		return errors.New("paper ledger startup failed; check the PostgreSQL configuration")
	}
	defer store.Close()
	receipt, err := papersim.Run(ctx, store, intent, quote, time.Now().UTC())
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(receipt)
}

func readInput(path string) ([]byte, error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > papersim.MaxInputBytes {
		return nil, errors.New("paper fixture must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(input, papersim.MaxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > papersim.MaxInputBytes {
		return nil, errors.New("paper fixture exceeds the file size limit")
	}
	return data, nil
}
