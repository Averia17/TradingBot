CREATE TABLE research_call_usage (
    job_id uuid NOT NULL,
    call_id uuid NOT NULL,
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 4096),
    payload_sha256 bytea NOT NULL CHECK (octet_length(payload_sha256) = 32),
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (job_id, call_id),
    FOREIGN KEY (job_id, call_id) REFERENCES research_call_permits(job_id, call_id)
);
