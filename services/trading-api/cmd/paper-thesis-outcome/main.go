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

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/accounts_http"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/jobs_http"
	"tradingbot/trading-api/internal/paper_thesis"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_ACCOUNTS_DATABASE_URL"), os.Args[1:], os.Stdout, nil)
	cancel()
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("paper thesis outcome report failed", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer, lookup researchLookup) error {
	flags := flag.NewFlagSet("paper-thesis-outcome", flag.ContinueOnError)
	path := flags.String("input", "", "local thesis outcome request file")
	digest := flags.String("sha256", "", "SHA-256 of the exact request bytes")
	archiveRoot := flags.String("archive-root", "", "root of the pinned local horizon quote tape")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" || *digest == "" || dsn == "" {
		return errors.New("provide --input, --sha256 and TRADINGBOT_ACCOUNTS_DATABASE_URL")
	}
	info, err := os.Stat(*path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > paper_thesis.MaxOutcomeInputBytes {
		return errors.New("paper thesis outcome input must be a bounded regular file")
	}
	file, err := os.Open(*path)
	if err != nil {
		return errors.New("open paper thesis outcome input failed")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, paper_thesis.MaxOutcomeInputBytes+1))
	if err != nil {
		return errors.New("read paper thesis outcome input failed")
	}
	input, err := paper_thesis.ParseOutcomeRequest(raw, *digest)
	if err != nil {
		return err
	}
	if lookup == nil {
		token, err := accounts_http.InternalToken()
		if err != nil {
			return err
		}
		research, err := jobs_http.NewClient(os.Getenv("TRADINGBOT_TRADING_API_URL"), token)
		if err != nil {
			return err
		}
		defer research.Close()
		lookup = research.ReadCompletedPaperResearch
	}
	var report paper_thesis.OutcomeReport
	err = accounts.WithReadSnapshot(ctx, dsn, func(snapshot accounts.ReadSnapshot) error {
		// Research completions read here are immutable and the projection
		// checks that their durable receipt predates the opening reservation.
		report, err = paper_thesis.BuildOutcomeFromArchive(ctx, &researchReceipt{lookup: lookup}, snapshot, input, snapshot.Cutoff(), *archiveRoot)
		return err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

type researchLookup func(context.Context, string) (jobs.CompletedPaperResearch, error)

type researchReceipt struct {
	lookup researchLookup
	stored jobs.CompletedPaperResearch
}

func (r *researchReceipt) load(ctx context.Context, id string) error {
	if id != "" && r.stored.Job.ID == id {
		return nil
	}
	completed, err := r.lookup(ctx, id)
	if err != nil {
		return err
	}
	r.stored = completed
	return nil
}

func (r *researchReceipt) Get(ctx context.Context, id string) (jobs.Job, error) {
	if err := r.load(ctx, id); err != nil {
		return jobs.Job{}, err
	}
	return r.stored.Job, nil
}

func (r *researchReceipt) GetResult(ctx context.Context, id string) (jobs.Result, error) {
	if err := r.load(ctx, id); err != nil {
		return jobs.Result{}, err
	}
	return r.stored.Result, nil
}

func (r *researchReceipt) GetPaperDispatch(ctx context.Context, id string) (jobs.PaperDispatchTask, error) {
	if err := r.load(ctx, id); err != nil {
		return jobs.PaperDispatchTask{}, err
	}
	if r.stored.Dispatch == nil {
		return jobs.PaperDispatchTask{}, jobs.ErrNotFound
	}
	return *r.stored.Dispatch, nil
}
