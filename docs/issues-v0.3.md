# v0.3 issues

Follow-ups to v0.3.0 that anyone can pick up. Each one stays inside what v0.3.0 shipped: access per route, OIDC and roles, the admin API and audit log, Postgres and replicas, metrics and tracing, the demo, images in chat and the Responses API. Nothing here starts v0.4.0 work: Responses API translation, stored responses, organisations, Bedrock and Vertex, caching and `LISTEN/NOTIFY` reload stay out.

Milestone: `v0.3.x follow-ups`. The first 12 are `good first issue`: small, with the files, an approach and the tests named. The other 15 are `help wanted`: bigger, with the behavior and tests written out. Each heading carries its issue number on GitHub. Line numbers are from `main` at v0.3.0. Comment on an issue before you start so nobody else takes it.

---

## #168 Check `listen_addr` and `ops_listen_addr` when the config loads

Labels: `enhancement`, `good first issue`

**Why:** someone who writes `listen_addr: "8080"` and leaves out the colon gets `transponder: config ok` from `--config-test`. The server then stops at start with `listen tcp: address 8080: missing port in address`, which does not name the key. `ops_listen_addr: ":8080"` next to `listen_addr: ":8080"` also passes and fails at start with "address already in use". Neither key is checked in `Bootstrap.validate` (`internal/config/config.go:362`).

**Change:** in `Bootstrap.validate`, check `listen_addr` and, when set, `ops_listen_addr` with `net.SplitHostPort`. A bad value is an error that names the key and the value, such as `config: listen_addr "8080" must be host:port, such as :8080`. When both are set and use the same port with the same host, or an empty host on either side, say they must differ. Stay offline: no listening and no DNS lookup.

**Tests:** `TestLoadRejects` in `internal/config/config_test.go`: `listen_addr: "8080"`, `ops_listen_addr: nope`, both `:8080`, and `:8080` with `0.0.0.0:8080`; the same bad `listen_addr` from `TRANSPONDER_LISTEN_ADDR`. One case in `TestConfigTest` in `cmd/transponder/main_test.go` that exits 1 with the key in stderr.

**Where:** `internal/config/config.go`, `internal/config/config_test.go`, `cmd/transponder/main_test.go`, `CHANGELOG.md` under Unreleased.

**Done when:** `--config-test` exits 1 for each case and the message names the key.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #169 Check that a provider's `base_url` in the config file is an http or https URL

Labels: `bug`, `good first issue`

**Why:** the admin API refuses a provider whose `base_url` is not an http or https URL (`putProvider`, `internal/admin/providers.go:154`). The config file does not (`ValidateRuntime`, `internal/config/config.go:424`). A file with `base_url: api.openai.com/v1` passes `--config-test`, and then every call to that provider fails, falls back, or ends in a 502. The file and the API should agree.

**Change:** in the providers loop of `ValidateRuntime`, run the same check as `putProvider`: it parses, the scheme is `http` or `https`, the host is not empty. Name the provider in the error but not the value, since a base URL can carry a password. `otel.endpoint` errors already leave the value out (`config.go:280`). Items stored through the API already pass this check, so they keep loading.

**Tests:** `TestLoadRejects`: no scheme, `ftp://example.com`, `https://` with no host, and `https://user:secret@` whose error does not contain `secret`.

**Where:** `internal/config/config.go`, `internal/config/config_test.go`, `CHANGELOG.md` under Unreleased, Fixed.

**Done when:** each bad URL fails `--config-test` with the provider named, and every existing config still loads.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #170 Refuse routes in the config file that can never match

Labels: `enhancement`, `good first issue`

**Why:** routes are tried in order and the first match wins (`Router.Plan`, `internal/router/router.go:133`). A second route with the same `match`, or any route after `match: "*"`, never gets a call. With access per route this is an easy trap: two `gpt-4o` routes with different `allow` lists look like they split callers between two providers, but every call takes the first route and the callers it leaves out get 403 `route_forbidden`. `--config-test` says `config ok`.

**Change:** in `Bootstrap.validate`, refuse a file route whose `match` equals an earlier one, and any route after a route whose `match` is `*`. The error names both and says how to fix it: `config: routes[1] "gpt-4o" can never match: routes[0] has the same match and comes first. Use a different model name for each set of callers.` Put it in `validate`, not `ValidateRuntime`, so routes made through the admin API keep their own positions.

**Tests:** `TestLoadRejects`: the same exact name twice, the same glob twice, a route after `*`. A file with `*` last still loads.

**Where:** `internal/config/config.go`, `internal/config/config_test.go`, one sentence in the Routes section of `docs/configuration.md`, `CHANGELOG.md` under Unreleased, Changed, since such a file no longer loads.

**Done when:** each case fails `--config-test` with both routes named, and `go test ./...` passes. That run also loads every config example in the docs.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #171 Refuse two budgets for one subject and period, and two bindings for one subject

Labels: `enhancement`, `good first issue`

