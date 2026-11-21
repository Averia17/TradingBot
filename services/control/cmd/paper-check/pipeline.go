package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/paper_workflow"
)

const pipelinePageSize = 100

type paperPipeline struct {
	StallAfter            string        `json:"stall_after"`
	Truncated             bool          `json:"truncated"`
	SelectedJobRegistered *bool         `json:"selected_job_registered,omitempty"`
	Jobs                  []pipelineJob `json:"jobs"`
	FillRelay             fillRelay     `json:"fill_relay"`
}

type fillRelay struct {
	PendingFills      int64      `json:"pending_fills"`
	OldestFillEventID string     `json:"oldest_fill_event_id,omitempty"`
	OldestCreatedAt   *time.Time `json:"oldest_created_at,omitempty"`
}

type pipelineJob struct {
	JobID                string     `json:"job_id"`
	Stage                string     `json:"stage"`
	ResearchStatus       string     `json:"research_status"`
	AttemptCount         int        `json:"attempt_count"`
	JobCreatedAt         time.Time  `json:"job_created_at"`
	LeaseExpiresAt       *time.Time `json:"lease_expires_at,omitempty"`
	ResearchCompletedAt  *time.Time `json:"research_completed_at,omitempty"`
	ApplicationCreatedAt *time.Time `json:"application_created_at,omitempty"`
	ApplicationReceipt   bool       `json:"application_receipt"`
	OrderID              string     `json:"order_id,omitempty"`
	OrderStatus          string     `json:"order_status,omitempty"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`
	FinishedStatus       string     `json:"finished_status,omitempty"`
}

