# Milestone 3: foundation before the Anthropic API

> Steps use checkbox (`- [ ]`) syntax for tracking. Follow the rules in AGENTS.md.

**Goal:** make the base solid before the Anthropic API: one place to register a provider, separate timeouts for the first byte and the whole request, stricter stream reading, a model list for clients, and a usage column name that works on Postgres. Released as v0.2.0.

**Architecture:** a new `internal/provider/registry` package holds one entry per provider type (how to build it, its default base URL, whether it needs a base URL, whether it is free). Config validation, `buildRouter` and pricing all read it, so a new provider is a package plus one line there. Timeouts move from one `http.Client.Timeout` to `Transport.ResponseHeaderTimeout` for the attempt and a per-request context for the whole call.

**Tech Stack:** Go 1.26, standard library. No new dependencies.

**Spec:** [docs/design.md](../design.md), sections 5, 7, 12 and 13.

## Global Constraints

- Provider packages written by contributors (`litellm`, `cloudflare`) keep their public `New` functions; the registry calls them.
- Defaults: `attempt_timeout` 60s until the upstream answers with headers, `request_timeout` 10m for the whole call (design section 7). Both are set under `settings:` in the file.
- No network in unit tests, no bare `time.Sleep`.
- Coverage stays at 80% or more.
- Branch `feat/foundation`, one PR, merged with `gh pr merge <n> --squash --delete-branch --admin --subject "<title> (#<n>)" --body ""` once every required check is green.

## Review Focus

1. A config naming a type the registry does not know fails at start with the list of known types.
2. An upstream that never sends headers fails after `attempt_timeout` with a 502, while a slow but steady stream is not cut by it.
3. A stream that ends without `[DONE]` ends the caller's stream with an error event (#24).
4. `GET /v1/models` never lists wildcard routes and never calls an upstream.
5. The migration that renames `user` keeps every existing usage row.

---

### Task 1: Provider registry

**Files:** create `internal/provider/registry/registry.go`, `registry_test.go`; modify `internal/config/config.go`, `cmd/transponder/main.go`, `internal/pricing/pricing.go`, their tests; `CONTRIBUTING.md` or `AGENTS.md` where adding a provider is described.

**Interfaces:**
```go
type Settings struct {
	Name, BaseURL, APIKey string
	Client *http.Client
}
type Spec struct {
	Type         string
	NeedsBaseURL bool
	Free         bool
	New          func(Settings) provider.Provider
}
func Lookup(typ string) (Spec, bool)
func Types() []string // sorted
```

- [ ] `TestLookup`: every current type (`echo`, `openai`, `ollama`, `litellm`, `cloudflare`) is found and builds a provider with the given name; `cloudflare` has `NeedsBaseURL`; `echo` and `ollama` are `Free`; an unknown type is not found.
- [ ] `TestTypesSorted`.
- [ ] Config: the unknown-type error lists `Types()`; the cloudflare base URL check comes from `NeedsBaseURL`.
- [ ] `buildRouter` uses `Lookup` instead of its switch.
- [ ] Pricing: `Cost` takes a `free bool` from the caller instead of hard-coding `echo` and `ollama`; the server passes `Spec.Free` through `router.Target`.
- [ ] Update the "How" and "Where" of the open provider issues (#44 to #63) to say: a package plus one line in `registry.go`.
- [ ] Commit `feat(provider): register provider types in one place`.

### Task 2: OpenRouter (#44)

**Files:** create `internal/provider/openrouter/openrouter.go`, test; one line in the registry; README provider sentence; CHANGELOG.

- [ ] Tests as the issue says: default base URL `https://openrouter.ai/api/v1`, `Authorization: Bearer <key>` reaches an `httptest` server.
- [ ] Commit `feat(provider): add OpenRouter` with `Closes #44`.

### Task 3: Attempt and request timeouts

**Files:** modify `internal/config/config.go` (`Settings{AttemptTimeout, RequestTimeout time.Duration}` under `settings:`), `cmd/transponder/main.go`, `internal/server/server.go` (`Deps.RequestTimeout`), tests.

- [ ] Config: defaults 60s and 10m; `settings: {attempt_timeout: 5s, request_timeout: 1m}` parses; zero or negative values are errors.
- [ ] `TestAttemptTimeout` in `cmd/transponder`: an upstream that waits on its request context before sending headers gives 502 after a 100ms attempt timeout.
- [ ] `TestSlowStreamNotCut`: an upstream that sends headers at once and then one chunk per tick, past the attempt timeout, streams to the end.
- [ ] `TestRequestTimeout` in `internal/server`: with a short `RequestTimeout`, a stream that never ends is closed with the error event.
- [ ] Commit `feat: separate attempt and request timeouts`.

### Task 4: Stricter SSE reading (#24, #26)

**Files:** `internal/dialect/openai/stream.go`, `internal/server/stream.go`, tests.

- [ ] `SSEReader.Next` returns a new `ErrNoDone` when the body ends cleanly before `[DONE]`; the native stream path treats it like a broken stream (error event, estimated usage).
- [ ] A line or an event over 16 MiB (a package variable tests can lower) returns `ErrEventTooLarge` without reading the rest.
- [ ] Commit `fix(dialect/openai): treat a missing [DONE] as cut and cap event size` with `Closes #24` and `Closes #26`.

### Task 5: `GET /v1/models` (#39)

**Files:** `internal/router/router.go` (`func (r *Router) Models() []string`), `internal/server/models.go`, tests, README, CHANGELOG.

- [ ] `TestModels`: exact matches and explicit target models, sorted, no duplicates, no globs.
- [ ] `TestListModels`: the OpenAI list shape, 401 without a key, empty `data` with no config.
- [ ] Commit `feat(server): list route names at GET /v1/models` with `Closes #39`.

### Task 6: Rename the `user` usage column

**Files:** `internal/store/sqlite/migrations/0002_rename_user.sql`, `usage.go`, tests.

- [ ] `TestMigrationKeepsUsage`: a database at version 1 with one usage row opens at version 2 with the row intact and `RecentUsage` returning its user.
- [ ] Column becomes `user_id`, since `user` is reserved in Postgres.
- [ ] Commit `refactor(store): rename the usage user column to user_id`.

### Finish

- [ ] Final review of the branch, fix what it finds, CHANGELOG under Unreleased, AGENTS.md layout and the merge command line.
- [ ] PR, green checks, merge, then the release PR `chore(release): v0.2.0` and `gh release create v0.2.0 --target <merge sha>`.
