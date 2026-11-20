<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/header-dark.svg">
  <img src="docs/assets/header-light.svg" alt="transponder: LLM gateway. One API in front of every model." width="100%">
</picture>

<div align="center">

[![ci](https://img.shields.io/github/actions/workflow/status/raven-clown/transponder/ci.yml?branch=main&style=for-the-badge&label=ci&logo=githubactions&logoColor=white)](https://github.com/raven-clown/transponder/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/raven-clown/transponder?style=for-the-badge&color=A855F7)](https://github.com/raven-clown/transponder/releases)
[![Go Reference](https://img.shields.io/badge/go-reference-A855F7?style=for-the-badge&logo=go&logoColor=white)](https://pkg.go.dev/github.com/raven-clown/transponder)
[![license](https://img.shields.io/badge/license-Apache--2.0-A855F7?style=for-the-badge)](LICENSE)
[![openssf scorecard](https://img.shields.io/ossf-scorecard/github.com/raven-clown/transponder?style=for-the-badge&label=openssf%20scorecard)](https://scorecard.dev/viewer/?uri=github.com/raven-clown/transponder)

</div>

An LLM gateway that knows who is spending what. One Go binary in front of Anthropic, OpenAI, Gemini, xAI (Grok), Vercel AI Gateway, Mistral AI, NVIDIA NIM, Groq, Cerebras, SiliconFlow, LM Studio and Ollama, with fallback across providers, cost per user from your own identity provider, and budgets that hold across replicas. Every feature is free.

> **Status: early, [v0.2.4](https://github.com/raven-clown/transponder/releases/tag/v0.2.4).** Releases up to v0.2.4 are out: the binary starts with no config, prints an admin key, and serves chat completions (plain and streamed), embeddings and a model list from OpenAI, Ollama, OpenRouter, LiteLLM, Cloudflare AI Gateway or any OpenAI-compatible server, with token counts and cost on every response. Claude Code works through `/v1/messages`, and Anthropic and Gemini are providers. Budgets, token limits and request limits are in, and a route falls back to its next provider when one fails. Sign-in with OIDC and the admin API come next. The design is settled in [docs/design.md](docs/design.md) and the work is tracked in [issues](https://github.com/raven-clown/transponder/issues). Star or watch the repository to follow along, and see [How to help](#how-to-help) if you want to build it with us.
> xAI (Grok) support is unreleased.
> Vercel AI Gateway support is unreleased.
> Mistral AI support is unreleased.
> NVIDIA NIM support is unreleased.
> Groq support is unreleased.
> Cerebras support is unreleased.
> SiliconFlow support is unreleased.
> LM Studio support is unreleased.

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/how-it-works-dark.svg">
  <img src="docs/assets/how-it-works-light.svg" alt="Three requests go through transponder. alice's call to OpenAI fails with 503 and falls back to Anthropic, bob's goes to Gemini, a nightly job goes to Ollama, and each caller's spend is counted." width="100%">
</picture>

## What it does

- Speaks the OpenAI API (`/v1/chat/completions`, `/v1/embeddings`, `/v1/models`) and the Anthropic API (`/v1/messages`), so the OpenAI and Anthropic SDKs, Claude Code and most tools work with only a base URL change.
- Serves `/healthz` (the process is up) and `/readyz` (the store answers) without an API key, for liveness and readiness probes.
- Serves Prometheus metrics on `/metrics`: requests, latency, tokens and cost by provider and model, fallbacks and limit refusals.
- Exports a trace for every call over OTLP when `otel.endpoint` is set, with a span for each provider it tried, and never the prompt or the answer.
- Sends each call to Anthropic, OpenAI, Gemini, xAI (Grok), Vercel AI Gateway, Mistral AI, NVIDIA NIM, Groq, Cerebras, SiliconFlow, LM Studio or Ollama, and to any OpenAI-compatible server such as vLLM or [llama.cpp server](https://github.com/ggml-org/llama.cpp/tree/master/tools/server).
- Falls back to the next provider when one is down, rate limited or out of credit, and skips a provider that keeps failing for a while.
- Knows the caller from an OIDC token (Okta, Entra, Keycloak, Zitadel, Google, Auth0 and others) or an API key, and records tokens and cost in USD for every call.
- Puts a daily or monthly budget on a user, a team or a key, and stops the calls once it is spent.
- Reports usage on every response in `X-Usage-Input-Tokens`, `X-Usage-Output-Tokens` and `X-Usage-Cost-USD`.
- Stores everything in SQLite with no setup, or in Postgres when you run several replicas.
- Can be managed entirely through a JSON admin API, so you never have to write YAML. A web console that uses it is planned.

## Why another gateway

[bifrost](https://github.com/maximhq/bifrost) and [litellm](https://github.com/BerriAI/litellm) are good projects, and if you need dozens of providers they are the better choice today. But both keep the features a team needs to share a gateway behind a paid license:

| | bifrost (open source) | litellm (open source) | transponder |
|---|---|---|---|
| Sign in with your OIDC provider | Enterprise | Enterprise (JWT auth) | Free |
| Budget per user from their token | No, budgets hang off virtual keys | Enterprise | Free |
| Several replicas sharing one budget | Enterprise | Free | Free |
| Admin roles | Enterprise | Enterprise | Free |
| Audit log of admin changes | Enterprise | Enterprise | Free |
| Circuit breaker | Enterprise | | Free |
| Organisations in one instance | | Enterprise | Planned, free |

Sources: bifrost's [enterprise overview](https://github.com/maximhq/bifrost/blob/main/docs/enterprise/overview.mdx) and [budget docs](https://github.com/maximhq/bifrost/blob/main/docs/features/governance/budget-and-limits.mdx), litellm's [enterprise page](https://docs.litellm.ai/docs/enterprise) and [JWT auth docs](https://docs.litellm.ai/docs/proxy/token_auth), read in November 2026. Blank cells are ones we have not checked.

transponder has no paid edition and never will. SSO, roles, the audit log, replicas and the admin API are all in the Apache-2.0 build.

It is also small on purpose. One provider is one Go package and one line in `internal/provider/registry`, so adding a backend is a weekend pull request.

## Part of mothership

transponder works on its own. It is also one of four projects that fit together:

- [airlock](https://github.com/raven-clown/airlock): an MCP gateway
- [rosetta](https://github.com/raven-clown/rosetta): serve one agent over MCP, A2A, ACP and AG-UI
- [logbook](https://github.com/raven-clown/logbook): shared agent memory as an MCP server
- transponder: the model gateway they call

The same user token flows through all four, so an agent's tool calls, memories and model spend line up under one person. The shared rules are in the [mothership contract](https://github.com/raven-clown/mothership/blob/main/CONTRACT.md).

## Install

Download a binary for Linux, macOS or Windows from the [releases page](https://github.com/raven-clown/transponder/releases), or run the container image:

```shell
docker run -p 8080:8080 -v transponder:/data ghcr.io/raven-clown/transponder:latest
```

Image tags follow the releases: `v0.2.1` for one release, `v0.2` for the newest patch of a minor line, and `latest`.

With Go 1.26 or later:

```shell
go install github.com/raven-clown/transponder/cmd/transponder@latest
transponder --version
```

## Updates

transponder never updates itself. To see what a newer release changes before you install it:

```shell
transponder --check-update --config transponder.yaml
```

It lists every release newer than the one running, says how urgent the update is (security fixes, breaking changes, bug fixes or new features) and puts first the changes that touch your config, such as a fix to a provider type you use. It only reads the public release list on GitHub.

To get the same news in the logs, set `update_check: true` (or `TRANSPONDER_UPDATE_CHECK=true`). transponder then checks at start and once a day and logs one `update available` line with its urgency, how many releases behind it is and how many changes touch your config. It is off by default, and a failed check is only logged at debug level. You can also watch the repository for releases on GitHub, or let Dependabot or Renovate bump the image tag.

## Configuration test

To check a configuration file for syntax errors, missing variables or bad provider routes without starting the server, opening a port or creating a database file:

```shell
transponder --config-test --config transponder.yaml
```

On success it prints `transponder: config ok` and exits 0. On failure it prints the error to stderr and exits 1. Without `--config` it verifies the built-in defaults.

## Try it

```shell
transponder
# first start: SQLite in the working directory, and a one-time admin key on stderr
```

With no config every model is answered by `echo`, a built-in test provider. To use real models, write a config file:

```yaml
# transponder.yaml
providers:
  - name: claude
    type: anthropic       # https://api.anthropic.com/v1 unless base_url is set
    api_key: ${ANTHROPIC_API_KEY}
  - name: gemini
    type: gemini          # Gemini API with an API key
    api_key: ${GEMINI_API_KEY}
  - name: openai
    type: openai
    api_key: ${OPENAI_API_KEY}
  - name: local
    type: ollama          # http://localhost:11434/v1 unless base_url is set
  - name: proxy
    type: litellm         # http://localhost:4000/v1 unless base_url is set
  - name: siliconflow
    type: siliconflow     # https://api.siliconflow.com/v1 unless base_url is set. China is https://api.siliconflow.cn/v1
    api_key: ${SILICONFLOW_API_KEY}
  - name: vllm
    type: vllm            # http://localhost:8000/v1 unless base_url is set
  - name: llamacpp
    type: llamacpp        # http://localhost:8080/v1 unless base_url is set. Note: llama-server listens on :8080 by default, so set base_url (e.g. http://localhost:8081/v1) or pass --port to transponder when running on the same host
    base_url: http://localhost:8081/v1
  - name: router
    type: openrouter      # https://openrouter.ai/api/v1 unless base_url is set
    api_key: ${OPENROUTER_API_KEY}
  - name: xai
    type: xai             # https://api.x.ai/v1 unless base_url is set
    api_key: ${XAI_API_KEY}
  - name: vercel
    type: vercel          # https://ai-gateway.vercel.sh/v1 unless base_url is set
    api_key: ${VERCEL_API_KEY}
  - name: mistral
    type: mistral         # https://api.mistral.ai/v1 unless base_url is set. Embeddings, including mistral-embed, use the same base URL
    api_key: ${MISTRAL_API_KEY}
  - name: nvidia
    type: nvidia          # https://integrate.api.nvidia.com/v1 unless base_url is set. A self-hosted NIM is usually http://localhost:8000/v1
    api_key: ${NVIDIA_API_KEY}
  - name: groq
    type: groq            # https://api.groq.com/openai/v1 unless base_url is set
    api_key: ${GROQ_API_KEY}
  - name: cerebras
    type: cerebras        # https://api.cerebras.ai/v1 unless base_url is set
    api_key: ${CEREBRAS_API_KEY}
  - name: lmstudio
    type: lmstudio        # http://localhost:1234/v1 unless base_url is set. api_key is optional
  - name: cf
    type: cloudflare      # set base_url to your AI Gateway compatibility endpoint
    base_url: https://gateway.ai.cloudflare.com/v1/<account_id>/<gateway_id>/compat
    api_key: ${CLOUDFLARE_API_TOKEN}
routes:
  - match: "claude-*"
    targets:
      - provider: claude
  - match: "gemini-*"
    targets:
      - provider: gemini
  - match: "gpt-*"
    targets:
      - provider: openai
  - match: "grok-4.7"
    targets:
      - provider: xai
  - match: "grok-*"
    targets:
      - provider: xai
  - match: "anthropic/claude-opus-5"
    targets:
      - provider: vercel
  - match: "mistral-*"
    targets:
      - provider: mistral
  - match: "nvidia-llama"
    targets:
      - provider: nvidia
        model: meta/llama-3.1-8b-instruct
  - match: "groq-llama"
    targets:
      - provider: groq
        model: llama-3.3-70b-versatile
  - match: "cerebras-gpt-oss"
    targets:
      - provider: cerebras
        model: gpt-oss-120b
  - match: "siliconflow-deepseek-r1"
    targets:
      - provider: siliconflow
        model: deepseek-ai/DeepSeek-R1
  - match: "lmstudio-*"
    targets:
      - provider: lmstudio
  - match: "fast"
    targets:
      - provider: openai
        model: gpt-4o-mini
  - match: "*"
    targets:
      - provider: local
```

```shell
transponder --config transponder.yaml
```

```shell
curl localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TRANSPONDER_KEY" \
  -d '{"model": "fast", "messages": [{"role": "user", "content": "hi"}]}' -i
# X-Usage-Input-Tokens, X-Usage-Output-Tokens and X-Usage-Cost-USD come back with the answer
```

Routes are tried in order and the first match wins.

### Fallback

A route can list several targets. They are tried in order, and the first that answers serves the call:

```yaml
routes:
  - match: "claude-*"
    targets:
      - provider: claude
      - provider: openai
        model: gpt-5
      - provider: local
        model: qwen3:8b
settings:
  cooldown_failures: 3   # failures in a row before a target goes last
  cooldown: 30s          # for how long
```

- The next target takes over when one is unreachable or times out, answers 408, 429, any 5xx or Anthropic's 529, rejects the gateway's own key (401, 403), fails a stream before its first event, or cannot take the request, such as an image at a URL going to Gemini, which takes inline images only.
- A request the provider rejects as wrong (any other 4xx) is not sent elsewhere. Nor is anything retried once the first byte has reached the caller, after the caller has gone, or past `request_timeout`. The same target is never tried twice in one call.
- After three failures in a row a target is put last for 30 seconds, so calls stop waiting on it. It is still tried when every other target has failed. This is kept per process, by provider and upstream model.
- A call that is not streamed has no timeout of its own per target, so a target that hangs holds it until `request_timeout`. Streams move on after `attempt_timeout`.
- `X-Transponder-Provider`, `X-Transponder-Model` and `X-Transponder-Fallbacks` say which target answered and how many were tried before it. The usage record has the same.
- With limits on, a call reserves the price of the route's dearest target and settles at what the answering target cost.
- `POST /v1/messages/count_tokens` goes to the first Anthropic target of the route.

### Images

Chat requests can carry images on both endpoints: `image_url` parts on `/v1/chat/completions`, as a URL or a `data:image/...;base64,` URL, and image blocks on `/v1/messages`, as a `url` or `base64` source. Text and images reach the provider in the order they were sent, translated to OpenAI `image_url` parts, Anthropic image blocks or Gemini `inlineData`.

| Target | Image at a URL | Inline image |
|---|---|---|
| OpenAI and OpenAI-compatible | yes | PNG, JPEG, WebP, GIF |
| Anthropic | yes | PNG, JPEG, WebP, GIF |
| Gemini | no | PNG, JPEG, WebP, HEIC, HEIF |
| echo | ignored | ignored; it answers with the text |

- A target that cannot take an image is skipped and the next one is tried. When none can, the caller gets a 400 that says why.
- An inline image the gateway translates may be at most 5 MiB once decoded; a larger one is a 400 `image_too_large`. A request that goes to a provider of its own dialect is sent on unopened, so the provider's own limit applies.
- Images go only in user messages.
- For limits, each image counts as 1,000 input tokens until the provider reports the real usage. That is about what a 1024 by 1024 picture costs on OpenAI, Anthropic and Gemini.

### Claude Code

Point Claude Code at transponder with a transponder API key:

```shell
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_AUTH_TOKEN=$TRANSPONDER_KEY claude
```

`/v1/messages` sends the request to Anthropic as it came, with only the model changed, so thinking, prompt caching and beta features keep working, and usage and cost are recorded for the key. A route can also send Claude Code's requests to Gemini, OpenAI or a local model. Those are translated, tools and images included, though features only Anthropic has, such as its server tools, PDF documents and images in tool results, are refused on those routes.

The admin API is on the roadmap below.

### Sign-in and roles

API keys always work. To also let people call transponder with a token from your OpenID Connect provider (Okta, Entra ID, Keycloak, Zitadel, Google, Auth0 and others), name the issuer and the audience:

```yaml
auth:
  oidc:
    issuer: "https://idp.example.com/realms/acme"   # the iss claim; keys are found through its discovery document
    audience: "transponder"                         # tokens issued for another service are refused
    # jwks_url: "https://idp.example.com/keys"      # only for an issuer without a discovery document
    user_claim: sub                                 # default sub; email or preferred_username also work
    teams_claim: groups                             # default groups
    roles_claim: realm_access.roles                 # a dotted name reads a nested claim
    role_mapping:                                   # claim value -> admin, viewer or member
      platform-admins: admin
      auditors: viewer
    clock_skew: 1m                                  # allowance on exp and nbf, default 1m
```

```shell
curl localhost:8080/v1/chat/completions -H "Authorization: Bearer $ID_TOKEN"   -d '{"model": "fast", "messages": [{"role": "user", "content": "hi"}]}'
```

- A bearer token or `x-api-key` value that is a JWT is checked against the issuer: signature, `iss`, `aud`, `exp` and `nbf`. Anything else is looked up as an API key. Only asymmetric signatures (RS, PS, ES and EdDSA) are accepted.
- The caller's user id comes from `user_claim` and their teams from `teams_claim`, so budgets and profiles for `user:` and `team:` subjects apply to people who sign in. A caller with a token has no `key:` subject.
- Every caller has a role. A token gets the highest role its `roles_claim` values map to, and `member` when none does. An API key carries the role it was created with.
- The signing keys are fetched when the first token arrives and kept for an hour. A token naming a key transponder has not seen makes it read the keys again, at most once every 10 seconds, so made-up key ids cannot flood the identity provider. When the provider cannot be reached, keys already fetched keep working; with none yet, calls with a token get 503 `auth_unavailable` while API keys keep working, and `/readyz` reports `oidc`.
- When `role_mapping` gives someone `admin`, the first start does not print an admin key, since that person can sign in instead.
- Each key can be set from the environment too, such as `TRANSPONDER_AUTH__OIDC__ISSUER`. `role_mapping` is a map, so it is set in the file.

### Access per route

A route can say who may call it. Leave `allow` out and every caller with a valid key or token may:

```yaml
routes:
  - match: "claude-*"
    allow: ["team:platform", "user:alice@example.com", "key:ci", "role:admin"]
    targets:
      - provider: claude
  - match: "*"
    targets:
      - provider: local
```

- `user:<id>` matches the user id from a token, and also an API key of that name, since a key counts as `user:<key name>`. Use `key:<name>` to let in only the key.
- `team:<name>` matches any of the caller's teams: the teams of a key, or the `teams_claim` of a token.
- `role:<role>` lets in that role and the ones above it, so `role:viewer` also lets in admins.
- Any one matching entry is enough. A caller outside the list gets 403 `route_forbidden`, and the message names the route. Nothing is recorded or charged. Anthropic-dialect callers get `permission_error` with the same code.
- `GET /v1/models` lists only the routes the caller may use.
- `--config-test` checks every entry.

### Limits

Budgets cap what a user, a team or a key spends in USD, per UTC day or month. Profiles, in the same format as the other mothership projects, cap tokens and requests:

A caller with an API key counts as `user:<key name>` as well as `key:<key name>`, so name keys after the people or services that hold them. A caller who signs in with a token counts as `user:` and `team:` from its claims.

```yaml
budgets:
  - {subject: "user:*", period: monthly, limit_usd: 20}       # every user without a rule of their own
  - {subject: "user:alice@example.com", period: monthly, limit_usd: 100}
  - {subject: "team:platform", period: daily, limit_usd: 50}
  - {subject: "key:ci", period: monthly, limit_usd: 10}

default_profile: free
profiles:
  - name: free
    limits:
      requests:   {rate: 60/m, concurrency: 4}
      llm.tokens: {rate: 20000/m, per_day: 100000, per_month: 2000000}
      llm.cost_usd: {per_month: 5}
      llm.max_input_tokens: 8192
      llm.max_output_tokens: 2048
      llm.max_reasoning_tokens: 4096
  - name: pro
    limits:
      llm.tokens: {per_day: 2000000}
profile_bindings:
  - {subject: "user:alice@example.com", profile: pro}   # key:, user: or team:, in that order
# profiles_file: /etc/mothership/profiles.yaml          # or share one file with the other projects
```

- `llm.tokens` counts input, output and reasoning tokens together. Reasoning tokens are counted apart where the provider reports them (OpenAI, Gemini) and come back in `X-Usage-Reasoning-Tokens`. Where a provider reports no usage, tokens are estimated on the high side: four characters a token for ASCII, one and a half tokens for every other character.
- A call reserves its estimated input, its output limit (4096 tokens when it sets none and no profile cap applies) and their cost before it runs, and settles to the real usage after. A failed call gives the reservation back. A plain call the client abandons keeps its reservation, since the provider may still bill the work; an abandoned stream settles to what it streamed. A kept reservation lives in memory only and is not rebuilt after a restart.
- An exact `user:<id>` budget replaces `user:*` for its own period only, so a larger monthly budget for one user keeps the daily `user:*` cap.
- `llm.max_output_tokens` lowers `max_tokens` or `max_completion_tokens`, never raises them. `llm.max_reasoning_tokens` lowers Anthropic's thinking budget, which is also kept under `max_tokens` and dropped when that leaves less than Anthropic's minimum of 1024. OpenAI and Gemini chat requests get no separate reasoning cap; for OpenAI, `max_completion_tokens` bounds reasoning and output together. A prompt over `llm.max_input_tokens` gets 400 `input_too_large`.
- A spent budget is 402 `budget_exceeded`, a spent token allowance 402 `quota_exceeded`, a rate 429 `rate_limited` with `Retry-After`, too many calls at once 429 `concurrency_limited`. OpenAI-dialect callers get these as `error.code`; Anthropic-dialect callers get Anthropic's `error.type` (`billing_error`, `rate_limit_error`, `invalid_request_error`) with the same code in `error.code`. Responses carry `RateLimit-Policy` and `RateLimit` headers for the rates that apply.
- `GET /v1/usage` shows the caller each budget and token allowance that applies to them: the limit, what is used (calls in flight count at what they reserved), what is left and when it resets.
- With SQLite, counters live in memory and are rebuilt from the usage table at start, so a restart keeps this month's recorded spend. A limit can be passed only by calls already in flight whose real usage turns out larger than their reservation, and by kept reservations lost in a restart.
- With Postgres, counters live in the database and every replica shares them, so a budget, an allowance, a rate or a concurrency cap holds across all of them. See below.

### Postgres and several replicas

SQLite serves one replica. To run more, point every replica at one Postgres database:

```yaml
store:
  postgres: postgres://transponder:${PGPASSWORD}@db.internal:5432/transponder?sslmode=require
```

`TRANSPONDER_STORE__POSTGRES` sets the same key. The value is a `postgres://` URL or a `key=value` connection string, and takes pool settings such as `pool_max_conns=20`. Set only one of `store.sqlite` and `store.postgres`.

- The schema is created and migrated at start. Replicas that start together take a lock, so each migration runs once.
- API keys and usage records are shared: a key made on one replica works on all of them, and the first replica to start prints the one-time admin key.
- Migrations only add tables and columns, and removals come in a later release, so replicas on the old and the new version can run side by side during a rolling update.
- Budgets, token allowances, rates and concurrency caps are counted in Postgres, shared by every replica. Each reservation is one atomic statement, so two replicas cannot both take the last of a budget. The first replica to start fills the counters from this month's usage once. This adds a few short queries to each limited call; `limits.store: memory` keeps the counters in each process instead, for a single replica.
- A change to the runtime config reaches every replica within 2 seconds, without a restart: each replica checks a version number in the database every 2 seconds and, when it moved, loads the whole config again and swaps it in. If the new config does not load, the old one keeps serving and the error is logged. Calls already running finish on the config they started with.
- A concurrency slot belongs to the replica that took it. If that replica dies, its slots free themselves within 30 seconds.
- If Postgres cannot be reached, `limits.on_store_error: local` (the default) counts in each replica with its share of every limit, the limit divided by the number of replicas seen in the last 30 seconds. `deny` answers 503 `overloaded` with `Retry-After: 1` instead.

```yaml
limits:
  store: sql             # the default with store.postgres; memory keeps counters in each process
  on_store_error: local  # or deny
```

### Metrics

`GET /metrics` serves Prometheus metrics. On the gateway's port it needs a key or token with the `viewer` or `admin` role, because that port is the one callers reach, and the metrics name your providers, models and routes and total the spend. Prometheus sends the key as a bearer token:

```yaml
scrape_configs:
  - job_name: transponder
    authorization:
      credentials_file: /etc/prometheus/transponder.key
    static_configs:
      - targets: ["transponder:8080"]
```

To scrape without a key, set `ops_listen_addr` (or `TRANSPONDER_OPS_LISTEN_ADDR`). `/healthz`, `/readyz` and `/metrics` are then also served on that second port, where `/metrics` is open, so keep the port inside your network:

```yaml
listen_addr: ":8080"
ops_listen_addr: ":9464"
```

| Metric | Labels | What it counts |
|---|---|---|
| `transponder_requests_total` | `endpoint`, `route`, `provider`, `model`, `code` | Requests answered. `provider` and `model` are the target that answered, and are empty when the request ended before a provider was asked (a 401, a 404, a limit). |
| `transponder_request_duration_seconds` | `endpoint`, `route` | Histogram of the time from the request arriving to the last byte of the answer, streams included. |
| `transponder_inflight_requests` | | Requests being served now. |
| `transponder_limited_total` | `reason` | Requests refused by a limit or a budget: `rate_limited`, `concurrency_limited`, `quota_exceeded`, `budget_exceeded`, `input_too_large`. |
| `transponder_events_dropped_total` | `sink` | Usage records that could not be written (`sql`) and spans that could not be exported (`otlp`). |
| `transponder_tokens_total` | `direction`, `provider`, `model` | Tokens used. `direction` is `input`, `output`, `cache_read`, `cache_write` or `reasoning`. |
| `transponder_cost_usd_total` | `provider`, `model` | Cost in US dollars. |
| `transponder_upstream_duration_seconds` | `provider`, `model` | Histogram of the time one attempt on a provider took. |
| `transponder_fallbacks_total` | `route`, `from`, `to`, `reason` | Moves to the next target of a route. `reason` is `server_error`, `rate_limited`, `timeout`, `unreachable`, `auth`, `unsupported` or `error`. |
| `transponder_unpriced_requests_total` | | Calls on a model that has no price. |
| `transponder_config_reloads_total` | `result` | Reloads of the runtime config: `ok` for a new version swapped in, `failed` for one that could not be read or loaded, counted on every check while it stays that way. |
| `transponder_build_info` | `version` | Always 1. |

No label carries a user, a team, a key or a request id, so the number of series follows your config and not your traffic. Spend per user is in the usage records and `GET /v1/usage`. A model name is a label when the config sets it. Behind a wildcard route whose target sets no model, the caller picks the name, so it becomes a label only once a provider has answered for it, for at most 200 names, and is counted as `other` otherwise. `/healthz`, `/readyz` and `/metrics` are not counted as requests.

### Tracing

Set `otel.endpoint` to the base URL of an OTLP/HTTP collector and transponder exports a trace for every call. It is off by default.

```yaml
otel:
  endpoint: http://collector:4318   # spans are posted to /v1/traces
  headers:                          # optional, for a collector that needs a key
    Authorization: Bearer ${OTEL_KEY}
  sample_ratio: 0.1                 # optional, the share of traces that start here; default all
```

- Each call is one span, named after the endpoint, such as `POST /v1/chat/completions`, with the status code, route, provider, model, tokens, cost and number of fallbacks.
- Each attempt on a provider is a child span, such as `chat gpt-4o-mini`, with the provider, the model, its tokens, and for a failed attempt the reason (`server_error`, `rate_limited`, `timeout` and the others listed under `transponder_fallbacks_total`) and the provider's status code. A route that fell back shows every target it tried, in order.
- Spans never hold prompt or completion text, and never a provider's error message, since those can quote the prompt.
- Only calls from a caller with a valid key or token are traced. A request that fails to sign in, a probe and a scrape leave no span, so someone without a key cannot fill your collector.
- `traceparent` and `tracestate` from the caller are kept. A call that arrives with a `traceparent` joins that trace and follows its sampled flag, and `sample_ratio` only decides for traces that start at the gateway. The provider is sent a `traceparent` that names the attempt's span, and `tracestate` as it came. A `tracestate` over the 512 characters the standard asks for is still passed on but left off the exported spans. With tracing off both headers are passed on unchanged.
- Spans are sent in batches every 5 seconds, over OTLP/HTTP with JSON bodies, which the OpenTelemetry Collector, Jaeger, Tempo and Grafana Alloy all accept on port 4318. OTLP over gRPC (port 4317) is not supported. A collector that is down or slow never holds up a call: spans that cannot be sent are dropped and counted in `transponder_events_dropped_total{sink="otlp"}`.
- The endpoint and the ratio can also come from the environment, as `TRANSPONDER_OTEL__ENDPOINT` and `TRANSPONDER_OTEL__SAMPLE_RATIO`. Headers are set in the file, where `${VAR}` keeps a key out of it.

## Roadmap

Each milestone ships as a release when it lands:

1. Released in v0.1.0. Skeleton: config, SQLite store, API keys, a test provider, `/v1/chat/completions` with usage headers
2. Released in v0.1.0. OpenAI and Ollama adapters, streaming, embeddings
3. Released in v0.2.0. Provider registry, split timeouts, `GET /v1/models`
4. Released in v0.2.2. Anthropic API in and out, the Gemini adapter, translation between dialects, Claude Code working
5. Released in v0.2.3. Budgets, token limits and request limits
6. Released in v0.2.4. Fallback across targets with a cooldown, `GET /v1/usage`
7. Access per route, OIDC and roles, the admin API with OpenAPI and an audit log, Postgres and several replicas
8. Metrics, tracing and a five-minute demo

After that: the OpenAI Responses API, a shared counter store for several replicas, organisations, Bedrock and Vertex, and caching.

## How to help

- Pick an issue labeled [`good first issue`](https://github.com/raven-clown/transponder/labels/good%20first%20issue). Each one says which files to change and which tests to add.
- Adding a provider is the easiest way to add something big. Look for the [`adapter`](https://github.com/raven-clown/transponder/labels/adapter) label.
- Read [CONTRIBUTING.md](CONTRIBUTING.md) first, and leave a comment on the issue before you start.
- Found a bug, or want something new? Open an issue with one of the [issue forms](https://github.com/raven-clown/transponder/issues/new/choose). Each one says what to include.
- Pull requests come with a template that lists what reviewers check first in this project, so you know what to cover before you open one.
- Ideas and questions are welcome too.

## Contributors

Thanks to everyone who has helped build transponder.

<a href="https://github.com/raven-clown/transponder/graphs/contributors"><img src="https://contrib.rocks/image?repo=raven-clown/transponder" alt="Contributors to transponder"></a>

## License

[Apache-2.0](LICENSE)