func checkPipeline(ctx context.Context, tx pgx.Tx, stallAfter time.Duration, r *report) error {
	var selected string
	if r.Research != nil {
		selected = r.Research.JobID
		r.Pipeline.SelectedJobRegistered = new(false)
	}
	// Limit candidates before reading and validating their document hashes.
	rows, err := tx.Query(ctx, `WITH candidates AS MATERIALIZED (
		SELECT d.job_id, j.status, j.attempt_count, j.created_at, j.request AS job_request,
			j.request_sha256 AS job_sha256, j.identity_registry_sha256 AS registry_sha
		FROM research_paper_dispatch d
		JOIN research_jobs j ON j.id=d.job_id
		WHERE d.account_id=$1 AND (d.finished_at IS NULL OR d.job_id=nullif($2,'')::uuid)
		ORDER BY (d.job_id=nullif($2,'')::uuid) DESC NULLS LAST, j.created_at, d.job_id
		LIMIT $3
	)
	SELECT c.job_id::text, c.status, c.attempt_count, c.created_at, a.expires_at, rr.completed_at, rr.attempt,
		p.created_at, p.result IS NOT NULL, coalesce(p.order_id::text,''), coalesce(o.status,''),
		d.finished_at, coalesce(d.finished_status,''), d.request, d.request_sha256,
		coalesce(p.account_id=d.account_id AND p.request_sha256=d.request_sha256
			AND p.quote_sha256 IS NOT DISTINCT FROM sha256(p.quote)
			AND p.result_sha256 IS NOT DISTINCT FROM sha256(p.result), p.job_id IS NULL),
		c.job_request, c.job_sha256, c.registry_sha, a.identity_registry_sha256,
		CASE WHEN c.registry_sha <> '' THEN rr.payload END,
		CASE WHEN c.registry_sha <> '' THEN rr.payload_sha256 END
	FROM candidates c
	JOIN research_paper_dispatch d ON d.job_id=c.job_id
	LEFT JOIN research_job_attempts a ON a.job_id=c.job_id AND a.attempt=c.attempt_count
	LEFT JOIN research_job_results rr ON rr.job_id=c.job_id
	LEFT JOIN paper_applications p ON p.job_id=c.job_id
	LEFT JOIN paper_orders o ON (o.account_id, o.order_id)=(p.account_id, p.order_id)
	ORDER BY (c.job_id=nullif($2,'')::uuid) DESC NULLS LAST, c.created_at, c.job_id`,
		r.AccountID, selected, pipelinePageSize+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if len(r.Pipeline.Jobs) == pipelinePageSize {
			r.Pipeline.Truncated = true
			r.add("paper_pipeline_truncated")
			break
		}
		var job pipelineJob
		var archivedJob jobs.Job
		var archivedResult jobs.Result
		var completedAttempt *int
		var request, requestHash, jobHash, resultHash []byte
		var registrySHA string
		var attemptRegistrySHA *string
		var applicationOK bool
		if err := rows.Scan(&job.JobID, &job.ResearchStatus, &job.AttemptCount, &job.JobCreatedAt,
			&job.LeaseExpiresAt, &job.ResearchCompletedAt, &completedAttempt,
			&job.ApplicationCreatedAt, &job.ApplicationReceipt, &job.OrderID, &job.OrderStatus,
			&job.FinishedAt, &job.FinishedStatus, &request, &requestHash, &applicationOK,
			&archivedJob.Request, &jobHash, &registrySHA, &attemptRegistrySHA, &archivedResult.Payload, &resultHash); err != nil {
			return err
		}
		if job.JobID == selected {
			*r.Pipeline.SelectedJobRegistered = true
		}
		job.JobCreatedAt = job.JobCreatedAt.UTC()
		for _, stamp := range []*time.Time{job.LeaseExpiresAt, job.ResearchCompletedAt, job.ApplicationCreatedAt, job.FinishedAt} {
			if stamp != nil {
				*stamp = stamp.UTC()
			}
		}
		archivedJob.ID, archivedJob.Status, archivedJob.CreatedAt = job.JobID, job.ResearchStatus, job.JobCreatedAt
		bindingOK := applicationOK && matchesDigest(request, requestHash) && matchesDigest(archivedJob.Request, jobHash) &&
			(attemptRegistrySHA == nil || *attemptRegistrySHA == registrySHA)
		if bindingOK && registrySHA != "" {
			var receipt *jobs.Result
			if completedAttempt != nil && job.ResearchCompletedAt != nil {
				archivedResult.JobID, archivedResult.Attempt, archivedResult.CompletedAt = job.JobID, *completedAttempt, *job.ResearchCompletedAt
				receipt = &archivedResult
			}
			bindingOK = (receipt == nil || matchesDigest(receipt.Payload, resultHash)) &&
				paper_workflow.CheckArchivedIdentityDispatch(request, archivedJob, receipt, r.AccountID, registrySHA, r.CheckedAt)
		} else if bindingOK {
			bindingOK = validLegacyDispatchBinding(request, archivedJob, r.AccountID)
		}
		code := job.classify(r.CheckedAt, stallAfter, bindingOK, completedAttempt)
		r.Pipeline.Jobs = append(r.Pipeline.Jobs, job)
		if code != "" {
			r.Issues = append(r.Issues, issue{Code: code, JobID: job.JobID, OrderID: job.OrderID})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return checkFillRelay(ctx, tx, stallAfter, r)
}

func checkFillRelay(ctx context.Context, tx pgx.Tx, stallAfter time.Duration, r *report) error {
	relay := &r.Pipeline.FillRelay
	err := tx.QueryRow(ctx, `SELECT count(*) OVER (), event_id::text, created_at
		FROM paper_fill_outbox WHERE account_id=$1 AND queued_at IS NULL
		ORDER BY created_at, event_id LIMIT 1`, r.AccountID).
		Scan(&relay.PendingFills, &relay.OldestFillEventID, &relay.OldestCreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	*relay.OldestCreatedAt = relay.OldestCreatedAt.UTC()
	if r.CheckedAt.Sub(*relay.OldestCreatedAt) >= stallAfter {
		r.Issues = append(r.Issues, issue{Code: "paper_fill_relay_stalled", FillEventID: relay.OldestFillEventID})
	}
	return nil
}

func validLegacyDispatchBinding(body []byte, job jobs.Job, accountID string) bool {
	var binding struct {
		Version  string          `json:"paper_apply_schema_version"`
		JobID    string          `json:"job_id"`
		Identity json.RawMessage `json:"identity_binding"`
		Context  struct {
			AccountID string `json:"account_id"`
		} `json:"snapshot_context"`
	}
	var input struct {
		Version  string          `json:"job_schema_version"`
		Registry json.RawMessage `json:"identity_registry"`
	}
	return json.Unmarshal(body, &binding) == nil && binding.Version == paper_workflow.Version && len(binding.Identity) == 0 &&
		binding.JobID == job.ID && binding.Context.AccountID == accountID &&
		json.Unmarshal(job.Request, &input) == nil && input.Version == "0.1" && len(input.Registry) == 0
}

func (j *pipelineJob) classify(now time.Time, stallAfter time.Duration, bindingOK bool, completedAttempt *int) string {
	if !bindingOK || (j.ResearchStatus == "completed") != (completedAttempt != nil) ||
		completedAttempt != nil && *completedAttempt != j.AttemptCount ||
		j.ResearchStatus == "leased" && j.LeaseExpiresAt == nil ||
		j.ResearchStatus != "completed" && j.ApplicationCreatedAt != nil ||
		j.OrderID != "" && j.OrderStatus == "" ||
		j.FinishedAt != nil && !j.validAcknowledgement() {
		j.Stage = "inconsistent"
		return "paper_dispatch_inconsistent"
	}
	if j.FinishedAt != nil {
		j.Stage = "finished"
		return ""
	}
	switch j.ResearchStatus {
	case "queued":
		j.Stage = "queued"
		return stalled(now, &j.JobCreatedAt, stallAfter, "research_queue_stalled")
	case "leased":
		j.Stage = "leased"
		if !now.Before(*j.LeaseExpiresAt) {
			j.Stage = "lease_expired"
			return "research_lease_expired"
		}
	case "completed":
		switch {
		case j.ApplicationCreatedAt == nil:
			j.Stage = "awaiting_application"
			return stalled(now, j.ResearchCompletedAt, stallAfter, "paper_dispatch_application_stalled")
		case !j.ApplicationReceipt:
			j.Stage = "awaiting_recovery"
			return stalled(now, j.ApplicationCreatedAt, stallAfter, "paper_dispatch_recovery_stalled")
		default:
			j.Stage = "awaiting_ack"
			return stalled(now, j.ApplicationCreatedAt, stallAfter, "paper_dispatch_ack_stalled")
		}
	case "failed", "expired":
		j.Stage = "research_" + j.ResearchStatus
		return "paper_dispatch_" + j.Stage
	}
	return ""
}

func (j pipelineJob) validAcknowledgement() bool {
	switch j.FinishedStatus {
	case "hold":
		return j.ResearchStatus == "completed" && j.ApplicationReceipt && j.OrderID == ""
	case "applied":
		return j.ResearchStatus == "completed" && j.ApplicationReceipt && j.OrderID != ""
	case "expired", "snapshot_stale":
		return j.ResearchStatus == "completed" && j.ApplicationCreatedAt == nil
	case "research_failed":
		return j.ResearchStatus == "failed"
	case "research_expired":
		return j.ResearchStatus == "expired"
	default:
		return false
	}
}

func stalled(now time.Time, from *time.Time, after time.Duration, code string) string {
	if from != nil && now.Sub(*from) >= after {
		return code
	}
	return ""
}
