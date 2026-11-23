ALTER TABLE research_jobs DROP CONSTRAINT research_jobs_status_check;
ALTER TABLE research_jobs ADD CONSTRAINT research_jobs_status_check
    CHECK (status IN ('queued', 'leased', 'failed', 'expired'));
ALTER TABLE research_jobs ADD COLUMN attempt_count integer NOT NULL DEFAULT 0
    CHECK (attempt_count >= 0);

CREATE TABLE research_job_attempts (
    job_id uuid NOT NULL REFERENCES research_jobs(id),
    attempt integer NOT NULL CHECK (attempt > 0),
    worker_id text NOT NULL CHECK (char_length(worker_id) BETWEEN 1 AND 128),
    claim_id uuid NOT NULL UNIQUE,
    lease_token uuid NOT NULL UNIQUE,
    started_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    deadline timestamptz NOT NULL,
    PRIMARY KEY (job_id, attempt),
    CHECK (started_at < expires_at AND expires_at <= deadline)
);

CREATE INDEX research_jobs_claim_order ON research_jobs (created_at, id)
    WHERE status IN ('queued', 'leased');
