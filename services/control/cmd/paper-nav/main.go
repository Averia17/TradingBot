package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papernav"
)

var errIncomplete = errors.New("paper NAV has dates without complete marks")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errIncomplete):
		slog.Warn(err.Error())
		os.Exit(1)
	default:
		slog.Error("paper NAV report failed", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-nav", flag.ContinueOnError)
	path := flags.String("input", "", "paper NAV input file")
	digest := flags.String("sha256", "", "SHA-256 of the exact input bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" || *digest == "" || dsn == "" {
		return errors.New("provide --input, --sha256 and TRADINGBOT_DATABASE_URL")
	}
	info, err := os.Stat(*path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > papernav.MaxInputBytes {
		return errors.New("paper NAV input must be a bounded regular file")
	}
	inputFile, err := os.Open(*path)
	if err != nil {
		return errors.New("open paper NAV input failed")
	}
	defer inputFile.Close()
	raw, err := io.ReadAll(io.LimitReader(inputFile, papernav.MaxInputBytes+1))
	if err != nil {
		return errors.New("read paper NAV input failed")
	}
	input, err := papernav.Parse(raw, *digest)
	if err != nil {
		return err
	}
	var report papernav.Report
	err = ledger.WithReadSnapshot(ctx, dsn, func(snapshot ledger.ReadSnapshot) error {
		var err error
		report, err = papernav.Build(ctx, snapshot, input, snapshot.Cutoff())
		return err
	})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	if report.Status == "incomplete" {
		return errIncomplete
	}
	return nil
}
