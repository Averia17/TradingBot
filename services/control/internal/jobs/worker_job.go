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

// lockedJob is the worker-facing state under the owning job's row lock.
// Heartbeats, retries, permits, callbacks and completion all advance from it.
type lockedJob struct {
	tx                  pgx.Tx
	id                  string
	status              string
	attempt             int
	legacyBudgetUnknown bool
}

func (s *Store) beginWorkerJob(ctx context.Context, id, operation string) (lockedJob, error) {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return lockedJob{}, fmt.Errorf("begin research %s: %w", operation, err)
	}
	job, err := lockJob(ctx, tx, id)
	if err == nil {
		return job, nil
	}
	rollback(tx)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedJob{}, ErrNotFound
	}
	return lockedJob{}, fmt.Errorf("lock research job for %s: %w", operation, err)
}

func lockJob(ctx context.Context, tx pgx.Tx, id string) (lockedJob, error) {
	job := lockedJob{tx: tx}
	err := tx.QueryRow(ctx, `SELECT id::text, status, attempt_count, legacy_budget_unknown
		FROM research_jobs WHERE id = $1 FOR UPDATE`, id).Scan(&job.id, &job.status, &job.attempt, &job.legacyBudgetUnknown)
	return job, err
}

// activeLease reads the attempt in a fresh statement after the job is locked.
// A candidate from an older statement snapshot can have a renewed expiry.
func (j lockedJob) activeLease(ctx context.Context) (Lease, time.Time, error) {
	if j.status != "leased" {
		return Lease{}, time.Time{}, ErrLeaseConflict
	}
	lease := Lease{Job: Job{ID: j.id, Status: j.status}, Attempt: j.attempt}
	var now time.Time
	err := j.tx.QueryRow(ctx, `SELECT worker_id, claim_id::text, lease_token::text, expires_at, deadline, clock_timestamp()
		FROM research_job_attempts WHERE job_id = $1 AND attempt = $2`, j.id, j.attempt).
		Scan(&lease.WorkerID, &lease.ClaimID, &lease.Token, &lease.ExpiresAt, &lease.Deadline, &now)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !now.Before(lease.ExpiresAt) {
		return Lease{}, time.Time{}, ErrLeaseConflict
	}
	if err != nil {
		return Lease{}, time.Time{}, fmt.Errorf("read latest research attempt: %w", err)
	}
	lease.ExpiresAt, lease.Deadline = lease.ExpiresAt.UTC(), lease.Deadline.UTC()
	return lease, now.UTC(), nil
}

func (j lockedJob) workerLease(ctx context.Context, workerID, token string) (Lease, time.Time, error) {
	lease, now, err := j.activeLease(ctx)
	if err != nil {
		return Lease{}, time.Time{}, err
	}
	if lease.WorkerID != workerID || lease.Token != token {
		return Lease{}, time.Time{}, ErrLeaseConflict
	}
	return lease, now, nil
}

func (j lockedJob) setStatus(ctx context.Context, status string) error {
	if _, err := j.tx.Exec(ctx, `UPDATE research_jobs SET status = $2 WHERE id = $1`, j.id, status); err != nil {
		return fmt.Errorf("set terminal research job state: %w", err)
	}
	return nil
}

type persistedJob struct {
	Job
	snapshotAt   time.Time
	registrySHA  string
	analysisMode string
	callBudget   jobCallBudget
}

// readJobInputs validates the immutable bytes shared by claim, permit grants,
// completion and read-only replay. A permit grant also interprets the budget.
func readJobInputs(ctx context.Context, tx pgx.Tx, id string) (persistedJob, error) {
	var job persistedJob
	var snapshotID string
	var snapshot, requestDigest, snapshotDigest []byte
	err := tx.QueryRow(ctx, `SELECT j.id::text, j.request, j.request_sha256, j.status, j.created_at,
		s.id::text, s.payload, s.payload_sha256, j.identity_registry_sha256, j.analysis_mode
		FROM research_jobs j JOIN research_snapshots s ON s.id = j.snapshot_id
		WHERE j.id = $1`, id).Scan(&job.ID, &job.Request, &requestDigest, &job.Status, &job.CreatedAt,
		&snapshotID, &snapshot, &snapshotDigest, &job.registrySHA, &job.analysisMode)
	if err != nil {
		return persistedJob{}, fmt.Errorf("read persisted research inputs: %w", err)
	}
	requestHash := sha256.Sum256(job.Request)
	snapshotHash := sha256.Sum256(snapshot)
	if !bytes.Equal(requestHash[:], requestDigest) || !bytes.Equal(snapshotHash[:], snapshotDigest) {
		return persistedJob{}, errors.New("persisted research inputs do not match their hashes")
	}
	var request struct {
		registryBinding
		ID        string          `json:"job_id"`
		Portfolio json.RawMessage `json:"portfolio"`
	}
	var portfolio struct {
		ID   string    `json:"snapshot_id"`
		AsOf time.Time `json:"as_of"`
	}
	err = json.Unmarshal(job.Request, &request)
	scope, scopeErr := request.scope()
	if err != nil || request.ID != job.ID ||
		(request.Version != "0.1" && request.Version != "0.2" && request.Version != "0.3") ||
		scopeErr != nil || scope != (claimScope{job.registrySHA, job.analysisMode}) ||
		!bytes.Equal(request.Portfolio, snapshot) || json.Unmarshal(snapshot, &portfolio) != nil ||
		portfolio.ID != snapshotID || portfolio.AsOf.IsZero() {
		return persistedJob{}, errors.New("persisted research inputs have inconsistent identities or version")
	}
	job.CreatedAt = job.CreatedAt.UTC()
	job.snapshotAt = portfolio.AsOf.UTC()
	job.callBudget = request.CallBudget
	return job, nil
}
