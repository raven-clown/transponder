# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `POST /v1/messages`, the Anthropic Messages API, so Claude Code and the Anthropic SDKs work with only a base URL change. Requests to Anthropic go through as sent, with only the model changed; streams are copied frame by frame and their usage is recorded. Requests to other providers are translated, tools included.
- `POST /v1/messages/count_tokens`, forwarded to the route's Anthropic target.
- Anthropic provider (`type: anthropic`). It forwards the caller's `anthropic-version` and `anthropic-beta`.
- Gemini provider (`type: gemini`) for chat, streams and embeddings through the Gemini API with an API key.
- OpenAI chat completions can now reach Anthropic and Gemini, and every translated call carries tools, tool calls and the system prompt.
- Prices for current Claude and Gemini models.
- `GET /healthz` answers 200 while the process is up, and `GET /readyz` answers 503 with the names of the dependencies that fail, such as `{"status":"unavailable","failed":["store"]}`, so Kubernetes and load balancers can restart the process or hold traffic. Neither needs an API key.

### Changed

- When a provider of another dialect rejects a translated request as invalid (a 4xx it would give again on a retry), the caller now gets that status and the provider's reason in its own dialect instead of a 502.


## [0.2.1] - 2026-11-07

### Added

- vLLM provider (`type: vllm`) defaulting to `http://localhost:8000/v1` and forwarding `api_key` as a bearer token. Targets using `vllm` count as free in usage costs.
- `transponder --check-update` lists the releases newer than the running one, how urgent they are and which changes touch the providers in your config. It only reads the release list and never installs anything.
- `update_check: true` logs one `update available` line at start and once a day when a newer release is out. Off by default.
- Releases come with binaries for Linux, macOS and Windows on amd64 and arm64, a `release.json` describing each change, and a container image at `ghcr.io/raven-clown/transponder`.

### Fixed

- `transponder --version` prints the commit and build time, as listed in 0.1.4. The helper was added then, but the flag still printed only the version.
- Boolean config keys can be set from the environment, such as `TRANSPONDER_UPDATE_CHECK=true`. Before, the value was read as text and the config failed to load.

## [0.2.0] - 2026-11-06

### Added

- OpenRouter provider (`type: openrouter`) defaulting to `https://openrouter.ai/api/v1`.
- An unknown provider `type` in the config now lists the known types.
- `GET /v1/models` lists the route names a caller can ask for, so clients such as Open WebUI can fill their model picker. Wildcard routes and the models a route sends upstream are left out.
- `settings.attempt_timeout` (default 60s) bounds how long a streamed call waits for its first event, and `settings.request_timeout` (default 10m) bounds every call, streams included. Calls that are not streamed get only the request timeout, since a provider sends their headers once the whole answer is ready. Before, one 10 minute limit covered everything, so a stream that never started held the caller for 10 minutes.

### Changed

- The `user` column of the `usage` table is now `user_id`, since `user` is a reserved word in Postgres. The migration runs at start and keeps every row; queries on `transponder.db` that read `user` need `user_id` instead.

### Fixed

- A provider stream that ends without `[DONE]` now ends the caller's stream with an error event and estimated usage, instead of looking complete.
- A stream line or event over 16 MiB ends the stream instead of growing memory.

## [0.1.4] - 2026-11-06

### Added

- LiteLLM Proxy provider (`type: litellm`) defaulting to `http://localhost:4000/v1` and forwarding `api_key` as a bearer token.
- `type: ollama` providers send `api_key` as a bearer token, for Ollama behind an auth proxy or hosted Ollama. Without a key no header is sent.
- `transponder --version` shows the commit and build time, for example `transponder dev (1a2b3c4, 2026-11-04T10:00:00Z)`, and `, modified` when the tree had uncommitted changes.

### Fixed

- A chat request with a wrong field type now says what to fix, such as `max_tokens must be an integer`, with `param` set to the field, instead of showing Go type names.

## [0.1.3] - 2026-11-05

### Added

- Cloudflare AI Gateway provider using its OpenAI-compatible endpoint.

## [0.1.2] - 2026-11-05

### Added

- Message `content` sent as an array of text parts is accepted and joined in order. Other part types, such as `image_url`, still return 400 `unsupported`.

### Fixed

- Restore the default signal handler after the first shutdown signal so a second Ctrl+C stops the process immediately.
- Reject requests declaring bodies larger than 20 MiB before reading them.

## [0.1.1] - 2026-11-05

### Fixed

- Preserve every data line when forwarding multiline streaming events.

## [0.1.0] - 2026-11-05

### Added

- `POST /v1/chat/completions` answered by a built-in `echo` provider, with `X-Usage-Input-Tokens`, `X-Usage-Output-Tokens` and `X-Usage-Cost-USD` headers.
- API keys checked from `Authorization: Bearer` or `x-api-key`, and a one-time admin key printed to stderr on first start.
- SQLite storage with migrations that run at startup, and a usage record for every call.
- Bootstrap config from `--config` YAML with `TRANSPONDER_` environment overrides.
- `providers` and `routes` in the config file. A route matches a model name or a glob, where `*` also matches `/`, and sends it to a provider, optionally under another model name. Providers declared with no routes are a config error.
- OpenAI and Ollama providers. OpenAI requests are forwarded with only `model` changed, so every field the API accepts keeps working. `base_url` points the OpenAI provider at any compatible server.
- Streaming chat completions, with the usage headers sent as trailers at the end of the stream.
- `POST /v1/embeddings`.
- A built-in price table for OpenAI models, so `X-Usage-Cost-USD` and the usage records carry real cost. Unpriced models are served at zero cost and logged as `unpriced`.
- Usage headers on error responses too, at zero.

[Unreleased]: https://github.com/raven-clown/transponder/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/raven-clown/transponder/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/raven-clown/transponder/compare/v0.1.4...v0.2.0
[0.1.4]: https://github.com/raven-clown/transponder/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/raven-clown/transponder/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/raven-clown/transponder/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/raven-clown/transponder/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/raven-clown/transponder/releases/tag/v0.1.0
