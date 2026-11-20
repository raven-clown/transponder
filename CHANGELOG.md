# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `store.postgres` (or `TRANSPONDER_STORE__POSTGRES`) keeps API keys and usage records in Postgres, so several replicas can share them. The schema is migrated at start, and replicas that start together apply each migration once.
- Every replica checks the `config_version` row in the store every 2 seconds and, when it changed, loads and validates the whole runtime config and swaps it in. A config that fails to load leaves the old one serving and is logged once. Calls already running finish on the config they started with. The admin API will raise the version with each write.
- With `store.postgres`, budgets, token allowances, request rates and concurrency caps are counted in Postgres, so they hold across every replica. Two replicas cannot both spend the last of a budget. `limits.store: memory` keeps per-process counters for a single replica. When Postgres cannot be reached, `limits.on_store_error: local` counts in each replica with its share of each limit, and `deny` answers 503 `overloaded`.
- Images in chat: `image_url` parts on `/v1/chat/completions` (a URL or a `data:` URL) and image blocks on `/v1/messages` are translated to OpenAI, Anthropic and Gemini targets with text and images kept in order. A target that cannot take an image, such as Gemini with an image at a URL, is skipped. A translated inline image over 5 MiB is a 400 `image_too_large`, and each image counts as 1,000 input tokens for limits until the provider reports usage (#41).
- OpenAI Responses API: `POST /v1/responses` is passed through to targets that speak the OpenAI API with `model` changed and `store` set to `false`, streamed or not, so the Codex CLI and newer OpenAI SDK code work through transponder. Usage, with cached and reasoning tokens, is read from the final body or from the event that ends the stream, so usage headers, usage records, budgets, limits, access per route and fallback work as they do for chat. Anthropic, Gemini and `echo` targets, and servers without the endpoint, are skipped. Every caller of a route shares the provider account, so nothing is stored there and `previous_response_id`, `conversation` and `item_reference` items are refused with a 400 `unsupported`: send the whole conversation in `input`. A caller with `llm.max_input_tokens` gets a 400 `input_not_countable` for a stored `prompt` or an `input_file`, which the limit could not measure; for other callers a file counts as 1,000 tokens like an image. `background: true` is refused, since its result and usage could not be fetched through the gateway (#42).
- `--config-test` flag to validate a configuration file, environment variables, provider routes and settings without starting the server, binding listening ports, or creating a database file (#103).
- Every response carries `X-Request-ID`: the caller's value when it is 1 to 128 printable ASCII characters, or a new 32-character hex id. The same id is recorded with usage and sent to the provider.
- Every line the JSON logger writes carries `"service":"transponder"`, so logs from the four projects can be told apart in one place.
- Mistral AI provider (`type: mistral`) defaulting to `https://api.mistral.ai/v1` and sending `api_key` as a bearer token. Chat and embeddings (`mistral-embed`) share that base URL. A `base_url` still overrides that default. No prices are included, so calls are logged as unpriced.
- NVIDIA NIM provider (`type: nvidia`) defaulting to `https://integrate.api.nvidia.com/v1` and sending `api_key` as a bearer token. A `base_url` still overrides that default. A self-hosted NIM is usually `http://localhost:8000/v1`. No prices are included, so calls are logged as unpriced.
- Groq provider (`type: groq`) defaulting to `https://api.groq.com/openai/v1` and sending `api_key` as a bearer token. A `base_url` still overrides that default. No prices are included, so calls are logged as unpriced.
- Cerebras provider (`type: cerebras`) defaulting to `https://api.cerebras.ai/v1` and sending `api_key` as a bearer token. A `base_url` still overrides that default. No prices are included, so calls are logged as unpriced.
- SiliconFlow provider (`type: siliconflow`) defaulting to `https://api.siliconflow.com/v1` and sending `api_key` as a bearer token. A `base_url` still overrides that default. The China host is `https://api.siliconflow.cn/v1`. No prices are included, so calls are logged as unpriced.
- LM Studio provider (`type: lmstudio`) defaulting to `http://localhost:1234/v1`. `api_key` is optional and, when set, is sent as a bearer token. Targets using `lmstudio` count as free in usage costs.
- Every caller has one of the roles `admin`, `viewer` or `member`, and the role of an API key now comes with the caller on each request, ready for the admin API and access per route. A key stored without a role is a `member` and calls the gateway as before.
- Sign-in with OpenID Connect next to API keys: set `auth.oidc.issuer` and `auth.oidc.audience`, and a JWT in `Authorization: Bearer` or `x-api-key` is checked for its signature, issuer, audience and expiry. `user_claim` (default `sub`), `teams_claim` (default `groups`), `roles_claim` with `role_mapping`, `jwks_url` and `clock_skew` (default 1m) map the token to a user, teams and a role. The issuer's keys are cached for an hour and read again for an unknown key id at most once every 10 seconds. With no keys fetched yet and the issuer down, token callers get 503 `auth_unavailable` and `/readyz` reports `oidc`. When `role_mapping` names an `admin`, the first start no longer prints an admin key.
- Admin API under `/admin/v1/`, described by `api/openapi.yaml` and served at `/admin/v1/openapi.yaml`: `GET /status`, API keys (`GET`, `POST`, `PATCH`, `DELETE` and `POST /keys/{name}/rotate`, the secret shown once), usage totals grouped by user, team, key, project, model, provider, route or day (`GET /usage`), single usage records (`GET /requests`) and the audit log (`GET /audit`). `viewer` may read and `admin` may write. Lists are cursor paginated, errors name the field, and `If-Match` with the item's `version` refuses a write over someone else's change with 409 `version_conflict`.
- Providers, routes, budgets and limit profiles can be created, changed and deleted through the admin API (`/admin/v1/providers`, `/routes`, `/budgets`, `/profiles`), so a new setup needs no YAML. Each write is live on the replica that took it before it answers and on every other replica within 2 seconds. A write that would leave a config that does not load is refused with 400 `invalid_config` and the old one keeps serving. Deleting a provider a route uses is 409 `in_use`.
- Provider keys sent to the admin API are sealed with AES-256-GCM under `secrets_key` (`TRANSPONDER_SECRETS_KEY`) before they are stored, and reads only show whether one is set and its last four characters. With SQLite and no `secrets_key`, one is generated into `transponder.key` next to the database; with Postgres it must be set, the same on every replica.
- The providers, routes, budgets and profiles of the YAML file are now stored at start and shown by the admin API as `managed_by_file`, read-only there. Removing one from the file and restarting removes it.
- Audit log: every write through the admin API records who made it, what changed (the item before and after, without secrets), when, and the request id, in the same transaction as the write, on SQLite and Postgres.
- `GET /metrics` serves Prometheus metrics: `transponder_requests_total`, `transponder_request_duration_seconds`, `transponder_inflight_requests`, `transponder_limited_total` and `transponder_events_dropped_total`, plus `transponder_tokens_total` and `transponder_cost_usd_total` by provider and model, `transponder_upstream_duration_seconds`, `transponder_fallbacks_total`, `transponder_unpriced_requests_total`, `transponder_config_reloads_total` and `transponder_build_info`. No label carries a user, a key or a request id. On the gateway's port `/metrics` needs a key or token with the `viewer` or `admin` role.
- Tracing over OTLP: set `otel.endpoint` to an OTLP/HTTP collector (port 4318) and each call is exported as one span with a child span for every provider it tried, carrying the provider, model, status, tokens and cost, and never prompt or completion text. `otel.headers` adds headers for a collector that needs a key, and `otel.sample_ratio` samples the traces that start at the gateway. A caller's `traceparent` and `tracestate` are kept: the call joins the caller's trace and the provider is sent the attempt's span. Off by default. An `otel.endpoint` that is not an http or https URL is now a config error, where it used to be ignored.
- `demo/`: `docker compose up` starts transponder in front of a fake provider with calls flowing through it, Prometheus, Jaeger, and a Grafana dashboard of requests, latency, tokens, cost, fallbacks and limits. No provider key is needed and nothing leaves the machine.
- `ops_listen_addr` (`TRANSPONDER_OPS_LISTEN_ADDR`) serves `/healthz`, `/readyz` and `/metrics` on a second port, where `/metrics` needs no key, for a Prometheus inside your network.
- Access per route: `allow` on a route lists who may call it, as `user:<id>`, `team:<name>`, `key:<name>` or `role:<role>` (that role and the ones above it). Anyone else gets 403 `route_forbidden` naming the route, with nothing recorded, and `/v1/models` lists only the routes the caller may use. Routes without `allow` stay open to every authenticated caller.

### Fixed

- Parallel tool calls streamed by an OpenAI-compatible server that leaves out each call's `index` are kept apart. They were merged into one call with both names and arguments run together, which broke tool use for `/v1/messages` callers and every translated stream.
- An Anthropic tool call with no input reaches an OpenAI caller with `"arguments": "{}"` when streamed, as it does when not streamed, instead of an empty string that does not parse. Input sent whole in `content_block_start` is passed on too.
- Tool call ids made by other providers, such as `functions.get_weather:0`, are rewritten to ones Anthropic accepts (letters, digits, `_` and `-`) in requests to Anthropic and in answers to `/v1/messages` callers, so a conversation that moves to Anthropic is no longer refused with 400.

## [0.2.4] - 2026-11-14

### Added

- Fallback: a route's targets are tried in order, and the next one takes over when a target is unreachable, times out, answers 408, 429, a 5xx or 529, rejects the gateway's key, fails a stream before its first event, or cannot take the request. A request the provider rejects as wrong is not retried elsewhere, and nothing falls back once a byte has reached the caller.
- Cooldown: after `settings.cooldown_failures` (3) failures in a row a target is tried last for `settings.cooldown` (30s).
- `X-Transponder-Provider`, `X-Transponder-Model` and `X-Transponder-Fallbacks` on routed responses, and `fallbacks` in the usage record.
- `GET /v1/usage` shows the caller what they have used and have left of each budget and token allowance that applies to them.
- Vercel AI Gateway provider (`type: vercel`) defaulting to `https://ai-gateway.vercel.sh/v1` and sending `api_key` as a bearer token. A `base_url` still overrides that default. No prices are included, so calls are logged as unpriced.
- xAI (Grok) provider (`type: xai`) defaulting to `https://api.x.ai/v1` and sending `api_key` as a bearer token. A `base_url` still overrides that default.
- xAI reasoning tokens are counted apart from completion tokens, as in its chat completion usage, and priced at the output rate.
- Prices for grok-4.7, grok-4.6, grok-4.5 and grok-4.3, using the rate for a prompt under 200k tokens on the global endpoint. Longer prompts, and the US endpoint, cost more than these rates.

### Changed

- With limits on, a call reserves the price of its route's dearest target instead of the first one, so a free first target cannot let calls through that a paid fallback would bill.
- `POST /v1/messages/count_tokens` goes to the first Anthropic target of the route, wherever it is in the list.

### Fixed

- A tool call id made by Gemini is shortened to one OpenAI accepts when a later turn of the conversation goes to an OpenAI target.

## [0.2.3] - 2026-11-09

### Added

- USD budgets per user (`user:*` or an exact user), team and key, daily or monthly. A spent budget answers 402 with the subject, the limit and the reset time: code `budget_exceeded` for OpenAI-dialect callers, and type `billing_error` with code `budget_exceeded` for Anthropic-dialect callers.
- Profiles in the shared mothership format, from the config or a shared `profiles_file`, bound to keys, users and teams, with a `default_profile`: `requests` rate and concurrency, `llm.tokens` rate and daily and monthly allowances, `llm.cost_usd`, and `llm.max_input_tokens`, `llm.max_output_tokens` and `llm.max_reasoning_tokens`. Calls reserve before they run and settle after, and responses carry `RateLimit-Policy` and `RateLimit` headers.
- Reasoning tokens are counted apart where OpenAI and Gemini report them, priced at the output rate, stored with the usage and sent in `X-Usage-Reasoning-Tokens`.
- Limit counters are rebuilt from this month's usage at start, so a restart does not reset anyone's spend.
- llama.cpp server provider (`type: llamacpp`) defaulting to `http://localhost:8080/v1` and forwarding `api_key` as a bearer token. Targets using `llamacpp` count as free in usage costs. Note that `llama-server` listens on `:8080` by default, so pass `--port` or configure `listen_addr` if running both on the same machine.

### Changed

- `X-Usage-Output-Tokens` no longer includes reasoning tokens for OpenAI and Gemini; they are in `X-Usage-Reasoning-Tokens`.
- Token estimates count every non-Latin character as one and a half tokens, so Thai, CJK and other scripts are counted on the high side rather than undercounted. It is a heuristic; a model with a small vocabulary can still use more.

### Fixed

- Anthropic cache writes kept for an hour are priced at their own rate, twice the input price, instead of the five-minute rate.

## [0.2.2] - 2026-11-08

### Added

- `POST /v1/messages`, the Anthropic Messages API, so Claude Code and the Anthropic SDKs work with only a base URL change. Requests to Anthropic go through as sent, with only the model changed; streams are copied frame by frame and their usage is recorded. Requests to other providers are translated, tools included.
- `POST /v1/messages/count_tokens`, forwarded to the route's Anthropic target.
- Anthropic provider (`type: anthropic`). It forwards the caller's `anthropic-version` and `anthropic-beta`.
- Gemini provider (`type: gemini`) for chat, streams and embeddings through the Gemini API with an API key.
- OpenAI chat completions can now reach Anthropic and Gemini, and every translated call carries tools, tool calls and the system prompt.
- Prices for current Claude and Gemini models.
- `GET /healthz` answers 200 while the process is up, and `GET /readyz` answers 503 with the names of the dependencies that fail, such as `{"status":"unavailable","failed":["store"]}`, so Kubernetes and load balancers can restart the process or hold traffic. Neither needs an API key.

### Changed

- When a provider of another dialect rejects a translated request as invalid (a 4xx it would give again on a retry), the caller now gets that status and the provider's reason in its own dialect instead of a 502.


## [0.2.1] - 2026-11-07

### Added

- vLLM provider (`type: vllm`) defaulting to `http://localhost:8000/v1` and forwarding `api_key` as a bearer token. Targets using `vllm` count as free in usage costs.
- `transponder --check-update` lists the releases newer than the running one, how urgent they are and which changes touch the providers in your config. It only reads the release list and never installs anything.
- `update_check: true` logs one `update available` line at start and once a day when a newer release is out. Off by default.
- Releases come with binaries for Linux, macOS and Windows on amd64 and arm64, a `release.json` describing each change, and a container image at `ghcr.io/raven-clown/transponder`.

### Fixed

- `transponder --version` prints the commit and build time, as listed in 0.1.4. The helper was added then, but the flag still printed only the version.
- Boolean config keys can be set from the environment, such as `TRANSPONDER_UPDATE_CHECK=true`. Before, the value was read as text and the config failed to load.

## [0.2.0] - 2026-11-06

### Added

- OpenRouter provider (`type: openrouter`) defaulting to `https://openrouter.ai/api/v1`.
- An unknown provider `type` in the config now lists the known types.
- `GET /v1/models` lists the route names a caller can ask for, so clients such as Open WebUI can fill their model picker. Wildcard routes and the models a route sends upstream are left out.
- `settings.attempt_timeout` (default 60s) bounds how long a streamed call waits for its first event, and `settings.request_timeout` (default 10m) bounds every call, streams included. Calls that are not streamed get only the request timeout, since a provider sends their headers once the whole answer is ready. Before, one 10 minute limit covered everything, so a stream that never started held the caller for 10 minutes.

### Changed

- The `user` column of the `usage` table is now `user_id`, since `user` is a reserved word in Postgres. The migration runs at start and keeps every row; queries on `transponder.db` that read `user` need `user_id` instead.

### Fixed

- A provider stream that ends without `[DONE]` now ends the caller's stream with an error event and estimated usage, instead of looking complete.
- A stream line or event over 16 MiB ends the stream instead of growing memory.

## [0.1.4] - 2026-11-06

### Added

- LiteLLM Proxy provider (`type: litellm`) defaulting to `http://localhost:4000/v1` and forwarding `api_key` as a bearer token.
- `type: ollama` providers send `api_key` as a bearer token, for Ollama behind an auth proxy or hosted Ollama. Without a key no header is sent.
- `transponder --version` shows the commit and build time, for example `transponder dev (1a2b3c4, 2026-11-04T10:00:00Z)`, and `, modified` when the tree had uncommitted changes.

### Fixed

- A chat request with a wrong field type now says what to fix, such as `max_tokens must be an integer`, with `param` set to the field, instead of showing Go type names.

## [0.1.3] - 2026-11-05

### Added

- Cloudflare AI Gateway provider using its OpenAI-compatible endpoint.

## [0.1.2] - 2026-11-05

### Added

- Message `content` sent as an array of text parts is accepted and joined in order. Other part types, such as `image_url`, still return 400 `unsupported`.

### Fixed

- Restore the default signal handler after the first shutdown signal so a second Ctrl+C stops the process immediately.
- Reject requests declaring bodies larger than 20 MiB before reading them.

## [0.1.1] - 2026-11-05

### Fixed

- Preserve every data line when forwarding multiline streaming events.

## [0.1.0] - 2026-11-05

### Added

- `POST /v1/chat/completions` answered by a built-in `echo` provider, with `X-Usage-Input-Tokens`, `X-Usage-Output-Tokens` and `X-Usage-Cost-USD` headers.
- API keys checked from `Authorization: Bearer` or `x-api-key`, and a one-time admin key printed to stderr on first start.
- SQLite storage with migrations that run at startup, and a usage record for every call.
- Bootstrap config from `--config` YAML with `TRANSPONDER_` environment overrides.
- `providers` and `routes` in the config file. A route matches a model name or a glob, where `*` also matches `/`, and sends it to a provider, optionally under another model name. Providers declared with no routes are a config error.
- OpenAI and Ollama providers. OpenAI requests are forwarded with only `model` changed, so every field the API accepts keeps working. `base_url` points the OpenAI provider at any compatible server.
- Streaming chat completions, with the usage headers sent as trailers at the end of the stream.
- `POST /v1/embeddings`.
- A built-in price table for OpenAI models, so `X-Usage-Cost-USD` and the usage records carry real cost. Unpriced models are served at zero cost and logged as `unpriced`.
- Usage headers on error responses too, at zero.

[Unreleased]: https://github.com/raven-clown/transponder/compare/v0.2.4...HEAD
[0.2.4]: https://github.com/raven-clown/transponder/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/raven-clown/transponder/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/raven-clown/transponder/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/raven-clown/transponder/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/raven-clown/transponder/compare/v0.1.4...v0.2.0
[0.1.4]: https://github.com/raven-clown/transponder/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/raven-clown/transponder/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/raven-clown/transponder/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/raven-clown/transponder/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/raven-clown/transponder/releases/tag/v0.1.0
