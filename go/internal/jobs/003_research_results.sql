ALTER TABLE research_jobs DROP CONSTRAINT research_jobs_status_check;
ALTER TABLE research_jobs ADD CONSTRAINT research_jobs_status_check
    CHECK (status IN ('queued', 'leased', 'failed', 'expired', 'completed'));

ALTER TABLE research_outbox DROP CONSTRAINT research_outbox_event_type_check;
ALTER TABLE research_outbox ADD CONSTRAINT research_outbox_event_type_check
    CHECK (event_type IN ('research_job.queued', 'research_job.completed'));

CREATE TABLE research_job_results (
    job_id uuid PRIMARY KEY,
    attempt integer NOT NULL,
    completed_at timestamptz NOT NULL,
    payload bytea NOT NULL,
    payload_sha256 bytea NOT NULL CHECK (octet_length(payload_sha256) = 32),
    FOREIGN KEY (job_id, attempt) REFERENCES research_job_attempts(job_id, attempt)
);
