package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	MaxResearchReplayJobs  = 32
	maxResearchReplayBytes = 32 << 20
)

type CompletedResearch struct {
	Job    Job
	Result Result
}

// ReadCompletedResearch reads the exact persisted bytes in one read-only
// snapshot. It can use a reporting role with no permission to run migrations.
func ReadCompletedResearch(ctx context.Context, dsn string, ids []string) ([]CompletedResearch, error) {
	if len(ids) == 0 || len(ids) > MaxResearchReplayJobs {
		return nil, errors.New("invalid research replay selection size")
	}
	items := make([]CompletedResearch, 0, len(ids))
	remaining := maxResearchReplayBytes
	err := withResearchReadSnapshot(ctx, dsn, func(tx pgx.Tx) error {
		for _, id := range ids {
			result, err := readResult(tx.QueryRow(ctx, selectResult, id))
			if err != nil {
				return fmt.Errorf("read completed result for %s: %w", id, err)
			}
			job, asOf, err := claimInputs(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("read immutable inputs for %s: %w", id, err)
			}
			if asOf.After(job.CreatedAt) {
				return fmt.Errorf("research job %s was recorded before its snapshot", id)
			}
			remaining -= len(job.Request) + len(result.Payload)
			if remaining < 0 {
				return errors.New("research replay selection exceeds its payload limit")
			}
			items = append(items, CompletedResearch{Job: job, Result: result})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func withResearchReadSnapshot(ctx context.Context, dsn string, read func(pgx.Tx) error) error {
	if strings.TrimSpace(dsn) == "" {
		return errors.New("missing research PostgreSQL configuration")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid research PostgreSQL configuration")
	}
	if config.ConnectTimeout <= 0 || config.ConnectTimeout > 5*time.Second {
		config.ConnectTimeout = 5 * time.Second
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return errors.New("connect to research PostgreSQL for reporting failed")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(ctx)
	}()

	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, read)
}
