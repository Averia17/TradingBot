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

	"tradingbot/trading-api/internal/jobs"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("research budget command failed", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(output, "research-budget set --day YYYY-MM-DD --max-calls N | research-budget show [--day YYYY-MM-DD]")
		return err
	}
	if len(args) == 0 || (args[0] != "set" && args[0] != "show") {
		return errors.New("choose research-budget set or show. See --help")
	}
	flags := flag.NewFlagSet("research-budget "+args[0], flag.ContinueOnError)
	day := flags.String("day", "", "UTC day YYYY-MM-DD (show defaults to the database's current UTC date)")
	var maxCalls int64
	if args[0] == "set" {
		flags.Int64Var(&maxCalls, "max-calls", -1, "immutable research permit limit for the day; may be zero")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" {
		return errors.New("provide TRADINGBOT_DATABASE_URL and the command flags")
	}
	if args[0] == "set" {
		if *day == "" || maxCalls < 0 {
			return errors.New("set requires --day and a non-negative --max-calls")
		}
		store, err := jobs.Open(ctx, dsn)
		if err != nil {
			return errors.New("research storage startup failed; check PostgreSQL configuration")
		}
		defer store.Close()
		if err := store.SetDailyCallBudget(ctx, *day, maxCalls); err != nil {
			return err
		}
	}
	report, err := jobs.ReadDailyCallBudget(ctx, dsn, *day)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}