**Why:** `budgetsFor` (`internal/limits/limiter.go:250`) applies every budget of a subject. With two daily budgets for `user:alice`, 1 USD and 5 USD, alice stops at 1 USD, so adding a line to raise her budget does nothing. `Limiter.Profile` (`limiter.go:80`) takes the first binding of a subject, so a second binding for `key:batch` is ignored. Neither case is reported.

**Change:** in `Bootstrap.validate`, refuse a second budget with the same `subject` and `period`, and a second profile binding for the same subject, also when one of them comes from `profiles_file`. Example: `config: budgets[1] repeats the daily budget of user:alice in budgets[0]; keep one`. A daily and a monthly budget for one subject stay fine. The admin API is out of scope here.

**Tests:** `TestLoadRejects`: two daily budgets for one user, two bindings for one key, a binding in the file plus one for the same subject in a `profiles_file`. A daily and a monthly budget for one user still load.

**Where:** `internal/config/config.go`, `internal/config/config_test.go`, `CHANGELOG.md` under Unreleased, Changed.

**Done when:** each duplicate fails `--config-test` with both entries named.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #172 Refuse an API key whose `expires_at` is already past

Labels: `enhancement`, `good first issue`

**Why:** `POST /admin/v1/keys` with `"expires_at": "2025-12-31T00:00:00Z"`, where the year was meant to be 2026, answers 201 with a secret that never works. `readExpiry` (`internal/admin/keys.go:308`) checks only that the value is RFC 3339. The caller finds out at the first 401, with no hint why.

**Change:** in `createKey` (the `readExpiry` call at `keys.go:113`), answer 400 `invalid` on the field `expires_at` when the time is not after `h.d.Now()`: `expires_at is in the past; a key that has expired cannot be used`. `PATCH` keeps taking a past time, since setting one is a way to retire a key at once.

**Tests:** in `internal/admin/keys_test.go`: create with a past time is 400 naming `expires_at`, create with a future time is 201, and `PATCH` with a past time is still 200. The tests use a fixed `testNow`, so no sleeping is needed.

**Where:** `internal/admin/keys.go`, `internal/admin/keys_test.go`, the `expires_at` description in `api/openapi.yaml`, `CHANGELOG.md` under Unreleased.

**Done when:** a key that would be born expired is refused with the field named.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #173 Say so when a token's issuer differs from `auth.oidc.issuer` only by a trailing slash

Labels: `enhancement`, `good first issue`

**Why:** Auth0 and some other providers put `https://tenant.auth0.com/` with a trailing slash in `iss`. If the config has the URL without it, every token is refused. The check is an exact compare (`internal/auth/oidc.go:134`), and the reason is logged at debug level only (`internal/server/observe.go:310`), so at the default `info` level the operator sees a wall of 401s and nothing in the log.

**Change:** when the two issuers are equal after trimming one trailing `/`, keep refusing the token, since the issuer must match exactly, but make the reason say so and quote the token's exact value, so the hint is right whichever side has the slash: `issuer is "https://x/", want "https://x": the two differ only by a trailing slash; set auth.oidc.issuer to "https://x/"`. Log this case once per process at warn level, through a `sync.Once` or similar, so the hint is seen at the default level without flooding.

**Tests:** in `internal/auth/oidc_test.go`, a table with the slash on either side: the token is refused and the reason contains `trailing slash`. A plainly different issuer keeps the old message. In `internal/server`, the warn line appears once for two such calls.

**Where:** `internal/auth/oidc.go`, `internal/server/observe.go`, their tests.

**Done when:** the slash mismatch shows up in the log at the default level with the fix spelled out.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #174 Test the usage estimate on `/v1/embeddings` and count token id input

Labels: `good first issue`

**Why:** when an OpenAI-compatible server answers embeddings without `usage`, `embedNative` estimates the input with `embedText` (`internal/server/embed.go:52` and `:94`) and marks the record as estimated. No test covers that path. Also, `input` given as token ids (`[1, 2, 3]` or `[[1, 2], [3]]`), which the OpenAI API accepts and transponder passes on, makes `ParseEmbedRequest` fail inside `embedText`, so the estimate is 0 tokens and the call costs nothing against a budget.

**Change:** count one token per id when `input` is a list of ids or a list of such lists. A small helper that tries the string forms first and then `[]int` and `[][]int` is enough. Text input keeps the current estimate.

**Tests:** a table in `internal/server/embed_test.go` with an upstream that answers without `usage`: a string, a list of strings, a list of ids, a list of lists. Check the status is 200, `X-Usage-Input-Tokens` is above 0 (and equals the id count for id input), and the record has `Estimated` with the same input tokens. `TestEmbeddingsNative` at `embed_test.go:27` shows the setup.

**Where:** `internal/server/embed.go`, `internal/server/embed_test.go`, `CHANGELOG.md` under Unreleased, Fixed, for id input.

**Done when:** the tests pass and `go test -cover ./internal/server` covers the estimate path.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #175 Benchmark OIDC token checks

Labels: `good first issue`

**Why:** v0.3.0 measured the API key lookup on every call (`BenchmarkAuthenticateKey`, `internal/auth/auth_bench_test.go:18`) and made it four times faster. Token callers have no benchmark. Every call with a token verifies a signature (`internal/auth/oidc.go:153`), and an RSA check costs far more than a map lookup, so a team that signs in with OIDC may pay more per call than the numbers in `docs/benchmarks.md` show.

