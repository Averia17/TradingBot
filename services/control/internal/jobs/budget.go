package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var (
	ErrBudgetExhausted    = errors.New("research job has no guaranteed call budget remaining")
	ErrCallPermitMismatch = errors.New("research result usage differs from attempt's durable call permits")
)

type CallPermit struct {
	JobID     string
	CallID    string
	Attempt   int
	Remaining int // Budget immediately after this grant, including any earlier attempts.
}

// AuthorizeCall consumes one slot from the immutable job budget before an
// invocation. Even a lost response, failed invocation or expired attempt keeps
// the slot consumed. A replay can retrieve only its own active attempt's permit.
func (s *Store) AuthorizeCall(ctx context.Context, jobID, workerID, token, callID string) (CallPermit, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return CallPermit{}, false, fmt.Errorf("begin research call authorization: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		return CallPermit{}, false, fmt.Errorf("configure durable research call authorization: %w", err)
	}
	// Serialize with other permits, heartbeat, attempts and completion.
	var legacyUnknown bool
	err = tx.QueryRow(ctx, `SELECT legacy_budget_unknown FROM research_jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&legacyUnknown)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallPermit{}, false, ErrNotFound
	}
	if err != nil {
		return CallPermit{}, false, fmt.Errorf("lock research job for call authorization: %w", err)
	}
	lease, _, err := currentLease(ctx, tx, jobID)
	if err != nil {
		return CallPermit{}, false, err
	}
	if lease.WorkerID != workerID || lease.Token != token {
		return CallPermit{}, false, ErrLeaseConflict
	}
	if legacyUnknown {
		return CallPermit{}, false, ErrBudgetExhausted
	}
	grant := CallPermit{JobID: lease.Job.ID, CallID: callID}
	var leaseActive bool
	err = tx.QueryRow(ctx, `SELECT attempt, remaining_llm_calls, clock_timestamp() < $3::timestamptz
		FROM research_call_permits WHERE job_id = $1 AND call_id = $2`, jobID, callID, lease.ExpiresAt).
		Scan(&grant.Attempt, &grant.Remaining, &leaseActive)
	if err == nil {
		if !leaseActive {
			return CallPermit{}, false, ErrLeaseConflict
		}
		if grant.Attempt != lease.Attempt {
			return CallPermit{}, false, ErrConflict
		}
		return grant, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CallPermit{}, false, fmt.Errorf("look up committed research call permit: %w", err)
	}
	limit, err := storedCallLimit(ctx, tx, lease.Job.ID)
	if err != nil {
		return CallPermit{}, false, err
	}
	// Use the database clock at the actual insert, after validation and any
	// waits. Count permits across every attempt while still holding the job lock.
	var created bool
	err = tx.QueryRow(ctx, `WITH budget AS MATERIALIZED (
			SELECT $4::integer - (SELECT count(*)::integer FROM research_call_permits WHERE job_id = $1) AS available,
			       clock_timestamp() AS now
		), inserted AS (
			INSERT INTO research_call_permits (job_id, attempt, call_id, remaining_llm_calls, granted_at)
			SELECT $1, $2, $3, available - 1, now FROM budget
			WHERE available > 0 AND now < $5::timestamptz RETURNING remaining_llm_calls
		)
		SELECT available - 1, now < $5::timestamptz, EXISTS (SELECT 1 FROM inserted) FROM budget`,
		jobID, lease.Attempt, callID, limit, lease.ExpiresAt).Scan(&grant.Remaining, &leaseActive, &created)
	if err != nil {
		return CallPermit{}, false, fmt.Errorf("persist research call permit: %w", err)
	}
	if !leaseActive {
		return CallPermit{}, false, ErrLeaseConflict
	}
	if !created {
		return CallPermit{}, false, ErrBudgetExhausted
	}
	if err := tx.Commit(ctx); err != nil {
		return CallPermit{}, false, fmt.Errorf("commit research call permit: %w", err)
	}
	grant.Attempt = lease.Attempt
	return grant, true, nil
}

func storedCallLimit(ctx context.Context, tx pgx.Tx, jobID string) (int, error) {
	job, _, err := claimInputs(ctx, tx, jobID)
	if err != nil {
		return 0, err
	}
	var request struct {
		MaxLLMCalls int `json:"max_llm_calls"`
	}
	if json.Unmarshal(job.Request, &request) != nil || request.MaxLLMCalls < 1 || request.MaxLLMCalls > 64 {
		return 0, errors.New("invalid persisted research call budget")
	}
	return request.MaxLLMCalls, nil
}
