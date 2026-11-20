# Failure modes

What can go wrong around transponder, what it does then, and the test that holds it to that. Paths are relative to the repository root.

## Upstream down

A target that cannot be reached, answers 5xx, 429, 408, 401 or 403, or closes before it answers, is skipped, and the next target of the route is tried. The response carries `X-Transponder-Fallbacks` with the number of skipped targets. A 400, 404, 413 or 422 from the provider is the caller's own mistake, which another provider would not fix, so it goes back to the caller as is. When every target fails, the caller gets 502 `upstream_error` naming the last provider.

After `settings.cooldown_failures` failures in a row (default 3), a target is put last in every route for `settings.cooldown` (default 30s). It is still tried when every other target has failed, and one success ends the cooldown. The cooldown is measured on the monotonic clock, and a config reload keeps it.

| Behaviour | Test |
|---|---|
| Fall back on 5xx, 429, 529, 401 and a closed connection | `TestFallbackOnServerError`, `TestFallbackReasons` in `internal/server/fallback_test.go` |
| No fallback on the caller's own bad request | `TestNoFallbackOnBadRequest` in `internal/server/fallback_test.go` |
| Every target fails | `TestAllTargetsFail` in `internal/server/fallback_test.go` |
| Cooldown order, end and restart | `TestCooldown` in `internal/router/router_test.go`, `TestCooldownSkipsFailingTarget` in `internal/server/fallback_test.go` |
| A reload keeps the cooldowns | `TestLoadLiveKeepsCooldowns` in `cmd/transponder/main_test.go`, `TestKeepHealth` in `internal/router/router_test.go` |
| The record of failing targets stays bounded | `TestHealthIsBounded` in `internal/router/router_test.go` |

## Upstream slow

`settings.attempt_timeout` (default 1m) bounds how long one target may take to send its response headers, or its first event when streaming. A target past it is skipped as above, with the reason `timeout`. Once the answer has started, a slow body is not cut by the attempt timeout. `settings.request_timeout` (default 10m) bounds the whole call, streaming included, and stops fallback when it runs out. There is no timeout between two events of a stream, since a reasoning model can be silent for minutes: a stream that stalls after its first output runs until `request_timeout`, ends with an error event, and counts as a failure toward the target's cooldown.

| Behaviour | Test |
|---|---|
| Attempt timeout before the first event | `TestAttemptTimeoutBeforeFirstEvent` in `internal/server/timeout_test.go` |
| A slow answer that has started is not cut | `TestAttemptTimeoutSparesSlowAnswers` in `internal/server/timeout_test.go` |
| Request timeout, streamed and not | `TestRequestTimeout`, `TestRequestTimeoutNonStreaming` in `internal/server/timeout_test.go` |
| The request deadline stops fallback | `TestRequestDeadlineStopsFallback` in `internal/server/fallback_test.go` |
| A stream that stalls after its first output ends at the request timeout and cools its target down | `TestStalledStream` in `internal/server/stall_test.go` |

## Partial stream

Events that carry no output yet (an OpenAI role chunk, `message_start`, a ping, an empty text block start, `response.created`, at most 64 of them) are held back until the first event that does. Until then nothing has reached the caller, so a stream that ends or sends an error, such as Anthropic's `overloaded_error` right after `message_start`, falls back like any other attempt. After that the caller already has the headers and part of the answer, so there is no fallback. The stream ends with an error event (`upstream_error`) in the caller's dialect, the status stays 200 and the usage trailers are sent, usage is recorded as estimated for what was sent, and the cut counts as a failure toward the target's cooldown. An event over 16 MiB is treated as a cut, and so is an event a translated stream cannot read.

| Behaviour | Test |
|---|---|
| Fallback before the first event | `TestStreamFallbackBeforeFirstEvent` in `internal/server/fallback_test.go` |
| Fallback after opening events that carry no output, on every streamed path | `TestStreamFailsInItsOpening` in `internal/server/opening_test.go`, `TestOpen` in `internal/server/opening_internal_test.go` |
| No fallback after output, error event sent once | `TestStreamNoFallbackAfterFirstByte` in `internal/server/fallback_test.go`, `TestStreamFailsAfterOutput` in `internal/server/opening_test.go` |
| Oversized and malformed events, before and after output, with status, headers and trailers | `TestBadUpstreamEvents` in `internal/server/hardening_test.go` |
| A finished stream whose last line holds no event is not taken for a cut | `TestReaderEndsCleanly` in `internal/sse/sse_test.go`, `TestChatStream` in `internal/provider/gemini/gemini_test.go` |
| A cut stream counts toward cooldown | `TestCutStreamsCoolTheTargetDown` in `internal/server/fallback_review_test.go` |

## Huge body

A request body over 20 MiB is refused with 413 `body_too_large`. When the caller declares the length, this happens before the body is read. A provider response over 64 MiB is refused with 502 `upstream_too_large` and is not retried on another target. A translated inline image over 5 MiB is a 400 `image_too_large`.

