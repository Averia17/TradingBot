package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/control/internal/contract"
)

//go:embed 008_research_call_usage.sql
var researchCallUsageMigration string

var (
	ErrCallPermitNotFound = errors.New("research invocation has no durable permit")
	ErrInvalidCallUsage   = errors.New("invalid research invocation usage")
)

const (
	maxCallUsagePayload = 4096
	maxJobCallPermits   = 64
)

type RecordResearchCallUsage struct {
	JobID    string
	WorkerID string
	Token    string
	Call     contract.ResearchLLMCall
	Provider bool
}

type CallUsageReceipt struct {
	JobID      string    `json:"job_id"`
	CallID     string    `json:"call_id"`
	Attempt    int       `json:"attempt"`
	RecordedAt time.Time `json:"recorded_at"`
}

// RecordCallUsage appends one terminal callback to an existing permit. An
// expired lease can replay only its own already-committed identical callback.
func (s *Store) RecordCallUsage(ctx context.Context, input RecordResearchCallUsage) (CallUsageReceipt, bool, error) {
	if err := validateCallMode(input.Call, input.Provider); err != nil {
		return CallUsageReceipt{}, false, ErrInvalidCallUsage
	}
	input.Call.StartedAt = input.Call.StartedAt.UTC()
	payload, err := json.Marshal(input.Call)
	if err != nil || len(payload) > maxCallUsagePayload {
		return CallUsageReceipt{}, false, ErrInvalidCallUsage
	}
	digest := sha256.Sum256(payload)
	job, err := s.beginWorkerJob(ctx, input.JobID, "call usage receipt")
	if err != nil {
		return CallUsageReceipt{}, false, err
	}
	defer rollback(job.tx)
	receipt := CallUsageReceipt{JobID: job.id, CallID: input.Call.RunID}
	var (
		owner, token         string
		expiresAt            time.Time
		existingAt           *time.Time
		stored, storedDigest []byte
		mode                 string
	)
	err = job.tx.QueryRow(ctx, `SELECT p.attempt, a.worker_id, a.lease_token::text, a.expires_at,
		u.recorded_at, u.payload, u.payload_sha256, j.analysis_mode
		FROM research_call_permits p
		JOIN research_jobs j ON j.id = p.job_id
		JOIN research_job_attempts a USING (job_id, attempt)
		LEFT JOIN research_call_usage u USING (job_id, call_id)
		WHERE p.job_id=$1 AND p.call_id=$2`, receipt.JobID, receipt.CallID).Scan(
		&receipt.Attempt, &owner, &token, &expiresAt, &existingAt, &stored, &storedDigest, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallUsageReceipt{}, false, ErrCallPermitNotFound
	}
	if err != nil {
		return CallUsageReceipt{}, false, fmt.Errorf("read research call permit owner: %w", err)
	}
	if owner != input.WorkerID || token != input.Token {
		return CallUsageReceipt{}, false, ErrLeaseConflict
	}
	if input.Provider != (mode == ProviderAnalysisMode) {
		return CallUsageReceipt{}, false, ErrInvalidCallUsage
	}
	if existingAt != nil {
		if !bytes.Equal(storedDigest, digest[:]) || !bytes.Equal(stored, payload) {
			return CallUsageReceipt{}, false, ErrConflict
		}
		receipt.RecordedAt = existingAt.UTC()
		return receipt, false, nil
	}
	if job.status != "leased" || job.attempt != receipt.Attempt {
		return CallUsageReceipt{}, false, ErrLeaseConflict
	}
	err = job.tx.QueryRow(ctx, `WITH observed AS MATERIALIZED (SELECT clock_timestamp() AS recorded_at)
		INSERT INTO research_call_usage (job_id,call_id,payload,payload_sha256,recorded_at)
		SELECT $1,$2,$3,$4,recorded_at FROM observed WHERE recorded_at < $5::timestamptz
		RETURNING recorded_at`, receipt.JobID, receipt.CallID, payload, digest[:], expiresAt).Scan(&receipt.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallUsageReceipt{}, false, ErrLeaseConflict
	}
	if err != nil {
		return CallUsageReceipt{}, false, fmt.Errorf("insert research call usage receipt: %w", err)
	}
	if err := job.tx.Commit(ctx); err != nil {
		return CallUsageReceipt{}, false, fmt.Errorf("commit research call usage receipt: %w", err)
	}
	receipt.RecordedAt = receipt.RecordedAt.UTC()
	return receipt, true, nil
}

func validateCallMode(call contract.ResearchLLMCall, provider bool) error {
	if provider {
		return call.ValidateProviderCall()
	}
	return call.ValidateOfflineCall()
}
