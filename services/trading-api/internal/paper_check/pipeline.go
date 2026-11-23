package paper_check

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/paper_workflow"
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

type researchPipelineRow struct {
	Job              pipelineJob `json:"job"`
	CompletedAttempt *int        `json:"completed_attempt,omitempty"`
	RequestSHA256    string      `json:"request_sha256"`
	BindingValid     bool        `json:"binding_valid"`
}

type accountPipelineRow struct {
	JobID       string    `json:"job_id"`
	SHA256      string    `json:"request_sha256"`
	CreatedAt   time.Time `json:"created_at"`
	HasReceipt  bool      `json:"has_receipt"`
	OrderID     string    `json:"order_id,omitempty"`
	OrderStatus string    `json:"order_status,omitempty"`
	Valid       bool      `json:"valid"`
}

func checkPipeline(ctx context.Context, tx pgx.Tx, stallAfter time.Duration, r *report) error {
	selected := ""
	if r.Research != nil {
		selected = r.Research.JobID
	}
	rows, truncated, err := readResearchPipeline(ctx, tx, r.AccountID, selected, r.CheckedAt)
	if err != nil {
		return err
	}
	applications, err := readAccountPipeline(ctx, tx, r.AccountID, pipelineJobIDs(rows), r.CheckedAt)
	if err != nil {
		return err
	}
	composePipeline(r, rows, truncated, applications, stallAfter)
	return checkFillRelay(ctx, tx, stallAfter, r)
}

