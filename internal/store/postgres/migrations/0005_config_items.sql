-- Providers, routes, budgets and profiles, from the YAML file (source 'file') or the admin API.
-- body is the item as JSON; secret is a sealed secret such as a provider's API key.
CREATE TABLE config_items (
    org_id     TEXT   NOT NULL DEFAULT 'default',
    kind       TEXT   NOT NULL,
    id         TEXT   NOT NULL,
    position   BIGINT NOT NULL DEFAULT 0,
    body       TEXT   NOT NULL DEFAULT '{}',
    secret     TEXT   NOT NULL DEFAULT '',
    version    BIGINT NOT NULL DEFAULT 1,
    source     TEXT   NOT NULL DEFAULT 'api',
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (org_id, kind, id)
);
