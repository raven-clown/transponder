-- Every write to the runtime config raises version in the same transaction, and each replica
-- reloads when it sees a new one.
CREATE TABLE config_version (
    org_id     TEXT    NOT NULL PRIMARY KEY DEFAULT 'default',
    version    INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

INSERT INTO config_version (org_id, version, updated_at) VALUES ('default', 1, 0);
