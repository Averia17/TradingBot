package paper_check

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var uuid = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

type issue struct {
	Code         string `json:"code"`
	JobID        string `json:"job_id,omitempty"`
	OrderID      string `json:"order_id,omitempty"`
	FillEventID  string `json:"fill_event_id,omitempty"`
	InstrumentID string `json:"instrument_id,omitempty"`
}

type report struct {
	SchemaVersion     string          `json:"schema_version"`
	Status            string          `json:"status"`
	CheckedAt         time.Time       `json:"checked_at,omitzero"`
	AccountID         string          `json:"account_id,omitempty"`
	AccountVersion    int64           `json:"account_version,omitempty"`
	ProjectionVersion int64           `json:"projection_version,omitempty"`
	Research          *researchBudget `json:"research,omitempty"`
	Pipeline          *paperPipeline  `json:"pipeline,omitempty"`
	Issues            []issue         `json:"issues"`
}

func (r *report) add(code string) {
	r.Issues = append(r.Issues, issue{Code: code})
}

func Run(ctx context.Context, args []string, output io.Writer) int {
	fixture := os.Getenv("TRADINGBOT_OFFLINE_TEST_DATABASE_URL")
	accountsURL := os.Getenv("TRADINGBOT_ACCOUNTS_URL")
	jobsURL := os.Getenv("TRADINGBOT_TRADING_API_URL")
	if fixture != "" && accountsURL == "" && jobsURL == "" {
		return run(ctx, fixture, args, output)
	}
	return runReport(ctx, args, output, fixture == "" && accountsURL != "", "internal_api_read_failed",
		func(ctx context.Context, unknownAfter, stallAfter time.Duration, r *report) error {
			return checkRemote(ctx, accountsURL, jobsURL, unknownAfter, stallAfter, r)
		})
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) int {
	return runReport(ctx, args, output, dsn != "", "postgres_read_failed",
		func(ctx context.Context, unknownAfter, stallAfter time.Duration, r *report) error {
			return check(ctx, dsn, unknownAfter, stallAfter, r)
		})
}

func runReport(ctx context.Context, args []string, output io.Writer, configured bool, unavailableCode string,
	inspect func(context.Context, time.Duration, time.Duration, *report) error) int {
	flags := flag.NewFlagSet("paper-check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	accountID := flags.String("account", "", "virtual paper account UUID")
	jobID := flags.String("job", "", "optional research job UUID")
	unknownAfter := flags.Duration("unknown-after", 5*time.Minute, "age at which an UNKNOWN order needs attention")
	pipeline := flags.Bool("pipeline", false, "inspect durable paper dispatch for the account")
	stallAfter := flags.Duration("stall-after", 5*time.Minute, "age at which a waiting pipeline stage needs attention")
	r := report{SchemaVersion: "0.1", Status: "unavailable", Issues: []issue{}}
	parseErr := flags.Parse(args)
	if errors.Is(parseErr, flag.ErrHelp) {
		_, _ = io.WriteString(output, "paper-check --account UUID [--job UUID] [--unknown-after 5m] [--pipeline [--stall-after 5m]]\nConfigure TRADINGBOT_ACCOUNTS_URL and, for research diagnostics, TRADINGBOT_TRADING_API_URL.\n")
		return 0
	}
	if parseErr != nil || flags.NArg() != 0 || !uuid.MatchString(*accountID) ||
		(*jobID != "" && !uuid.MatchString(*jobID)) || *unknownAfter <= 0 || *stallAfter <= 0 || !configured {
		r.add("invalid_configuration")
		return writeReport(output, r, 2)
	}
	r.AccountID = *accountID
	if *jobID != "" {
		r.Research = &researchBudget{JobID: *jobID}
	}
	if *pipeline {
		r.Pipeline = &paperPipeline{StallAfter: stallAfter.String(), Jobs: []pipelineJob{}}
	}
	if err := inspect(ctx, *unknownAfter, *stallAfter, &r); err != nil {
		// The DSN and database/provider error strings never enter the report.
		r.add(unavailableCode)
		return writeReport(output, r, 2)
	}
	if len(r.Issues) > 0 {
		r.Status = "attention"
		return writeReport(output, r, 1)
	}
	r.Status = "ok"
	return writeReport(output, r, 0)
}

func writeReport(output io.Writer, r report, status int) int {
	if err := json.NewEncoder(output).Encode(r); err != nil {
		return 2
	}
	return status
}

func check(ctx context.Context, dsn string, unknownAfter, stallAfter time.Duration, r *report) error {
	return readDiagnostics(ctx, dsn, func(tx pgx.Tx, checkedAt time.Time) error {
		r.CheckedAt = checkedAt
		if err := checkAccount(ctx, tx, unknownAfter, r); err != nil {
			return err
		}
		if r.Research != nil {
			if err := checkBudget(ctx, tx, r); err != nil {
				return err
			}
		}
		if r.Pipeline != nil {
			return checkPipeline(ctx, tx, stallAfter, r)
		}
		return nil
	})
}

func readDiagnostics(ctx context.Context, dsn string, read func(pgx.Tx, time.Time) error) error {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	if config.ConnectTimeout <= 0 || config.ConnectTimeout > 5*time.Second {
		config.ConnectTimeout = 5 * time.Second
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}()
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {

		var checkedAt time.Time
		if err := tx.QueryRow(ctx, "SELECT transaction_timestamp()").Scan(&checkedAt); err != nil {
			return err
		}
		return read(tx, checkedAt.UTC())
	})
}
