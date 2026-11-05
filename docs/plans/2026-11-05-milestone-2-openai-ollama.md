# Milestone 2: OpenAI and Ollama, streaming, embeddings

> Steps use checkbox (`- [ ]`) syntax for tracking. Follow the rules in AGENTS.md.

**Goal:** an OpenAI SDK pointed at transponder gets chat completions (plain and streamed) and embeddings from OpenAI, Ollama or the echo provider, with real token counts and USD cost in headers, trailers and the usage table.

**Architecture:** providers and routes are read from the YAML file into memory (they move to SQL in milestone 6). A minimal router picks the first matching route and its first target (fallback comes in milestone 4). When the caller and the provider both speak OpenAI, the raw body is forwarded with only `model` rewritten (native passthrough) and usage is parsed from the upstream response. Otherwise, as for echo, the call goes through the canonical types and the OpenAI dialect package encodes the result.

**Tech Stack:** Go 1.26, `net/http`, `go.yaml.in/yaml/v3`, `modernc.org/sqlite`. No new dependencies.

**Spec:** [docs/design.md](../design.md), sections 5, 6, 7, 10 and 13.

## Global Constraints

- Module `github.com/raven-clown/transponder`, everything under `internal/`.
- No network in unit tests: upstreams are `httptest.Server` fakes. No bare `time.Sleep`.
- Money is int64 nano-USD. Prices are USD per million tokens in `internal/pricing/prices.yaml`, keyed `type/model`.
- Usage headers: `X-Usage-Input-Tokens`, `X-Usage-Output-Tokens`, `X-Usage-Cost-USD` on every response; trailers with the same names on streams.
- Canonical `Usage.InputTokens` excludes cache reads. For OpenAI, `InputTokens = prompt_tokens - cached_tokens` and `CacheReadTokens = cached_tokens`.
- Upstream errors: a non-retryable 4xx from a same-dialect upstream is passed to the caller with its status and body. Everything else that fails upstream is 502 `upstream_error`.
- Retryable statuses (design section 7): 401, 403, 408, 429, every 5xx, 529, and connection errors.
- Request bodies are capped at 20 MiB, upstream error bodies are read up to 1 MiB.
- Coverage stays at 80% or more (`make check`).
- Work on branch `milestone-2`; push after every commit; merge to main through a pull request once CI is green.

## Review Focus

1. A caller field transponder does not know (`logprobs`, `response_format`, `tools`) must reach OpenAI unchanged, and an upstream field (`system_fingerprint`) must reach the caller. Pinned in Task 8.
2. A streaming caller that did not ask for `stream_options.include_usage` must not see the usage-only chunk, but transponder still records the usage. Pinned in Task 9.
3. An upstream that dies mid-stream must end the caller's stream with an error event, not a hang or a silent cut. Pinned in Task 9.
4. A streaming upstream that fails before its first event must give the caller a normal JSON error with a status code, not a 200 with an empty stream. Pinned in Task 9.
5. An OpenAI-compatible server that returns no `usage` must still produce a usage record, marked `estimated`, with counts from characters divided by 4. Pinned in Task 8.

---

### Task 1: Providers and routes in the config file

**Files:** modify `internal/config/config.go`, `internal/config/config_test.go`.

**Interfaces:**
- Produces:
  ```go
  type Provider struct {
  	Name    string `yaml:"name"`
  	Type    string `yaml:"type"`     // echo, openai, ollama
  	BaseURL string `yaml:"base_url"`
  	APIKey  string `yaml:"api_key"`
  }
  type Route struct {
  	Match   string   `yaml:"match"`
  	Targets []Target `yaml:"targets"`
  }
  type Target struct {
  	Provider string `yaml:"provider"`
  	Model    string `yaml:"model"`
  }
  // Bootstrap gains: Providers []Provider `yaml:"providers"`; Routes []Route `yaml:"routes"`
  ```

- [ ] Write `TestLoadProvidersAndRoutes`: a file with two providers (`openai` with `api_key: ${OPENAI_KEY}`, `ollama` with no key) and two routes (`gpt-*` to openai, `local` to ollama with `model: llama3.2`) loads into the expected structs, key expanded.
- [ ] Add `TestLoadRejects` cases: provider with empty name; duplicate provider name; unknown type `bedrock`; route with empty match; route with no targets; target naming a provider that is not declared. Each error message names the offending item.
- [ ] Add a case to `TestLoadIgnoresUnknownEnv`: `TRANSPONDER_PROVIDERS=x` is ignored, so lists are never env leaves.
- [ ] Run `go test ./internal/config/`. Expected: FAIL on the new tests.
- [ ] Implement: add the types and fields, skip slice fields in `leafPaths`, extend `validate`.
- [ ] Run `go test ./internal/config/`. Expected: ok.
- [ ] Commit `feat(config): declare providers and routes in the config file`.

