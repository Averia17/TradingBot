package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"

	"tradingbot/control/internal/paper_quotes"
)

func main() {
	err := run(os.Args[1:], os.Stdout)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("quote study report failed", "error", err)
		os.Exit(2)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-quote-study", flag.ContinueOnError)
	plan := flags.String("plan", "", "preregistered local quote study plan")
	planSHA := flags.String("plan-sha256", "", "SHA-256 of the exact plan bytes")
	archive := flags.String("observations", "", "local quote observation archive")
	archiveSHA := flags.String("observations-sha256", "", "SHA-256 of the exact observation bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *plan == "" || *planSHA == "" || *archive == "" || *archiveSHA == "" {
		return errors.New("provide --plan, --plan-sha256, --observations and --observations-sha256")
	}
	pinnedPlan, err := readDocument(*plan, paper_quotes.MaxStudyPlanBytes)
	if err != nil {
		return err
	}
	pinnedArchive, err := readDocument(*archive, paper_quotes.MaxStudyArchiveBytes)
	if err != nil {
		return err
	}
	report, err := paper_quotes.SummarizeStudy(pinnedPlan, *planSHA, pinnedArchive, *archiveSHA)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

func readDocument(path string, maximum int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("quote study input must be a bounded regular file")
	}
	reader, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open quote study input failed")
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, maximum+1))
}
