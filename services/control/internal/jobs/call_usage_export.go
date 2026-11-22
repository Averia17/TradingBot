package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/control/internal/contract"
)

type ResearchCallUsage struct {
	JobID               string
	Status              string
	AnalysisMode        string
	Attempts            int
	LegacyBudgetUnknown bool
	PermitHistoryKnown  bool
	Records             []CallUsageRecord
}

type CallUsageRecord struct {
	CallID        string
	Attempt       int
	GrantedAt     time.Time
	Payload       []byte
	PayloadSHA256 string
	RecordedAt    *time.Time
}

// ReadResearchCallUsage returns all permits for a job from the same read-only
// snapshot. An unobserved permit has no callback bytes, digest or receipt time.
// Attempts that started before permit accounting keep their history unknown.
func ReadResearchCallUsage(ctx context.Context, dsn, jobID string) (ResearchCallUsage, error) {
	result := ResearchCallUsage{Records: make([]CallUsageRecord, 0)}
	err := withResearchReadSnapshot(ctx, dsn, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT j.id::text,j.status,j.attempt_count,j.legacy_budget_unknown,
			COALESCE((SELECT NOT EXISTS (
				SELECT 1 FROM research_job_attempts a
				WHERE a.job_id=j.id AND a.started_at<m.applied_at
			) FROM research_schema_migrations m WHERE m.version=4 AND m.applied_at IS NOT NULL), false),j.analysis_mode
			FROM research_jobs j WHERE j.id=$1`, jobID).Scan(
			&result.JobID, &result.Status, &result.Attempts, &result.LegacyBudgetUnknown, &result.PermitHistoryKnown, &result.AnalysisMode)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read research call usage job: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT p.call_id::text,p.attempt,p.granted_at,u.payload,u.payload_sha256,u.recorded_at
			FROM research_call_permits p LEFT JOIN research_call_usage u USING (job_id,call_id)
			WHERE p.job_id=$1 ORDER BY p.granted_at,p.call_id LIMIT $2`, result.JobID, maxJobCallPermits+1)
		if err != nil {
			return fmt.Errorf("read research call permits: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			if len(result.Records) == maxJobCallPermits {
				return errors.New("research job exceeds the permit archive size limit")
			}
			var record CallUsageRecord
			var digest []byte
			if err := rows.Scan(&record.CallID, &record.Attempt, &record.GrantedAt, &record.Payload, &digest, &record.RecordedAt); err != nil {
				return fmt.Errorf("read research call usage row: %w", err)
			}
			if err := record.verify(digest, result.AnalysisMode == ProviderAnalysisMode); err != nil {
				return err
			}
			result.Records = append(result.Records, record)
		}
		return rows.Err()
	})
	if err != nil {
		return ResearchCallUsage{}, err
	}
	return result, nil
}

func (record *CallUsageRecord) verify(digest []byte, provider bool) error {
	record.GrantedAt = record.GrantedAt.UTC()
	if record.RecordedAt == nil {
		if record.Payload != nil || digest != nil {
			return errors.New("invalid missing research call usage receipt")
		}
		return nil
	}
	at := record.RecordedAt.UTC()
	record.RecordedAt = &at
	actual := sha256.Sum256(record.Payload)
	if len(record.Payload) == 0 || len(record.Payload) > maxCallUsagePayload ||
		!bytes.Equal(actual[:], digest) || at.Before(record.GrantedAt) {
		return errors.New("invalid persisted research call usage receipt")
	}
	var call contract.ResearchLLMCall
	if contract.Decode(record.Payload, contract.ResearchLLMCallSchema, &call) != nil ||
		validateCallMode(call, provider) != nil || call.RunID != record.CallID {
		return errors.New("invalid persisted research call usage payload")
	}
	record.PayloadSHA256 = hex.EncodeToString(digest)
	return nil
}
