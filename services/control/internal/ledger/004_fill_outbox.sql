CREATE TABLE paper_fill_outbox (
    account_id uuid NOT NULL,
    event_id uuid NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    created_at timestamptz NOT NULL,
    queued_at timestamptz,
    PRIMARY KEY (account_id, event_id),
    FOREIGN KEY (account_id, event_id) REFERENCES paper_fills(account_id, event_id)
);

CREATE INDEX paper_fill_outbox_pending ON paper_fill_outbox (created_at, account_id, event_id)
    WHERE queued_at IS NULL;
