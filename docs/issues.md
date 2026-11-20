# Milestone 1 issues

Milestone: `v0.1.0 skeleton`. Goal: config, SQLite store, API keys with a first-start admin key, the echo provider, and `POST /v1/chat/completions` end to end with usage headers. Design reference: [docs/design.md](design.md).

The issues marked `claimed` are on the critical path and are being built by the maintainer now. The `good first issue` items are independent of them and open to anyone.

---

## 1. Load the bootstrap config from YAML and environment variables

Labels: `enhancement`, `help wanted`, `claimed`

**Why:** transponder needs a listen address, a store location and a few other settings before it can reach its database. The contract says these come from a YAML file passed with `--config`, and any key can be overridden by an environment variable.

**Proposed API:** package `internal/config`.

```go
type Bootstrap struct {
	ListenAddr string `yaml:"listen_addr"`
	Store      Store  `yaml:"store"`
	SecretsKey string `yaml:"secrets_key"`
	OTel       OTel   `yaml:"otel"`
	LogLevel   string `yaml:"log_level"`
}

type Store struct {
	SQLite   string `yaml:"sqlite"`
	Postgres string `yaml:"postgres"`
}

type OTel struct {
	Endpoint string `yaml:"endpoint"`
}

func Load(path string, environ []string) (*Bootstrap, error)
```

**Behavior:**
- `path == ""` returns defaults: `listen_addr: ":8080"`, `store.sqlite: "./transponder.db"`, `log_level: "info"`.
- `${VAR}` in the file is replaced from `environ` before parsing. A missing variable is an error that names it.
- `TRANSPONDER_LISTEN_ADDR` overrides `listen_addr`; `__` separates nested keys, so `TRANSPONDER_STORE__SQLITE` overrides `store.sqlite`. Names are matched case-insensitively.
- Unknown keys in the file are an error, so typos do not pass silently. `TRANSPONDER_` variables that do not name a key are ignored, because Kubernetes injects `TRANSPONDER_PORT` and similar for a service called transponder.
- Setting both `store.sqlite` and `store.postgres` is an error. `log_level` must be one of `debug`, `info`, `warn`, `error`.

**Tests:** defaults with no file; a full file; `${VAR}` expansion and the missing-variable error; env override of a top-level and a nested key; unknown key in the file; unknown env key; both stores set; bad log level.

---

## 2. SQLite store with embedded migrations

Labels: `enhancement`, `help wanted`, `claimed`

**Why:** API keys and usage records must survive a restart. The contract asks for SQLite by default through a pure-Go driver, with migrations that run at startup.

**Proposed API:** package `internal/store` for interfaces and types, `internal/store/sqlite` for the implementation using `modernc.org/sqlite`.

```go
func Open(ctx context.Context, path string) (*DB, error)
func (db *DB) Ping(ctx context.Context) error
func (db *DB) Close() error
```

Migrations live in `internal/store/sqlite/migrations/NNNN_name.sql`, embedded with `embed`.

**Behavior:**
- `Open` creates the file if needed, turns on WAL mode and foreign keys, and applies every migration not yet in `schema_migrations`, each in its own transaction.
- Opening an already migrated database applies nothing and succeeds.
- A failing migration leaves the database at the previous version and returns an error naming the file.
- The first migration creates `schema_migrations`, `api_keys` and `usage`, each with an `org_id` column that defaults to `default`, as the contract requires.

**Tests:** open a new file in `t.TempDir()`; open it twice; a broken migration in a test filesystem rolls back; `Ping` after `Close` fails.

---

## 3. API keys and the first-start admin key

Labels: `enhancement`, `help wanted`, `claimed`

**Why:** every call needs a caller. Before OIDC lands, API keys are the way in, and a fresh install must print one admin key so a new user can start without YAML.

**Proposed API:** package `internal/auth`.

```go
type Role string // "admin", "viewer", "member"

type Identity struct {
	User  string
	Teams []string
	Key   string
	Role  Role
}

func NewKey() (plaintext string, hash [32]byte, last4 string, err error)
func (a *Authenticator) Authenticate(r *http.Request) (Identity, error)
func EnsureAdmin(ctx context.Context, keys store.KeyStore, stderr io.Writer) error
```

**Behavior:**
- A key is `tp_` followed by 32 random bytes in unpadded base64url. Only its SHA-256 hash and last four characters are stored.
- `Authenticate` reads `Authorization: Bearer <key>` or `x-api-key: <key>`. A missing, unknown, disabled or expired key returns `ErrUnauthenticated`.
- `EnsureAdmin` creates an admin key named `admin` when the store holds no enabled admin key, and writes it once to `stderr` with a line telling the user to save it. Otherwise it does nothing.

**Tests:** key format and uniqueness; hash lookup; both headers; missing header; unknown, disabled and expired keys; `EnsureAdmin` on an empty store prints once, and a second call prints nothing.

---

## 4. Canonical chat types, the Provider interface and the echo provider

Labels: `enhancement`, `help wanted`, `claimed`

**Why:** every adapter converts to and from one set of types. The echo provider lets tests and the mothership end-to-end run work without real keys.

