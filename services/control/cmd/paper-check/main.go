package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

var uuid = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

type issue struct {
	Code         string `json:"code"`
	OrderID      string `json:"order_id,omitempty"`
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
	Issues            []issue         `json:"issues"`
}

func (r *report) add(code string) {
	r.Issues = append(r.Issues, issue{Code: code})
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	code := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	os.Exit(code)
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) int {
	flags := flag.NewFlagSet("paper-check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	accountID := flags.String("account", "", "virtual paper account UUID")
	jobID := flags.String("job", "", "optional research job UUID")
	unknownAfter := flags.Duration("unknown-after", 5*time.Minute, "age at which an UNKNOWN order needs attention")
	r := report{SchemaVersion: "0.1", Status: "unavailable", Issues: []issue{}}
	parseErr := flags.Parse(args)
	if errors.Is(parseErr, flag.ErrHelp) {
		_, _ = io.WriteString(output, "paper-check --account UUID [--job UUID] [--unknown-after 5m]\nPostgreSQL configuration is read from TRADINGBOT_DATABASE_URL.\n")
		return 0
	}
	if parseErr != nil || flags.NArg() != 0 || !uuid.MatchString(*accountID) ||
		(*jobID != "" && !uuid.MatchString(*jobID)) || *unknownAfter <= 0 || dsn == "" {
		r.add("invalid_configuration")
		return writeReport(output, r, 2)
	}
	r.AccountID = *accountID
	if *jobID != "" {
		r.Research = &researchBudget{JobID: *jobID}
	}
	if err := check(ctx, dsn, *unknownAfter, &r); err != nil {
		// The DSN and database/provider error strings never enter the report.
		r.add("postgres_read_failed")
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

func check(ctx context.Context, dsn string, unknownAfter time.Duration, r *report) error {
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
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&r.CheckedAt); err != nil {
			return err
		}
		r.CheckedAt = r.CheckedAt.UTC()
		if err := checkAccount(ctx, tx, unknownAfter, r); err != nil {
			return err
		}
		if r.Research != nil {
			return checkBudget(ctx, tx, r)
		}
		return nil
	})
}
