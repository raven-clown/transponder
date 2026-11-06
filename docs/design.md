# transponder design

Status: approved, revision 4
Date: 2026-11-04
Contract: `mothership/CONTRACT.md` as of commit 1ed968a

## 1. What it is

transponder is an LLM gateway in one Go binary. Clients talk to it with the OpenAI API (`/v1/chat/completions`, `/v1/embeddings`) or the Anthropic API (`/v1/messages`). It sends each call to Anthropic, OpenAI, Gemini or Ollama, falls back to the next provider when one fails, records what every call cost and who made it, and stops a user or team once their budget is spent. Everything it is configured with can also be read and changed through a JSON admin API under `/admin/v1/`, which the planned mothership console will use.

It runs alone. In the mothership stack, rosetta agents and logbook call it for chat, embeddings and fact extraction, and the caller's token that airlock and rosetta forward tells transponder which user to charge.

## 2. Why build it next to bifrost and litellm

Research done on 2026-11-04.

- maximhq/bifrost: Go, Apache-2.0, about 9.4k stars, very active. 23+ providers, a web UI, semantic caching, virtual keys with budgets, fallbacks, an MCP gateway and an agent gateway. It is fast and broad, and transponder will not beat it on either.
- BerriAI/litellm: Python, about 63k stars, 100+ providers, the default choice for most teams.
- agentgateway (Linux Foundation, Rust): LLM, MCP and A2A gateway aimed at Kubernetes. Budgets are written as rate limit policies counted in tokens or micro-dollars.

What each one charges for. Only items read in their own docs on 2026-11-04 are listed; anything not verified is left out.

bifrost Enterprise, from `docs/enterprise/overview.mdx` in maximhq/bifrost, which calls Enterprise "a strict superset of the open-source gateway":

- Audit logs of configuration changes, and log exports
- OIDC login with user and group sync (Okta, Entra, Keycloak, Zitadel, Google Workspace)
- Role-based access control and data access control
- Access profiles (reusable provider, model, budget and rate limit policies per user, role or team), projects, business units
- Guardrails (PII, secrets detection, redaction)
- Clustering, adaptive routing and the circuit breaker
- Running several nodes on one database. Its budget docs say: "Running multiple OSS Bifrost nodes with a Postgres backend is not supported", and that this "is part of how we draw the OSS vs Enterprise line". OSS keeps budgets and usage in memory on one node.

litellm Enterprise, from docs.litellm.ai/docs/enterprise and docs.litellm.ai/docs/proxy/token_auth:

- JWT-based authentication ("JWT-based Auth requires a LiteLLM Enterprise license"), and with it per-user budgets mapped from a JWT claim
- SSO beyond 5 users ("SSO is free for up to 5 users")
- Audit logs with retention policies
- Role-based access control, org and team admins
- Multi-tenant architecture (organizations above teams)
- Secret managers
- Guardrails per key or team, and several built-in moderation callbacks

Side by side:

| | bifrost OSS | litellm OSS | agentgateway | transponder |
|---|---|---|---|---|
| OIDC / JWT callers | Enterprise | Enterprise | Yes | Yes |
| Budget per user taken from the token | No, budgets hang off virtual keys | Enterprise (JWT mapping) | Yes, as a rate limit expression | Yes, in USD, from a claim |
| Several replicas sharing one budget | Enterprise | Yes, with Postgres and Redis | Needs a global rate limit service | Yes, with Postgres |
| Roles for admins | Enterprise | Enterprise | Not checked | Yes |
| Audit log of admin changes | Enterprise | Enterprise | Not checked | Yes |
| Circuit breaker | Enterprise | Not checked | Not checked | Yes, simple cooldown |
| Runs as | npx, Docker, Go SDK | Python service with Postgres | Kubernetes first | One static binary |
| Size of one provider | bifrost's Anthropic package is over 1 MB of non-test Go source | Not checked | Not checked | Target under 800 lines |