**Change:** add `BenchmarkVerifyToken` next to the key benchmark, with sub-benchmarks for RS256 and ES256 keys and a parallel run, using the local provider in `internal/auth/authtest` so nothing leaves the machine. Report allocations.

**Tests:** the benchmark is the test. Run it with `go test -run '^$' -bench VerifyToken -benchmem ./internal/auth` and put the numbers and your CPU in the pull request.

**Where:** `internal/auth/auth_bench_test.go`, and a short row in `docs/benchmarks.md` next to the key lookup.

**Done when:** the benchmark runs offline with the command above. If a token check turns out to cost more than about 100 µs, say so in the pull request so a follow-up can look at caching.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #176 Show access per route in the demo

Labels: `enhancement`, `good first issue`

**Why:** the demo shows fallbacks, rate limits and budgets, but not access per route, one of the main v0.3.0 features. `demo/transponder.yaml` has no `allow`, and the traffic in `demo/sim/traffic.go:32` never gets a 403.

**Change:**

- `demo/transponder.yaml`: a route such as `match: private` with `allow: ["key:alice"]`, sent to `steady` as `gpt-4o-mini`, with a short comment like the others.
- `demo/sim/traffic.go`: two scenarios with small weights, `tp_demo_alice` on `private` (allowed) and `tp_demo_bob` on `private` (refused). Take the weight from existing scenarios.
- `demo/check.sh`: wait until `sum(transponder_requests_total{code="403"})` is above 0, like the rate and budget checks.
- `demo/README.md`: one bullet under "What you see".

**Tests:** `go test ./demo/...`. Run `docker compose up --build -d --wait` and `./check.sh` in `demo/`, and say in the pull request that you did. If the requests panel in Grafana does not show the 403s, add them to `demo/grafana/dashboards/transponder.json`.

**Where:** `demo/transponder.yaml`, `demo/sim/traffic.go`, `demo/check.sh`, `demo/README.md`.

**Done when:** `./check.sh` passes on a fresh demo and the dashboard shows refused calls.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #177 Make the shutdown drain time a setting

Labels: `enhancement`, `good first issue`

**Why:** on a stop signal, open calls get 10 seconds to finish (`shutdownTimeout`, `cmd/transponder/main.go:45`), then they are cut. A reasoning model can stream for minutes, so every rolling deploy cuts the long streams in flight, and the callers see a broken answer. Kubernetes gives a pod 30 seconds by default and lets you raise it. transponder should be able to use that time.

**Proposed API:** `settings.shutdown_timeout`, a duration, default `10s`, environment variable `TRANSPONDER_SETTINGS__SHUTDOWN_TIMEOUT`.

**Change:** add the field to the settings struct in `internal/config/config.go` and check it is positive in `validate`, like the other durations there. Pass it into `serve` and use it where `shutdownTimeout` is used now (`main.go:255`). Keep the variable for the default so tests can still shorten it.

**Tests:** `TestLoadRejects` with `shutdown_timeout: -1s`. A test in `cmd/transponder` that a call still running when the stop signal comes finishes when the setting is longer than the call. Look at how the current shutdown tests in `main_test.go` stop the server.

**Where:** `internal/config/config.go`, `cmd/transponder/main.go`, their tests, `docs/configuration.md` under "Calls to providers" (`TestConfigReference` fails until the row is there), `CHANGELOG.md`.

**Done when:** `shutdown_timeout: 5m` lets a 2-minute stream finish during a stop.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #178 Filter the audit log by action

Labels: `enhancement`, `good first issue`

**Why:** "who deleted the provider `openai` last week?" needs `GET /admin/v1/audit?resource=providers/openai` and then reading every record for `"action": "delete"`. For a busy resource that means paging through every update. `GET /admin/v1/audit` filters by actor, resource and time (`api/openapi.yaml:650`), not by action, though every record has one (`internal/admin/audit.go:15`).

**Proposed API:** `GET /admin/v1/audit?action=delete`, one of `create`, `update`, `delete` or `rotate`. Anything else is 400 `invalid` on `action`.

**Change:**

- `Action string` in `store.AuditQuery` (`internal/store/store.go:111`).
- The filter in both stores: `internal/store/sqlite/audit.go:33`, next to `actor`, and `internal/store/postgres/audit.go:27`.
- Read and check the parameter in `listAudit` (`internal/admin/audit.go:106`).
- The parameter in `api/openapi.yaml`. `spec_test.go` checks the handler against it.

**Tests:** a case in the audit test of `internal/store/storetest/admin.go` so both stores are covered, and cases in the audit table of `internal/admin/usage_test.go` for a match, no match and a bad value.

**Where:** the files above, `CHANGELOG.md`.

**Done when:** `?action=delete` returns only deletes on SQLite and Postgres.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #179 An optional log line for every call

Labels: `enhancement`, `good first issue`

