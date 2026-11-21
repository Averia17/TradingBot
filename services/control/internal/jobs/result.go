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
	JobID            string
	WorkerID         string
	Token            string
	Payload          json.RawMessage
	ExpiresAt        time.Time
	ExpectedAttempt  int // Zero for protocols with no attempt number in the result.
	ExpectedLLMCalls int // Number of permits the submitting attempt consumed.
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
	job, err := s.beginWorkerJob(ctx, input.JobID, "completion")
	if err != nil {
		return Result{}, false, err
	}
	defer rollback(job.tx)
	if job.status == "completed" {
		result, err := job.replayCompletion(ctx, input)
		return result, false, err
	}
	lease, _, err := job.workerLease(ctx, input.WorkerID, input.Token)
	if err != nil {
		return Result{}, false, err
	}
	if input.ExpectedAttempt != 0 && job.attempt != input.ExpectedAttempt {
		return Result{}, false, ErrLeaseConflict
	}
	if verify == nil {
		return Result{}, false, errors.New("research completion requires local contract validation")
	}
	var permits int
	if err := job.tx.QueryRow(ctx, `SELECT count(*) FROM research_call_permits WHERE job_id = $1 AND attempt = $2`,
		job.id, job.attempt).Scan(&permits); err != nil {
		return Result{}, false, fmt.Errorf("read research attempt call permits: %w", err)
	}
	if permits != input.ExpectedLLMCalls {
		return Result{}, false, ErrCallPermitMismatch
	}
	inputs, err := readJobInputs(ctx, job.tx, job.id)
	if err != nil {
		return Result{}, false, err
	}
	var verifyTime time.Time
	if err := job.tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&verifyTime); err != nil {
		return Result{}, false, fmt.Errorf("read research validation clock: %w", err)
	}
	if !verifyTime.Before(lease.ExpiresAt) {
		return Result{}, false, ErrLeaseConflict
	}
	if !verifyTime.Before(input.ExpiresAt) {
		return Result{}, false, ErrResultExpired
	}
	if err := verify(inputs.Job, verifyTime.UTC()); err != nil {
		return Result{}, false, err
	}
	result, err := job.insertCompletion(ctx, input.Payload, input.ExpiresAt)
	if err != nil {
		return Result{}, false, err
	}
	if err := job.tx.Commit(ctx); err != nil {
		return Result{}, false, fmt.Errorf("commit research completion: %w", err)
	}
	return result, true, nil
}

func (j lockedJob) insertCompletion(ctx context.Context, payload json.RawMessage, expiresAt time.Time) (Result, error) {
	digest := sha256.Sum256(payload)
	result := Result{JobID: j.id, Attempt: j.attempt, Payload: payload}
	var leaseActive, batchFresh, inserted bool
	// After local verification, check both expiry bounds at the exact time
	// recorded on the receipt. Return the failed predicate without another clock read.
	err := j.tx.QueryRow(ctx, `WITH receipt AS MATERIALIZED (
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
	if err := j.setStatus(ctx, "completed"); err != nil {
		return Result{}, err
	}
	if _, err := j.tx.Exec(ctx, `INSERT INTO research_outbox (job_id, event_type, created_at) VALUES ($1, 'research_job.completed', $2)`,
		result.JobID, result.CompletedAt); err != nil {
		return Result{}, fmt.Errorf("store completed research job event: %w", err)
	}
	result.CompletedAt = result.CompletedAt.UTC()
	return result, nil
}

func (j lockedJob) replayCompletion(ctx context.Context, input CompleteJob) (Result, error) {
	var result Result
	var digest []byte
	var workerID, token string
	err := j.tx.QueryRow(ctx, `SELECT r.job_id::text, r.attempt, r.completed_at, r.payload, r.payload_sha256,
		a.worker_id, a.lease_token::text FROM research_job_results r
		JOIN research_job_attempts a ON r.job_id = a.job_id AND r.attempt = a.attempt
		WHERE r.job_id = $1 AND r.attempt = $2`, j.id, j.attempt).
		Scan(&result.JobID, &result.Attempt, &result.CompletedAt, &result.Payload, &digest, &workerID, &token)
	if err != nil {
		return Result{}, fmt.Errorf("read completing research attempt: %w", err)
	}
	if workerID != input.WorkerID || token != input.Token {
		return Result{}, ErrLeaseConflict
	}
	result, err = verifyResultDigest(result, digest)
	if err != nil {
		return Result{}, err
	}
	if !bytes.Equal(result.Payload, input.Payload) {
		return Result{}, ErrConflict
	}
	if input.ExpectedAttempt != 0 && result.Attempt != input.ExpectedAttempt {
		return Result{}, ErrLeaseConflict
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
	return verifyResultDigest(result, digest)
}

func verifyResultDigest(result Result, digest []byte) (Result, error) {
	expected := sha256.Sum256(result.Payload)
	if !bytes.Equal(expected[:], digest) {
		return Result{}, errors.New("persisted research result does not match its hash")
	}
	result.CompletedAt = result.CompletedAt.UTC()
	return result, nil
}
