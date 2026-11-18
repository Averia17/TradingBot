LOCK TABLE paper_accounts IN ACCESS EXCLUSIVE MODE;

CREATE TABLE paper_account_outbox (
    account_id uuid NOT NULL,
    account_version bigint NOT NULL CHECK (account_version > 0),
    event_id uuid NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    created_at timestamptz NOT NULL,
    queued_at timestamptz,
    PRIMARY KEY (account_id, account_version),
    UNIQUE (account_id, event_id),
    FOREIGN KEY (account_id, event_id) REFERENCES paper_events(account_id, event_id),
    FOREIGN KEY (account_id, account_version) REFERENCES paper_events(account_id, account_version)
);

CREATE INDEX paper_account_outbox_pending ON paper_account_outbox (created_at, account_id, account_version)
    WHERE queued_at IS NULL;

CREATE VIEW paper_account_analytics_heads AS
SELECT account_id, version AS account_version FROM paper_accounts;