**Proposed API:** `internal/schema` (`ChatRequest`, `ChatResponse`, `Message`, `Usage`), `internal/provider` (the `Provider` interface from design section 5), `internal/provider/echo`.

**Behavior:**
- echo replies with the text of the last user message, prefixed with `echo: `.
- Usage is deterministic: input tokens are the characters of all messages divided by 4, rounded up; output tokens the same for the reply.
- `ChatStream` and `Embed` return `provider.ErrUnsupported` for now.

**Tests:** reply text, usage numbers, empty messages, the unsupported methods.

---

## 5. `POST /v1/chat/completions` through the echo provider with usage headers

Labels: `enhancement`, `help wanted`, `claimed`

**Why:** this is the first call that goes all the way through: auth, decode, provider, usage record, headers.

**Proposed API:** package `internal/server`, `func New(deps Deps) http.Handler`.

**Behavior:**
- Accepts the OpenAI request shape for non-streaming calls with string message content. `stream: true` returns 400 `unsupported` until milestone 2.
- Unauthenticated calls get 401 in the OpenAI error shape.
- The response is the OpenAI chat completion shape with `usage`, plus `X-Usage-Input-Tokens`, `X-Usage-Output-Tokens` and `X-Usage-Cost-USD` (`0.000000` for echo).
- One usage record is written per call.

**Tests:** end to end through `httptest.Server` with a SQLite store in a temp dir: success, 401, bad JSON, missing `model`, streaming rejected, usage record written.

---

## 6. Keep or generate `X-Request-ID`

Labels: `enhancement`, `good first issue`

**Why:** the contract says `X-Request-ID` is kept when the caller sends one and generated when not, so one request can be followed through logs across all four projects.

**Proposed API:** in `internal/server/requestid.go`:

```go
func RequestID(next http.Handler) http.Handler
func RequestIDFrom(ctx context.Context) string
```

**Behavior:**
- If the request has `X-Request-ID` with 1 to 128 printable ASCII characters, keep it. Otherwise generate 16 random bytes as lowercase hex.
- Put the value in the request context and set it on the response header before the handler runs.

**Tests:** kept when valid; replaced when empty, too long or containing control characters; generated values are 32 hex characters and differ between calls; `RequestIDFrom` on a bare context returns `""`.

**Where:** add `internal/server/requestid.go` and `internal/server/requestid_test.go`. No other files.

---

## 7. `/healthz` and `/readyz`

Labels: `enhancement`, `good first issue`

**Why:** the contract asks every project for `/healthz` (the process is up) and `/readyz` (its dependencies are reachable), so orchestrators can restart or hold traffic.

**Proposed API:** in `internal/server/health.go`:

```go
type Pinger interface {
	Ping(ctx context.Context) error
}

func Healthz() http.Handler
func Readyz(checks map[string]Pinger) http.Handler
```

**Behavior:**
- `GET /healthz` always returns 200 with `{"status":"ok"}`.
- `GET /readyz` pings every check with a 2 second timeout. All good: 200 and `{"status":"ok"}`. Any failure: 503 and `{"status":"unavailable","failed":["store"]}`, names sorted.
- Other methods return 405.

**Tests:** healthz; readyz with all checks passing; one failing; one that blocks past the timeout (use a check that waits on `ctx.Done()`, no sleeps); wrong method.

**Where:** add `internal/server/health.go` and `internal/server/health_test.go`. No other files.

---

## 8. Show the commit and build time in `--version`

Labels: `enhancement`, `good first issue`

**Why:** bug reports are easier when `transponder --version` says exactly which build someone runs.

**Proposed API:** in `cmd/transponder/main.go`:

```go
func versionString(version string, info *debug.BuildInfo, ok bool) string
```

`run` prints `versionString(version, debug.ReadBuildInfo())`.

**Behavior:**
- With build info that has `vcs.revision` and `vcs.time`: `transponder dev (1a2b3c4, 2026-11-04T10:00:00Z)` with the revision cut to 7 characters.
- Add `, modified` inside the brackets when `vcs.modified` is `true`.
- Without build info, or without VCS settings: `transponder dev`.
- A release build sets `version` with `-ldflags`, and the output uses that value instead of `dev`.

**Tests:** table test over hand-built `debug.BuildInfo` values: full info, modified, no VCS settings, `ok == false`, a release version.

**Where:** `cmd/transponder/main.go` and `cmd/transponder/main_test.go`.

---

## 9. JSON logger with a configurable level

Labels: `enhancement`, `good first issue`

**Why:** the contract says logs are JSON on stdout. Every package should get its logger from one place so the format and level are the same everywhere.

**Proposed API:** in `internal/telemetry/log.go`:

```go
func NewLogger(w io.Writer, level string) (*slog.Logger, error)
```

**Behavior:**
- Returns a `slog.JSONHandler` logger writing to `w`.
- `level` is `debug`, `info`, `warn` or `error`, case-insensitive. Empty means `info`. Anything else is an error naming the value.
- Every record has a `service` attribute set to `transponder`.

**Tests:** each level filters lower levels; empty level; bad level; output is valid JSON with `service`, `level`, `msg` and `time`.

**Where:** add `internal/telemetry/log.go` and `internal/telemetry/log_test.go`. No other files.
