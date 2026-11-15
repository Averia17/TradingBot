package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrResultExpired = errors.New("research result expired before its receipt")

type CompleteJob struct {
	JobID     string
	WorkerID  string
	Token     string
	Payload   json.RawMessage
	ExpiresAt time.Time
}

type Result struct {
	JobID       string
	Attempt     int
	CompletedAt time.Time
	Payload     json.RawMessage
}

// Complete atomically records a result, completes the job and writes its outbox
// event. verify performs local contract validation against the immutable job.
// An identical replay from the completing attempt bypasses time-varying checks.
func (s *Store) Complete(ctx context.Context, input CompleteJob, verify func(job Job, dbNow time.Time) error) (Result, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Result{}, false, fmt.Errorf("begin research completion: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		return Result{}, false, fmt.Errorf("configure durable research completion: %w", err)
	}
	var jobID, status string
	err = tx.QueryRow(ctx, `SELECT id::text, status FROM research_jobs WHERE id = $1 FOR UPDATE`, input.JobID).Scan(&jobID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, ErrNotFound
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("lock research job for completion: %w", err)
	}
	if status == "completed" {
		result, err := replayCompletion(ctx, tx, jobID, input)
		return result, false, err
	}
	lease, _, err := currentLease(ctx, tx, jobID)
	if err != nil {
		return Result{}, false, err
	}
	if lease.WorkerID != input.WorkerID || lease.Token != input.Token {
		return Result{}, false, ErrLeaseConflict
	}
	if verify == nil {
		return Result{}, false, errors.New("research completion requires local contract validation")
	}
	job, _, err := claimInputs(ctx, tx, jobID)
	if err != nil {
		return Result{}, false, err
	}
	var verifyTime time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&verifyTime); err != nil {
		return Result{}, false, fmt.Errorf("read research validation clock: %w", err)
	}
	if !verifyTime.Before(lease.ExpiresAt) {
		return Result{}, false, ErrLeaseConflict
	}
	if !verifyTime.Before(input.ExpiresAt) {
		return Result{}, false, ErrResultExpired
	}
	if err := verify(job, verifyTime.UTC()); err != nil {
		return Result{}, false, err
	}
	result, err := insertCompletion(ctx, tx, lease, input.Payload, input.ExpiresAt)
	if err != nil {
		return Result{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, false, fmt.Errorf("commit research completion: %w", err)
	}
	return result, true, nil
}

func insertCompletion(ctx context.Context, tx pgx.Tx, lease Lease, payload json.RawMessage, expiresAt time.Time) (Result, error) {
	digest := sha256.Sum256(payload)
	result := Result{JobID: lease.Job.ID, Attempt: lease.Attempt, Payload: payload}
	var leaseActive, batchFresh, inserted bool
	// After local verification, check both expiry bounds at the exact time
	// recorded on the receipt. Return the failed predicate without another clock read.
	err := tx.QueryRow(ctx, `WITH receipt AS MATERIALIZED (
			SELECT a.job_id, a.attempt, observed.recorded_at, a.expires_at > observed.recorded_at AS lease_active,
			$5::timestamptz > observed.recorded_at AS batch_fresh
			FROM research_job_attempts a CROSS JOIN (SELECT clock_timestamp() AS recorded_at) observed
			WHERE a.job_id = $1 AND a.attempt = $2
		), inserted AS (
			INSERT INTO research_job_results (job_id, attempt, completed_at, payload, payload_sha256)
			SELECT job_id, attempt, recorded_at, $3, $4 FROM receipt WHERE lease_active AND batch_fresh
			RETURNING job_id
		)
		SELECT recorded_at, lease_active, batch_fresh, EXISTS (SELECT 1 FROM inserted) FROM receipt`,
		result.JobID, result.Attempt, []byte(payload), digest[:], expiresAt).Scan(&result.CompletedAt, &leaseActive, &batchFresh, &inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrLeaseConflict
	}
	if err != nil {
		return Result{}, fmt.Errorf("store immutable research result: %w", err)
	}
	if !leaseActive {
		return Result{}, ErrLeaseConflict
	}
	if !batchFresh {
		return Result{}, ErrResultExpired
	}
	if !inserted {
		return Result{}, errors.New("research result insert did not return a receipt")
	}
	if err := setJobStatus(ctx, tx, result.JobID, "completed"); err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO research_outbox (job_id, event_type, created_at) VALUES ($1, 'research_job.completed', $2)`,
		result.JobID, result.CompletedAt); err != nil {
		return Result{}, fmt.Errorf("store completed research job event: %w", err)
	}
	result.CompletedAt = result.CompletedAt.UTC()
	return result, nil
}

func replayCompletion(ctx context.Context, tx pgx.Tx, id string, input CompleteJob) (Result, error) {
	var workerID, token string
	err := tx.QueryRow(ctx, `SELECT a.worker_id, a.lease_token::text FROM research_job_results r
		JOIN research_jobs j ON j.id = r.job_id AND j.attempt_count = r.attempt
		JOIN research_job_attempts a ON r.job_id = a.job_id AND r.attempt = a.attempt
		WHERE r.job_id = $1`, id).Scan(&workerID, &token)
	if err != nil {
		return Result{}, fmt.Errorf("read completing research attempt: %w", err)
	}
	if workerID != input.WorkerID || token != input.Token {
		return Result{}, ErrLeaseConflict
	}
	result, err := readResult(tx.QueryRow(ctx, selectResult, id))
	if err != nil {
		return Result{}, err
	}
	if !bytes.Equal(result.Payload, input.Payload) {
		return Result{}, ErrConflict
	}
	return result, nil
}

const selectResult = `SELECT r.job_id::text, r.attempt, r.completed_at, r.payload, r.payload_sha256
	FROM research_job_results r JOIN research_jobs j ON j.id = r.job_id AND j.attempt_count = r.attempt
	WHERE r.job_id = $1 AND j.status = 'completed'`

func (s *Store) GetResult(ctx context.Context, id string) (Result, error) {
	return readResult(s.pool.QueryRow(ctx, selectResult, id))
}

func readResult(row pgx.Row) (Result, error) {
	var result Result
	var digest []byte
	err := row.Scan(&result.JobID, &result.Attempt, &result.CompletedAt, &result.Payload, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, fmt.Errorf("read research result: %w", err)
	}
	expected := sha256.Sum256(result.Payload)
	if !bytes.Equal(expected[:], digest) {
		return Result{}, errors.New("persisted research result does not match its hash")
	}
	result.CompletedAt = result.CompletedAt.UTC()
	return result, nil
}
