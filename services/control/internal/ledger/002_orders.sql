CREATE TABLE paper_orders (
    account_id uuid NOT NULL REFERENCES paper_accounts(account_id),
    order_id uuid NOT NULL,
    decision_id uuid NOT NULL,
    quote_id uuid NOT NULL,
    request jsonb NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    status text NOT NULL CHECK (status IN ('reserved', 'unknown', 'cancelled', 'filled')),
    account_version bigint NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, order_id),
    UNIQUE (account_id, decision_id),
    UNIQUE (account_id, quote_id)
);

CREATE TABLE paper_fills (
    account_id uuid NOT NULL,
    order_id uuid NOT NULL,
    event_id uuid NOT NULL,
    gross_usd numeric(38,8) NOT NULL CHECK (gross_usd > 0 AND gross_usd < 'Infinity'::numeric),
    fee_usd numeric(38,8) NOT NULL CHECK (fee_usd >= 0 AND fee_usd < 'Infinity'::numeric),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, order_id),
    FOREIGN KEY (account_id, order_id) REFERENCES paper_orders(account_id, order_id),
    FOREIGN KEY (account_id, event_id) REFERENCES paper_events(account_id, event_id)
);
