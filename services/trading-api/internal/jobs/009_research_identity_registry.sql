ALTER TABLE research_jobs ADD COLUMN identity_registry_sha256 text NOT NULL DEFAULT ''
    CHECK (identity_registry_sha256 = '' OR identity_registry_sha256 ~ '^[0-9a-f]{64}$');

ALTER TABLE research_job_attempts ADD COLUMN identity_registry_sha256 text NOT NULL DEFAULT ''
    CHECK (identity_registry_sha256 = '' OR identity_registry_sha256 ~ '^[0-9a-f]{64}$');