**Why:** at `info`, transponder logs only exceptions: an unpriced model, a caller who left, a failed provider (`internal/server/server.go:221`, `upstream.go:132`, `upstream.go:138`). A team that ships logs to Loki or Elasticsearch, and doesn't want to query the usage table, has no line per call to search by request id when someone reports "my call at 10:42 failed".

**Proposed API:** `log_requests: true` (`TRANSPONDER_LOG_REQUESTS`), off by default.

**Change:** when set, `recordCall` (`internal/server/limits.go:69`) writes one info line, `call`, with the fields the usage record already has: request id, endpoint, route, provider, model, status, duration, input and output tokens, cost, user and key name. Never the prompt, the answer or a provider error body, the same rule as the span attributes in `AGENTS.md`.

**Tests:** in `internal/server`, with a log buffer: one call gives one `call` line with those fields; with the setting off there is none; a prompt with a marker string never shows up in the buffer.

**Where:** `internal/config/config.go`, `internal/server/limits.go`, wiring in `cmd/transponder/main.go`, the Server table in `docs/configuration.md`, `CHANGELOG.md`.

**Done when:** `log_requests: true` gives one searchable line per call with no prompt text in it.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #180 Read OpenAI-compatible answers whose content is a list of parts

Labels: `bug`, `help wanted`

**Why:** when a `/v1/messages` caller such as Claude Code is routed to an OpenAI-dialect target, the answer is translated. `DecodeChatResponse` reads `message.content` with `messageText` (`internal/dialect/openai/chat.go:130`), which takes a string or a list of `text` parts only. Any other part fails with a `RequestError` naming `messages` (`chat.go:149`), although it is the provider's answer that could not be read. The attempt counts as a failure: transponder falls back and cools the target down after the provider has done and billed the work. Streams have the same gap: `delta.content` is read as a string (`internal/dialect/openai/request.go:161`), so a list makes the chunk unreadable and the stream is treated as cut.

Mistral's reasoning models answer with `content` as a list of `thinking` and `text` parts, and `type: mistral` is new in v0.3.0. Check the current shape in Mistral's API docs and use it in the tests.

**Behavior:**

- In an answer, plain or streamed, `content` may be a string, `null` or a list. Text parts are joined in order. A `thinking` part, or any part with nothing for the caller, is skipped without an error. Passing thinking on as reasoning is a later change.
- An answer that really cannot be read gets an error that says the provider's answer could not be read, not one about `messages`.
- `DecodeChatRequest` keeps its rules.

**Tests:** `internal/dialect/openai`: `DecodeChatResponse` with text parts, `thinking` then text, and `null`; the chunk decoder with a list in `delta.content`. `internal/server/messages_test.go`: `/v1/messages` to an OpenAI target answering with such a list, plain and streamed, gets 200 with the text and no fallback. Keep long bodies in `internal/server/testdata`.

**Where:** `internal/dialect/openai/chat.go`, `request.go`, their tests, `internal/server/messages_test.go`, `CHANGELOG.md` under Fixed.

**Done when:** a translated answer or stream with list content reaches the caller.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #181 Answer 503 when the store cannot be read to check an API key

Labels: `enhancement`, `help wanted`

**Why:** while Postgres is unreachable, the key lookup (`internal/auth/auth.go:69`) returns the store error, and `authFailed` answers every key caller with 500 "could not check the credentials" and logs an error each time (`internal/server/observe.go:319`). `TestStoreErrorWhileAuthenticating` (`internal/server/oidc_test.go:129`) pins the 500. SDKs treat a 500 as the server's own bug. A failure that will pass should be a 503 with `Retry-After`, as the identity provider being down already is (`observe.go:315`). A caller who leaves during the lookup is logged as an error too.

**Proposed API:** a new error code, `store_unavailable`, with status 503 and `Retry-After: 5`.

**Behavior:**

- A store error during the key lookup answers 503 `store_unavailable` with `Retry-After`, in the caller's dialect, logged at warn.
- A caller whose context was cancelled during the lookup is logged at debug, not as an error.
- The admin API's `deny` (`internal/admin/admin.go:131`) answers the same way.
- Token callers do not change: their check does not touch the store.

**Tests:** update `TestStoreErrorWhileAuthenticating` for `/v1/chat/completions` and `/v1/messages`; an admin API call with a closed store; a cancelled lookup writes no error line.

**Docs:** "Store locked or full" in `docs/failure-modes.md` says what a key caller gets while the store is down, with a test row. `CHANGELOG.md` under Changed.

**Where:** `internal/server/observe.go`, `internal/admin/admin.go`, their tests, the docs above.

**Done when:** a store outage gives key callers 503 `store_unavailable` on every endpoint and the admin API.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #182 Refuse an admin API write that leaves no admin

Labels: `enhancement`, `help wanted`

**Why:** an admin can delete, disable, expire or demote the last key with the `admin` role (`deleteKey` and `updateKey`, `internal/admin/keys.go:263` and `:167`). Nothing stops it. Unless OIDC maps someone to `admin`, nobody can use the admin API afterwards. The only way back is a restart, after which `EnsureAdmin` (`internal/auth/bootstrap.go:14`) mints a new key and prints it to stderr, on whichever replica restarted first.