func readResearchPipeline(ctx context.Context, tx pgx.Tx, accountID, selected string, checkedAt time.Time) ([]researchPipelineRow, bool, error) {
	// Only the jobs owner reads these tables. Limit candidates before validating
	// the stored hashes and identity bindings.
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
		d.finished_at, coalesce(d.finished_status,''), d.request, d.request_sha256,
		c.job_request, c.job_sha256, c.registry_sha, a.identity_registry_sha256,
		CASE WHEN c.registry_sha <> '' THEN rr.payload END,
		CASE WHEN c.registry_sha <> '' THEN rr.payload_sha256 END
	FROM candidates c
	JOIN research_paper_dispatch d ON d.job_id=c.job_id
	LEFT JOIN research_job_attempts a ON a.job_id=c.job_id AND a.attempt=c.attempt_count
	LEFT JOIN research_job_results rr ON rr.job_id=c.job_id
	ORDER BY (c.job_id=nullif($2,'')::uuid) DESC NULLS LAST, c.created_at, c.job_id`,
		accountID, selected, pipelinePageSize+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	facts := make([]researchPipelineRow, 0)
	var truncated bool
	for rows.Next() {
		if len(facts) == pipelinePageSize {
			truncated = true
			break
		}
		var row researchPipelineRow
		job := &row.Job
		var archivedJob jobs.Job
		var archivedResult jobs.Result
		var request, requestHash, jobHash, resultHash []byte
		var registrySHA string
		var attemptRegistrySHA *string
		if err := rows.Scan(&job.JobID, &job.ResearchStatus, &job.AttemptCount, &job.JobCreatedAt,
			&job.LeaseExpiresAt, &job.ResearchCompletedAt, &row.CompletedAttempt,
			&job.FinishedAt, &job.FinishedStatus, &request, &requestHash,
			&archivedJob.Request, &jobHash, &registrySHA, &attemptRegistrySHA, &archivedResult.Payload, &resultHash); err != nil {
			return nil, false, err
		}
		job.JobCreatedAt = job.JobCreatedAt.UTC()
		for _, stamp := range []*time.Time{job.LeaseExpiresAt, job.ResearchCompletedAt, job.FinishedAt} {
			if stamp != nil {
				*stamp = stamp.UTC()
			}
		}
		archivedJob.ID, archivedJob.Status, archivedJob.CreatedAt = job.JobID, job.ResearchStatus, job.JobCreatedAt
		row.RequestSHA256 = hex.EncodeToString(requestHash)
		row.BindingValid = matchesDigest(request, requestHash) && matchesDigest(archivedJob.Request, jobHash) &&
			(attemptRegistrySHA == nil || *attemptRegistrySHA == registrySHA)
		if row.BindingValid && registrySHA != "" {
			var receipt *jobs.Result
			if row.CompletedAttempt != nil && job.ResearchCompletedAt != nil {
				archivedResult.JobID, archivedResult.Attempt, archivedResult.CompletedAt = job.JobID, *row.CompletedAttempt, *job.ResearchCompletedAt
				receipt = &archivedResult
			}
			row.BindingValid = (receipt == nil || matchesDigest(receipt.Payload, resultHash)) &&
				paper_workflow.CheckArchivedIdentityDispatch(request, archivedJob, receipt, accountID, registrySHA, checkedAt)
		} else if row.BindingValid {
			row.BindingValid = validLegacyDispatchBinding(request, archivedJob, accountID)
		}
		facts = append(facts, row)
	}
	rows.Close()
	return facts, truncated, rows.Err()
}

func readAccountPipeline(ctx context.Context, tx pgx.Tx, accountID string, ids []string, asOf time.Time) ([]accountPipelineRow, error) {
	applications := make([]accountPipelineRow, 0)
	if len(ids) == 0 {
		return applications, nil
	}
	// Selecting by job id detects mismatched ownership, but the response never
	// discloses another account's order fields.
	rows, err := tx.Query(ctx, `SELECT p.job_id::text, encode(p.request_sha256,'hex'), p.created_at, p.result IS NOT NULL,
		CASE WHEN p.account_id=$1 THEN coalesce(p.order_id::text,'') ELSE '' END,
		CASE WHEN p.account_id=$1 THEN coalesce(o.status,'') ELSE '' END,
		p.account_id=$1 AND p.quote_sha256 IS NOT DISTINCT FROM sha256(p.quote)
			AND p.result_sha256 IS NOT DISTINCT FROM sha256(p.result)
	FROM paper_applications p
	LEFT JOIN paper_orders o ON (o.account_id, o.order_id)=(p.account_id, p.order_id)
	WHERE p.job_id IN (SELECT id::uuid FROM unnest($2::text[]) ids(id)) AND p.created_at <= $3`, accountID, ids, asOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row accountPipelineRow
		if err := rows.Scan(&row.JobID, &row.SHA256, &row.CreatedAt, &row.HasReceipt, &row.OrderID, &row.OrderStatus, &row.Valid); err != nil {
			return nil, err
		}
		row.CreatedAt = row.CreatedAt.UTC()
		applications = append(applications, row)
	}
	return applications, rows.Err()
}

func pipelineJobIDs(rows []researchPipelineRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Job.JobID)
	}
	return ids
}

func composePipeline(r *report, rows []researchPipelineRow, truncated bool, applications []accountPipelineRow, stallAfter time.Duration) {
	if r.Research != nil {
		r.Pipeline.SelectedJobRegistered = new(false)
	}
	if truncated {
		r.Pipeline.Truncated = true
		r.add("paper_pipeline_truncated")
	}
	byJob := make(map[string]accountPipelineRow, len(applications))
	for _, row := range applications {
		byJob[row.JobID] = row
	}
	for _, row := range rows {
		job := row.Job
		if r.Research != nil && job.JobID == r.Research.JobID {
			*r.Pipeline.SelectedJobRegistered = true
		}
		valid := row.BindingValid
		if application, ok := byJob[job.JobID]; ok {
			job.ApplicationCreatedAt, job.ApplicationReceipt = &application.CreatedAt, application.HasReceipt
			job.OrderID, job.OrderStatus = application.OrderID, application.OrderStatus
			valid = valid && application.Valid && row.RequestSHA256 == application.SHA256
		}
		code := job.classify(r.CheckedAt, stallAfter, valid, row.CompletedAttempt)
		r.Pipeline.Jobs = append(r.Pipeline.Jobs, job)
		if code != "" {
			r.Issues = append(r.Issues, issue{Code: code, JobID: job.JobID, OrderID: job.OrderID})
		}
	}
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
