CREATE TABLE paper_risk_refusals (
    account_id uuid NOT NULL REFERENCES paper_accounts(account_id),
    job_id uuid NOT NULL,
    decision_id uuid NOT NULL,
    candidate_order_id uuid NOT NULL,
    snapshot_version bigint NOT NULL CHECK (snapshot_version > 0),
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    quote_sha256 bytea NOT NULL CHECK (octet_length(quote_sha256) = 32),
    reason text NOT NULL CHECK (reason IN (
        'instrument_not_allowed', 'order_notional', 'maximum_fee', 'held_quantity',
        'unresolved_order', 'daily_turnover', 'minimum_free_cash',
        'instrument_concentration', 'group_concentration', 'gross_exposure'
    )),
    first_observed_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, job_id, candidate_order_id, request_sha256, reason)
);

CREATE INDEX paper_risk_refusals_cutoff_idx
    ON paper_risk_refusals (account_id, job_id, first_observed_at DESC);
