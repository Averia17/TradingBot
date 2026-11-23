-- The singleton serializes owner activation with new permits across all jobs.
CREATE TABLE research_daily_call_policy (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    active_from date
);
INSERT INTO research_daily_call_policy (singleton) VALUES (true);

CREATE TABLE research_daily_call_limits (
    utc_day date PRIMARY KEY,
    max_calls bigint NOT NULL CHECK (max_calls >= 0)
);

-- Count existing receipts as well as permits issued after activation.
CREATE INDEX research_call_permits_granted_at_idx ON research_call_permits (granted_at);
