CREATE TABLE research_snapshots (
    id uuid PRIMARY KEY,
    payload bytea NOT NULL,
    payload_sha256 bytea NOT NULL CHECK (octet_length(payload_sha256) = 32)
);

CREATE TABLE research_jobs (
    id uuid PRIMARY KEY,
    snapshot_id uuid NOT NULL REFERENCES research_snapshots(id),
    request bytea NOT NULL,
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    status text NOT NULL DEFAULT 'queued' CHECK (status = 'queued'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE research_outbox (
    job_id uuid NOT NULL REFERENCES research_jobs(id),
    event_type text NOT NULL CHECK (event_type = 'research_job.queued'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (job_id, event_type)
);
