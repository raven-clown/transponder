# transponder

[![ci](https://github.com/raven-clown/transponder/actions/workflows/ci.yml/badge.svg)](https://github.com/raven-clown/transponder/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/raven-clown/transponder)](https://github.com/raven-clown/transponder/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/raven-clown/transponder.svg)](https://pkg.go.dev/github.com/raven-clown/transponder)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

An LLM gateway that knows who is spending what. One Go binary in front of Anthropic, OpenAI, Gemini and Ollama, with fallback across providers, cost per user from your own identity provider, and budgets that hold across replicas. Every feature is free.

> **Status: early, [v0.1.1](https://github.com/raven-clown/transponder/releases/tag/v0.1.1).** Milestones 1 and 2 are released: the binary starts with no config, prints an admin key, and serves chat completions (plain and streamed) and embeddings from OpenAI, Ollama or any OpenAI-compatible server, with token counts and cost on every response. The Anthropic API comes next. The design is settled in [docs/design.md](docs/design.md) and the work is tracked in [issues](https://github.com/raven-clown/transponder/issues). Star or watch the repository to follow along, and see [How to help](#how-to-help) if you want to build it with us.

## What it does

- Speaks the OpenAI API (`/v1/chat/completions`, `/v1/embeddings`) and the Anthropic API (`/v1/messages`), so the OpenAI and Anthropic SDKs, Claude Code and most tools work with only a base URL change.
- Sends each call to Anthropic, OpenAI, Gemini or Ollama, and to any OpenAI-compatible server such as vLLM or LM Studio.
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

It is also small on purpose. One provider is one Go package, a few hundred lines with recorded test fixtures, so adding a backend is a weekend pull request.

## Part of mothership

transponder works on its own. It is also one of four projects that fit together:

- [airlock](https://github.com/raven-clown/airlock): an MCP gateway
- [rosetta](https://github.com/raven-clown/rosetta): serve one agent over MCP, A2A, ACP and AG-UI
- [logbook](https://github.com/raven-clown/logbook): shared agent memory as an MCP server
- transponder: the model gateway they call

The same user token flows through all four, so an agent's tool calls, memories and model spend line up under one person. The shared rules are in the [mothership contract](https://github.com/raven-clown/mothership/blob/main/CONTRACT.md).

## Install

With Go 1.26 or later:

```shell
go install github.com/raven-clown/transponder/cmd/transponder@latest
transponder -version
```

Prebuilt binaries for Linux, macOS and Windows and a container image are planned.

## Try it

```shell
transponder
# first start: SQLite in the working directory, and a one-time admin key on stderr
```

With no config every model is answered by `echo`, a built-in test provider. To use real models, write a config file:

```yaml
# transponder.yaml
providers:
  - name: openai
    type: openai
    api_key: ${OPENAI_API_KEY}
  - name: local
    type: ollama          # http://localhost:11434/v1 unless base_url is set
routes:
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
curl localhost:8080/v1/chat/completions   -H "Authorization: Bearer $TRANSPONDER_KEY"   -d '{"model": "fast", "messages": [{"role": "user", "content": "hi"}]}' -i
# X-Usage-Input-Tokens, X-Usage-Output-Tokens and X-Usage-Cost-USD come back with the answer
```

Routes are tried in order and the first match wins. Fallback to the next target, the admin API and the Anthropic API are on the roadmap below; these commands describe where it is going:

```shell
curl -X POST localhost:8080/admin/v1/providers   -H "Authorization: Bearer $ADMIN_KEY"   -d '{"name": "anthropic", "type": "anthropic", "settings": {"api_key": "sk-ant-..."}}'
```

```shell
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_AUTH_TOKEN=$MY_KEY claude
```

## Roadmap

Each milestone ships as a release when it lands:

1. Released in v0.1.0. Skeleton: config, SQLite store, API keys, a test provider, `/v1/chat/completions` with usage headers
2. Released in v0.1.0. OpenAI and Ollama adapters, streaming, embeddings
3. Anthropic API in and out, translation between the two, Claude Code working
4. Fallback, cooldown, access per route, the Gemini adapter
5. Budgets, `/v1/usage`, Postgres
6. Admin API with OpenAPI, audit log, live reload, encrypted provider keys
7. Metrics, tracing, a five-minute demo, release binaries and container image

After that: the OpenAI Responses API, per-user request rate limits, organisations, Bedrock and Vertex, and caching.

## How to help

- Pick an issue labeled [`good first issue`](https://github.com/raven-clown/transponder/labels/good%20first%20issue). Each one says which files to change and which tests to add.
- Adding a provider is the easiest way to add something big. Look for the [`adapter`](https://github.com/raven-clown/transponder/labels/adapter) label.
- Read [CONTRIBUTING.md](CONTRIBUTING.md) first, and leave a comment on the issue before you start.
- Found a bug, or want something new? Open an issue with one of the [issue forms](https://github.com/raven-clown/transponder/issues/new/choose). Each one says what to include.
- Pull requests come with a template that lists what reviewers check first in this project, so you know what to cover before you open one.
- Ideas and questions are welcome too.

## License

[Apache-2.0](LICENSE)