So transponder is for a team that already has an identity provider and wants to know, for free, what each person spends on models and cap it, on one or several replicas, with one binary, and manage it through a console without a sales call. Every row above that says Enterprise is free in transponder, and the contract rules out a paid edition for good. That is the clearest reason it exists. It is also a narrow one. Provider breadth and raw throughput stay bifrost's. The README should say so plainly, point people to bifrost when breadth is what they need, and lead with the fact that SSO, roles, the audit log, replicas and the admin API are all in the Apache-2.0 build.

The second reason is the stack. A user who signs in once is the same user in rosetta, airlock, logbook and transponder, so an agent run, its tool calls, its memories and its model spend all line up under one identity and one `traceparent`, and one console manages all four.

## 3. Goals and non-goals for v1

Goals:

- OpenAI-compatible `/v1/chat/completions` (streaming and not) and `/v1/embeddings`.
- Anthropic-compatible `/v1/messages` (streaming and not), good enough that Claude Code and the Anthropic SDKs work with only a base URL change.
- Four backends: Anthropic, OpenAI, Gemini, Ollama. Any OpenAI-compatible server (vLLM, LM Studio, Groq and so on) works through the OpenAI adapter with a `base_url`.
- Ordered fallback across providers, with a cooldown for targets that keep failing.
- Usage and cost per call, in the contract's response headers, in Prometheus metrics, and in SQL.
- Budgets in USD per user, team or key, per day or month, enforced across replicas when the store is Postgres.
- Callers identified by OIDC JWT or static API key, with three roles and per-route access.
- An admin API under `/admin/v1/` that covers every common task, so a new user can go from a fresh binary to a working gateway without writing YAML.
- An audit log of every admin change.
- A demo anyone can run in five minutes.

Not in v1: a UI of any kind (the contract puts that in the separate console), semantic or exact caching, request-per-minute rate limits, guardrails, the OpenAI Responses API, batch, images, audio, weighted load balancing, Bedrock, Vertex and Azure. Section 14 lists the ones most likely to come next.

## 4. Architecture

```
client (OpenAI or Anthropic dialect)
   |
   v
[http]  auth -> request id / trace -> decode into dialect
   |
   v
[access] may this caller use this route?
   |
   v
[budget] check every budget that applies to the caller
   |
   v
[router] pick route by model name -> ordered targets
   |      for each target that is not cooling down:
   |        same dialect?  -> native call, body forwarded with model rewritten
   |        other dialect? -> translate through the canonical schema
   |      retryable failure -> next target
   v
[provider adapter] anthropic | openai | gemini | ollama
   |
   v
[usage] parse token counts -> price -> store record + add to spend -> headers, metrics, log


admin client / console
   |
   v
[/admin/v1/] auth + role -> validate -> write SQL + audit row + bump config version
                                             |
            every replica polls the version and reloads its runtime config
```

Two kinds of configuration:

- Bootstrap settings, which the process needs before it can reach its database: listen address, store, the secrets key, OTLP endpoint, log level. These come only from the YAML file and environment variables.
- Runtime configuration: providers, routes, budgets, API keys, price overrides, OIDC and role settings, timeouts. This lives in SQL and is changed through the admin API. The YAML file may also declare any of it (section 9).

Packages:

```
cmd/transponder          main, flags, bootstrap config, server start
internal/config          bootstrap YAML, env overrides, ${VAR} expansion; runtime config types and validation
internal/auth            OIDC JWT verification, API keys, Identity and roles
internal/server          HTTP mux, middleware, gateway handlers
internal/admin           /admin/v1 handlers, cursor pagination, audit writes
internal/schema          canonical request, response, stream event and usage types
internal/dialect/openai  decode and encode the OpenAI wire format, usage parsing
internal/dialect/anthropic  same for Anthropic
internal/router          routes, access check, target order, fallback rules, cooldown
internal/provider        Provider interface, registry, shared HTTP helpers
internal/provider/anthropic
internal/provider/openai
internal/provider/gemini
internal/provider/ollama
internal/provider/echo   deterministic fake for tests and the mothership e2e test
internal/provider/providertest  conformance suite every adapter must pass
internal/pricing         embedded price table, overrides, cost math
internal/budget          which budgets apply, the check, the period math
internal/runtime         holds the live runtime config, reloads it when the version changes
internal/secrets         AES-GCM sealing of provider keys at rest
internal/store           Store interface, sqlite and postgres implementations, migrations
internal/telemetry       slog JSON logger, Prometheus metrics, OTLP tracing
internal/testutil        shared fakes and helpers, as the shared CONTRIBUTING asks
api/openapi.yaml         the admin API description
```

