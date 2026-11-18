package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ScheduleEvidence describes durable facts for a requested calendar job ID.
// Nothing here claims that a corresponding scheduler process ran.
type ScheduleEvidence struct {
	Observation *ScheduleObservation
	Job         *ScheduleJobEvidence
}

type ScheduleObservation struct {
	ScheduleID         string
	ScheduledAt        string
	MaxLatenessSeconds int
	RequestSHA256      string
	RecordedAt         time.Time
}

type ScheduleJobEvidence struct {
	Status                  string
	CreatedAt               time.Time
	AttemptCount            int
	LeaseExpiresAt          *time.Time
	CompletedAt             *time.Time
	RequestSHA256           string
	RequestIntegrity        bool
	ResultIntegrity         bool
	PermitHistoryKnown      bool
	DurablePermits          int
	RecordedCallbacks       int
	InvalidCallbackReceipts int
}

// ReadScheduleEvidence uses the database clock and one read-only snapshot for
// the entire set of declared job IDs, including those with no durable rows.
func ReadScheduleEvidence(ctx context.Context, dsn string, ids []string) (time.Time, map[string]ScheduleEvidence, error) {
	if len(ids) == 0 || len(ids) > 128 {
		return time.Time{}, nil, errors.New("invalid schedule evidence size")
	}
	var checkedAt time.Time
	evidence := make(map[string]ScheduleEvidence, len(ids))
	err := withResearchReadSnapshot(ctx, dsn, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&checkedAt); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT declared.id::text,
			coalesce(o.schedule_id,''),coalesce(o.scheduled_at,''),coalesce(o.max_lateness_seconds,0),
			coalesce(encode(o.request_sha256,'hex'),''),o.recorded_at,
			coalesce(j.status,''),j.created_at,coalesce(j.attempt_count,0),a.expires_at,r.completed_at,
			coalesce(encode(j.request_sha256,'hex'),''),coalesce(j.request_sha256=sha256(j.request),false),
			CASE WHEN j.status='completed' THEN coalesce(r.attempt=j.attempt_count AND r.payload_sha256=sha256(r.payload),false)
				ELSE r.job_id IS NULL END,
			coalesce(NOT j.legacy_budget_unknown AND m.applied_at IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM research_job_attempts old WHERE old.job_id=j.id AND old.started_at<m.applied_at
			),false),
			permits.total,permits.receipts,permits.invalid
			FROM unnest($1::uuid[]) AS declared(id)
			LEFT JOIN research_schedule_occurrences o ON o.job_id=declared.id
			LEFT JOIN research_jobs j ON j.id=declared.id
			LEFT JOIN research_job_attempts a ON a.job_id=j.id AND a.attempt=j.attempt_count AND j.status='leased'
			LEFT JOIN research_job_results r ON r.job_id=j.id
			LEFT JOIN research_schema_migrations m ON m.version=4
			CROSS JOIN LATERAL (
				SELECT count(*) AS total,count(u.call_id) AS receipts,
				count(*) FILTER (WHERE u.call_id IS NOT NULL AND
					(u.payload_sha256<>sha256(u.payload) OR u.recorded_at<p.granted_at)) AS invalid
				FROM research_call_permits p LEFT JOIN research_call_usage u USING (job_id,call_id) WHERE p.job_id=j.id
			) permits`, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var observation ScheduleObservation
			var job ScheduleJobEvidence
			var recordedAt, createdAt *time.Time
			if err := rows.Scan(&id, &observation.ScheduleID, &observation.ScheduledAt, &observation.MaxLatenessSeconds,
				&observation.RequestSHA256, &recordedAt, &job.Status, &createdAt, &job.AttemptCount,
				&job.LeaseExpiresAt, &job.CompletedAt, &job.RequestSHA256, &job.RequestIntegrity, &job.ResultIntegrity,
				&job.PermitHistoryKnown, &job.DurablePermits, &job.RecordedCallbacks, &job.InvalidCallbackReceipts); err != nil {
				return err
			}
			var value ScheduleEvidence
			if recordedAt != nil {
				observation.RecordedAt = recordedAt.UTC()
				value.Observation = &observation
			}
			if createdAt != nil {
				job.CreatedAt = createdAt.UTC()
				for _, stamp := range []*time.Time{job.LeaseExpiresAt, job.CompletedAt} {
					if stamp != nil {
						*stamp = stamp.UTC()
					}
				}
				value.Job = &job
			}
			evidence[id] = value
		}
		return rows.Err()
	})
	if err != nil {
		return time.Time{}, nil, err
	}
	return checkedAt.UTC(), evidence, nil
}
