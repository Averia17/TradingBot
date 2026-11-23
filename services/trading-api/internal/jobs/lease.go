package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	leaseTTL             = time.Minute
	maxAttemptDuration   = 15 * time.Minute
	maxAttempts          = 3
	maxClaimCandidates   = 16
	offlineAnalysisMode  = "offline"
	ProviderAnalysisMode = "prepared_provider"
)

var (
	ErrNoJob         = errors.New("no research job available")
	ErrLeaseConflict = errors.New("research lease is expired, superseded, or belongs to another worker")
)

var registryDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Lease struct {
	Job       Job
	WorkerID  string
	ClaimID   string
	Token     string
	Attempt   int
	ExpiresAt time.Time
	Deadline  time.Time
}

type claimScope struct {
	registrySHA string
	mode        string
}

// Claim creates one attempt or replays the same still-active claim. A claim ID
// that found no work is unbound until an attempt is committed.
func (s *Store) Claim(ctx context.Context, workerID, claimID string, maxSnapshotAge time.Duration) (Lease, error) {
	return s.claim(ctx, workerID, claimID, maxSnapshotAge, claimScope{mode: offlineAnalysisMode})
}

// ClaimWithIdentityRegistry selects jobs whose immutable registry digest
// the worker is configured to verify.
func (s *Store) ClaimWithIdentityRegistry(ctx context.Context, workerID, claimID string, maxSnapshotAge time.Duration, registrySHA string) (Lease, error) {
	if !registryDigest.MatchString(registrySHA) {
		return Lease{}, errors.New("invalid research claim registry digest")
	}
	return s.claim(ctx, workerID, claimID, maxSnapshotAge, claimScope{registrySHA, offlineAnalysisMode})
}

func (s *Store) ClaimPreparedProvider(ctx context.Context, workerID, claimID string, maxSnapshotAge time.Duration, registrySHA string) (Lease, error) {
	if !registryDigest.MatchString(registrySHA) {
		return Lease{}, errors.New("invalid research claim registry digest")
	}
	return s.claim(ctx, workerID, claimID, maxSnapshotAge, claimScope{registrySHA, ProviderAnalysisMode})
}

func (s *Store) claim(ctx context.Context, workerID, claimID string, maxSnapshotAge time.Duration, scope claimScope) (Lease, error) {
	if maxSnapshotAge <= 0 {
		return Lease{}, errors.New("invalid research snapshot age limit")
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("begin research claim: %w", err)
	}
	defer rollback(tx)
	// Serialize duplicate requests for the same claim before choosing a job.
	// A hash collision here can only make unrelated requests wait for each other.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 737292))`, claimID); err != nil {
		return Lease{}, fmt.Errorf("lock research claim identity: %w", err)
	}
	if lease, err := replayClaim(ctx, tx, workerID, claimID, maxSnapshotAge, scope); !errors.Is(err, pgx.ErrNoRows) {
		return lease, err
	}
	for range maxClaimCandidates {
		lease, err := claimNext(ctx, tx, workerID, claimID, maxSnapshotAge, scope)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return Lease{}, err
		}
		if lease.Token != "" {
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

func replayClaim(ctx context.Context, tx pgx.Tx, workerID, claimID string, maxSnapshotAge time.Duration, scope claimScope) (Lease, error) {
	job := lockedJob{tx: tx}
	var prior claimScope
	err := tx.QueryRow(ctx, `SELECT j.id::text, j.status, j.attempt_count, a.identity_registry_sha256, j.analysis_mode
		FROM research_job_attempts a JOIN research_jobs j ON a.job_id = j.id
		WHERE a.claim_id = $1 FOR UPDATE OF j`, claimID).Scan(&job.id, &job.status, &job.attempt, &prior.registrySHA, &prior.mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, pgx.ErrNoRows
	}
	if err != nil {
		return Lease{}, fmt.Errorf("find prior research claim: %w", err)
	}
	lease, now, err := job.activeLease(ctx)
	if err != nil {
		return Lease{}, err
	}
	if lease.ClaimID != claimID || lease.WorkerID != workerID || prior != scope {
		return Lease{}, ErrLeaseConflict
	}
	inputs, err := readJobInputs(ctx, tx, job.id)
	if err != nil {
		return Lease{}, err
	}
	if now.Before(inputs.snapshotAt) || !now.Before(inputs.snapshotAt.Add(maxSnapshotAge)) {
		return Lease{}, ErrLeaseConflict
	}
	lease.Job = inputs.Job
	return lease, nil
}

func claimNext(ctx context.Context, tx pgx.Tx, workerID, claimID string, maxSnapshotAge time.Duration, scope claimScope) (Lease, error) {
	job := lockedJob{tx: tx}
	err := tx.QueryRow(ctx, `SELECT j.id::text, j.status, j.attempt_count FROM research_jobs j
		LEFT JOIN research_job_attempts a ON a.job_id = j.id AND a.attempt = j.attempt_count
		WHERE j.identity_registry_sha256 = $1 AND j.analysis_mode = $2
			AND (j.status = 'queued' OR (j.status = 'leased' AND a.expires_at <= statement_timestamp()))
		ORDER BY j.created_at, j.id LIMIT 1 FOR UPDATE OF j SKIP LOCKED`, scope.registrySHA, scope.mode).Scan(&job.id, &job.status, &job.attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, pgx.ErrNoRows
	}
	if err != nil {
		return Lease{}, fmt.Errorf("select research job for claim: %w", err)
	}
	return job.claim(ctx, workerID, claimID, maxSnapshotAge, scope.registrySHA)
}

func (j lockedJob) claim(ctx context.Context, workerID, claimID string, maxSnapshotAge time.Duration, registrySHA string) (Lease, error) {
	// A heartbeat may commit between the candidate query's statement snapshot
	// and the job row lock. Re-read the attempt after locking the owning job.
	if j.status == "leased" {
		_, _, err := j.activeLease(ctx)
		if !errors.Is(err, ErrLeaseConflict) {
			return Lease{}, err
		}
	}
	if j.attempt >= maxAttempts {
		return Lease{}, j.setStatus(ctx, "failed")
	}
	inputs, err := readJobInputs(ctx, j.tx, j.id)
	if err != nil {
		return Lease{}, err
	}
	// The clock is read after acquiring the row lock and checking stored bytes.
	var now time.Time
	if err := j.tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Lease{}, fmt.Errorf("read research queue clock: %w", err)
	}
	cutoff := inputs.snapshotAt.Add(maxSnapshotAge)
	if !now.Before(cutoff) {
		return Lease{}, j.setStatus(ctx, "expired")
	}
	if now.Before(inputs.snapshotAt) {
		// A request admitted by the control clock may briefly be ahead of the
		// database. Leave it queued and try another poll after clocks converge.
		return Lease{}, pgx.ErrNoRows
	}
	lease := Lease{
		Job:      inputs.Job,
		WorkerID: workerID,
		ClaimID:  claimID,
		Attempt:  j.attempt + 1,
		Deadline: minTime(now.Add(maxAttemptDuration), cutoff),
	}
	lease.ExpiresAt = minTime(now.Add(leaseTTL), lease.Deadline)
	err = j.tx.QueryRow(ctx, `WITH new_attempt AS (
			INSERT INTO research_job_attempts
			(job_id, attempt, worker_id, claim_id, lease_token, started_at, expires_at, deadline, identity_registry_sha256)
			VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, $6, $7, $8) RETURNING lease_token
		)
		UPDATE research_jobs SET status = 'leased', attempt_count = $2 WHERE id = $1
		RETURNING (SELECT lease_token::text FROM new_attempt)`,
		j.id, lease.Attempt, workerID, claimID, now, lease.ExpiresAt, lease.Deadline, registrySHA).Scan(&lease.Token)
	if err != nil {
		return Lease{}, fmt.Errorf("record current research attempt: %w", err)
	}
	lease.Job.Status = "leased"
	return lease, nil
}