**Proposed API:** 409 `last_admin`: "this would leave no usable admin key; create another admin key first".

**Behavior:**

- Inside the same `UpdateConfig` transaction as the write, after applying it, count the usable admin keys of the org (enabled, not expired). If there are none, roll back and answer 409. The count belongs in the transaction so two admins removing each other's keys at once cannot both pass.
- Only a write that takes the count from one or more down to zero is refused. A setup that never had an admin key, such as one that signs admins in with OIDC, is not affected.
- When `auth.oidc.role_mapping` maps some value to `admin`, the check is skipped, since someone is expected to sign in as admin. The handler needs to know this from its dependencies. transponder can't see whether anyone holds that value, so the API reference and the README say plainly: with an admin role mapping, deleting the last admin key leaves only the identity provider. If nobody holds the mapped value, a restart does not help, because start prints no key when `role_mapping` names an admin. The way back is to fix the group in the identity provider, or to make a key with `transponder keys create` (#187).
- Rotating a key is not affected.

**Tests:** in `internal/admin/keys_test.go`: delete, disable, past expiry and demotion of the only admin key are each 409 and change nothing, and no audit record is written; the same with a second admin key succeeds; with an admin role mapping it succeeds. A store-level helper for the count goes in both stores and `internal/store/storetest`.

**Where:** `internal/admin/keys.go`, both stores and the suite, `api/openapi.yaml` (the 409 on those operations), `CHANGELOG.md`.

**Done when:** without an admin role mapping, no single API write can lock every admin out, and the case with one is documented with its way back.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #183 Count refused credentials by reason

Labels: `enhancement`, `help wanted`

**Why:** a refused key or token shows in `/metrics` only as `transponder_requests_total{code="401"}` (see `TestMetricsForRefusedRequests`, `internal/server/metrics_test.go:156`), and the reason goes to the debug log (`internal/server/observe.go:310`). An operator can't tell a client with an old key from an OIDC setup that refuses every token for a wrong audience, or from someone trying random keys. Each needs a different response.

**Proposed API:** `transponder_auth_refused_total{reason}`, where `reason` is one of a fixed set: `missing`, `unknown_key`, `disabled_key`, `expired_key`, `bad_token`, `wrong_issuer`, `wrong_audience`, `expired_token`. Never a key name, a user or a token.

**Behavior:**

- `internal/auth` returns the reason in a typed error that wraps `ErrUnauthenticated`, instead of only a `rejectf` string (`internal/auth/oidc.go`), so `errors.Is` keeps working everywhere.
- `authFailed` and the admin API's `deny` count the reason.
- The metric is registered with every reason at 0, so a dashboard alert works before the first refusal.
- Add it to the metric list in the README and, if you like, a panel in `demo/grafana/dashboards/transponder.json` (`go test ./demo/...` checks the queries).

**Tests:** in `internal/auth`, each case gives its reason. In `internal/server/metrics_test.go`, a scrape after refused calls shows each count and no label holds caller data.

**Where:** `internal/auth`, `internal/server/observe.go`, `internal/admin/admin.go`, `internal/telemetry/metrics.go`, README, `CHANGELOG.md`.

**Done when:** a wrong audience and a wrong key show up as different series.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #184 Report "not ready" while draining for a stop

Labels: `enhancement`, `help wanted`

**Why:** on a stop signal, `srv.Shutdown` (`cmd/transponder/main.go:257`) closes the listener at once. In Kubernetes the endpoint is removed only some time later, so for a few seconds new calls hit a closed port and fail with "connection refused". The usual fix is for `/readyz` to answer 503 first, wait a short delay while the load balancer notices, and only then stop listening.

**Proposed API:** `settings.shutdown_delay`, a duration, default `0s` so nothing changes unless asked.

**Behavior:**

- On a stop signal, `/readyz` on both ports answers 503 with `{"status":"draining"}`, and calls keep being served for `shutdown_delay`.
- Then the listener closes and the drain runs as today.
- A second Ctrl+C still stops at once, as it does since #18.
- `/healthz` stays 200 while draining, so the process is not restarted.

**Tests:** in `cmd/transponder`: after the signal, `/readyz` is 503 and a call still succeeds within the delay; after the delay, the port is closed. Use a short delay and poll with a deadline. No bare `time.Sleep`.

**Where:** `cmd/transponder/main.go`, `cmd/transponder/ops.go`, `internal/server/health.go`, `internal/config/config.go`, `docs/configuration.md`, a sentence in the README's deployment part, `CHANGELOG.md`.

**Done when:** a rolling deploy with `shutdown_delay: 5s` drops no new calls in a local test with two processes behind a simple proxy. Describe what you ran.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #185 Show the targets in cooldown in `/admin/v1/status`

Labels: `enhancement`, `help wanted`

**Why:** after `settings.cooldown_failures` failures in a row, a target is tried last for `settings.cooldown`. That state lives in the router (`internal/router/router.go`, the `health` map) and is visible nowhere. During an incident "is the router still sending traffic to the provider that is down?" can only be guessed from `transponder_fallbacks_total`. `GET /admin/v1/status` (`internal/admin/status.go:7`) shows the store and the config, not this.

