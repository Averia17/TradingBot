package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/trading_api"
)

const maxExportBytes = 48 << 20

var jobUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type replayExport struct {
	Version string      `json:"schema_version"`
	Jobs    []replayJob `json:"jobs"`
}

type replayJob struct {
	JobID         string    `json:"job_id"`
	Attempt       int       `json:"attempt"`
	CreatedAt     time.Time `json:"created_at"`
	CompletedAt   time.Time `json:"completed_at"`
	Request       []byte    `json:"request_base64"`
	RequestSHA256 string    `json:"request_sha256"`
	Result        []byte    `json:"result_base64"`
	ResultSHA256  string    `json:"result_sha256"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("research replay export failed", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("research-replay-export", flag.ContinueOnError)
	ids := make([]string, 0)
	flags.Func("job", "completed research job UUID (repeat for each job)", func(id string) error {
		ids = append(ids, id)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(ids) == 0 || strings.TrimSpace(dsn) == "" {
		return errors.New("provide at least one --job and TRADINGBOT_DATABASE_URL")
	}
	if len(ids) > jobs.MaxResearchReplayJobs {
		return errors.New("research replay selection exceeds its job limit")
	}
	for i, id := range ids {
		if !jobUUID.MatchString(id) || slices.Contains(ids[:i], id) {
			return errors.New("provide unique lowercase job UUIDs")
		}
	}
	rows, err := jobs.ReadCompletedResearch(ctx, dsn, ids)
	if err != nil {
		return err
	}
	data, err := buildExport(rows)
	if err != nil {
		return err
	}
	n, err := output.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func buildExport(rows []jobs.CompletedResearch) ([]byte, error) {
	export := replayExport{Version: "0.1", Jobs: make([]replayJob, 0, len(rows))}
	for _, row := range rows {
		if err := trading_api.ValidateArchivedResearchResult(row.Job, row.Result); err != nil {
			return nil, fmt.Errorf("research job %s cannot be exported: %w", row.Job.ID, err)
		}
		requestDigest, resultDigest := sha256.Sum256(row.Job.Request), sha256.Sum256(row.Result.Payload)
		export.Jobs = append(export.Jobs, replayJob{
			JobID: row.Job.ID, Attempt: row.Result.Attempt,
			CreatedAt: row.Job.CreatedAt.UTC(), CompletedAt: row.Result.CompletedAt.UTC(),
			Request: row.Job.Request, RequestSHA256: hex.EncodeToString(requestDigest[:]),
			Result: row.Result.Payload, ResultSHA256: hex.EncodeToString(resultDigest[:]),
		})
	}
	slices.SortFunc(export.Jobs, func(a, b replayJob) int {
		if order := a.CompletedAt.Compare(b.CompletedAt); order != 0 {
			return order
		}
		return strings.Compare(a.JobID, b.JobID)
	})
	data, err := json.Marshal(export)
	if err != nil {
		return nil, err
	}
	if len(data)+1 > maxExportBytes {
		return nil, errors.New("research replay export exceeds its document limit")
	}
	return append(data, '\n'), nil
}
