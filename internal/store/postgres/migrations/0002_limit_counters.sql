-- Budgets and token allowances: one row per subject, window and window start.
CREATE TABLE limit_counters (
    org_id TEXT   NOT NULL DEFAULT 'default',
    key    TEXT   NOT NULL,
    win    TEXT   NOT NULL,
    start  BIGINT NOT NULL,
    n      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, key, win, start)
);

-- Rates: the theoretical arrival time of GCRA, in Unix nanoseconds.
CREATE TABLE limit_rates (
    org_id TEXT   NOT NULL DEFAULT 'default',
    key    TEXT   NOT NULL,
    tat    BIGINT NOT NULL,
    PRIMARY KEY (org_id, key)
);

-- Concurrency: one row per running call, kept alive by the replica that holds it.
CREATE TABLE limit_slots (
    id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id     TEXT        NOT NULL DEFAULT 'default',
    key        TEXT        NOT NULL,
    replica    TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX limit_slots_key ON limit_slots (org_id, key);

-- Replicas seen lately, so a replica cut off from the database knows its share of a limit.
CREATE TABLE limit_replicas (
    id      TEXT        PRIMARY KEY,
    seen_at TIMESTAMPTZ NOT NULL
);

-- Set once the counters were first filled from the usage table.
CREATE TABLE limit_seeded (
    org_id    TEXT   PRIMARY KEY,
    seeded_at BIGINT NOT NULL
);
