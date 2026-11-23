package jobs

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/trading-api/internal/contract"
)

//go:embed 007_research_calendar_occurrences.sql
var researchCalendarMigration string

var ErrCalendarWindow = errors.New("research calendar window is not closed or outside the observation lookback")

const selectCalendarOccurrence = `SELECT o.job_id::text, o.schedule_id, o.scheduled_at, o.max_lateness_seconds,
	encode(o.request_sha256, 'hex'), o.recorded_at, j.created_at,
	CASE WHEN j.id IS NULL THEN 'missed'
		WHEN j.request_sha256 = o.request_sha256 THEN 'job_recorded' ELSE 'job_conflict' END
	FROM research_schedule_occurrences o LEFT JOIN research_jobs j ON j.id = o.job_id
	WHERE o.job_id = $1`

// RecordCalendarOccurrence registers that a declared submission window closed.
// The returned job state is projected from the jobs table, including a job
// received concurrently or whose original HTTP response was lost.
func (s *Store) RecordCalendarOccurrence(ctx context.Context, occurrence contract.ResearchCalendarOccurrence) (contract.ResearchCalendarReceipt, bool, error) {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return contract.ResearchCalendarReceipt{}, false, fmt.Errorf("begin calendar observation: %w", err)
	}
	defer rollback(tx)
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return contract.ResearchCalendarReceipt{}, false, fmt.Errorf("read calendar observation clock: %w", err)
	}
	if deadline := occurrence.Deadline(); now.Before(deadline) || now.Sub(deadline) > contract.ResearchCalendarObservationLookback {
		return contract.ResearchCalendarReceipt{}, false, ErrCalendarWindow
	}
	jobID := contract.ResearchCalendarJobID(occurrence.ScheduleID, occurrence.ScheduledAt)
	written, err := tx.Exec(ctx, `INSERT INTO research_schedule_occurrences
		(job_id, schedule_id, scheduled_at, max_lateness_seconds, request_sha256, recorded_at)
		VALUES ($1, $2, $3, $4, decode($5, 'hex'), $6) ON CONFLICT (job_id) DO NOTHING`,
		jobID, occurrence.ScheduleID, occurrence.ScheduledAt.UTC().Format(time.RFC3339Nano),
		occurrence.MaxLatenessSeconds, occurrence.RequestSHA256, now)
	if err != nil {
		return contract.ResearchCalendarReceipt{}, false, fmt.Errorf("store calendar observation: %w", err)
	}
	receipt, err := readCalendarOccurrence(tx.QueryRow(ctx, selectCalendarOccurrence, jobID))
	if err != nil {
		return contract.ResearchCalendarReceipt{}, false, err
	}
	if receipt.ScheduleID != occurrence.ScheduleID || !receipt.ScheduledAt.Equal(occurrence.ScheduledAt) ||
		receipt.MaxLatenessSeconds != occurrence.MaxLatenessSeconds || receipt.RequestSHA256 != occurrence.RequestSHA256 {
		return contract.ResearchCalendarReceipt{}, false, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return contract.ResearchCalendarReceipt{}, false, fmt.Errorf("commit calendar observation: %w", err)
	}
	return receipt, written.RowsAffected() == 1, nil
}

func (s *Store) GetCalendarOccurrence(ctx context.Context, jobID string) (contract.ResearchCalendarReceipt, error) {
	return readCalendarOccurrence(s.pool.QueryRow(ctx, selectCalendarOccurrence, jobID))
}

func readCalendarOccurrence(row pgx.Row) (contract.ResearchCalendarReceipt, error) {
	var receipt contract.ResearchCalendarReceipt
	var scheduledAt string
	if err := row.Scan(&receipt.JobID, &receipt.ScheduleID, &scheduledAt, &receipt.MaxLatenessSeconds,
		&receipt.RequestSHA256, &receipt.RecordedAt, &receipt.JobCreatedAt, &receipt.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contract.ResearchCalendarReceipt{}, ErrNotFound
		}
		return contract.ResearchCalendarReceipt{}, fmt.Errorf("read calendar observation: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, scheduledAt)
	if err != nil || at.UTC().Format(time.RFC3339Nano) != scheduledAt || contract.ResearchCalendarJobID(receipt.ScheduleID, at) != receipt.JobID {
		return contract.ResearchCalendarReceipt{}, errors.New("persisted calendar timestamp does not match its identity")
	}
	receipt.Version = "0.1"
	receipt.ScheduledAt = at.UTC()
	receipt.RecordedAt = receipt.RecordedAt.UTC()
	if receipt.JobCreatedAt != nil {
		*receipt.JobCreatedAt = receipt.JobCreatedAt.UTC()
	}
	return receipt, nil
}
