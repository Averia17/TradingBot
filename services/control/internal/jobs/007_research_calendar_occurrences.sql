CREATE TABLE research_schedule_occurrences (
    job_id uuid PRIMARY KEY,
    schedule_id text NOT NULL CHECK (schedule_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    scheduled_at text NOT NULL,
    max_lateness_seconds integer NOT NULL CHECK (max_lateness_seconds BETWEEN 1 AND 3600),
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    recorded_at timestamptz NOT NULL
);
