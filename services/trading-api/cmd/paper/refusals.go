package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/paper_sim"
)

type refusalReport struct {
	Version   string                 `json:"schema_version"`
	Source    string                 `json:"source_mode"`
	AccountID string                 `json:"account_id"`
	JobID     string                 `json:"job_id"`
	AsOf      time.Time              `json:"as_of"`
	Truncated bool                   `json:"truncated"`
	Refusals  []accounts.RiskRefusal `json:"refusals"`
}

func refusals(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper refusals", flag.ContinueOnError)
	accountID := flags.String("account", "", "virtual paper account UUID")
	jobID := flags.String("job", "", "completed research job UUID")
	cutoff := flags.String("as-of", "", "observation cutoff in RFC3339, defaults to the PostgreSQL read cutoff")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if dsn == "" {
		dsn = os.Getenv("TRADINGBOT_ACCOUNTS_DATABASE_URL")
	}
	if flags.NArg() != 0 || dsn == "" || *accountID == "" || *jobID == "" {
		return errors.New("TRADINGBOT_ACCOUNTS_DATABASE_URL, --account and --job are required")
	}
	report := refusalReport{Version: "0.1", Source: paper_sim.Source, AccountID: *accountID, JobID: *jobID}
	err := accounts.WithReadSnapshot(ctx, dsn, func(snapshot accounts.ReadSnapshot) error {
		asOf, err := parseCutoff(*cutoff, snapshot.Cutoff())
		if err != nil {
			return err
		}
		report.AsOf = asOf
		report.Refusals, report.Truncated, err = snapshot.RiskRefusals(ctx, *accountID, *jobID, asOf)
		return err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}