**Proposed API:** a `cooldowns` list in the status response, each `{"route", "provider", "model", "failures", "until"}`. It is this replica's view, since cooldown state is per replica by design (`docs/design.md` section 14), and the field's description in `api/openapi.yaml` says so.

**Behavior:**

- The router gets a read-only method that returns a copy of the targets whose cooldown has not ended, sorted for a stable answer.
- The status handler reads it through its dependencies. With no router, the list is empty, not missing.
- Nothing new goes on metric labels.

**Tests:** in `internal/router`, the method returns only targets still cooling and is a copy. In `internal/admin`, status shows a target after it fails `cooldown_failures` times and drops it after a success. `spec_test.go` validates the response against the file.

**Where:** `internal/router/router.go`, `internal/admin/status.go`, `cmd/transponder/main.go` for the wiring, `api/openapi.yaml`, `CHANGELOG.md`.

**Done when:** `GET /admin/v1/status` names the target that is down and when it will be tried first again.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #186 Show the identity provider's key state in `/admin/v1/status`

Labels: `enhancement`, `help wanted`

**Why:** once the issuer's keys are cached, a failing refresh is only a warn line in one replica's log. `/readyz` reports `oidc` only while no keys have ever been fetched. If the issuer rotates its keys while it can't be reached, tokens signed with the new key are refused, and nothing in the status says the keys are stale. The key cache in `internal/auth/jwks.go` knows when it last fetched, how many keys it holds and the last error.

**Proposed API:** an `oidc` object in the status response when OIDC is on: `{"issuer", "keys", "fetched_at", "last_error", "last_error_at"}`. Leave it out when OIDC is off.

**Behavior:**

- `internal/auth` gets a read-only snapshot of the key set: the count, times and error text. The error text holds no token.
- The status handler includes it. It is this replica's view.

**Tests:** in `internal/auth`, the snapshot before the first fetch, after a fetch, and after a failed refresh with cached keys. In `internal/admin`, status with OIDC on and off. `spec_test.go` validates the response.

**Where:** `internal/auth/jwks.go`, `internal/auth/oidc.go`, `internal/admin/status.go`, wiring in `cmd/transponder/main.go`, `api/openapi.yaml`, `CHANGELOG.md`.

**Done when:** a failed key refresh is visible in `GET /admin/v1/status`.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #187 `transponder keys create` to make a key without the admin API

Labels: `enhancement`, `help wanted`

**Why:** the first admin key is printed to stderr (`internal/auth/bootstrap.go:42`). In Kubernetes or Docker, stderr goes to the log pipeline, so a working admin secret ends up in Loki or CloudWatch, readable by anyone who can read logs. Teams that set up transponder with Terraform or a Helm hook also need a way to make a key from a script before anything can call the API.

**Proposed API:** `transponder keys create --config <file> --name <name> [--role admin|viewer|member] [--team <t>]...`, next to `transponder secrets rotate` (`cmd/transponder/secrets.go`). It prints only the secret to stdout and exits 0, or prints an error and exits 1. And `bootstrap_admin_key: false` (`TRANSPONDER_BOOTSTRAP_ADMIN_KEY`), default `true`, to skip printing a first key at start.

**Behavior:**

- The command opens the configured store, SQLite or Postgres, and creates the key in one `UpdateConfig` with an audit record whose actor is `cli`.
- A name that exists is an error that names it. Exit 1, and nothing is written.
- With `bootstrap_admin_key: false`, start creates no key and logs one info line saying how to make one.

**Tests:** in `cmd/transponder/keys_test.go`: create on SQLite and read the key back through `auth`; a duplicate name; a bad role; the audit record. Postgres through `pgtest` when `TRANSPONDER_TEST_POSTGRES` is set. A start with `bootstrap_admin_key: false` prints no key.

**Where:** `cmd/transponder/main.go`, a new `cmd/transponder/keys.go`, `internal/auth/bootstrap.go`, `internal/config/config.go`, `docs/configuration.md` (Command line and Server tables), the README, `CHANGELOG.md`.

**Done when:** a key can be made from a script with its secret on stdout only, and start can be told not to print one.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #188 Extra headers per provider

Labels: `enhancement`, `help wanted`

**Why:** a provider sends only its own `Authorization` and the trace headers (`internal/provider/http.go:19`). Some backends need one more:

- Cloudflare AI Gateway with authentication on wants `cf-aig-authorization: Bearer <token>` next to the provider key, so `type: cloudflare` cannot reach an authenticated gateway today.
- OpenRouter uses `HTTP-Referer` and `X-Title` to attribute traffic to an app.
- A company proxy in front of a self-hosted model may want its own key header.

`config.Provider` (`internal/config/config.go:231`) has no place for any of these.

**Proposed API:** `providers[].headers`, a map of header name to value, in the file and in `/admin/v1/providers`. Values may use `${VAR}`.

**Behavior:**