### Task 2: Price table and cost math

**Files:** create `internal/pricing/pricing.go`, `internal/pricing/prices.yaml`, `internal/pricing/pricing_test.go`.

**Interfaces:**
- Produces:
  ```go
  type Price struct{ Input, Output, CacheRead, CacheWrite int64 } // nano-USD per million tokens
  type Table struct{ /* unexported */ }
  func Default() *Table                       // the embedded table; panics only if the embedded file is broken
  func Parse(data []byte) (*Table, error)
  func (t *Table) Cost(typ, model string, u schema.Usage) (nusd int64, priced bool)
  ```

`prices.yaml` holds USD strings per million tokens, read from developers.openai.com/api/docs/pricing on 2026-11-05: `openai/gpt-5` 1.25 / cache read 0.125 / output 10; `gpt-5-mini` 0.25 / 0.025 / 2; `gpt-5-nano` 0.05 / 0.005 / 0.40; `gpt-5.1` 1.25 / 0.125 / 10; `gpt-5.2` 1.75 / 0.175 / 14; `gpt-5.4` 2.50 / 0.25 / 15; `gpt-5.4-mini` 0.75 / 0.075 / 4.50; `gpt-5.4-nano` 0.20 / 0.02 / 1.25; `gpt-5.5` 5 / 0.50 / 30; `gpt-4.1` 2 / 0.50 / 8; `gpt-4.1-mini` 0.40 / 0.10 / 1.60; `gpt-4o` 2.50 / 1.25 / 10; `gpt-4o-mini` 0.15 / 0.075 / 0.60; `text-embedding-3-small` 0.02; `text-embedding-3-large` 0.13. A header comment gives the source and date.

Cost is `round((in*Input + out*Output + cr*CacheRead + cw*CacheWrite) / 1e6)` in integer math. Types `echo` and `ollama` cost 0 and count as priced. A missing entry returns `0, false`.

- [ ] Write `TestCost` (table): `openai/gpt-4o-mini` with 1000 in and 500 out is 450_000 nUSD; `openai/gpt-5` with 1000 in, 200 cache read, 100 out is 2_275_000; `openai/text-embedding-3-small` with 1_000_000 in is 20_000_000; `ollama/anything` is 0 priced; `echo/m` is 0 priced; `openai/unknown` is 0 not priced.
- [ ] Write `TestParseRejects`: a negative price, a non-number, an unknown field.
- [ ] Write `TestDefaultLoads`: `Default()` has an entry for every model listed above.
- [ ] Run `go test ./internal/pricing/`. Expected: FAIL, package missing.
- [ ] Implement with `embed`, parsing each USD string with `strconv.ParseFloat` and `math.Round(f * 1e9)`.
- [ ] Run `go test ./internal/pricing/`. Expected: ok.
- [ ] Commit `feat(pricing): embed a price table and compute cost in nano-USD`.

### Task 3: Provider errors and native calls

**Files:** modify `internal/provider/provider.go`; create `internal/provider/provider_test.go`.

**Interfaces:**
- Produces:
  ```go
  type Error struct {
  	Provider  string
  	Status    int    // 0 for connection errors
  	Retryable bool
  	Body      []byte // upstream error body, at most 1 MiB
  	Err       error
  }
  func (e *Error) Error() string
  func (e *Error) Unwrap() error
  func RetryableStatus(status int) bool

  type NativeCall struct {
  	Path   string      // "/chat/completions" or "/embeddings"
  	Body   []byte      // model already rewritten
  	Header http.Header // X-Request-ID, traceparent, tracestate to send upstream
  }
  type Native interface {
  	Native(ctx context.Context, call *NativeCall) (*http.Response, error)
  }
  ```

- [ ] Write `TestRetryableStatus` (table): 400, 404, 422 false; 401, 403, 408, 429, 500, 503, 529 true; 200 false.
- [ ] Write `TestErrorMessage`: the message names the provider and status, and `errors.Is` reaches `Err`.
- [ ] Run, expect FAIL; implement; run, expect ok.
- [ ] Commit `feat(provider): add the shared error type and native calls`.

### Task 4: OpenAI adapter

**Files:** create `internal/provider/openai/openai.go`, `internal/provider/openai/openai_test.go`.

