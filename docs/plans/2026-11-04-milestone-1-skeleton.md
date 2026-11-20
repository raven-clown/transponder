# Milestone 1 Skeleton Implementation Plan

> Execute task by task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a transponder binary that starts with no config, prints a one-time admin key, and answers `POST /v1/chat/completions` through the echo provider with usage headers and a usage record in SQLite.

**Architecture:** bootstrap config from YAML plus env; a SQLite store with embedded migrations; API key auth; canonical chat types with a `Provider` interface and the echo provider; an HTTP handler that decodes the OpenAI shape, calls the provider, records usage and writes headers. Routing, other providers and the admin API come in later milestones.

**Tech Stack:** Go 1.26 (`go 1.26` in go.mod, built with 1.27), `net/http` with method patterns, `log/slog`, `go.yaml.in/yaml/v3`, `modernc.org/sqlite`.

**Spec:** `docs/design.md` (sections 4, 5, 8, 9, 10, 12, 13). Issues #1 to #5.

## Global Constraints

- Module `github.com/raven-clown/transponder`; `go 1.26` in go.mod.
- Pure Go only, no cgo. SQLite through `modernc.org/sqlite`, driver name `sqlite`.
- Every runtime and usage table has `org_id TEXT NOT NULL DEFAULT 'default'`, and every query filters by it. `store.DefaultOrg = "default"`.
- Env overrides use the `TRANSPONDER_` prefix and `__` between nested keys.
- The first-start admin key goes to stderr, never to the JSON log on stdout.
- Usage headers: `X-Usage-Input-Tokens`, `X-Usage-Output-Tokens`, `X-Usage-Cost-USD` with six decimals.
- Money is int64 nano-USD.
- Doc comments on exported names only, short. No other comments unless the why is not obvious. No em-dashes. Follow the rules in AGENTS.md.
- Each task ends with one Conventional Commits commit, pushed to `main` right away. Commits that finish an issue say `Closes #N` in the body. AGENTS.md layout is updated in the same commit when a package lands.
- Local test command on this machine: `go test ./...` (no gcc, so no `-race`). Lint: `golangci-lint run ./...`. CI runs `-race`.

## Review Focus

1. Many requests at once writing usage to SQLite should all succeed, with no `database is locked` errors. Pinned in Task 6 (`TestChatCompletionsConcurrent`).
2. A huge request body should get 413 rather than being read into memory. Pinned in Task 6 (`TestChatCompletionsBodyTooLarge`).
3. `bearer` in any case and extra spaces around the key should still authenticate. Pinned in Task 4 (`TestAuthenticateHeaderForms`).
4. Restarting on an existing database should apply no migration twice and print no second admin key. Pinned in Task 2 (`TestOpenTwice`) and Task 4 (`TestEnsureAdminOnce`).
5. Non-ASCII text should be counted by characters, not bytes. Pinned in Task 5 (`TestEchoUsageCountsRunes`).

---

### Task 1: Bootstrap config (issue #1)

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `go.mod`, `go.sum`, `AGENTS.md`

**Interfaces:**
- Produces: `config.Bootstrap{ListenAddr string; Store config.Store; SecretsKey string; OTel config.OTel; LogLevel string}`, `config.Store{SQLite, Postgres string}`, `config.OTel{Endpoint string}`, `func Load(path string, environ []string) (*Bootstrap, error)`.

- [ ] **Step 1: Write the failing tests** in `config_test.go`, table-driven where it fits:
  - `TestLoadDefaults`: `Load("", nil)` gives `ListenAddr ":8080"`, `Store.SQLite "./transponder.db"`, `LogLevel "info"`.
  - `TestLoadFile`: a temp file with every key set round-trips into the struct.
  - `TestLoadExpandsVars`: `secrets_key: ${K}` with `environ = []string{"K=abc"}` gives `"abc"`; `${MISSING}` errors and the message contains `MISSING`.
  - `TestLoadEnvOverrides`: `TRANSPONDER_LISTEN_ADDR=:9000` and `transponder_store__sqlite=/tmp/x.db` override file values.
  - `TestLoadRejects`: unknown file key `listen_adr`, unknown env key `TRANSPONDER_NOPE`, both stores set, `log_level: loud`, missing file path. Each returns an error.