Everything sits under `internal/` in v1 so the interfaces can still change. Making `provider` public is an open choice (section 14).

## 5. Provider adapters

This is the main place contributors add code (the `adapter` label), so the interface stays small.

```go
type Provider interface {
	Name() string
	Dialect() string
	Chat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error)
	ChatStream(ctx context.Context, req *schema.ChatRequest) (schema.Stream, error)
	Embed(ctx context.Context, req *schema.EmbedRequest) (*schema.EmbedResponse, error)
}

type Native interface {
	Native(ctx context.Context, call *NativeCall) (*http.Response, error)
}
```

- `Dialect()` returns `openai`, `anthropic` or `none`. When it matches the caller's dialect and the adapter implements `Native`, the router forwards the original body with only `model` rewritten. This keeps every field the gateway does not know about, such as Anthropic thinking blocks, `cache_control` and new beta fields, so Claude Code keeps working as Anthropic adds features.
- In every other case the router goes through `Chat`, `ChatStream` or `Embed` with canonical types.
- `Embed` returns `provider.ErrUnsupported` when a backend has no embeddings (Anthropic). The router treats that target as skipped.
- Errors carry the upstream status code and a retryable flag through a shared `provider.Error` type.
- Each adapter registers a small settings schema (field names, which are secret, defaults). The admin API uses it to validate a new provider and to tell the console which fields to show.

The four adapters:

- anthropic: Messages API. Native for Anthropic callers, translated for OpenAI callers. No embeddings.
- openai: Chat Completions and Embeddings. Native for OpenAI callers. Takes `base_url`, so it also covers any OpenAI-compatible server.
- gemini: `generateContent`, `streamGenerateContent` and `embedContent` on the Generative Language API with an API key. Always translated.
- ollama: a preset over the openai adapter pointing at `http://localhost:11434/v1`, no key, zero price by default. Its own package so Ollama-specific behaviour (model pull hints, `keep_alive`) has a home.

Adding a provider means one package with an adapter, recorded fixtures in `testdata/`, a line in the registry, and prices in the price table. `providertest.Run(t, factory)` replays the fixtures through a local `httptest` server and checks text, tool calls, streaming order, usage parsing and error mapping. The CONTRIBUTING additions walk through it with the ollama package as the short example.

## 6. Canonical schema and translation

The canonical types follow the OpenAI Chat Completions shape, since most people already know it: messages with roles, content parts (text, image as URL or base64), tools, tool calls and tool results, `max_tokens`, `temperature`, `top_p`, `stop`, a response format, and `stream`. Usage is:

```go
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}
```

Stream events are a small set: message start, text delta, tool call start, tool call argument delta, finish with reason, usage, error. Each dialect package encodes these as its own SSE format, so an Anthropic caller whose request lands on OpenAI still gets `message_start`, `content_block_delta` and `message_stop` events.

Rules for translation:

- Fields with an equivalent are mapped (system prompt, tools, tool choice, stop sequences, images, finish reasons).
- Optional tuning fields with no equivalent, such as Anthropic `thinking` or OpenAI `logprobs`, are dropped and the drop is logged at debug level.
- Content the target cannot represent at all, such as a PDF document block going to Ollama, makes that target incompatible. The router skips it and tries the next one. If none are left the caller gets a 400 that names the problem.
- Tool call ids are kept across the translation so multi-turn tool use works after a fallback.

## 7. Routing and fallback

A route, shown in YAML form (the admin API takes the same fields as JSON):