**Interfaces:**
- Consumes: `provider.Error`, `provider.NativeCall`, `provider.RetryableStatus` (Task 3).
- Produces:
  ```go
  type Options struct {
  	Name    string
  	BaseURL string // default "https://api.openai.com/v1"
  	APIKey  string // empty sends no Authorization header
  	Client  *http.Client // default http.DefaultClient
  }
  func New(o Options) *Adapter
  func (a *Adapter) BaseURL() string
  // Name, Dialect() == "openai", Native; Chat, ChatStream, Embed return provider.ErrUnsupported until milestone 3
  ```

- [ ] Write `TestNativeForwards`: against an `httptest.Server`, `Native` posts to `BaseURL + Path` with the exact body, `Authorization: Bearer k`, `Content-Type: application/json`, and the `X-Request-ID` and `traceparent` from the call. The 200 response comes back unread.
- [ ] Write `TestNativeNoKey`: with an empty key no `Authorization` header is sent.
- [ ] Write `TestNativeErrors` (table): upstream 400, 401, 429, 500, 529 give a `*provider.Error` with that status, the body, and `Retryable` from `RetryableStatus`; the response body is closed.
- [ ] Write `TestNativeConnectionError`: a closed server gives `*provider.Error{Status: 0, Retryable: true}`.
- [ ] Write `TestBaseURLDefault` and `TestCanonicalUnsupported`.
- [ ] Run, expect FAIL; implement with `http.NewRequestWithContext`, trimming a trailing `/` from the base URL; run, expect ok.
- [ ] Commit `feat(provider/openai): forward OpenAI calls natively`.

### Task 5: Ollama preset

**Files:** create `internal/provider/ollama/ollama.go`, `internal/provider/ollama/ollama_test.go`.

**Interfaces:**
- Consumes: `openai.New`, `openai.Options` (Task 4).
- Produces: `func New(name, baseURL string, client *http.Client) *openai.Adapter`, default base URL `http://localhost:11434/v1`, no key.

- [ ] Write `TestDefaults` (empty base URL gives the default) and `TestForwards` (a call through `Native` reaches an `httptest.Server` with no `Authorization`).
- [ ] Run, expect FAIL; implement; run, expect ok.
- [ ] Commit `feat(provider/ollama): add the Ollama preset`.

### Task 6: Minimal router

**Files:** create `internal/router/router.go`, `internal/router/router_test.go`.

**Interfaces:**
- Produces:
  ```go
  type Target struct {
  	Provider provider.Provider
  	Type     string // provider type, for pricing
  	Model    string // empty keeps the requested model
  }
  type Route struct {
  	Match   string
  	Targets []Target
  }
  type Resolved struct {
  	Route    string // the route's Match, recorded as the usage route
  	Provider provider.Provider
  	Type     string
  	Model    string // the model to send upstream
  }
  var ErrNoRoute = errors.New("router: no route matches the model")
  func New(routes []Route) *Router
  func (r *Router) Resolve(model string) (Resolved, error)
  ```

Matching: exact name, else `path.Match` glob, first route wins, first target only.

- [ ] Write `TestResolve` (table): exact match; glob `gpt-*`; the first of two matching routes wins; target with `Model` rewrites the model; target without keeps it; no match gives `ErrNoRoute`; a bad glob pattern never matches.
- [ ] Run, expect FAIL; implement; run, expect ok.
- [ ] Commit `feat(router): resolve a model to its first route target`.

### Task 7: OpenAI dialect for passthrough and streams

**Files:** create `internal/dialect/openai/native.go`, `native_test.go`, `stream.go`, `stream_test.go`, `embed.go`, `embed_test.go`.

**Interfaces:**
- Produces:
  ```go
  type Peek struct {
  	Model        string
  	Stream       bool
  	IncludeUsage bool
  }
  func PeekRequest(body []byte) (Peek, error)                      // *RequestError for invalid_json, missing_field model
  func RewriteRequest(body []byte, model string, forceUsage bool) ([]byte, error)
  func ParseUsage(body []byte) (schema.Usage, bool)                 // false when no usage object
  func ParseChatRequest(body []byte) (*schema.ChatRequest, error)   // DecodeChatRequest over bytes

  type SSEReader struct{ /* bufio.Reader */ }
  func NewSSEReader(r io.Reader) *SSEReader
  func (s *SSEReader) Next() (data []byte, err error)              // one data payload; io.EOF after [DONE] or end
  func ChunkUsage(data []byte) (u schema.Usage, ok bool, usageOnly bool)
  func EncodeChunk(id, model string, created time.Time, ev schema.Event) ([]byte, error) // a full "data: ...\n\n" frame, nil for events with nothing to send
  func EncodeStreamError(code, message string) []byte
  const DoneFrame = "data: [DONE]\n\n"

  func ParseEmbedRequest(body []byte) (*schema.EmbedRequest, error) // input as string or []string
  func EncodeEmbedResponse(resp *schema.EmbedResponse) ([]byte, error)
  ```