| Behaviour | Test |
|---|---|
| Request body over the cap, declared and not | `TestChatCompletionsBodyTooLarge`, `TestChatCompletionsDeclaredBodyTooLargeWithoutReading`, `TestChatCompletionsUnknownLengthBodyTooLarge` in `internal/server/chat_test.go`, `TestResponsesBodyTooLarge` in `internal/server/responses_test.go` |
| Provider response over the cap | `TestUpstreamBodyTooLarge` in `internal/server/embed_test.go` |
| Inline image over the cap | `TestInlineImageSizeCap` in `internal/server/images_test.go` |
| A body of about 16 MiB in any script reaches the provider unchanged, in every dialect pair | `TestUnicodeAndLargeMessages` in `internal/server/hardening_test.go` |

## Caller gone

A caller that disconnects ends the call: the upstream request is cancelled through its context, and no other target is tried. The call is recorded with status 499 and logged at info level, and the target is not counted as failing. When the caller left before the answer, what the call reserved against the caller's limits stays counted, since the provider may have done and billed the work. An admin API write whose caller leaves after the commit stays committed, and its reload runs to the end.

| Behaviour | Test |
|---|---|
| No fallback after the caller leaves | `TestNoFallbackAfterCallerLeaves` in `internal/server/fallback_test.go` |
| The upstream request is cancelled on every path, and the call is recorded as 499 without a warning | `TestCallerDisconnect` in `internal/server/disconnect_test.go` |
| An admin write's reload does not run on the caller's context | `TestCreateKey` in `internal/admin/keys_test.go`, `TestCheckWithCancelledContext` in `internal/runtime/runtime_test.go` |
| A caller that leaves during the identity provider key fetch gets 503, not 500 | `TestOIDCCallerGivesUpDuringFetch` in `internal/auth/oidc_test.go` |
| A caller that leaves is not taken for a limit store outage | `TestFailoverCancelledCaller` in `internal/limits/failover_test.go` |

## Store locked or full

**SQLite locked.** transponder writes through one connection and reads in WAL mode, so its own writes never lock each other out. When another process holds the write lock, such as a backup tool or a second transponder pointed at the same file, a write waits up to 5 seconds (`busy_timeout`) and then fails.

**Disk full.** A write fails with `database or disk is full`. What is already stored stays readable, and writes work again once there is room. An admin API write that fails is rolled back and answered 500. A usage record that cannot be written is logged and counted in `transponder_events_dropped_total`, and the call itself still succeeds.

**Postgres unreachable.** A call that cannot reach the limit counters follows `limits.on_store_error`. With `local` (the default), each replica counts locally with its share of each limit. With `deny`, the call is answered 503 `overloaded`. Each reservation is settled in the store that counted it, so a call that spans an outage neither loses spend from the shared budget nor takes it off other calls. A reload that cannot read the store keeps the config it has.

**Migrations on an old file.** Each migration runs in its own transaction with its version row, so a failed migration leaves the file as it was before that migration. A v0.2.x database upgrades in place and keeps its usage.

| Behaviour | Test |
|---|---|
| A write waits for another process's lock | `TestWriteWaitsForALockedDatabase` in `internal/store/sqlite/failure_test.go` |
| A full database fails writes, keeps reads and recovers | `TestWriteFailsWhenTheDatabaseIsFull` in `internal/store/sqlite/failure_test.go` |
| A failed usage write is counted | `TestMetricsCountDroppedUsageRecords` in `internal/server/metrics_test.go` |
| Limit store down, local and deny | `TestLimitStoreDown` in `internal/server/limits_test.go`, `TestFailoverLocalShare`, `TestFailoverDeny`, `TestAdmitWhenTheStoreFails` in `internal/limits/failover_test.go` |
| A reservation settles where it was counted | `TestFailoverSettlesWhereReserved` in `internal/limits/failover_test.go` |
| Migrations on an old file and a failed migration | `TestMigrationKeepsUsage` in `internal/store/sqlite/usage_test.go`, `TestMigrateRollsBack`, `TestMigrationsExpandAndContract` in `internal/store/sqlite/sqlite_test.go` |
| A reload when the store cannot be read | `internal/runtime/runtime_test.go` |

## Clock jump

Budgets and token allowances count per UTC day and month. When the clock jumps back across midnight, the counter stays on the later day, so the earlier day's allowance is not handed out again; a settlement dated before the jump does not change the later day. A jump forward starts the new window as the real midnight would. Cooldowns and timeouts use Go's monotonic clock, so a wall clock jump does not lengthen or shorten them. OIDC tokens are checked against the wall clock with `auth.oidc.clock_skew` (default 1m) on `exp`, `nbf` and `iat`.

| Behaviour | Test |
|---|---|
| A jump back or forward across midnight | `TestMemoryClockJumps` in `internal/limits/store_test.go` |
| Day and month windows in UTC | `TestWindows` in `internal/limits/store_test.go` |
| Token expiry with skew | `TestOIDCClockSkew` in `internal/auth/oidc_test.go` |
| A token issued in the future, malformed `nbf` or `iat` | `TestOIDCTimeClaims` in `internal/auth/oidc_test.go` |

