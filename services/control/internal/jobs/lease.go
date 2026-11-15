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

const (
	leaseTTL           = time.Minute
	maxAttemptDuration = 15 * time.Minute
	maxAttempts        = 3
	maxClaimCandidates = 16
)

var (
	ErrNoJob         = errors.New("no research job available")
	ErrLeaseConflict = errors.New("research lease is expired, superseded, or belongs to another worker")
)

type Lease struct {
	Job       Job
	WorkerID  string
	ClaimID   string
	Token     string
	Attempt   int
	ExpiresAt time.Time
	Deadline  time.Time
}

// Claim creates one attempt or replays the same still-active claim. A claim ID
// that found no work is unbound until an attempt is committed.
func (s *Store) Claim(ctx context.Context, workerID, claimID string, maxSnapshotAge time.Duration) (Lease, error) {
	if maxSnapshotAge <= 0 {
		return Lease{}, errors.New("invalid research snapshot age limit")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Lease{}, fmt.Errorf("begin research claim: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		return Lease{}, fmt.Errorf("configure durable claim: %w", err)
	}
	// Serialize duplicate requests for the same claim before choosing a job.
	// A hash collision here can only make unrelated requests wait for each other.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 737292))`, claimID); err != nil {
		return Lease{}, fmt.Errorf("lock research claim identity: %w", err)
	}
	if lease, err := replayClaim(ctx, tx, workerID, claimID, maxSnapshotAge); !errors.Is(err, pgx.ErrNoRows) {
		return lease, err
	}
	for range maxClaimCandidates {
		lease, err := claimNext(ctx, tx, workerID, claimID, maxSnapshotAge)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := tx.Commit(ctx); err != nil {
				return Lease{}, fmt.Errorf("commit research queue update: %w", err)
			}
			return Lease{}, ErrNoJob
		case err != nil:
			return Lease{}, err
		case lease.Token != "":
			if err := tx.Commit(ctx); err != nil {
				return Lease{}, fmt.Errorf("commit research claim: %w", err)
			}
			return lease, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Lease{}, fmt.Errorf("commit research queue update: %w", err)
	}
	return Lease{}, ErrNoJob
}

func replayClaim(ctx context.Context, tx pgx.Tx, workerID, claimID string, maxSnapshotAge time.Duration) (Lease, error) {
	var jobID string
	err := tx.QueryRow(ctx, `SELECT j.id::text FROM research_job_attempts a JOIN research_jobs j ON a.job_id = j.id
		WHERE a.claim_id = $1 FOR UPDATE OF j`, claimID).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, pgx.ErrNoRows
	}
	if err != nil {
		return Lease{}, fmt.Errorf("find prior research claim: %w", err)
	}
	lease, now, err := currentLease(ctx, tx, jobID)
	if err != nil {
		return Lease{}, err
	}
	if lease.ClaimID != claimID || lease.WorkerID != workerID {
		return Lease{}, ErrLeaseConflict
	}
	job, asOf, err := claimInputs(ctx, tx, jobID)
	if err != nil {
		return Lease{}, err
	}
	if now.Before(asOf) || !now.Before(asOf.Add(maxSnapshotAge)) {
		return Lease{}, ErrLeaseConflict
	}
	lease.Job = job
	return lease, nil
}

func claimNext(ctx context.Context, tx pgx.Tx, workerID, claimID string, maxSnapshotAge time.Duration) (Lease, error) {
	var jobID, status string
	var attempts int
	err := tx.QueryRow(ctx, `SELECT j.id::text, j.status, j.attempt_count FROM research_jobs j
		LEFT JOIN research_job_attempts a ON a.job_id = j.id AND a.attempt = j.attempt_count
		WHERE j.status = 'queued' OR (j.status = 'leased' AND a.expires_at <= statement_timestamp())
		ORDER BY j.created_at, j.id LIMIT 1 FOR UPDATE OF j SKIP LOCKED`).Scan(&jobID, &status, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, pgx.ErrNoRows
	}
	if err != nil {
		return Lease{}, fmt.Errorf("select research job for claim: %w", err)
	}
	return claimLockedJob(ctx, tx, jobID, status, attempts, workerID, claimID, maxSnapshotAge)
}

func claimLockedJob(ctx context.Context, tx pgx.Tx, jobID, status string, attempts int, workerID, claimID string, maxSnapshotAge time.Duration) (Lease, error) {
	// A heartbeat may commit between the candidate query's statement snapshot
	// and the job row lock. Re-read the attempt after locking the owning job.
	if status == "leased" {
		_, _, err := currentLease(ctx, tx, jobID)
		if !errors.Is(err, ErrLeaseConflict) {
			return Lease{}, err
		}
	}
	if attempts >= maxAttempts {
		return Lease{}, setJobStatus(ctx, tx, jobID, "failed")
	}
	job, asOf, err := claimInputs(ctx, tx, jobID)
	if err != nil {
		return Lease{}, err
	}
	// The clock is read after acquiring the row lock and checking stored bytes.
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Lease{}, fmt.Errorf("read research queue clock: %w", err)
	}
	cutoff := asOf.Add(maxSnapshotAge)
	if !now.Before(cutoff) {
		return Lease{}, setJobStatus(ctx, tx, jobID, "expired")
	}
	if now.Before(asOf) {
		// A request admitted by the control clock may briefly be ahead of the
		// database. Leave it queued and try another poll after clocks converge.
		return Lease{}, pgx.ErrNoRows
	}
	lease := Lease{
		Job:      job,
		WorkerID: workerID,
		ClaimID:  claimID,
		Attempt:  attempts + 1,
		Deadline: minTime(now.Add(maxAttemptDuration), cutoff),
	}
	lease.ExpiresAt = minTime(now.Add(leaseTTL), lease.Deadline)
	err = tx.QueryRow(ctx, `INSERT INTO research_job_attempts
		(job_id, attempt, worker_id, claim_id, lease_token, started_at, expires_at, deadline)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, $6, $7) RETURNING lease_token::text`,
		jobID, lease.Attempt, workerID, claimID, now, lease.ExpiresAt, lease.Deadline).Scan(&lease.Token)
	if err != nil {
		return Lease{}, fmt.Errorf("write research attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE research_jobs SET status = 'leased', attempt_count = $2 WHERE id = $1`, jobID, lease.Attempt); err != nil {
		return Lease{}, fmt.Errorf("record current research attempt: %w", err)
	}
	lease.Job.Status = "leased"
	return lease, nil
}