- [ ] Write `TestPeekRequest` (table): model and stream read; `stream_options.include_usage` read; invalid JSON and missing model give `*RequestError` codes `invalid_json` and `missing_field`.
- [ ] Write `TestRewriteRequest`: unknown fields (`logprobs`, nested `tools`) survive byte-for-byte as JSON values; `model` replaced; `forceUsage` adds `stream_options.include_usage: true` and keeps other `stream_options` keys.
- [ ] Write `TestParseUsage` (table): chat usage with `prompt_tokens_details.cached_tokens` 200 of 1000 gives Input 800, CacheRead 200; embeddings usage with only `prompt_tokens`; no usage gives false.
- [ ] Write `TestSSEReader`: frames with `data:` and `data: `, comment lines, blank lines, CRLF, and `[DONE]` give the payloads then `io.EOF`; a stream cut mid-frame gives `io.ErrUnexpectedEOF`.
- [ ] Write `TestChunkUsage`: a chunk with `choices: []` and usage is usage-only; a chunk with content and no usage is not.
- [ ] Write `TestEncodeChunk`: start event gives a role delta; text gives a content delta; finish gives `finish_reason`; usage gives `choices: []` with the usage block; all frames are `chat.completion.chunk` with the id, model and created time.
- [ ] Write `TestEmbedRoundTrip`: string and array input decode; missing input is `missing_field`; the response encodes `object: list`, `data[i].embedding`, `index`, and usage.
- [ ] Run, expect FAIL; implement; decode bodies into `map[string]json.RawMessage` for the rewrite; run, expect ok.
- [ ] Commit `feat(dialect/openai): peek, rewrite, usage and SSE helpers`.

### Task 8: Chat completions through the router

**Files:** modify `internal/server/server.go`, `chat.go`, `headers.go`, `chat_test.go`; create `internal/server/upstream.go`, `internal/server/fake_test.go`; modify `cmd/transponder/main.go` only enough to compile (Task 11 wires config).

**Interfaces:**
- Consumes: Tasks 2 to 7.
- Produces: `Deps{Auth *auth.Authenticator; Router *router.Router; Prices *pricing.Table; Usage store.UsageStore; Logger *slog.Logger; Now func() time.Time}`.

Flow: authenticate, read the body under the cap, `PeekRequest`, `Resolve`. When the target's dialect is `openai` and it implements `provider.Native`, rewrite and forward; on 200 copy the upstream body to the caller and parse usage from it. Otherwise parse the canonical request and call `Chat`. Price the usage, record it, set the usage headers on every response, error responses included (0 tokens, cost `0.000000`).

- [ ] Write `TestChatNativePassthrough`: a fake OpenAI server asserts the body it got has `model: "gpt-4o-mini"` (route `fast` rewrites it) and still has `logprobs: true` and the `tools` array; it answers with `system_fingerprint` and usage 1000 in / 500 out. The caller sees `system_fingerprint`, `X-Usage-Cost-USD: 0.000450`, and one usage record with route `fast`, provider name, model `gpt-4o-mini` and cost 450_000.
- [ ] Write `TestChatNativeNoUsage`: the upstream sends no usage; the record is `Estimated` with counts from characters divided by 4.
- [ ] Write `TestChatUpstreamErrors` (table): upstream 400 reaches the caller as 400 with the upstream body; upstream 500, 401 and a closed server give 502 `upstream_error`; each writes a usage record with the status.
- [ ] Write `TestChatModelNotFound`: 404 `model_not_found`.
- [ ] Keep the existing echo tests green through a router with `*` to echo, and add a check that a 401 response carries `X-Usage-Cost-USD: 0.000000`.
- [ ] Run `go test ./internal/server/`. Expected: FAIL to compile, `Deps.Router` missing.
- [ ] Implement.
- [ ] Run `go test ./...`. Expected: ok.
- [ ] Commit `feat(server): route chat completions and pass OpenAI calls through`.

### Task 9: Streaming chat completions

**Files:** create `internal/server/stream.go`, `internal/server/stream_test.go`; modify `chat.go`; modify `internal/schema/schema.go` and `internal/provider/echo/echo.go` (+ test) for event types and an echo stream.

