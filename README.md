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

An LLM gateway that knows who is spending what. One Go binary in front of Anthropic, OpenAI, Gemini and Ollama, with fallback across providers, cost per user from your own identity provider, and budgets that hold across replicas. Every feature is free.

> **Status: early, [v0.2.3](https://github.com/raven-clown/transponder/releases/tag/v0.2.3).** Releases up to v0.2.3 are out: the binary starts with no config, prints an admin key, and serves chat completions (plain and streamed), embeddings and a model list from OpenAI, Ollama, OpenRouter, LiteLLM, Cloudflare AI Gateway or any OpenAI-compatible server, with token counts and cost on every response. Claude Code works through `/v1/messages`, and Anthropic and Gemini are providers. Budgets, token limits and request limits are in. Fallback across providers comes next. The design is settled in [docs/design.md](docs/design.md) and the work is tracked in [issues](https://github.com/raven-clown/transponder/issues). Star or watch the repository to follow along, and see [How to help](#how-to-help) if you want to build it with us.

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/how-it-works-dark.svg">
  <img src="docs/assets/how-it-works-light.svg" alt="Three requests go through transponder. alice's call to OpenAI fails with 503 and falls back to Anthropic, bob's goes to Gemini, a nightly job goes to Ollama, and each caller's spend is counted." width="100%">
</picture>

## What it does

- Speaks the OpenAI API (`/v1/chat/completions`, `/v1/embeddings`, `/v1/models`) and the Anthropic API (`/v1/messages`), so the OpenAI and Anthropic SDKs, Claude Code and most tools work with only a base URL change.
- Serves `/healthz` (the process is up) and `/readyz` (the store answers) without an API key, for liveness and readiness probes.
- Sends each call to Anthropic, OpenAI, Gemini or Ollama, and to any OpenAI-compatible server such as vLLM, [llama.cpp server](https://github.com/ggml-org/llama.cpp/tree/master/tools/server) or LM Studio.
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
  - name: vllm
    type: vllm            # http://localhost:8000/v1 unless base_url is set
  - name: llamacpp
    type: llamacpp        # http://localhost:8080/v1 unless base_url is set. Note: llama-server listens on :8080 by default, so set base_url (e.g. http://localhost:8081/v1) or pass --port to transponder when running on the same host
    base_url: http://localhost:8081/v1
  - name: router
    type: openrouter      # https://openrouter.ai/api/v1 unless base_url is set
    api_key: ${OPENROUTER_API_KEY}
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

### Claude Code

Point Claude Code at transponder with a transponder API key:

```shell
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_AUTH_TOKEN=$TRANSPONDER_KEY claude
```

`/v1/messages` sends the request to Anthropic as it came, with only the model changed, so thinking, prompt caching and beta features keep working, and usage and cost are recorded for the key. A route can also send Claude Code's requests to Gemini, OpenAI or a local model. Those are translated, tools included, though features only Anthropic has, such as its server tools and images, are refused on those routes.

Fallback to the next target and the admin API are on the roadmap below.

### Limits

Budgets cap what a user, a team or a key spends in USD, per UTC day or month. Profiles, in the same format as the other mothership projects, cap tokens and requests:

Until sign-in with OIDC lands, the caller is the API key: each key counts as `user:<key name>` as well as `key:<key name>`, so name keys after the people or services that hold them.

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
- Counters live in memory and are rebuilt from the usage table at start, so a restart keeps this month's recorded spend. A limit can be passed only by calls already in flight whose real usage turns out larger than their reservation, and by kept reservations lost in a restart. With several replicas each keeps its own counters until the shared store lands.

## Roadmap

Each milestone ships as a release when it lands:

1. Released in v0.1.0. Skeleton: config, SQLite store, API keys, a test provider, `/v1/chat/completions` with usage headers
2. Released in v0.1.0. OpenAI and Ollama adapters, streaming, embeddings
3. Released in v0.2.0. Provider registry, split timeouts, `GET /v1/models`
4. Released in v0.2.2. Anthropic API in and out, the Gemini adapter, translation between dialects, Claude Code working
5. Released in v0.2.3. Budgets, token limits and request limits
6. Fallback with a cooldown, access per route (v0.2.4)
7. OIDC and roles, the admin API with OpenAPI and an audit log, Postgres and several replicas
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
