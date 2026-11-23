ALTER TABLE research_jobs ADD COLUMN legacy_budget_unknown boolean NOT NULL DEFAULT false;

-- Calls from earlier attempts were not recorded. Their absence cannot be used
-- as proof that capacity is available for a new attempt.
UPDATE research_jobs SET legacy_budget_unknown = true
    WHERE attempt_count > 0 AND status IN ('queued', 'leased');

CREATE TABLE research_call_permits (
    job_id uuid NOT NULL,
    attempt integer NOT NULL,
    call_id uuid NOT NULL,
    remaining_llm_calls integer NOT NULL CHECK (remaining_llm_calls BETWEEN 0 AND 63),
    granted_at timestamptz NOT NULL,
    PRIMARY KEY (job_id, call_id),
    FOREIGN KEY (job_id, attempt) REFERENCES research_job_attempts(job_id, attempt)
);
