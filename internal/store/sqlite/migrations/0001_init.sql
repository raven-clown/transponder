CREATE TABLE api_keys (
    org_id     TEXT    NOT NULL DEFAULT 'default',
    name       TEXT    NOT NULL,
    hash       BLOB    NOT NULL UNIQUE,
    last4      TEXT    NOT NULL,
    role       TEXT    NOT NULL,
    teams      TEXT    NOT NULL DEFAULT '[]',
    disabled   INTEGER NOT NULL DEFAULT 0,
    expires_at INTEGER,
    version    INTEGER NOT NULL DEFAULT 1,
    source     TEXT    NOT NULL DEFAULT 'api',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (org_id, name)
);

CREATE TABLE usage (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id             TEXT    NOT NULL DEFAULT 'default',
    time               INTEGER NOT NULL,
    request_id         TEXT    NOT NULL DEFAULT '',
    user               TEXT    NOT NULL DEFAULT '',
    teams              TEXT    NOT NULL DEFAULT '[]',
    key                TEXT    NOT NULL DEFAULT '',
    project            TEXT    NOT NULL DEFAULT '',
    route              TEXT    NOT NULL DEFAULT '',
    provider           TEXT    NOT NULL DEFAULT '',
    model              TEXT    NOT NULL DEFAULT '',
    endpoint           TEXT    NOT NULL DEFAULT '',
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_nusd          INTEGER NOT NULL DEFAULT 0,
    estimated          INTEGER NOT NULL DEFAULT 0,
    status             INTEGER NOT NULL DEFAULT 0,
    latency_ms         INTEGER NOT NULL DEFAULT 0,
    fallbacks          INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX usage_org_time ON usage (org_id, time);
