CREATE TABLE IF NOT EXISTS analytics_accounts (
    account_id uuid PRIMARY KEY,
    applied_version bigint NOT NULL DEFAULT 0,
    seen_version bigint NOT NULL DEFAULT 0,
    state jsonb,
    head_version bigint NOT NULL DEFAULT 0,
    head_observed_at timestamptz,
    CHECK (0 <= applied_version AND applied_version <= seen_version),
    CHECK ((applied_version = 0) = (state IS NULL)),
    CHECK ((head_version = 0) = (head_observed_at IS NULL))
);

CREATE TABLE IF NOT EXISTS analytics_account_events (
    account_id uuid NOT NULL REFERENCES analytics_accounts(account_id),
    account_version bigint NOT NULL CHECK (account_version > 0),
    event_id uuid NOT NULL,
    digest bytea NOT NULL CHECK (octet_length(digest) = 32),
    payload jsonb NOT NULL,
    PRIMARY KEY (account_id, account_version),
    UNIQUE (account_id, event_id)
);

CREATE TABLE IF NOT EXISTS analytics_valuations (
    account_id uuid NOT NULL,
    id text NOT NULL,
    digest bytea NOT NULL CHECK (octet_length(digest) = 32),
    payload jsonb NOT NULL,
    PRIMARY KEY (account_id, id)
);
