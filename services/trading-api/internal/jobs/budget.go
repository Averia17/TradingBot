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

type jobCallBudget struct {
	limit   int
	invalid bool
}

func (b *jobCallBudget) UnmarshalJSON(value []byte) error {
	// Only authorization requires a budget; read-only replay and completion
	// still accept older jobs whose otherwise valid requests omit it.
	if json.Unmarshal(value, &b.limit) != nil {
		b.invalid = true
	}
	return nil
}

// AuthorizeCall reserves a job slot and any required daily capacity before an
// invocation. Even a lost response, failed invocation or expired attempt keeps
// the slot consumed. A replay can retrieve only its own active attempt's permit.
func (s *Store) AuthorizeCall(ctx context.Context, jobID, workerID, token, callID string) (CallPermit, bool, error) {
	job, err := s.beginWorkerJob(ctx, jobID, "call authorization")
	if err != nil {
		return CallPermit{}, false, err
	}
	defer rollback(job.tx)
	lease, _, err := job.workerLease(ctx, workerID, token)
	if err != nil {
		return CallPermit{}, false, err
	}
	if job.legacyBudgetUnknown {
		return CallPermit{}, false, ErrBudgetExhausted
	}
	grant := CallPermit{JobID: job.id, CallID: callID}
	var leaseActive bool
	err = job.tx.QueryRow(ctx, `SELECT attempt, remaining_llm_calls, clock_timestamp() < $3::timestamptz
		FROM research_call_permits WHERE job_id = $1 AND call_id = $2`, job.id, callID, lease.ExpiresAt).
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
	inputs, err := readJobInputs(ctx, job.tx, job.id)
	if err != nil {
		return CallPermit{}, false, err
	}
	budget := inputs.callBudget
	if budget.invalid || budget.limit < 1 || budget.limit > maxJobCallPermits {
		return CallPermit{}, false, errors.New("invalid persisted research call budget")
	}
	activeFrom, err := lockDailyCallPolicy(ctx, job.tx)
	if err != nil {
		return CallPermit{}, false, err
	}
	// Use the database clock at the actual insert, after validation and any
	// waits. A new statement sees every receipt committed under the policy lock.
	var created bool
	err = job.tx.QueryRow(ctx, `WITH budget AS MATERIALIZED (
			SELECT $4::integer - (SELECT count(*)::integer FROM research_call_permits WHERE job_id = $1) AS available,
			       clock_timestamp() AS now
		), inserted AS (
			INSERT INTO research_call_permits (job_id, attempt, call_id, remaining_llm_calls, granted_at)
			SELECT $1, $2, $3, available - 1, now FROM budget
			WHERE available > 0 AND now < $5::timestamptz
			  AND ($6::date IS NULL OR (now AT TIME ZONE 'UTC')::date < $6::date OR
				(SELECT max_calls FROM research_daily_call_limits WHERE utc_day = (budget.now AT TIME ZONE 'UTC')::date) >
				(SELECT count(*) FROM research_call_permits
				 WHERE granted_at >= date_trunc('day', budget.now, 'UTC')
				   AND granted_at < date_trunc('day', budget.now, 'UTC') + interval '24 hours'))
			RETURNING remaining_llm_calls
		)
		SELECT available - 1, now < $5::timestamptz, EXISTS (SELECT 1 FROM inserted) FROM budget`,
		job.id, job.attempt, callID, budget.limit, lease.ExpiresAt, activeFrom).Scan(&grant.Remaining, &leaseActive, &created)
	if err != nil {
		return CallPermit{}, false, fmt.Errorf("persist research call permit: %w", err)
	}
	if !leaseActive {
		return CallPermit{}, false, ErrLeaseConflict
	}
	if !created {
		return CallPermit{}, false, ErrBudgetExhausted
	}
	if err := job.tx.Commit(ctx); err != nil {
		return CallPermit{}, false, fmt.Errorf("commit research call permit: %w", err)
	}
	grant.Attempt = job.attempt
	return grant, true, nil
}