func setJobStatus(ctx context.Context, tx pgx.Tx, jobID, status string) error {
	if _, err := tx.Exec(ctx, `UPDATE research_jobs SET status = $2 WHERE id = $1`, jobID, status); err != nil {
		return fmt.Errorf("set terminal research job state: %w", err)
	}
	return nil
}

func claimInputs(ctx context.Context, tx pgx.Tx, id string) (Job, time.Time, error) {
	var job Job
	var snapshotID string
	var snapshot, requestDigest, snapshotDigest []byte
	err := tx.QueryRow(ctx, `SELECT j.id::text, j.request, j.request_sha256, j.status, j.created_at,
		s.id::text, s.payload, s.payload_sha256 FROM research_jobs j JOIN research_snapshots s ON s.id = j.snapshot_id
		WHERE j.id = $1`, id).Scan(&job.ID, &job.Request, &requestDigest, &job.Status, &job.CreatedAt,
		&snapshotID, &snapshot, &snapshotDigest)
	if err != nil {
		return Job{}, time.Time{}, fmt.Errorf("read persisted research inputs: %w", err)
	}
	requestHash := sha256.Sum256(job.Request)
	snapshotHash := sha256.Sum256(snapshot)
	if !bytes.Equal(requestHash[:], requestDigest) || !bytes.Equal(snapshotHash[:], snapshotDigest) {
		return Job{}, time.Time{}, errors.New("persisted research inputs do not match their hashes")
	}
	var request struct {
		ID        string          `json:"job_id"`
		Version   string          `json:"job_schema_version"`
		Portfolio json.RawMessage `json:"portfolio"`
	}
	var portfolio struct {
		ID   string    `json:"snapshot_id"`
		AsOf time.Time `json:"as_of"`
	}
	if json.Unmarshal(job.Request, &request) != nil || request.ID != job.ID || request.Version != "0.1" ||
		!bytes.Equal(request.Portfolio, snapshot) || json.Unmarshal(snapshot, &portfolio) != nil ||
		portfolio.ID != snapshotID || portfolio.AsOf.IsZero() {
		return Job{}, time.Time{}, errors.New("persisted research inputs have inconsistent identities or version")
	}
	job.CreatedAt = job.CreatedAt.UTC()
	return job, portfolio.AsOf.UTC(), nil
}

// Heartbeat extends only the latest unexpired attempt, bounded by its deadline.
func (s *Store) Heartbeat(ctx context.Context, jobID, workerID, token string) (Lease, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Lease{}, fmt.Errorf("begin research heartbeat: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		return Lease{}, fmt.Errorf("configure durable heartbeat: %w", err)
	}
	var lockedID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM research_jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrNotFound
	}
	if err != nil {
		return Lease{}, fmt.Errorf("lock research job for heartbeat: %w", err)
	}
	lease, now, err := currentLease(ctx, tx, lockedID)
	if err != nil {
		return Lease{}, err
	}
	if lease.WorkerID != workerID || lease.Token != token {
		return Lease{}, ErrLeaseConflict
	}
	lease.ExpiresAt = minTime(now.Add(leaseTTL), lease.Deadline)
	if _, err := tx.Exec(ctx, `UPDATE research_job_attempts SET expires_at = $3 WHERE job_id = $1 AND attempt = $2`,
		lockedID, lease.Attempt, lease.ExpiresAt); err != nil {
		return Lease{}, fmt.Errorf("renew research lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Lease{}, fmt.Errorf("commit research heartbeat: %w", err)
	}
	return lease, nil
}

// currentLease runs only after locking the owning job. The expiry check uses
// clock_timestamp rather than the time this transaction started waiting.
func currentLease(ctx context.Context, tx pgx.Tx, id string) (Lease, time.Time, error) {
	var lease Lease
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT j.id::text, j.status, a.worker_id, a.claim_id::text, a.lease_token::text,
		a.attempt, a.expires_at, a.deadline, clock_timestamp()
		FROM research_jobs j JOIN research_job_attempts a ON a.job_id = j.id AND a.attempt = j.attempt_count
		WHERE j.id = $1 AND j.status = 'leased'`, id).Scan(&lease.Job.ID, &lease.Job.Status, &lease.WorkerID, &lease.ClaimID,
		&lease.Token, &lease.Attempt, &lease.ExpiresAt, &lease.Deadline, &now)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !now.Before(lease.ExpiresAt)) {
		return Lease{}, time.Time{}, ErrLeaseConflict
	}
	if err != nil {
		return Lease{}, time.Time{}, fmt.Errorf("read latest research attempt: %w", err)
	}
	lease.ExpiresAt, lease.Deadline = lease.ExpiresAt.UTC(), lease.Deadline.UTC()
	return lease, now.UTC(), nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a.UTC()
	}
	return b.UTC()
}