- [ ] **Step 2: Run** `go test ./internal/config/`. Expected: build failure, `Load` undefined.
- [ ] **Step 3: Implement `Load`.** Read the file, expand `${VAR}` with `os.Expand` against a map built from `environ`, tracking missing names. Unmarshal into `map[string]any`, apply each `TRANSPONDER_` variable by lowercasing the rest, splitting on `__` and setting the nested key, then marshal the map back and decode it strictly into a `Bootstrap` pre-filled with defaults (`yaml.Decoder.KnownFields(true)`). Then validate.
- [ ] **Step 4: Run** `go test ./internal/config/` and `golangci-lint run ./...`. Expected: PASS, 0 issues.
- [ ] **Step 5: Commit and push** `feat(config): load bootstrap config from YAML and environment` with `Closes #1`. Add `internal/config` to AGENTS.md as existing.

### Task 2: SQLite store and migrations (issue #2)

**Files:**
- Create: `internal/store/store.go`, `internal/store/sqlite/sqlite.go`, `internal/store/sqlite/migrate.go`, `internal/store/sqlite/migrations/0001_init.sql`, `internal/store/sqlite/sqlite_test.go`
- Modify: `go.mod`, `go.sum`, `AGENTS.md`

**Interfaces:**
- Produces: `store.DefaultOrg`, `store.ErrNotFound`, `store.ErrConflict`, `func sqlite.Open(ctx context.Context, path string) (*sqlite.DB, error)`, `(*DB).Ping(ctx) error`, `(*DB).Close() error`, and the unexported `migrate(ctx, db *sql.DB, fsys fs.FS) error` used by tests.

Migration `0001_init.sql` creates:
- `api_keys(org_id, name, hash BLOB UNIQUE, last4, role, teams TEXT DEFAULT '[]', disabled INTEGER DEFAULT 0, expires_at INTEGER NULL, version INTEGER DEFAULT 1, source TEXT DEFAULT 'api', created_at INTEGER, updated_at INTEGER, PRIMARY KEY(org_id, name))`
- `usage(id INTEGER PRIMARY KEY AUTOINCREMENT, org_id, time INTEGER, request_id, user, teams, key, project, route, provider, model, endpoint, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_nusd, estimated, status, latency_ms, fallbacks)` with index `(org_id, time)`.
Times are unix milliseconds in INTEGER columns so the Postgres schema can match later.

- [ ] **Step 1: Write the failing tests:**
  - `TestOpenNew`: `Open` on `t.TempDir()/t.db` succeeds; `schema_migrations` holds `1`; `Ping` is nil.
  - `TestOpenTwice`: open, close, open again; still one row in `schema_migrations`.
  - `TestMigrateRollsBack`: `migrate` with an `fstest.MapFS` holding a good `0001_a.sql` and a broken `0002_b.sql` returns an error containing `0002_b.sql`, and the max version is `1`.
  - `TestPingAfterClose`: `Ping` after `Close` returns an error.
- [ ] **Step 2: Run** `go test ./internal/store/...`. Expected: build failure.
- [ ] **Step 3: Implement.** DSN `file:<path>?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)`. `migrate` reads `NNNN_name.sql` files sorted by number, skips applied versions, runs each file and its `schema_migrations` insert in one transaction, and wraps errors with the file name.
- [ ] **Step 4: Run** tests and lint. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(store): add SQLite store with embedded migrations` (issue #2 stays open until Task 3).

### Task 3: Key and usage storage (issue #2)

**Files:**
- Modify: `internal/store/store.go`, `internal/store/sqlite/sqlite.go`
- Create: `internal/store/sqlite/keys.go`, `internal/store/sqlite/usage.go`, `internal/store/sqlite/keys_test.go`, `internal/store/sqlite/usage_test.go`

**Interfaces:**
- Produces in `internal/store`:
  - `type APIKey struct{ OrgID, Name string; Hash [32]byte; Last4, Role string; Teams []string; Disabled bool; ExpiresAt, CreatedAt time.Time }` (zero `ExpiresAt` means no expiry)
  - `type KeyStore interface{ CreateKey(ctx, APIKey) error; KeyByHash(ctx, [32]byte) (APIKey, error); HasAdminKey(ctx, orgID string, now time.Time) (bool, error) }`
  - `type UsageRecord struct{ OrgID string; Time time.Time; RequestID, User string; Teams []string; Key, Project, Route, Provider, Model, Endpoint string; InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens, CostNUSD int64; Estimated bool; Status int; Latency time.Duration; Fallbacks int }`
  - `type UsageStore interface{ RecordUsage(ctx, UsageRecord) error; RecentUsage(ctx, orgID string, limit int) ([]UsageRecord, error) }`
- `*sqlite.DB` implements both.

- [ ] **Step 1: Write the failing tests:**
  - `TestCreateAndFindKey`: create, then `KeyByHash` returns the same name, role, teams and last4.
  - `TestCreateKeyConflict`: same org and name twice gives `errors.Is(err, store.ErrConflict)`.
  - `TestKeyByHashNotFound`: unknown hash gives `store.ErrNotFound`.
  - `TestHasAdminKey`: false on empty; false with only a disabled admin; false with an admin expired before `now`; true with an enabled one; false for another org.
  - `TestRecordAndRecentUsage`: record three, `RecentUsage(ctx, "default", 2)` returns the newest two, newest first, with every field round-tripped; another org's record is not returned.
- [ ] **Step 2: Run** `go test ./internal/store/...`. Expected: build failure.
- [ ] **Step 3: Implement** with plain `database/sql`. Teams stored as a JSON array. Map the SQLite unique constraint error to `store.ErrConflict` by checking the driver error code for `SQLITE_CONSTRAINT_PRIMARYKEY` and `SQLITE_CONSTRAINT_UNIQUE`.
- [ ] **Step 4: Run** tests and lint. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(store): store API keys and usage records` with `Closes #2`. Update AGENTS.md.