**Interfaces:**
- Produces: `schema.EventStart`, `EventText`, `EventFinish`, `EventUsage` (`EventType` constants `"start"`, `"text"`, `"finish"`, `"usage"`). Echo's `ChatStream` sends start, one text event per word of the reply (spaces kept), finish `stop`, usage.

Native stream: force `include_usage` upstream, read the first SSE payload before writing anything; a failure before it is a normal error response. Then declare the trailers, write `200 text/event-stream`, and copy frames, flushing after each. Drop the usage-only chunk when the caller did not ask for it. At the end write `DoneFrame`, set the trailers, record usage. A read error after the first frame writes `EncodeStreamError("upstream_error", ...)` and ends the stream; the record keeps status 200 and the usage seen so far, estimated.

- [ ] Write `TestEchoStream` in the echo package: events in order, reply text joined equals the `Chat` reply, usage equals `Chat` usage.
- [ ] Write `TestStreamEcho`: a streamed echo call gives role, content and finish chunks, then `[DONE]`; trailers carry the usage; no usage chunk unless `include_usage` was asked.
- [ ] Write `TestStreamNative`: the fake upstream asserts it got `stream_options.include_usage: true`, sends three chunks plus a usage-only chunk and `[DONE]`; the caller sees the three chunks and `[DONE]`, not the usage chunk; trailers and the record carry the usage and cost.
- [ ] Write `TestStreamNativeKeepsUsageChunk`: when the caller asked for usage the usage chunk is passed on.
- [ ] Write `TestStreamUpstreamFailsFirst`: upstream 500 before any event gives 502 JSON with `Content-Type: application/json`.
- [ ] Write `TestStreamUpstreamCut`: the fake writes one chunk, flushes, and closes the connection by hijacking; the caller gets that chunk and an error frame, and the handler returns.
- [ ] Run, expect FAIL; implement; run `go test ./...`, expect ok.
- [ ] Commit `feat(server): stream chat completions with usage trailers`.

### Task 10: Embeddings

**Files:** create `internal/server/embed.go`, `internal/server/embed_test.go`; modify `server.go`, `internal/provider/echo/echo.go` (+ test).

**Interfaces:**
- Consumes: `ParseEmbedRequest`, `EncodeEmbedResponse`, `ParseUsage`, `Resolve`, `Native`.
- Produces: `POST /v1/embeddings`, endpoint name `embeddings` in usage records. Echo `Embed` returns an 8-dimension vector per input from the first 8 bytes of its SHA-256, each byte divided by 255, and input tokens of characters divided by 4 rounded up.

- [ ] Write `TestEchoEmbed`: same input gives the same vector, different inputs differ, one vector per input in order.
- [ ] Write `TestEmbeddingsNative`: a fake OpenAI server gets the body with the model rewritten; usage 1_000_000 tokens on `text-embedding-3-small` gives `X-Usage-Cost-USD: 0.020000` and a record with endpoint `embeddings`.
- [ ] Write `TestEmbeddingsEcho` and `TestEmbeddingsErrors` (401, 400 missing input, 404 no route, upstream 500 gives 502).
- [ ] Run, expect FAIL; implement, sharing the auth, body and record helpers with chat; run `go test ./...`, expect ok.
- [ ] Commit `feat(server): serve embeddings`.

### Task 11: Wire providers and routes at start, docs

**Files:** modify `cmd/transponder/main.go`, `main_test.go`, `README.md`, `CHANGELOG.md`, `AGENTS.md`; create `docs/issues-milestone-2.md`.

**Interfaces:**
- Produces: `func buildRouter(cfg *config.Bootstrap, client *http.Client) (*router.Router, error)`. Types map to `echo.New`, `openai.New`, `ollama.New`. With no routes in the file, one route `*` goes to a built-in `echo` provider, so a bare start still answers.

- [ ] Write `TestBuildRouter` (table): no providers gives echo for any model; an openai provider with a route resolves to it with the rewritten model; an ollama provider resolves with the default base URL.
- [ ] Write `TestServeWithOpenAIProvider`: a config whose openai provider points at an `httptest.Server` answers a chat call end to end with a nonzero cost header.
- [ ] Run, expect FAIL; implement; run `go test ./...` and `make check` (or lint plus coverage), expect ok.
- [ ] Update README status and a config example with providers and routes, CHANGELOG Unreleased, AGENTS layout (router, pricing, openai, ollama no longer planned).
- [ ] Write `docs/issues-milestone-2.md`: the claimed issues for Tasks 1 to 11 and good first issues from the deferred minors M4 to M7, then post them under a `v0.2.0 openai and ollama` milestone.
- [ ] Commit `feat: start with providers and routes from the config file` and `docs: milestone 2 issues`.