type registryBinding struct {
	Version    string        `json:"job_schema_version"`
	Mode       string        `json:"analysis_mode"`
	CallBudget jobCallBudget `json:"max_llm_calls"`
	Registry   *struct {
		SHA256 string `json:"sha256"`
	} `json:"identity_registry"`
}

func researchClaimScope(raw []byte) (claimScope, error) {
	var binding registryBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return claimScope{}, err
	}
	return binding.scope()
}

func (binding registryBinding) scope() (claimScope, error) {
	if binding.Version == "0.2" || binding.Version == "0.3" {
		isProvider := binding.Version == "0.3"
		if isProvider && (binding.CallBudget.invalid || binding.CallBudget.limit != 1) {
			return claimScope{}, errors.New("prepared provider jobs require exactly one call")
		}
		if binding.Registry != nil && registryDigest.MatchString(binding.Registry.SHA256) &&
			(isProvider && binding.Mode == ProviderAnalysisMode || !isProvider && binding.Mode == "") {
			mode := offlineAnalysisMode
			if isProvider {
				mode = ProviderAnalysisMode
			}
			return claimScope{binding.Registry.SHA256, mode}, nil
		}
		return claimScope{}, errors.New("invalid research job registry or analysis mode")
	}
	if binding.Registry != nil || binding.Mode != "" {
		return claimScope{}, errors.New("unexpected research job registry or analysis mode")
	}
	return claimScope{mode: offlineAnalysisMode}, nil
}

// Heartbeat extends only the latest unexpired attempt, bounded by its deadline.
func (s *Store) Heartbeat(ctx context.Context, jobID, workerID, token string) (Lease, error) {
	job, err := s.beginWorkerJob(ctx, jobID, "heartbeat")
	if err != nil {
		return Lease{}, err
	}
	defer rollback(job.tx)
	if job.status != "leased" {
		return Lease{}, ErrLeaseConflict
	}
	lease := Lease{Job: Job{ID: job.id, Status: job.status}, WorkerID: workerID, Token: token, Attempt: job.attempt}
	// The fresh clock is evaluated after the job row lock, in the same
	// statement that checks the attempt's owner and extends the lease.
	err = job.tx.QueryRow(ctx, `WITH measured AS MATERIALIZED (SELECT clock_timestamp() AS now)
		UPDATE research_job_attempts a
		SET expires_at = LEAST(measured.now + make_interval(secs => $5::integer), a.deadline)
		FROM measured WHERE a.job_id = $1 AND a.attempt = $2 AND a.worker_id = $3
			AND a.lease_token::text = $4 AND measured.now < a.expires_at
		RETURNING a.claim_id::text, a.expires_at, a.deadline`, job.id, job.attempt, workerID, token, int(leaseTTL/time.Second)).
		Scan(&lease.ClaimID, &lease.ExpiresAt, &lease.Deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrLeaseConflict
	}
	if err != nil {
		return Lease{}, fmt.Errorf("renew research lease: %w", err)
	}
	if err := job.tx.Commit(ctx); err != nil {
		return Lease{}, fmt.Errorf("commit research heartbeat: %w", err)
	}
	lease.ExpiresAt, lease.Deadline = lease.ExpiresAt.UTC(), lease.Deadline.UTC()
	return lease, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a.UTC()
	}
	return b.UTC()
}