### Task 4: API keys, authentication and the first admin key (issue #3)

**Files:**
- Create: `internal/auth/auth.go`, `internal/auth/keys.go`, `internal/auth/bootstrap.go`, and a `_test.go` for each
- Modify: `AGENTS.md`

**Interfaces:**
- Consumes: `store.KeyStore`, `store.APIKey`, `store.ErrNotFound`, `store.ErrConflict`, `store.DefaultOrg`.
- Produces:
  - `type Role string` with `RoleAdmin = "admin"`, `RoleViewer = "viewer"`, `RoleMember = "member"`
  - `type Identity struct{ OrgID, User string; Teams []string; Key string; Role Role }`
  - `var ErrUnauthenticated`
  - `func NewKey() (plaintext string, hash [32]byte, last4 string, err error)`
  - `func HashKey(plaintext string) [32]byte`
  - `func NewAuthenticator(keys store.KeyStore, now func() time.Time) *Authenticator`
  - `func (a *Authenticator) Authenticate(r *http.Request) (Identity, error)`
  - `func EnsureAdmin(ctx context.Context, keys store.KeyStore, now func() time.Time, stderr io.Writer) error`

- [ ] **Step 1: Write the failing tests** (use a small in-memory fake `KeyStore` in the test file, and one test against `sqlite` in `bootstrap_test.go`):
  - `TestNewKeyFormat`: prefix `tp_`, length `3 + 43`, base64url alphabet only, `last4` equals the last four characters, two calls differ, `HashKey(plaintext) == hash`.
  - `TestAuthenticateHeaderForms`: `Authorization: Bearer <k>`, `authorization: bearer <k>`, `Authorization: Bearer   <k>  `, and `x-api-key: <k>` all succeed with `User` equal to the key name, `Key` equal to the key name, and `OrgID "default"`.
  - `TestAuthenticateRejects`: no header, `Basic xyz`, unknown key, disabled key, expired key. All give `errors.Is(err, ErrUnauthenticated)`.
  - `TestEnsureAdminOnce`: against a real SQLite store, first call writes one line containing `tp_` to stderr and creates an admin key that authenticates as `RoleAdmin`; second call writes nothing.
  - `TestEnsureAdminNameTaken`: when a disabled key named `admin` exists, the new key is named `admin-<last4>`.
- [ ] **Step 2: Run** `go test ./internal/auth/`. Expected: build failure.
- [ ] **Step 3: Implement.** 32 bytes from `crypto/rand`, `base64.RawURLEncoding`. `Authenticate` trims spaces and matches the scheme case-insensitively, then looks up `HashKey`. The stderr text is exactly:

  ```
  transponder: created an admin API key. It is shown once, so save it now:
  <key>
  ```

- [ ] **Step 4: Run** tests and lint. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(auth): add API keys and the first-start admin key` with `Closes #3`. Update AGENTS.md.

### Task 5: Canonical types, Provider interface and echo (issue #4)