```yaml
routes:
  - match: "claude-*"
    allow: []
    targets:
      - provider: anthropic
      - provider: openai
        model: gpt-5
      - provider: local
        model: qwen3:8b
  - match: "fast"
    allow: ["team:platform"]
    targets:
      - provider: gemini
        model: gemini-2.5-flash
      - provider: local
        model: llama3.2
  - match: "text-embedding-3-small"
    targets:
      - provider: openai
      - provider: local
        model: nomic-embed-text
```

- The request's `model` is matched against routes by `position`, lowest first. `match` is an exact name or a glob. The first route that matches wins.
- A target without `model` keeps the model the caller asked for.
- `allow` lists who may use the route: `user:<id>`, `team:<name>` or `key:<name>`. Empty means every authenticated caller. A caller outside the list gets 403 `route_forbidden`. This is the "give someone access" task in the console.
- With the `allow_direct` setting, `provider/model` (for example `local/llama3.2`) goes straight to that provider with no fallback. Only admins may use it when any route has a non-empty `allow`, so direct calls cannot get around access rules.
- No match gives 404 `model_not_found`.
- Route names are stable aliases, as the contract says. Other projects ask for a role, such as logbook's `memory-extract`, and the admin decides which provider serves it. Renaming a route through the admin API is allowed but logged in the audit as a breaking change.
- `X-Project`, when present, is stored on the usage record so spend can be grouped by project. It does not change routing in v1.

A target is retried on the next one when the call fails with a connection error, a timeout, 408, 429, any 5xx, Anthropic's 529, or 401 and 403 from the upstream (that means transponder's provider key is bad, which is not the caller's fault). It is not retried on other 4xx: a bad request stays bad on every provider.

Cooldown: after 3 failures in a row a target is skipped for 30 seconds. Both numbers are runtime settings. A target in cooldown is still tried when it is the only one left. Cooldown state is kept per replica in memory; it is a short-lived signal and sharing it is not worth a round trip. There is no retry of the same target in v1; fallback is the retry.

Streaming: transponder reads the upstream until its first event before writing anything to the client. A failure before that point falls back like any other call. After the first byte is sent the target is committed, and a later failure ends the stream with an error event in the caller's dialect.

Each streamed attempt has a timeout (`attempt_timeout`, default 60s until the first event) and every request has `request_timeout` (default 10m, since long generations are normal). A call that is not streamed has only the request timeout: its headers arrive once the whole answer is generated, so an attempt clock would cut long answers.

## 8. Identity, roles and secrets

Every request, gateway or admin, needs a caller, as the contract says:

- `Authorization: Bearer <token>`. If the token is a JWT it is verified against the configured OIDC issuer (JWKS fetched and cached through `coreos/go-oidc`), audience checked. Otherwise it is looked up as an API key.
- `x-api-key: <token>` is accepted the same way, because Anthropic clients send that header when given an API key.

The result is an `Identity{User, Teams, Key, Role}`. There are three roles:

| Role | Gateway endpoints | `/v1/usage` | Admin API |
|---|---|---|---|
| `member` | Yes, subject to route access and budgets | Own usage | No |
| `viewer` | Same as member | Own usage | Read everything, secrets never shown |
| `admin` | Yes, may use direct `provider/model` | Own usage | Read and write everything |

OIDC settings: issuer, audience, `user_claim` (default `sub`), `teams_claim` (optional, such as `groups`), and `roles_claim` with a mapping from claim values to `admin` or `viewer`. Anyone else with a valid token is a `member`.

API keys are created through the admin API (or declared in YAML). A key looks like `tp_` followed by 32 random bytes in base64url. It is shown once when created; the database keeps only its SHA-256 hash and the last four characters for display. Each key has a name, teams, a role, an optional expiry, and can be disabled. For a key, `User` is the key name.

First start: if the database has no admin key and no OIDC admin mapping, transponder creates an admin key and prints it once to stderr, the way Grafana and Jenkins do it, as the contract requires. With that key the console, or curl, can do the rest.

