CREATE TABLE IF NOT EXISTS jupiter_execution_attempts (
    owner text NOT NULL,
    intent_id uuid NOT NULL,
    reservation_id uuid NOT NULL UNIQUE,
    provider_request_id text NOT NULL UNIQUE,
    signature text NOT NULL UNIQUE,
    approval_hash text NOT NULL,
    last_valid_block_height text NOT NULL,
    provider_status text NOT NULL CHECK (provider_status IN ('unknown', 'accepted', 'rejected')),
    provider_code integer,
    provider_slot text,
    claimed_at timestamptz NOT NULL,
    provider_observed_at timestamptz,
    PRIMARY KEY (owner, intent_id)
);
