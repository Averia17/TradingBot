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

type CompletedPaperResearch struct {
	CompletedResearch
	Dispatch *PaperDispatchTask
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
			completed, _, err := readCompletedResearch(ctx, tx, id)
			if err != nil {
				return err
			}
			remaining -= len(completed.Job.Request) + len(completed.Result.Payload)
			if remaining < 0 {
				return errors.New("research replay selection exceeds its payload limit")
			}
			items = append(items, completed)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// ReadCompletedPaperResearch reads the immutable proof for a paper opening or
// exit. Registry-bound jobs must have a durable owner dispatch. Legacy
// reporting needs no access to the dispatch table.
func ReadCompletedPaperResearch(ctx context.Context, dsn, id string) (CompletedPaperResearch, error) {
	var receipt CompletedPaperResearch
	err := withResearchReadSnapshot(ctx, dsn, func(tx pgx.Tx) error {
		completed, registry, err := readCompletedResearch(ctx, tx, id)
		if err != nil {
			return err
		}
		size := len(completed.Job.Request) + len(completed.Result.Payload)
		if registry != "" {
			dispatch, err := readPaperDispatch(tx.QueryRow(ctx, selectPaperDispatch, id))
			if err != nil {
				return fmt.Errorf("read owner paper dispatch for %s: %w", id, err)
			}
			receipt.Dispatch = &dispatch
			size += len(dispatch.Request)
		}
		if size > maxResearchReplayBytes {
			return errors.New("completed paper research exceeds its payload limit")
		}
		receipt.CompletedResearch = completed
		return nil
	})
	if err != nil {
		return CompletedPaperResearch{}, err
	}
	return receipt, nil
}

func readCompletedResearch(ctx context.Context, tx pgx.Tx, id string) (CompletedResearch, string, error) {
	result, err := readResult(tx.QueryRow(ctx, selectResult, id))
	if err != nil {
		return CompletedResearch{}, "", fmt.Errorf("read completed result for %s: %w", id, err)
	}
	inputs, err := readJobInputs(ctx, tx, id)
	if err != nil {
		return CompletedResearch{}, "", fmt.Errorf("read immutable inputs for %s: %w", id, err)
	}
	if inputs.snapshotAt.After(inputs.CreatedAt) {
		return CompletedResearch{}, "", fmt.Errorf("research job %s was recorded before its snapshot", id)
	}
	return CompletedResearch{Job: inputs.Job, Result: result}, inputs.registrySHA, nil
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