Provider secrets: provider API keys created through the admin API are sealed with AES-256-GCM before they are written. The 32-byte key comes from `secrets_key` in the bootstrap config or `TRANSPONDER_SECRETS_KEY`. When neither is set and the store is SQLite, transponder generates one into `transponder.key` next to the database with 0600 permissions, so a single binary still needs no setup. With Postgres the key must be set explicitly, and every replica gets the same one. Secrets are write-only through the API: reads return `"set": true` and the last four characters.

The caller's token is never sent to a provider. Providers get transponder's own provider keys, plus `traceparent`, `tracestate` and `X-Request-ID`.

## 9. Configuration: file, database and reload

The bootstrap file:

```yaml
listen_addr: ":8080"
store:
  sqlite: ./transponder.db
  # postgres: postgres://...
secrets_key: ${TRANSPONDER_SECRETS_KEY}
otel:
  endpoint: ""
log_level: info
```

Any key can be overridden by environment variable, with `__` between nested keys as the contract says: `TRANSPONDER_LISTEN_ADDR` for `listen_addr`, `TRANSPONDER_STORE__POSTGRES` for `store.postgres`.

The same file may also contain `providers`, `routes`, `budgets`, `keys`, `prices` and `settings`. At start, transponder upserts those into SQL and marks them `source: file`. Items marked that way show up in the admin API like any other but cannot be changed or deleted there (409 `managed_by_file`), so a GitOps setup and the console never fight over the same item. Items created through the API are `source: api`. Removing an item from the file and restarting deletes it. A user who never writes YAML never sees any of this.

Reload: every write through the admin API bumps a `config_version` row in the same transaction. Each replica checks that row every 2 seconds and, when it changed, loads the whole runtime config, validates it, and swaps it in atomically. A replica keeps serving with its old config if the new one fails to load, logs why, and reports it in `/admin/v1/status`. In-flight requests finish on the config they started with. On one replica the admin handler also triggers the reload directly, so a change is live before the response returns.

## 10. Usage, cost and budgets

Usage. Token counts come from the provider's response: the `usage` object for non-streaming calls, and the usage events while streaming. For OpenAI-dialect streams transponder adds `stream_options.include_usage` upstream, and drops the usage-only chunk again if the caller did not ask for it. When a provider returns no usage (some OpenAI-compatible servers), the record is marked `estimated` and counts are approximated from character length divided by 4. That rule undercounts Thai and other non-Latin scripts, which cost more tokens per character, so milestone 3 counts them at a higher rate and lets a route name a tokenizer for exact counts.

Reasoning tokens. `schema.Usage` gains `ReasoningTokens`. Each adapter reads them where its provider reports them apart: OpenAI's `completion_tokens_details.reasoning_tokens` and Gemini's `thoughtsTokenCount`. Anthropic, Ollama and vLLM count them inside the output tokens, so they stay there. The usage record, the `X-Usage-Reasoning-Tokens` header and the usage event carry the number.

Price. `internal/pricing/prices.yaml` is embedded in the binary, keyed by `type/model`, with USD per million input, output, cache read and cache write tokens. Overrides and new entries are stored in SQL and set through the admin API or the file. Ollama and `echo` cost zero. A model with no price is served and costs zero, the log line says `unpriced`, and `transponder_unpriced_requests_total` goes up. Money is kept as int64 nano-USD so sums never drift.

Headers on every response:

```
X-Request-ID: <kept or generated>
X-Usage-Input-Tokens: 1234
X-Usage-Output-Tokens: 56
X-Usage-Cost-USD: 0.004542
X-Transponder-Provider: openai
X-Transponder-Model: gpt-5
X-Transponder-Fallbacks: 1
```

For streams, headers go out before the counts are known. transponder declares `Trailer: X-Usage-Input-Tokens, X-Usage-Output-Tokens, X-Usage-Cost-USD` and sends the values as HTTP trailers, and the counts also appear in the stream's normal usage event. This is a clarification the contract needs (section 13).

