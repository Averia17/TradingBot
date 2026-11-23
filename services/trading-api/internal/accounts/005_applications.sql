CREATE TABLE paper_applications (
    job_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES paper_accounts(account_id),
    snapshot_version bigint NOT NULL CHECK (snapshot_version > 0),
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    order_id uuid,
    quote bytea CHECK (octet_length(quote) BETWEEN 1 AND 262144),
    quote_sha256 bytea CHECK (octet_length(quote_sha256) = 32),
    result bytea CHECK (octet_length(result) BETWEEN 1 AND 262144),
    result_sha256 bytea CHECK (octet_length(result_sha256) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (account_id, order_id) REFERENCES paper_orders(account_id, order_id),
    CHECK ((order_id IS NULL) = (quote IS NULL)),
    CHECK ((quote IS NULL) = (quote_sha256 IS NULL)),
    CHECK ((result IS NULL) = (result_sha256 IS NULL)),
    CHECK (order_id IS NOT NULL OR result IS NOT NULL)
);
