ALTER TABLE research_jobs ADD COLUMN analysis_mode text NOT NULL DEFAULT 'offline'
    CHECK (analysis_mode IN ('offline', 'prepared_provider'));
ALTER TABLE research_jobs ADD CONSTRAINT research_provider_requires_registry
    CHECK (analysis_mode <> 'prepared_provider' OR identity_registry_sha256 <> '');
CREATE INDEX research_claim_scope_idx
    ON research_jobs (identity_registry_sha256, analysis_mode, created_at, id)
    WHERE status IN ('queued', 'leased');
