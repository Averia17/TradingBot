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

	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperthesis"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("paper thesis outcome report failed", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-thesis-outcome", flag.ContinueOnError)
	path := flags.String("input", "", "local thesis outcome request file")
	digest := flags.String("sha256", "", "SHA-256 of the exact request bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" || *digest == "" || dsn == "" {
		return errors.New("provide --input, --sha256 and TRADINGBOT_DATABASE_URL")
	}
	info, err := os.Stat(*path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > paperthesis.MaxOutcomeInputBytes {
		return errors.New("paper thesis outcome input must be a bounded regular file")
	}
	file, err := os.Open(*path)
	if err != nil {
		return errors.New("open paper thesis outcome input failed")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, paperthesis.MaxOutcomeInputBytes+1))
	if err != nil {
		return errors.New("read paper thesis outcome input failed")
	}
	input, err := paperthesis.ParseOutcomeRequest(raw, *digest)
	if err != nil {
		return err
	}
	var report paperthesis.OutcomeReport
	err = ledger.WithReadSnapshot(ctx, dsn, func(snapshot ledger.ReadSnapshot) error {
		// Research completions read here are immutable and the projection
		// checks that their durable receipt predates the opening reservation.
		report, err = paperthesis.BuildOutcome(ctx, &researchReceipt{dsn: dsn}, snapshot, input, snapshot.Cutoff())
		return err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

type researchReceipt struct {
	dsn    string
	stored jobs.CompletedResearch
}

func (r *researchReceipt) Get(ctx context.Context, id string) (jobs.Job, error) {
	if r.stored.Job.ID == id {
		return r.stored.Job, nil
	}
	completed, err := jobs.ReadCompletedResearch(ctx, r.dsn, []string{id})
	if err != nil {
		return jobs.Job{}, err
	}
	r.stored = completed[0]
	return r.stored.Job, nil
}

func (r *researchReceipt) GetResult(_ context.Context, id string) (jobs.Result, error) {
	if r.stored.Result.JobID != id {
		return jobs.Result{}, jobs.ErrNotFound
	}
	return r.stored.Result, nil
}
