package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const paperOperationTimeout = 30 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Getenv("TRADINGBOT_OFFLINE_TEST_DATABASE_URL"), os.Args[1:], os.Stdout, time.Now)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("paper command failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer, clock func() time.Time) error {
	if len(args) == 0 {
		return errors.New("use paper snapshot, apply, follow, orders, reconcile, refusals, review, schedule-entry or schedule-review")
	}
	command, args := args[0], args[1:]
	switch command {
	case "schedule-entry":
		return scheduleEntry(ctx, dsn, args, output)
	case "schedule-review":
		return scheduleReview(ctx, dsn, args, output)
	case "follow":
		return follow(ctx, dsn, args, output)
	case "orders", "dispatch":
		return runOrders(ctx, dsn, args, output)
	}
	ctx, cancel := context.WithTimeout(ctx, paperOperationTimeout)
	defer cancel()
	switch command {
	case "snapshot":
		return snapshot(ctx, dsn, args, output, clock().UTC())
	case "apply":
		return apply(ctx, dsn, args, output, clock)
	case "reconcile":
		return reconcile(ctx, dsn, args, output)
	case "refusals":
		return refusals(ctx, dsn, args, output)
	case "review":
		return review(ctx, dsn, args, output, clock().UTC())
	default:
		return errors.New("unknown paper command")
	}
}

func validPollInterval(interval time.Duration) bool {
	return interval == 0 || interval >= time.Second && interval <= time.Hour
}

func waitForPoll(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
