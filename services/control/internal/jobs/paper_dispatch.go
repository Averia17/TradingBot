package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

const MaxPaperDispatchPage = 32

var paperDispatchUUID = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

const selectPaperDispatch = `SELECT job_id::text, account_id::text, request, request_sha256
		FROM research_paper_dispatch WHERE job_id=$1`

// PaperDispatchInput is supplied only by the owner-side review scheduler.
// The normal research HTTP contract cannot opt a job into paper application.
type PaperDispatchInput struct {
	AccountID string
	Request   json.RawMessage
}

type PaperDispatchTask struct {
	PaperDispatchInput
	JobID         string
	RequestSHA256 [sha256.Size]byte
}

func insertPaperDispatch(ctx context.Context, tx pgx.Tx, jobID string, input PaperDispatchInput) error {
	if !paperDispatchUUID.MatchString(input.AccountID) || len(input.Request) > 256*1024 || !json.Valid(input.Request) {
		return errors.New("invalid paper dispatch request")
	}
	digest := sha256.Sum256(input.Request)
	if _, err := tx.Exec(ctx, `INSERT INTO research_paper_dispatch (job_id, account_id, request, request_sha256)
		VALUES ($1,$2,$3,$4)`, jobID, input.AccountID, []byte(input.Request), digest[:]); err != nil {
		return fmt.Errorf("store scheduled paper dispatch: %w", err)
	}
	return nil
}

// GetPaperDispatch reads the immutable, owner-scheduled application context
// independently of the current research status or dispatch acknowledgement.
func (s *Store) GetPaperDispatch(ctx context.Context, jobID string) (PaperDispatchTask, error) {
	if !paperDispatchUUID.MatchString(jobID) {
		return PaperDispatchTask{}, errors.New("invalid paper dispatch job ID")
	}
	return readPaperDispatch(s.pool.QueryRow(ctx, selectPaperDispatch, jobID))
}

func readPaperDispatch(row pgx.Row) (PaperDispatchTask, error) {
	var task PaperDispatchTask
	var digest []byte
	err := row.Scan(&task.JobID, &task.AccountID, &task.Request, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaperDispatchTask{}, ErrNotFound
	}
	if err != nil {
		return PaperDispatchTask{}, fmt.Errorf("read paper dispatch: %w", err)
	}
	task.RequestSHA256 = sha256.Sum256(task.Request)
	if !bytes.Equal(task.RequestSHA256[:], digest) {
		return PaperDispatchTask{}, errors.New("persisted paper dispatch request does not match its hash")
	}
	return task, nil
}

// ReadyPaperDispatch lists a fair keyset page of ended research jobs for an
// account. Until terminal acknowledgement, a task survives every dispatcher.
func (s *Store) ReadyPaperDispatch(ctx context.Context, accountID, after string) ([]PaperDispatchTask, error) {
	if !paperDispatchUUID.MatchString(accountID) || after != "" && !paperDispatchUUID.MatchString(after) {
		return nil, errors.New("invalid paper dispatch account or cursor")
	}
	rows, err := s.pool.Query(ctx, `SELECT d.job_id::text, d.account_id::text, d.request, d.request_sha256
		FROM research_paper_dispatch d JOIN research_jobs j ON j.id=d.job_id
		WHERE d.account_id=$1 AND d.finished_status IS NULL AND (nullif($2,'') IS NULL OR d.job_id > nullif($2,'')::uuid)
			AND j.status IN ('completed','failed','expired')
		ORDER BY d.job_id LIMIT $3`, accountID, after, MaxPaperDispatchPage)
	if err != nil {
		return nil, fmt.Errorf("list ready paper dispatch: %w", err)
	}
	defer rows.Close()
	tasks := make([]PaperDispatchTask, 0, MaxPaperDispatchPage)
	for rows.Next() {
		task, err := readPaperDispatch(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ready paper dispatch: %w", err)
	}
	return tasks, nil
}

func (s *Store) FinishPaperDispatch(ctx context.Context, task PaperDispatchTask, status string) error {
	switch status {
	case "hold", "applied", "expired", "snapshot_stale", "research_failed", "research_expired":
	default:
		return errors.New("paper dispatch status is not terminal")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin paper dispatch acknowledgement: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		return fmt.Errorf("configure paper dispatch durability: %w", err)
	}
	result, err := tx.Exec(ctx, `UPDATE research_paper_dispatch
		SET finished_status=$4, finished_at=clock_timestamp()
		WHERE job_id=$1 AND account_id=$2 AND request_sha256=$3 AND finished_status IS NULL
			AND EXISTS (SELECT 1 FROM research_jobs WHERE id=$1 AND status IN ('completed','failed','expired'))`,
		task.JobID, task.AccountID, task.RequestSHA256[:], status)
	if err != nil {
		return fmt.Errorf("acknowledge paper dispatch: %w", err)
	}
	if result.RowsAffected() == 0 {
		var previous string
		err := tx.QueryRow(ctx, `SELECT coalesce(finished_status,'') FROM research_paper_dispatch
			WHERE job_id=$1 AND account_id=$2 AND request_sha256=$3`, task.JobID, task.AccountID, task.RequestSHA256[:]).Scan(&previous)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && previous != status {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("read prior paper dispatch acknowledgement: %w", err)
		}
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit paper dispatch acknowledgement: %w", err)
	}
	return nil
}