- Sent on every call to that provider, after the provider's own headers. A header the adapter sets itself (`Authorization`, `x-api-key`, `anthropic-version`, `Content-Type`) or a trace header is refused at load with an error naming it, so a config can't break auth by accident.
- A header name must be a valid HTTP token, and a value may not contain a newline.
- Headers go over the same connection as `api_key`, so they get the same rule: a `base_url` with plain `http`, common for a local server, sends both in the clear. The docs for `headers` say so next to the one for `api_key`, and a secret header belongs behind `https`.
- Through the admin API, header values are sealed with `secrets_key` like `api_key`, since they are often secrets, and reads show only the names and the last four characters.

**Tests:** in `internal/provider`, the headers reach an `httptest` upstream. Config: a refused name, a newline. Admin: create and read back without the values; values survive `transponder secrets rotate`.

**Where:** `internal/config/config.go`, `internal/provider/http.go`, `internal/provider/registry`, `internal/admin/providers.go`, `internal/admin/reseal.go`, `api/openapi.yaml`, `docs/configuration.md`, `CHANGELOG.md`. Comment on the issue with your plan first: it touches the admin API and sealed secrets.

**Done when:** an authenticated Cloudflare AI Gateway works with `type: cloudflare` and a `headers` entry.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #189 An attempt timeout per provider

Labels: `enhancement`, `help wanted`

**Why:** `settings.attempt_timeout` (default `1m`) applies to every target (`cmd/transponder/main.go:211`). A route that tries a local Ollama first and a cloud provider second needs two different values. Ollama loads a large model on its first call and can take longer than a minute to send its first byte, while a cloud API that hasn't answered in 15 seconds is better skipped. Today one value has to serve both, so either the cold local model is always skipped or a hung cloud call holds the caller for a full minute before fallback.

**Proposed API:** `providers[].attempt_timeout`, a duration that overrides `settings.attempt_timeout` for that provider. Also in `/admin/v1/providers`.

**Behavior:**

- Each attempt uses its provider's value when set, else the global one.
- `settings.request_timeout` still bounds the whole call.
- A value of 0 or below is a config error naming the provider.

**Tests:** in `internal/server/timeout_test.go`: a route whose first target has a short timeout and stalls falls back quickly, while a second route with a long one waits. The router or resolved target carries the value. Config and admin validation cases.

**Where:** `internal/config/config.go`, `internal/router`, `internal/server/upstream.go`, `internal/admin/providers.go`, `api/openapi.yaml`, `docs/configuration.md`, `CHANGELOG.md`.

**Done when:** a cold Ollama and a fast cloud provider can share one route without one timeout hurting the other.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #190 Answer 429 when every target was rate limited

Labels: `enhancement`, `help wanted`

**Why:** when every target of a route answers 429, the caller gets 502 `upstream_error`, "the provider did not answer" (`internal/server/upstream.go:164`). That is wrong twice. The providers did answer. And SDKs and Claude Code treat a 502 as a broken server, retrying quickly or giving up, while a 429 with `Retry-After` makes them back off and come back. `provider.Error` keeps the status and body of a failed answer but drops its `Retry-After` header (`internal/provider/http.go:44`), so transponder couldn't pass it on today.

**Proposed API:** 429 `upstream_rate_limited`, "every provider for this model is rate limiting; retry after N seconds", with `Retry-After` set to the shortest delay any target gave, or 1 second when none gave one.

**Behavior:**

- `provider.Error` gets a `RetryAfter time.Duration`, read from `Retry-After` in seconds or as an HTTP date, and bounded to at most 1 hour.
- Only when every attempt failed with 429 or 529. A mix of 429 and 5xx keeps the 502, with its message changed to "no provider for this model could answer" so it no longer says nobody answered.
- Fallback still happens as today. This only changes the final answer.
- Usage is recorded with status 429.

**Tests:** in `internal/provider`, `RetryAfter` from seconds, a date, a bad value and a huge value. In `internal/server/fallback_test.go`: two targets that both answer 429 with different `Retry-After`, giving 429 with the shorter one; one 429 and one 503 still giving 502; the message on the 502.

**Where:** `internal/provider/http.go`, `internal/provider/provider.go`, `internal/server/upstream.go`, `docs/failure-modes.md` under "Upstream down", `CHANGELOG.md` under Changed.

**Done when:** a caller behind rate-limited providers is told to wait, with a real wait time.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #191 Measure the Responses API in the load suite

Labels: `help wanted`

**Why:** v0.3.0 promised that no path is more than 5% slower than v0.2.4 and that each one is measured. `POST /v1/responses` is new in v0.3.0 and has no scenario in `bench/` (`scenarios`, `bench/run.go:67`) and no Go benchmark of its usage reader. Its stream path looks at every event for `response.completed`, which is the kind of per-event work the chat stream benchmarks exist to watch.

**Change:**

- `bench/upstream.go`: answer `/v1/responses`, plain and streamed, with a body and event count like the chat answers. Use `internal/server/testdata/responses/` for the shapes.
- `bench/run.go`: two scenarios, `responses` and `responses stream`, at the fixed rate.
- A Go benchmark for the stream usage reader in `internal/dialect/openai/responses.go`, next to the ones in `openai_bench_test.go`.
- `docs/benchmarks.md`: the new rows, with the machine and method the page already uses.

