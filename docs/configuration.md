# Configuration reference

Every setting transponder reads, with its default and its environment variable. The tables are checked against the code by `TestConfigReference` in `internal/config`, so a key that is added, renamed or given a new default fails the build until this page says so.

For a guided start, read the [README](../README.md) first. This page is the lookup.

## Contents

- [Where settings come from](#where-settings-come-from)
- [Command line](#command-line)
- [Server](#server)
- [Store and secrets](#store-and-secrets)
- [Sign-in with OIDC](#sign-in-with-oidc)
- [Providers](#providers)
- [Routes](#routes)
- [Calls to providers](#calls-to-providers)
- [Budgets](#budgets)
- [Limit profiles](#limit-profiles)
- [Limit counters](#limit-counters)
- [Tracing](#tracing)
- [Other environment variables](#other-environment-variables)
- [A full example](#a-full-example)

## Where settings come from

transponder starts with no config at all: SQLite in the working directory, a one-time admin key on stderr, and the built-in `echo` provider answering every model. Everything else comes from three places:

1. **The YAML file** named by `--config`. An unknown key is an error, so a typo does not go unnoticed.
2. **Environment variables** that start with `TRANSPONDER_`. They override the file. The name is the key's path in capitals with `__` (two underscores) between levels: `store.postgres` is `TRANSPONDER_STORE__POSTGRES`. Lists and maps (`providers`, `routes`, `otel.headers` and the like) can only be set in the file. A variable that names no key is ignored, since platforms such as Kubernetes set `TRANSPONDER_PORT` and similar for a service of the same name.
3. **The admin API.** Providers, routes, budgets and profiles can also be made under `/admin/v1/` and are stored in the database. Those from the file are stored at start with `"source": "file"` and are read-only through the API. See [Admin API](../README.md#admin-api).

`${NAME}` and `$NAME` in the file are replaced from the environment after the YAML is parsed, so a value is used whole: a `#`, a quote or a newline in a password stays part of it. A missing variable is an error that names it. Comments are never expanded. A variable fills in part of one value; it cannot add keys or list items. A `$` followed by a letter, a digit, `_`, `{` or one of `$*#@!?-` starts a variable, and there is no escape for it, so a value with such a `$` in it, such as a password, belongs in a variable of its own.

> [!IMPORTANT]
> Up to v0.2.4, a variable whose value held YAML, such as a list of providers in one variable, was spliced into the file before it was parsed. v0.3.0 expands variables inside values only. Move such lists into the file and keep only the secrets in variables.

Durations are written as Go durations: `30s`, `1m`, `10m`, `1h30m`.

Check a file without starting the server, opening a port or creating a database:

```shell
transponder --config-test --config transponder.yaml
```

## Command line

| Flag or command | What it does |
|---|---|
| `--config <file>` | The YAML file to read. Without it, only defaults and environment variables apply. |
| `--config-test` | Loads the config and builds every provider and route, prints `transponder: config ok` and exits 0, or prints the error and exits 1. |
| `--version` | Prints the version, the commit and the build time. |
| `--check-update` | Lists newer releases, how urgent each is, and which changes touch this config, then exits. It reads the public release list on GitHub and nothing else. |
| `transponder secrets rotate --config <file>` | Seals every stored provider key again under `secrets_key`, for after `secrets_key` changed. See [Provider keys at rest](../README.md#provider-keys-at-rest). |

## Server

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `listen_addr` | `:8080` | `TRANSPONDER_LISTEN_ADDR` | Address of the gateway and the admin API. |
| `ops_listen_addr` | none | `TRANSPONDER_OPS_LISTEN_ADDR` | A second address that also serves `/healthz`, `/readyz` and `/metrics`, where `/metrics` needs no key. Keep it inside your network. |
| `log_level` | `info` | `TRANSPONDER_LOG_LEVEL` | `debug`, `info`, `warn` or `error`. Logs are JSON on stdout. |
| `update_check` | `false` | `TRANSPONDER_UPDATE_CHECK` | At start and once a day, logs one `update available` line when a newer release is out. Off by default. |
| `log_requests` | `false` | `TRANSPONDER_LOG_REQUESTS` | Logs one `call` line per call, with its request id, endpoint, route, provider, model, status, duration, tokens, cost, user and key name. Never the prompt, the answer or a provider error. Off by default. |

## Store and secrets

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `store.sqlite` | `./transponder.db` | `TRANSPONDER_STORE__SQLITE` | Path of the SQLite file, for one replica. Used when `store.postgres` is not set. |
| `store.postgres` | none | `TRANSPONDER_STORE__POSTGRES` | A `postgres://` URL or `key=value` connection string, for several replicas sharing keys, config, usage and limits. Takes pool settings such as `pool_max_conns=20`. Set only one of `store.sqlite` and `store.postgres`. |
| `secrets_key` | generated into `transponder.key` next to a SQLite file; none with Postgres | `TRANSPONDER_SECRETS_KEY` | 32 bytes, as base64 or 64 hex digits, that seal provider keys stored through the admin API with AES-256-GCM. With Postgres, set the same value on every replica. |
| `previous_secrets_key` | none | `TRANSPONDER_PREVIOUS_SECRETS_KEY` | The old `secrets_key`, still read while `transponder secrets rotate` seals the stored keys again. Remove it afterwards. |

## Sign-in with OIDC

API keys always work. These keys let callers also sign in with a token from an OpenID Connect provider. Sign-in is off while `auth.oidc.issuer` is empty.

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `auth.oidc.issuer` | none | `TRANSPONDER_AUTH__OIDC__ISSUER` | The `iss` claim. The signing keys are found through its discovery document. |
| `auth.oidc.audience` | none, needed with an issuer | `TRANSPONDER_AUTH__OIDC__AUDIENCE` | A value the token's `aud` must hold, so tokens issued for another service are refused. |
| `auth.oidc.jwks_url` | from the discovery document | `TRANSPONDER_AUTH__OIDC__JWKS_URL` | Where the signing keys are, for an issuer without a discovery document. |
| `auth.oidc.user_claim` | `sub` | `TRANSPONDER_AUTH__OIDC__USER_CLAIM` | The claim with the user id, such as `email` or `preferred_username`. |
| `auth.oidc.teams_claim` | `groups` | `TRANSPONDER_AUTH__OIDC__TEAMS_CLAIM` | The claim with the user's teams. |
| `auth.oidc.roles_claim` | none | `TRANSPONDER_AUTH__OIDC__ROLES_CLAIM` | The claim `role_mapping` reads. A dotted name such as `realm_access.roles` reads a nested claim. |
| `auth.oidc.role_mapping` | none: everyone is `member` | file only | A map from a value of `roles_claim` to `admin`, `viewer` or `member`. A token gets the highest role any of its values maps to. |
| `auth.oidc.clock_skew` | `1m` | `TRANSPONDER_AUTH__OIDC__CLOCK_SKEW` | How far a token's `exp`, `nbf` and `iat` may be off. |

## Providers

Each item of `providers` is one backend. The [provider table](../README.md#providers) lists every type, its default base URL and what it supports.

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `providers[].name` | none, needed | file only | The name routes use to send calls to it. |
| `providers[].type` | none, needed | file only | The provider type, such as `openai`, `anthropic`, `gemini` or `ollama`. |
| `providers[].base_url` | the type's own | file only | Where the API is. Needed for `cloudflare`. |
| `providers[].api_key` | none | file only | The key sent to the provider. Use `${VAR}` to keep it out of the file. |

## Routes

Routes are tried in order and the first whose `match` fits the requested model wins. A route's targets are tried in order until one answers. A file whose route can never match, because an earlier route has the same `match` or is `*`, does not load, so give each set of callers its own model name and put `*` last.

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `routes[].match` | none, needed | file only | A model name, or a pattern with `*`, such as `claude-*` or `*`. |
| `routes[].allow` | everyone with a key or token | file only | Who may call the route: `user:<id>`, `team:<name>`, `key:<name>` or `role:<role>` (that role and the ones above it). |
| `routes[].targets[].provider` | none, needed | file only | A provider's `name`. |
| `routes[].targets[].model` | the requested model | file only | The model to ask the provider for. |

## Calls to providers

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `settings.attempt_timeout` | `1m` | `TRANSPONDER_SETTINGS__ATTEMPT_TIMEOUT` | How long one target may take to start answering a stream before the next is tried. |
| `settings.request_timeout` | `10m` | `TRANSPONDER_SETTINGS__REQUEST_TIMEOUT` | The longest a whole call may take, streams and fallbacks included. |
| `settings.cooldown_failures` | `3` | `TRANSPONDER_SETTINGS__COOLDOWN_FAILURES` | Failures in a row after which a target is tried last in its routes. |
| `settings.cooldown` | `30s` | `TRANSPONDER_SETTINGS__COOLDOWN` | How long such a target stays last. |

## Budgets

A budget caps what a subject spends in USD per UTC day or month. A spent budget answers 402 `budget_exceeded`.

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `budgets[].subject` | none, needed | file only | `user:*` (every user without a rule of their own), `user:<id>`, `team:<name>` or `key:<name>`. |
| `budgets[].period` | none, needed | file only | `daily` or `monthly`. |
| `budgets[].limit_usd` | none, needed | file only | The cap in USD. `0` refuses every paid call. |

## Limit profiles

Profiles cap tokens and requests, in the format the four mothership projects share. A caller gets the profile bound to their key, else to their user, else to the first of their teams that has one, else `default_profile`.

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `profiles[].name` | none, needed | file only | The name bindings use. |
| `profiles[].priority` | `0` | file only | Part of the shared format. transponder keeps it and does not use it. |
| `profiles[].limits` | none | file only | The limits, from the table below. Keys that start with `tools.`, `agent.` or `memory.` belong to the other projects and are ignored. |
| `default_profile` | none | `TRANSPONDER_DEFAULT_PROFILE` | The profile of callers without a binding. |
| `profile_bindings[].subject` | none, needed | file only | `key:<name>`, `user:<id>` or `team:<name>`. |
| `profile_bindings[].profile` | none, needed | file only | A profile's `name`. |
| `profiles_file` | none | `TRANSPONDER_PROFILES_FILE` | A YAML file with `profiles`, `default_profile` and `profile_bindings`, to share one set with the other projects. Its profiles and bindings are added to the file's; `default_profile` in the main file wins. |

The limits a profile can hold:

| Limit | Example | What it caps |
|---|---|---|
| `requests.rate` | `60/m` | Calls per second, minute, hour or day (`s`, `m`, `h`, `d`). Over it: 429 `rate_limited` with `Retry-After`. |
| `requests.concurrency` | `4` | Calls at once. Over it: 429 `concurrency_limited`. |
| `llm.tokens.rate` | `20000/m` | Input, output and reasoning tokens per period. |
| `llm.tokens.per_day` | `100000` | Tokens per UTC day. Spent: 402 `quota_exceeded`. |
| `llm.tokens.per_month` | `2000000` | Tokens per UTC month. |
| `llm.cost_usd.per_day` | `1` | USD per UTC day. |
| `llm.cost_usd.per_month` | `5` | USD per UTC month. |
| `llm.max_input_tokens` | `8192` | The estimated prompt. Over it: 400 `input_too_large`. |
| `llm.max_output_tokens` | `2048` | Lowers `max_tokens`, `max_completion_tokens` or `max_output_tokens`, never raises them. |
| `llm.max_reasoning_tokens` | `4096` | Lowers Anthropic's thinking budget. |

In YAML, a limit with a dot in its first part is written with the group as one key, as in `llm.tokens: {rate: 20000/m, per_day: 100000}`.

## Limit counters

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `limits.store` | `sql` with `store.postgres`, `memory` otherwise | `TRANSPONDER_LIMITS__STORE` | Where budgets, allowances, rates and concurrency are counted: `sql` in Postgres, shared by every replica, or `memory` in each process. |
| `limits.on_store_error` | none, which means `local` | `TRANSPONDER_LIMITS__ON_STORE_ERROR` | When Postgres cannot be reached: `local` counts in each replica with its share of each limit, `deny` answers 503 `overloaded`. |

## Tracing

Off while `otel.endpoint` is empty.

| Key | Default | Environment variable | What it sets |
|---|---|---|---|
| `otel.endpoint` | none | `TRANSPONDER_OTEL__ENDPOINT` | Base URL of an OTLP/HTTP collector, such as `http://collector:4318`. Spans are posted to `/v1/traces`. gRPC is not supported. |
| `otel.headers` | none | file only | Headers sent with every export, for a collector that needs a key. |
| `otel.sample_ratio` | all traces | `TRANSPONDER_OTEL__SAMPLE_RATIO` | The share, from 0 to 1, of traces that start at the gateway to record. A call with a `traceparent` follows its sampled flag. |

## Other environment variables

| Variable | What it does |
|---|---|
| `TRANSPONDER_PPROF_ADDR` | In a binary built with `-tags pprof` only, serves `net/http/pprof` on this address. Release builds have no pprof. |
| `TRANSPONDER_TEST_POSTGRES` | For the tests only: the URL of a Postgres server the store tests may use. Without it they are skipped. |

## A full example

Every key that has a value to set, in one file. It loads with `transponder --config-test` once the variables it names are set.

```yaml
listen_addr: ":8080"
ops_listen_addr: ":9464"
log_level: info
update_check: true

store:
  postgres: postgres://transponder:${PGPASSWORD}@db.internal:5432/transponder?sslmode=require
secrets_key: ${SECRETS_KEY}

auth:
  oidc:
    issuer: "https://idp.example.com/realms/acme"
    audience: "transponder"
    user_claim: email
    teams_claim: groups
    roles_claim: realm_access.roles
    role_mapping:
      platform-admins: admin
      auditors: viewer
    clock_skew: 1m

providers:
  - name: claude
    type: anthropic
    api_key: ${ANTHROPIC_API_KEY}
  - name: openai
    type: openai
    api_key: ${OPENAI_API_KEY}
  - name: local
    type: ollama
    base_url: http://ollama.internal:11434/v1

routes:
  - match: "claude-*"
    allow: ["team:platform", "role:admin"]
    targets:
      - provider: claude
      - provider: openai
        model: gpt-5
  - match: "*"
    targets:
      - provider: local

settings:
  attempt_timeout: 1m
  request_timeout: 10m
  cooldown_failures: 3
  cooldown: 30s

budgets:
  - {subject: "user:*", period: monthly, limit_usd: 20}
  - {subject: "team:platform", period: daily, limit_usd: 50}

default_profile: free
profiles:
  - name: free
    limits:
      requests: {rate: 60/m, concurrency: 4}
      llm.tokens: {rate: 20000/m, per_day: 100000, per_month: 2000000}
      llm.cost_usd: {per_day: 1, per_month: 5}
      llm.max_input_tokens: 8192
      llm.max_output_tokens: 2048
      llm.max_reasoning_tokens: 4096
  - name: pro
    limits:
      llm.tokens: {per_day: 2000000}
profile_bindings:
  - {subject: "user:alice@example.com", profile: pro}

limits:
  store: sql
  on_store_error: local

otel:
  endpoint: http://collector:4318
  headers:
    Authorization: Bearer ${OTEL_KEY}
  sample_ratio: 0.1
```
