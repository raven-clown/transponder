-- One row per write through the admin API. Rows are never changed or deleted.
CREATE TABLE audit (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id     TEXT    NOT NULL DEFAULT 'default',
    time       INTEGER NOT NULL,
    actor      TEXT    NOT NULL,
    action     TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    resource   TEXT    NOT NULL,
    diff       TEXT    NOT NULL DEFAULT '{}',
    request_id TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX audit_org_id ON audit (org_id, id);
