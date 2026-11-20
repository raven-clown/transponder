CREATE TABLE api_keys (
    org_id     TEXT    NOT NULL DEFAULT 'default',
    name       TEXT    NOT NULL,
    hash       BYTEA   NOT NULL UNIQUE,
    last4      TEXT    NOT NULL,
    role       TEXT    NOT NULL,
    teams      JSONB   NOT NULL DEFAULT '[]',
    disabled   BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at BIGINT,
    version    BIGINT  NOT NULL DEFAULT 1,
    source     TEXT    NOT NULL DEFAULT 'api',
    created_at BIGINT  NOT NULL,
    updated_at BIGINT  NOT NULL,
    PRIMARY KEY (org_id, name)
);

CREATE TABLE usage (
    id                 BIGINT  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id             TEXT    NOT NULL DEFAULT 'default',
    time               BIGINT  NOT NULL,
    request_id         TEXT    NOT NULL DEFAULT '',
    user_id            TEXT    NOT NULL DEFAULT '',
    teams              JSONB   NOT NULL DEFAULT '[]',
    key                TEXT    NOT NULL DEFAULT '',
    project            TEXT    NOT NULL DEFAULT '',
    route              TEXT    NOT NULL DEFAULT '',
    provider           TEXT    NOT NULL DEFAULT '',
    model              TEXT    NOT NULL DEFAULT '',
    endpoint           TEXT    NOT NULL DEFAULT '',
    input_tokens       BIGINT  NOT NULL DEFAULT 0,
    output_tokens      BIGINT  NOT NULL DEFAULT 0,
    cache_read_tokens  BIGINT  NOT NULL DEFAULT 0,
    cache_write_tokens BIGINT  NOT NULL DEFAULT 0,
    reasoning_tokens   BIGINT  NOT NULL DEFAULT 0,
    cost_nusd          BIGINT  NOT NULL DEFAULT 0,
    estimated          BOOLEAN NOT NULL DEFAULT FALSE,
    status             INTEGER NOT NULL DEFAULT 0,
    latency_ms         BIGINT  NOT NULL DEFAULT 0,
    fallbacks          INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX usage_org_time ON usage (org_id, time);