## Bad config

A config file that does not load stops the start with an error that names the key, and `transponder --config-test` checks a file without starting. A missing `${VAR}` is an error that names it; variables are filled into the parsed YAML, so a value with `#`, quotes or newlines cannot change the file's structure.

While running, the runtime config (providers, routes, budgets, profiles) is reloaded when `config_version` changes. A config that fails to load leaves the old one serving and shows in `/admin/v1/status`, and calls already running finish on the config they started with. An admin API write that would leave a config that does not load is refused with 400 `invalid_config`. A config file whose items no longer fit the items made through the admin API is refused at start, and the stored config is kept.

| Behaviour | Test |
|---|---|
| File and environment errors | `TestLoadRejects`, `TestLoadExpandsEdgeCases`, `TestLoadIntEnvOverride` in `internal/config/config_test.go`, `FuzzExpand` in the same file |
| `--config-test` | `TestConfigTest` in `cmd/transponder/main_test.go` |
| A bad reload keeps the old config, in-flight calls keep theirs | `internal/runtime/runtime_test.go` |
| An admin write that breaks the config is rolled back | `TestInvalidConfigRollsBack` in `internal/admin/items_test.go` |
| A config file that conflicts with API items | `TestSyncFileKeepsTheStoreLoadable` in `internal/admin/runtime_test.go` |
| Providers left with no routes answer `model_not_found`, not echo | `TestBuildRouterProvidersWithoutRoutes` in `cmd/transponder/main_test.go` |

## Many replicas

With `store.postgres`, replicas share keys, config, usage and limit counters. Each checks `config_version` every 2 seconds, so a change made on one reaches the others within that time; the replica that took an admin write reloads before it answers. Config writes take a row lock on `config_version` first, so two writes at the same moment run one after the other and each validates against the other's change. A budget holds across replicas: two replicas cannot both spend its last part. Migrations run once when replicas start together.

A budget or token allowance added while running counts spend from the counters as they were filled at start, plus what callers spent since under a limit. Spend by a subject that had no limit at all is not counted until one applies, so a new budget can start lower than the subject's real spend this period. This is a known limit: counting every subject on every call would add several writes to each call on Postgres.

| Behaviour | Test |
|---|---|
| Two replicas share keys and usage | `TestServeTwoReplicasOnPostgres` in `cmd/transponder/main_test.go` |
| A budget holds across replicas | `TestServeTwoReplicasShareABudget` in `cmd/transponder/main_test.go`, `internal/store/postgres/counters_test.go` |
| A change reaches every replica | `TestReloadReachesEveryReplica` in `cmd/transponder/main_test.go`, `TestServeAdminWriteReachesOtherReplica` in `cmd/transponder/admin_test.go` |
| Concurrent config writes are serialised | `UpdateConfigSerialised` in `internal/store/storetest/storetest.go`, run by `TestStore` on SQLite and Postgres |
| Counters filled once from usage | `TestBuildLimiterSeedsFromUsage`, `TestBuildLimiterSharedSeedsOnce` in `cmd/transponder/main_test.go` |

## Identity provider down

With OIDC on, the issuer's keys are cached for an hour and read again for an unknown `kid` at most once every 10 seconds. A token without `kid` that no cached key verifies is treated the same way. While the provider cannot be reached, the cached keys stay in use. With no keys fetched yet, token callers get 503 `auth_unavailable` with `Retry-After`, and `/readyz` reports `oidc`. API keys keep working throughout.

| Behaviour | Test |
|---|---|
| Provider down, with and without cached keys | `TestOIDCProviderDown`, `TestOIDCProviderUnreachable` in `internal/auth/oidc_test.go`, `TestIdentityProviderDown` in `internal/server/oidc_test.go` |
| Refresh for an unknown key is bounded | `TestOIDCUnknownKeyRefreshIsBounded`, `TestOIDCConcurrentCallersShareOneFetch` in `internal/auth/oidc_test.go` |
| Key rotation, with and without `kid` | `TestOIDCRotatedKey`, `TestOIDCRotatedKeyWithoutKid` in `internal/auth/oidc_test.go` |

## Secrets key changed

Provider keys stored through the admin API are sealed under `secrets_key`. A replica whose key opens none of them exits at start with an error naming the provider and saying how to recover. A running replica keeps the config it has and reports the failed reload. To change the key, set the new `secrets_key` and the old one as `previous_secrets_key` on every replica, run `transponder secrets rotate` once, then remove `previous_secrets_key`. See "Provider keys at rest" in the README.

| Behaviour | Test |
|---|---|
| A previous key opens, a wrong key is `ErrWrongKey` | `TestPreviousKey` in `internal/secrets/secrets_test.go` |
| Sealing every stored key again in one write | `TestResealAfterSecretsKeyChange` in `internal/admin/items_test.go` |
| The `secrets rotate` command | `TestSecretsRotate` in `cmd/transponder/secrets_test.go` |
| A key that does not open blocks writes rather than corrupting them | `TestStoredKeyNeedsTheSameSecretsKey` in `internal/admin/items_test.go` |
