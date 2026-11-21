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
	"tradingbot/control/internal/paper_nav"
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
	verifyPath := flags.String("verify-report", "", "saved paper NAV report to verify by replaying the ledger")
	reportDigest := flags.String("report-sha256", "", "SHA-256 of the exact saved report bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" || *digest == "" || dsn == "" {
		return errors.New("provide --input, --sha256 and TRADINGBOT_DATABASE_URL")
	}
	if (*verifyPath == "") != (*reportDigest == "") {
		return errors.New("provide --verify-report and --report-sha256 together")
	}
	raw, err := readBounded(*path, "input", paper_nav.MaxInputBytes)
	if err != nil {
		return err
	}
	input, err := paper_nav.Parse(raw, *digest)
	if err != nil {
		return err
	}
	var archived []byte
	if *verifyPath != "" {
		raw, err := readBounded(*verifyPath, "report", paper_nav.MaxReportBytes)
		if err != nil {
			return err
		}
		archived, err = paper_nav.CanonicalReport(raw, *reportDigest)
		if err != nil {
			return err
		}
	}
	var report paper_nav.Report
	err = ledger.WithReadSnapshot(ctx, dsn, func(snapshot ledger.ReadSnapshot) error {
		var err error
		report, err = paper_nav.Build(ctx, snapshot, input, snapshot.Cutoff())
		return err
	})
	if err != nil {
		return err
	}
	if archived != nil {
		if err := paper_nav.CompareReport(archived, report); err != nil {
			return err
		}
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	if report.Status == "incomplete" {
		return errIncomplete
	}
	return nil
}

func readBounded(path, kind string, maxSize int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSize {
		return nil, errors.New("paper NAV " + kind + " must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open paper NAV " + kind + " failed")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return nil, errors.New("read paper NAV " + kind + " failed")
	}
	return raw, nil
}
