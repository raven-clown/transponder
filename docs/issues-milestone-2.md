# Milestone 2 issues

Milestone: `v0.2.0 openai and ollama`. Goal: an OpenAI SDK pointed at transponder gets chat completions, plain and streamed, and embeddings from OpenAI, Ollama or any OpenAI-compatible server, with real token counts and cost. Plan: [docs/plans/2026-11-05-milestone-2-openai-ollama.md](plans/2026-11-05-milestone-2-openai-ollama.md).

Issues 1 to 5 are built on the `milestone-2` branch and close when it merges. Issues 6 to 9 are open to anyone.

---

## 1. Providers and routes in the config file

Labels: `enhancement`, `claimed`

**Why:** the gateway has to know which backends exist and which one serves a model before the admin API lands in milestone 6.

**Behavior:** `providers` (name, type `echo`/`openai`/`ollama`, `base_url`, `api_key`) and `routes` (`match` as an exact name or a glob, `targets` with a provider and an optional model) are read from the file. The first matching route wins and its first target serves the call; fallback comes in milestone 4. With no routes, every model goes to a built-in `echo` provider. Bad names, unknown types and routes that name an undeclared provider are errors at start.

---

## 2. Price table and cost in nano-USD

Labels: `enhancement`, `claimed`

**Why:** `X-Usage-Cost-USD` and the usage records need real numbers for real providers.

**Behavior:** `internal/pricing/prices.yaml` is embedded, keyed `type/model`, in USD per million input, output, cache read and cache write tokens. Cost is computed in int64 nano-USD and rounded once. Echo and Ollama are free. A model with no price is served at zero cost and logged as `unpriced`.

---

## 3. OpenAI and Ollama adapters with native passthrough

Labels: `enhancement`, `adapter`, `claimed`

**Why:** an OpenAI caller talking to an OpenAI-compatible backend should keep every field the API accepts, including ones transponder does not know.

**Behavior:** the body is forwarded with only `model` changed. `X-Request-ID`, `traceparent` and `tracestate` go upstream. Usage is read from the response; when it is missing the record is marked `estimated` and counts come from characters divided by 4. A 4xx that would fail anywhere (400, 404, 422) is passed to the caller as is. 401, 403, 408, 429, 5xx and connection errors become 502 `upstream_error`. The Ollama preset points at `http://localhost:11434/v1` with no key.

---

## 4. Streaming chat completions with usage trailers

Labels: `enhancement`, `claimed`

**Why:** most chat clients stream, and the contract puts usage in trailers on streams.

**Behavior:** transponder asks the upstream for `stream_options.include_usage`, reads the first event before it answers so an early failure is a normal JSON error, then copies chunks as they arrive. The usage-only chunk reaches the caller only if the caller asked for it. A stream cut by the upstream ends with an error event. The usage headers arrive as trailers.

---

## 5. `POST /v1/embeddings`

Labels: `enhancement`, `claimed`

**Why:** logbook and most retrieval setups need embeddings through the same gateway and budget.

**Behavior:** same routing and passthrough as chat. Echo returns a deterministic 8-dimension vector per input.

---

## 6. Accept text-only content arrays in the canonical chat decoder

Labels: `enhancement`, `good first issue`

**Why:** OpenAI clients often send `"content": [{"type": "text", "text": "hi"}]`. Passthrough handles it, but the canonical path used by echo, and later by translation, rejects it as `unsupported`.

**Behavior:** in `DecodeChatRequest`, content that is an array of parts with `type: "text"` becomes one string, parts joined in order. Arrays with any other part type, such as `image_url`, still give 400 `unsupported` until images land.

**Tests:** one part, several parts, an empty array, a part with another type, a part with no `text`.

**Where:** `internal/dialect/openai/chat.go` and `chat_test.go`.

---

## 7. Reject a declared body over 20 MiB before reading it

Labels: `enhancement`, `good first issue`

**Why:** a request whose `Content-Length` already says it is too large can be refused at once instead of being read up to the limit.

**Behavior:** in `begin` in `internal/server/server.go`, when `r.ContentLength > maxBodyBytes`, answer 413 `body_too_large` without reading the body. Chunked bodies with no length keep the current check.

**Tests:** a request with a large `Content-Length` and a small body gets 413 and the handler never reads it (use a body reader that fails the test if read); a normal request still works.

**Where:** `internal/server/server.go` and `internal/server/chat_test.go`.

---

## 8. Plain error messages for bad chat requests

Labels: `enhancement`, `good first issue`

**Why:** a wrong type in a chat request gives a message like `json: cannot unmarshal number into Go struct field wireRequest.max_tokens of type int`, which shows internal names.

**Behavior:** `DecodeChatRequest` reports `max_tokens must be an integer`, `temperature must be a number`, `messages must be a list` and so on, with `param` set to the field. Use `*json.UnmarshalTypeError` and its `Field`.

**Tests:** a table of wrong types for each field, checking `param`, `code` (`invalid_json`) and that the message has no `Go` or `wire` in it.

**Where:** `internal/dialect/openai/chat.go` and `chat_test.go`.

---

## 9. A second Ctrl+C stops at once

Labels: `enhancement`, `good first issue`

**Why:** shutdown waits up to 10 seconds for open requests. Someone pressing Ctrl+C again expects the process to stop now.

**Behavior:** in `run` in `cmd/transponder/main.go`, call `stop()` from `signal.NotifyContext` as soon as the context is done, so the next signal uses the default handler and ends the process.

**Tests:** a test of a small helper that takes the context and the stop function and calls stop once the context is done, so no real signal is needed.

**Where:** `cmd/transponder/main.go` and `main_test.go`.