Budgets, shown in YAML form:

```yaml
budgets:
  - subject: "user:*"
    period: monthly
    limit_usd: 20
  - subject: "user:alice@example.com"
    period: monthly
    limit_usd: 100
  - subject: "team:platform"
    period: daily
    limit_usd: 50
  - subject: "key:logbook"
    period: monthly
    limit_usd: 10
```

- Subjects are `user:<id>`, `team:<name>` or `key:<name>`. `user:*` is the default for any user without a rule of their own; the exact user rule replaces it.
- Periods are `daily` or `monthly` on UTC calendar boundaries.
- Before a call, every budget that applies (the caller's user rule, each of their teams, their key) must have room. If one is spent the caller gets HTTP 402 with error type `budget_exceeded`, the subject, the limit and the reset time, in the caller's dialect. 402 is used on purpose: the OpenAI and Anthropic SDKs retry 429 automatically, which would only burn time.
- After the call, the real cost is added to every subject's spend in the same transaction as the usage record. Requests already in flight can push a subject a little past its limit. The overshoot is at most the cost of those requests, and the docs say so.
- `GET /v1/usage` returns the caller's own spend and remaining budget for each rule that applies to them. Everything wider is in the admin API.

Token limits, milestone 3. Besides money, a caller can be limited in tokens, through the `llm.` keys of the shared profiles in mothership's `docs/scale.md`:

```yaml
llm.tokens:               {rate: 20000/m, per_day: 100000, per_month: 2000000}
llm.max_input_tokens:     8192
llm.max_output_tokens:    2048
llm.max_reasoning_tokens: 4096
```

- `llm.tokens` counts input, output and reasoning tokens together. `rate` is tokens per minute, so one caller cannot spend a day's allowance in a minute and slow everyone else on the same GPUs.
- Tokens are reserved before the call, from the prompt and `max_tokens`, and settled to the real counts after it. A failed call gives the reservation back.
- A prompt over `llm.max_input_tokens` gets 400 `input_too_large` before any provider is called.
- `llm.max_output_tokens` and `llm.max_reasoning_tokens` lower the request's `max_tokens` and reasoning budget to the cap. They never raise them.
- A spent `per_day` or `per_month` allowance is 402 `quota_exceeded`. A spent `rate` is 429 `rate_limited` with `Retry-After`.

## 11. Admin API

JSON under `/admin/v1/`, described by `api/openapi.yaml` (OpenAPI 3.1) and served at `/admin/v1/openapi.yaml`. A test checks that every route the server registers is in the file and the other way round, so the two cannot drift.

Conventions:

- Same identity rules as the gateway. `viewer` may call every `GET`; everything else needs `admin`.
- Resources are addressed by a stable name chosen at creation (`/providers/anthropic`, `/keys/logbook`); budgets and routes get a generated id.
- Every resource has a `version` integer. `PATCH` and `DELETE` take it in `If-Match`; a mismatch is 409 `version_conflict`, so two people editing in the console do not overwrite each other.
- Lists take `cursor` and `limit` (default 50, at most 500) and return `{"items": [...], "next_cursor": "..."}`. `next_cursor` is absent on the last page. The cursor is opaque, built from the sort key and id.
- Errors are `{"error": {"code": "...", "message": "...", "field": "..."}}` with the right status. Validation errors name the field so the console can put the message next to it.
- Every write adds an audit record.

Endpoints:

| Area | Endpoints |
|---|---|
| Status | `GET /status`: version, uptime, store kind and health, config version and last reload result, per target cooldown state and recent error rate |
| Settings | `GET /settings`, `PATCH /settings`: OIDC, role mapping, `allow_direct`, timeouts, cooldown, retention |
| Provider types | `GET /provider-types`: each adapter's settings schema, for building forms |
| Providers | `GET /providers`, `POST /providers`, `GET/PATCH/DELETE /providers/{name}`, `POST /providers/{name}/test` (sends a one-token request and reports latency or the error) |
| Routes | `GET /routes`, `POST /routes`, `GET/PATCH/DELETE /routes/{id}`, `POST /routes/reorder`, `GET /routes/resolve?model=` (which route and targets a model name would use) |
| Budgets | `GET /budgets`, `POST /budgets`, `GET/PATCH/DELETE /budgets/{id}`, `GET /budgets/status?subject=` (spend, limit, reset time) |
| Keys | `GET /keys`, `POST /keys` (returns the secret once), `GET/PATCH/DELETE /keys/{name}`, `POST /keys/{name}/rotate` |
| Prices | `GET /prices` (embedded and overrides, marked), `PUT /prices/{type}/{model}`, `DELETE /prices/{type}/{model}` |
| Usage | `GET /usage?group_by=user|team|key|project|model|provider|day&from=&to=&user=&team=&project=` (totals), `GET /requests` (single usage records, newest first, cursor paginated) |
| Audit | `GET /audit?actor=&resource=&from=&to=` (cursor paginated) |

Deleting a provider that a route still uses is 409 `in_use` with the route ids, so the console can say what to fix.

## 12. Store

State that must survive a restart lives in SQL, as the contract requires.

- SQLite through `modernc.org/sqlite` (pure Go). Default: `store: {sqlite: ./transponder.db}`. One replica.
- Postgres through `pgx/v5`. Several replicas share configuration and spend, and budgets hold across all of them.

The `Store` interface is split by concern (`UsageStore`, `ConfigStore`, `AuditStore`) so tests and handlers depend only on the part they use. Both implementations share the same SQL where the dialects agree and keep the few differences (placeholders, upsert syntax details, JSON columns) in one file each.

Tables:

- `usage`: id, org_id, time, request_id, user_id, teams, key, project, route, provider, model, endpoint, input, output, cache_read, cache_write, cost_nusd, estimated, status, latency_ms, fallbacks.
- `spend`: org_id, subject, period, period_start, cost_nusd, primary key on the first four. Updated with `INSERT ... ON CONFLICT DO UPDATE SET cost_nusd = spend.cost_nusd + excluded.cost_nusd`, which both databases support, so concurrent replicas never lose an update.
- `providers`, `routes`, `route_targets`, `budgets`, `api_keys`, `prices`, `settings`: runtime config, each with `org_id`, `version`, `source`, `created_at`, `updated_at`.
- `audit`: id, org_id, time, actor (user and key), action, resource kind and id, a JSON diff of the change with secrets replaced by `"***"`, request id.
- `config_version`: one row.
- `schema_migrations`: applied migration numbers.

Spend rows are kept for every user, team and key, budgeted or not, so adding a budget later starts from real numbers. Migrations are embedded numbered SQL files, one set per database, applied in a transaction at start. On Postgres a migration takes an advisory lock so replicas starting together do not race. `usage` rows older than the `retention_days` setting (default 90) are deleted once a day; `spend` and `audit` rows are kept.

## 13. Operations and contract fit

- `transponder --config transponder.yaml`. With no `--config` it starts with defaults: SQLite in the working directory, `:8080`, a generated secrets key and a one-time admin key printed to stderr.
- `/healthz` is always 200 while the process runs. `/readyz` checks the store and, when OIDC is configured, that the JWKS has been fetched. Providers are left out of readiness on purpose: one provider down is what fallback is for.
- `/metrics`: `transponder_requests_total{endpoint,route,provider,model,status}`, `transponder_tokens_total{direction,provider,model}`, `transponder_cost_usd_total{provider,model}`, `transponder_upstream_duration_seconds{provider,model}`, `transponder_fallbacks_total{route,from,to,reason}`, `transponder_budget_rejections_total{subject_kind}`, `transponder_unpriced_requests_total`, `transponder_config_reloads_total{result}`. No user label, to keep cardinality bounded; per-user numbers live in SQL.
- Tracing over OTLP when `otel.endpoint` is set: one span per request, one child span per upstream attempt, with provider, model, tokens and cost as attributes.
- Logs: JSON on stdout through `log/slog`, one line per request with id, user, project, route, provider, model, tokens, cost, status, latency and fallbacks. Prompts and completions are never logged.
- `GET /v1/models` lists the literal route names the caller may use and, for admins, direct `provider/model` entries, so tools such as Open WebUI can show a model picker.
- `POST /v1/messages/count_tokens` is forwarded to the first Anthropic target of the route, and returns 404 when the route has none.
- Releases: GoReleaser builds static binaries for Linux, macOS and Windows on amd64 and arm64, and a distroless container image.

| Contract item | transponder |
|---|---|
| `/v1/chat/completions`, `/v1/embeddings`, `/v1/messages` | Yes |
| Bearer token, OIDC JWT or static key | Yes, plus `x-api-key` for Anthropic clients |
| Forward caller token unchanged | transponder is the last hop. It never forwards the caller's token to a provider, since providers need their own keys |
| `traceparent`, `tracestate`, `X-Request-ID` | Kept, generated when missing, sent upstream |
| `X-Usage-Input-Tokens`, `X-Usage-Output-Tokens`, `X-Usage-Cost-USD` | Headers on normal responses, trailers on streams |
| YAML via `--config`, env overrides | `TRANSPONDER_` prefix |
| `/healthz`, `/readyz`, `/metrics`, OTLP, JSON logs | Yes |
| Everything is free | No license checks, no edition flags. README says so |
| Storage: SQLite default, Postgres for teams, pure Go, migrations at start | Section 12 |
| Admin API under `/admin/v1/`, OpenAPI file, same identity, cursor pagination, no UI | Section 11 |
| Shared repository files, `make check`, merge rules, commit and push after every change | Section 15 |
| `github.com/raven-clown/transponder`, Apache-2.0, Go 1.26+ | `go 1.26` in go.mod |

The five contract changes this spec proposed (usage trailers on streams, `x-api-key` for transponder only, the `echo` provider, shared admin conventions, the first-start admin key) were agreed and are in the contract at 1ed968a, along with `org_id` on every table, `__` for nested env keys, and route names as stable aliases.

## 14. Decisions made, and open choices

Decided here, with the reason:

- Position: per-user USD budgets from OIDC and multi-replica budgets, free. Agreed in chat on 2026-11-04.
- Canonical schema shaped like OpenAI Chat Completions, plus native passthrough when caller and provider speak the same dialect. Passthrough keeps Claude Code working as Anthropic adds fields; translation covers fallback across dialects.
- Ollama through its OpenAI-compatible endpoint. Less code, and the same adapter serves vLLM and friends.
- Fallback is ordered and has no same-target retry. Easier to explain and test than weighted balancing.
- Fall back on upstream 401 and 403. Those are gateway key problems.
- Budget errors use 402 so SDKs do not retry.
- Prices embedded and overridable; unpriced models allowed at zero cost with a metric.
- Money in int64 nano-USD.
- Runtime config in SQL, YAML optional and read-only through the API, reload by polling a version row every 2 seconds. Postgres `LISTEN/NOTIFY` would be faster and can come later.
- Provider keys sealed with AES-256-GCM; a key file is generated for SQLite, required for Postgres.
- API keys stored as SHA-256 hashes, shown once.
- Three roles, plus `allow` lists on routes for access.
- Cooldown state per replica, not shared.
- Everything under `internal/` for v1.
- Gemini through the API key endpoint only; Vertex later.

Settled on 2026-11-04, after the spec review:

- `provider` stays internal in v1. It becomes public once the interface has had real contributors.
- Unpriced models are served at zero cost, logged as `unpriced` and counted in a metric.
- Budget rejections use 402.
- Every runtime and usage table has an `org_id` column, `default` in v1, and every query filters by it, as the contract requires. Organisation endpoints and org admins ship in the release after v1.
- After v1, in this order: the OpenAI Responses API (Codex CLI uses it), request-per-minute limits per user, organisations, Bedrock and Vertex adapters, exact-match caching, `LISTEN/NOTIFY` reload.