**Files:**
- Create: `internal/schema/schema.go`, `internal/provider/provider.go`, `internal/provider/echo/echo.go`, `internal/provider/echo/echo_test.go`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces in `internal/schema`:
  - `type Message struct{ Role, Content string }`
  - `type ChatRequest struct{ Model string; Messages []Message; MaxTokens *int; Temperature *float64; Stream bool }`
  - `type ChatResponse struct{ ID, Model string; Message Message; FinishReason string; Usage Usage }`
  - `type Usage struct{ InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64 }`
  - `type EventType string`, `type Event struct{ Type EventType; Text, FinishReason string; Usage *Usage }`, `type Stream interface{ Recv() (Event, error); Close() error }`
  - `type EmbedRequest struct{ Model string; Input []string }`, `type EmbedResponse struct{ Model string; Vectors [][]float32; Usage Usage }`
- Produces in `internal/provider`: `var ErrUnsupported`, and the `Provider` interface exactly as design section 5.
- Produces in `internal/provider/echo`: `func New(name string) *Echo` implementing `provider.Provider` with `Dialect() == "none"`.

- [ ] **Step 1: Write the failing tests:**
  - `TestEchoReply`: messages system `"be brief"`, user `"hi"`, assistant `"yo"`, user `"ping"` give content `"echo: ping"`, role `"assistant"`, finish reason `"stop"`, model equal to the request model.
  - `TestEchoUsage`: user `"abcdefgh"` (8 characters) gives input 2; reply `"echo: abcdefgh"` (14 characters) gives output 4.
  - `TestEchoUsageCountsRunes`: user `"สวัสดี"` (6 runes, 18 bytes) gives input 2.
  - `TestEchoNoUserMessage`: no user message gives `"echo: "`.
  - `TestEchoUnsupported`: `ChatStream` and `Embed` return `provider.ErrUnsupported`.
- [ ] **Step 2: Run** `go test ./internal/provider/...`. Expected: build failure.
- [ ] **Step 3: Implement.** Tokens are `ceil(utf8.RuneCountInString(s) / 4)` summed over every message for input. The response ID is `echo-` plus 8 random hex bytes.
- [ ] **Step 4: Run** tests and lint. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(provider): add canonical chat types and the echo provider` with `Closes #4`. Update AGENTS.md.

### Task 6: `POST /v1/chat/completions` (issue #5)

**Files:**
- Create: `internal/dialect/openai/chat.go`, `internal/dialect/openai/errors.go`, `internal/dialect/openai/chat_test.go`, `internal/server/server.go`, `internal/server/chat.go`, `internal/server/headers.go`, `internal/server/chat_test.go`, `internal/server/headers_test.go`
- Modify: `AGENTS.md`

**Interfaces:**
- Consumes: Tasks 3, 4 and 5.
- Produces in `internal/dialect/openai`:
  - `func DecodeChatRequest(r io.Reader) (*schema.ChatRequest, error)` returning `*RequestError{Param, Code, Message string}` for invalid input
  - `func EncodeChatResponse(resp *schema.ChatResponse, created time.Time) ([]byte, error)`
  - `func WriteError(w http.ResponseWriter, status int, errType, code, param, message string)` writing `{"error":{"message","type","param","code"}}`
- Produces in `internal/server`:
  - `type Deps struct{ Auth *auth.Authenticator; Provider provider.Provider; Usage store.UsageStore; Logger *slog.Logger; Now func() time.Time }`
  - `func New(d Deps) http.Handler`
  - `func formatUSD(nusd int64) string`, `const maxBodyBytes = 20 << 20`

