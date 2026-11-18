CREATE TABLE research_paper_dispatch (
    job_id uuid PRIMARY KEY REFERENCES research_jobs(id),
    account_id uuid NOT NULL,
    request bytea NOT NULL CHECK (octet_length(request) BETWEEN 1 AND 262144),
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    finished_status text CHECK (finished_status IN ('hold', 'applied', 'expired', 'snapshot_stale', 'research_failed', 'research_expired')),
    finished_at timestamptz,
    CHECK ((finished_status IS NULL) = (finished_at IS NULL))
);

CREATE INDEX research_paper_dispatch_pending ON research_paper_dispatch (account_id, job_id)
    WHERE finished_status IS NULL;