**Tests:** `bench/bench_test.go` covers the new upstream answers the way it covers chat. Run `go run ./bench run -bin ./transponder -only responses -count 3` and put the table in the pull request.

**Where:** `bench/`, `internal/dialect/openai/openai_bench_test.go`, `docs/benchmarks.md`.

**Done when:** the Responses API has numbers in `docs/benchmarks.md` that anyone can reproduce.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #192 Send Responses API calls through the demo

Labels: `enhancement`, `help wanted`

**Why:** `/v1/responses` is how the Codex CLI talks to transponder, and it is new in v0.3.0. The demo's fake provider answers only chat and embeddings (`demo/sim/upstream.go:72`), so the dashboard and Jaeger never show one.

**Behavior:**

- `demo/sim/upstream.go` answers `POST /v1/responses`, plain and streamed, after the same delay as chat, with an `output_text` message and `usage` that includes cached and reasoning tokens: in the final body when plain, in `response.completed` when streamed. Take the shapes from `internal/server/testdata/responses/`. A body that is not JSON or has no `model` is a 400.
- `demo/sim/traffic.go` sends both kinds with small weights, on a route to `steady`.
- `demo/check.sh` waits for requests on the Responses endpoint in `transponder_requests_total`. Find the `endpoint` label value in `internal/server`.
- `demo/README.md` mentions them under "What you see".

**Tests:** in `demo/sim/sim_test.go`, a plain answer has `usage`, a stream ends with `response.completed` carrying `usage`, and bad bodies give 400. Run the demo and `./check.sh`, and say so in the pull request.

**Where:** `demo/sim/upstream.go`, `traffic.go`, `sim_test.go`, `demo/check.sh`, `demo/README.md`, `demo/transponder.yaml`.

**Done when:** `./check.sh` passes on a fresh demo and Jaeger shows Responses API calls with tokens and cost.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #193 Run the demo on Postgres with two replicas

Labels: `enhancement`, `help wanted`

**Why:** Postgres with several replicas is a headline of v0.3.0, but the only place it runs is a CI test (`TestServeTwoReplicasShareABudget` in `cmd/transponder/main_test.go`). Someone deciding whether to run transponder with replicas can't see it work without writing their own setup.

**Behavior:**

- `demo/docker-compose.replicas.yml`, used with `docker compose -f docker-compose.yml -f docker-compose.replicas.yml up --build -d --wait`. The default demo does not change.
- Postgres, with the same `postgres:17-alpine` image and digest as `.github/workflows/ci.yml`, and two transponder replicas on it with `store.postgres` and a fixed `secrets_key` that the README calls public and demo-only.
- The seed (`demo/sim/seed.go`) writes the demo keys to Postgres, plus `tp_demo_admin`.
- Traffic is spread over both replicas, and Prometheus scrapes both.
- `demo/check-replicas.sh` checks that the `trial` budget runs out across both, and that a route created through `/admin/v1/routes` on one is listed by `/v1/models` on the other within 5 seconds.
- A short "Two replicas on Postgres" section in `demo/README.md`.

**Tests:** if the seed gets a Postgres path, test it in `demo/sim` with `internal/store/postgres/pgtest`, which skips without `TRANSPONDER_TEST_POSTGRES`. Run both setups and both check scripts, and say what you ran. Ask in the issue before adding a CI job.

**Where:** `demo/`, `demo/sim/seed.go`, `demo/README.md`.

**Done when:** the replicas setup comes up with one command and `check-replicas.sh` passes, while the default demo still passes `./check.sh`.

If transponder is useful to you, a star on the repo helps other people find it.

---

## #194 `make test-postgres` to run the Postgres tests locally

Labels: `enhancement`, `help wanted`

**Why:** the Postgres tests skip unless `TRANSPONDER_TEST_POSTGRES` is set, and only CI sets it (`.github/workflows/ci.yml:50`). The `Makefile` has no way to run them. Someone changing `internal/store`, the shared limit counters or the replica tests finds out about a failure only after pushing, and a test in `internal/store/storetest` that fails only on Postgres is invisible locally.

**Behavior:**

- `make test-postgres` starts a throwaway container with the same image and digest as `ci.yml`, on a free port or `PG_PORT`, and waits for `pg_isready`.
- It runs `go test -count=1 ./...` with `TRANSPONDER_TEST_POSTGRES` pointing at it.
- It removes the container when the tests end, also on failure or Ctrl+C.
- Without Docker it says Docker is needed and exits non-zero.
- If the steps don't fit the Makefile cleanly, put them in a small shell script the target calls.

**Tests:** in the pull request, show a passing run, a run with one Postgres test made to fail (the container is gone afterwards, check `docker ps -a`), and a run with Docker stopped.

**Docs:** the Tests section of `CONTRIBUTING.md` and the Commands list in `AGENTS.md`. Say that on Windows it runs from Git Bash or WSL.

**Where:** `Makefile`, an optional script, `CONTRIBUTING.md`, `AGENTS.md`.

**Done when:** one command runs the whole suite with the Postgres tests and leaves no container behind.

If transponder is useful to you, a star on the repo helps other people find it.