- [ ] **Step 1: Write the failing tests.**
  - `openai/chat_test.go`, `TestDecodeChatRequest`: valid request decodes model, messages, `max_tokens`, `temperature`, `stream`. Errors: bad JSON (`code "invalid_json"`), missing model (`param "model"`), empty messages (`param "messages"`), array content (`code "unsupported"`, `param "messages"`).
  - `openai/chat_test.go`, `TestEncodeChatResponse`: JSON has `object "chat.completion"`, `choices[0].message.content`, `finish_reason`, and `usage.prompt_tokens`, `completion_tokens`, `total_tokens`.
  - `headers_test.go`, `TestFormatUSD`: `0` gives `"0.000000"`, `4542000` gives `"0.004542"`, `1_500_000_000` gives `"1.500000"`.
  - `chat_test.go` uses `httptest.NewServer(server.New(deps))` with a temp SQLite store, a created member key and the echo provider:
    - `TestChatCompletions`: 200, content `"echo: ping"`, headers `X-Usage-Input-Tokens`, `X-Usage-Output-Tokens` and `X-Usage-Cost-USD: 0.000000`, and `RecentUsage` holds one record with user, key, provider `echo`, endpoint `chat.completions`, status 200.
    - `TestChatCompletionsUnauthenticated`: 401, `error.code "invalid_api_key"`, no usage record.
    - `TestChatCompletionsBadRequest`: bad JSON, missing model and `stream: true` give 400; for streaming, `error.code "unsupported"`.
    - `TestChatCompletionsBodyTooLarge`: a body of `maxBodyBytes + 1` bytes gives 413.
    - `TestChatCompletionsConcurrent`: 20 parallel requests all return 200, and `RecentUsage(..., 100)` holds 20 records.
    - `TestChatCompletionsWrongMethod`: `GET` gives 405.
- [ ] **Step 2: Run** `go test ./internal/dialect/... ./internal/server/`. Expected: build failure.
- [ ] **Step 3: Implement.** Register `POST /v1/chat/completions` on an `http.ServeMux`. Wrap the body with `http.MaxBytesReader` and map `*http.MaxBytesError` to 413. A provider error gives 502 with type `api_error`, and the usage record is still written with status 502. A failure to write the usage record is logged at error level and does not change the response. `RequestID` comes from the incoming `X-Request-ID` header for now; issue #6 replaces that with the middleware.
- [ ] **Step 4: Run** `go test ./...` and lint. Expected: PASS.
- [ ] **Step 5: Commit and push** `feat(server): serve chat completions through the echo provider`. Update AGENTS.md. Issue #5 stays open until Task 7.

### Task 7: Wire the binary (issue #5)

**Files:**
- Modify: `cmd/transponder/main.go`, `cmd/transponder/main_test.go`, `CHANGELOG.md`, `README.md` (status line), `AGENTS.md`

**Interfaces:**
- Consumes: everything above.
- Produces: `func serve(ctx context.Context, cfg *config.Bootstrap, ln net.Listener, stdout, stderr io.Writer) error` and `run(args []string, stdout, stderr io.Writer) int`, which adds `--config` and serves until SIGINT or SIGTERM.

- [ ] **Step 1: Update the tests.** Replace the `no gateway yet` case with `missing config file` (`--config` pointing at a path that does not exist gives exit code 1, and stderr mentions the path). Add `TestServeEndToEnd`: listen on `127.0.0.1:0`, run `serve` in a goroutine with SQLite in a temp dir and stderr as an `io.Pipe`, read lines until one starts with `tp_`, `POST` a chat completion with that key and expect 200 with `X-Usage-Cost-USD`, then cancel the context and expect `serve` to return nil within 5 seconds.
- [ ] **Step 2: Run** `go test ./cmd/...`. Expected: FAIL.
- [ ] **Step 3: Implement.** If `store.postgres` is set, return the error `postgres store is not supported yet`. Otherwise open SQLite, call `auth.EnsureAdmin`, build `server.New` with `echo.New("echo")` and a `slog.JSONHandler` on stdout, and run `http.Server{ReadHeaderTimeout: 10 * time.Second}`. On context cancel, call `Shutdown` with a 10 second timeout, then close the store. `run` uses `signal.NotifyContext` and `net.Listen("tcp", cfg.ListenAddr)`.
- [ ] **Step 4: Run** `go test ./...`, `golangci-lint run ./...`, and the coverage check `go test -coverprofile=cover.out ./... && go run github.com/vladopajic/go-test-coverage/v2@v2.21.0 --config=.testcoverage.yml`. Expected: PASS, coverage 80% or more.
- [ ] **Step 5: Smoke test by hand.** Run `go run ./cmd/transponder` in a temp dir and copy the key from stderr. Then `curl -i -X POST localhost:8080/v1/chat/completions -H "Authorization: Bearer <key>" -d '{"model":"x","messages":[{"role":"user","content":"ping"}]}'` should show 200 and the three usage headers.
- [ ] **Step 6: Commit and push** `feat: run the gateway with a first-start admin key` with `Closes #5`. In the same commit: CHANGELOG under Unreleased gets "Chat completions through a built-in echo provider, API keys, SQLite storage", the README status line says milestone 1 is done, and AGENTS.md lists the commands to run the binary.
