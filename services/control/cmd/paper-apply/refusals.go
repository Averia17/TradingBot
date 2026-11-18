package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papersim"
)

type refusalReport struct {
	Version   string               `json:"schema_version"`
	Source    string               `json:"source_mode"`
	AccountID string               `json:"account_id"`
	JobID     string               `json:"job_id"`
	AsOf      time.Time            `json:"as_of"`
	Truncated bool                 `json:"truncated"`
	Refusals  []ledger.RiskRefusal `json:"refusals"`
}

func refusals(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-apply refusals", flag.ContinueOnError)
	accountID := flags.String("account", "", "virtual paper account UUID")
	jobID := flags.String("job", "", "completed research job UUID")
	cutoff := flags.String("as-of", "", "observation cutoff in RFC3339, defaults to the PostgreSQL read cutoff")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *accountID == "" || *jobID == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --account and --job are required")
	}
	report := refusalReport{Version: "0.1", Source: papersim.Source, AccountID: *accountID, JobID: *jobID}
	err := ledger.WithReadSnapshot(ctx, dsn, func(snapshot ledger.ReadSnapshot) error {
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
