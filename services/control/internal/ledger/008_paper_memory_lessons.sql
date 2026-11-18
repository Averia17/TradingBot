CREATE TABLE paper_memory_lessons (
    account_id uuid NOT NULL,
    lesson_id text NOT NULL CHECK (lesson_id ~ '^[0-9a-f]{64}$'),
    source_fill_event_id uuid NOT NULL,
    fact bytea NOT NULL CHECK (octet_length(fact) BETWEEN 1 AND 8192),
    PRIMARY KEY (account_id, lesson_id),
    UNIQUE (account_id, source_fill_event_id),
    FOREIGN KEY (account_id, source_fill_event_id) REFERENCES paper_fills(account_id, event_id)
);

CREATE TABLE paper_memory_lesson_events (
    account_id uuid NOT NULL,
    lesson_id text NOT NULL,
    event_id uuid NOT NULL,
    revision integer NOT NULL CHECK (revision >= 1),
    action text NOT NULL CHECK (
        (revision = 1 AND action = 'record') OR
        (revision > 1 AND action IN ('retract', 'reinstate'))
    ),
    created_at timestamptz NOT NULL,
    available_at timestamptz NOT NULL CHECK (available_at >= created_at),
    PRIMARY KEY (account_id, event_id),
    UNIQUE (account_id, lesson_id, revision),
    FOREIGN KEY (account_id, lesson_id) REFERENCES paper_memory_lessons(account_id, lesson_id)
);

CREATE INDEX paper_memory_lesson_events_cutoff
    ON paper_memory_lesson_events (account_id, available_at);

CREATE FUNCTION reject_paper_memory_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'paper memory records are append-only';
END;
$$;

CREATE TRIGGER paper_memory_lessons_append_only
    BEFORE UPDATE OR DELETE ON paper_memory_lessons
    FOR EACH ROW EXECUTE FUNCTION reject_paper_memory_mutation();

CREATE TRIGGER paper_memory_lesson_events_append_only
    BEFORE UPDATE OR DELETE ON paper_memory_lesson_events
    FOR EACH ROW EXECUTE FUNCTION reject_paper_memory_mutation();
