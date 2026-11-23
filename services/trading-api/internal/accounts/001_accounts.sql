CREATE TABLE paper_accounts (
    account_id uuid PRIMARY KEY,
    creation_hash bytea NOT NULL CHECK (octet_length(creation_hash) = 32),
    cash_usd numeric(38,8) NOT NULL CHECK (cash_usd >= 0 AND cash_usd < 'Infinity'::numeric),
    reserved_cash_usd numeric(38,8) NOT NULL DEFAULT 0 CHECK (reserved_cash_usd BETWEEN 0 AND cash_usd),
    version bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE paper_positions (
    account_id uuid NOT NULL REFERENCES paper_accounts(account_id),
    instrument_id text NOT NULL,
    registry_version text NOT NULL,
    decimals integer NOT NULL CHECK (decimals BETWEEN 0 AND 18),
    quantity_raw numeric(38,0) NOT NULL CHECK (quantity_raw >= 0 AND quantity_raw < 'Infinity'::numeric),
    reserved_quantity_raw numeric(38,0) NOT NULL DEFAULT 0 CHECK (reserved_quantity_raw BETWEEN 0 AND quantity_raw),
    PRIMARY KEY (account_id, instrument_id)
);

CREATE TABLE paper_events (
    account_id uuid NOT NULL REFERENCES paper_accounts(account_id),
    event_id uuid NOT NULL,
    account_version bigint NOT NULL CHECK (account_version >= 1),
    kind text NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    payload jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, event_id),
    UNIQUE (account_id, account_version)
);
