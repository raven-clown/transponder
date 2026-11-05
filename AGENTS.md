# AGENTS.md

Instructions for coding agents working in this repository. Keep this file current: when a command, a package layout or a rule changes, update it in the same commit.

## Project

transponder is an LLM gateway in one Go binary. It serves OpenAI-compatible `/v1/chat/completions` and `/v1/embeddings` and Anthropic-compatible `/v1/messages` in front of Anthropic, OpenAI, Gemini and Ollama, falls back across providers, records usage and cost per user, and enforces budgets. Its configuration lives in SQL and is managed through a JSON admin API under `/admin/v1/`. It is used by teams that want per-user model spend and limits from their existing OIDC provider, and by the other mothership projects for chat and embeddings. The design is in `docs/design.md`.

It is one of four projects described in the shared contract at https://github.com/raven-clown/mothership/blob/main/CONTRACT.md. Follow the contract. A change to how the projects connect goes to the contract first.

## Commands

```shell
go build ./...
go test -race ./...
golangci-lint run ./...
make check   # lint plus tests with the 80% coverage check
```

Run the gateway locally (SQLite in the working directory, admin key on stderr):

```shell
go run ./cmd/transponder
go run ./cmd/transponder --config transponder.yaml
```

On Windows without gcc, `-race` is not available: run `go test ./...` locally and let CI run the race detector.

## Layout

Planned in `docs/design.md` section 4. Packages marked (planned) do not exist yet; update this list as packages land.

- `cmd/transponder`: main, flags, bootstrap config, server start
- `internal/config`: bootstrap YAML, env overrides, and the providers and routes declared in the file
- `internal/auth`: OIDC JWT verification, API keys, identity and roles
- `internal/server`: HTTP mux, middleware, gateway handlers
- `internal/admin` (planned): `/admin/v1` handlers, pagination, audit writes
- `internal/schema`: canonical request, response, stream and usage types
- `internal/dialect/openai`, `internal/dialect/anthropic` (anthropic planned): wire formats and usage parsing
- `internal/router`: routes and target choice; access, fallback and cooldown are planned
- `internal/provider`, `internal/provider/<name>`: the Provider interface, the shared `Error` type and one adapter per backend (`echo`, `openai`, `ollama` so far); `providertest` (planned) is the conformance suite
- `internal/store`, `internal/store/sqlite`: store interfaces, SQLite with embedded migrations in `internal/store/sqlite/migrations`
- `internal/pricing`: the embedded price table in `prices.yaml` and cost math in nano-USD
- `internal/budget`, `internal/runtime`, `internal/secrets`, `internal/telemetry` (planned): budgets, live config, sealed secrets, logs and metrics
- `internal/testutil` (planned): shared fakes
- `api/openapi.yaml` (planned): the admin API description

## Rules

- Follow CONTRIBUTING.md: tests with every change, a failing test first for a bug fix, table-driven tests, no network in unit tests, no bare `time.Sleep`.
- Commit after every change, with a Conventional Commits message, and push it.
- Never commit straight to `main`. Start each piece of work on its own branch off `main`, named `feat/<topic>`, `fix/<topic>`, `ci/<topic>` or `docs/<topic>`, and push it after every commit.
- When the work is done, open a pull request with a Conventional Commits title and a plain English description. Wait until every required check is green: the three test jobs, coverage, lint, analyze, dependency review and pr title. A macOS job cancelled while waiting for a runner is not a failure: rerun it with `gh run rerun <id> --failed`.
- Then merge it with `gh pr merge <n> --squash --delete-branch --admin` and delete the local branch with `git switch main && git pull && git branch -D <branch>`. Never merge a red pull request, and never force-push `main`.
- Never mention AI, assistants or code generation in code, comments, docs, commit messages, pull requests or issues.
- Never add `Co-Authored-By`, `Generated with` or similar trailers.
- No em-dashes. Keep comments short and about why.
- Every feature ships in the free build. Never add a license check or a paid-only code path.
- Update CHANGELOG.md under Unreleased for anything users will notice, and update README.md when usage changes.
