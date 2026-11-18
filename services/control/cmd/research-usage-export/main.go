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
	"regexp"
	"strings"
	"syscall"
	"time"

	"tradingbot/control/internal/jobs"
)

var jobUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type permitArchive struct {
	Version             string         `json:"schema_version"`
	Source              string         `json:"source_mode"`
	JobID               string         `json:"job_id"`
	Status              string         `json:"job_status"`
	Attempts            int            `json:"attempts"`
	LegacyBudgetUnknown bool           `json:"legacy_budget_unknown"`
	PermitHistoryKnown  bool           `json:"permit_history_known"`
	Permits             []permitRecord `json:"permits"`
}

type permitRecord struct {
	CallID          string     `json:"call_id"`
	Attempt         int        `json:"attempt"`
	GrantedAt       time.Time  `json:"granted_at"`
	CallbackPayload []byte     `json:"callback_payload"`
	CallbackSHA256  *string    `json:"callback_sha256"`
	RecordedAt      *time.Time `json:"recorded_at"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("research usage export failed", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("research-usage-export", flag.ContinueOnError)
	id := flags.String("job", "", "research job UUID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !jobUUID.MatchString(*id) || strings.TrimSpace(dsn) == "" {
		return errors.New("provide a lowercase --job UUID and TRADINGBOT_DATABASE_URL")
	}
	usage, err := jobs.ReadResearchCallUsage(ctx, dsn, *id)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(projectArchive(usage))
}

func projectArchive(usage jobs.ResearchCallUsage) permitArchive {
	report := permitArchive{
		Version: "0.1", Source: "synthetic_fixture", JobID: usage.JobID,
		Status: usage.Status, Attempts: usage.Attempts, LegacyBudgetUnknown: usage.LegacyBudgetUnknown,
		PermitHistoryKnown: usage.PermitHistoryKnown,
		Permits:            make([]permitRecord, 0, len(usage.Records)),
	}
	for _, source := range usage.Records {
		record := permitRecord{CallID: source.CallID, Attempt: source.Attempt, GrantedAt: source.GrantedAt}
		if source.RecordedAt != nil {
			record.CallbackPayload = source.Payload
			record.CallbackSHA256 = &source.PayloadSHA256
			record.RecordedAt = source.RecordedAt
		}
		report.Permits = append(report.Permits, record)
	}
	return report
}
