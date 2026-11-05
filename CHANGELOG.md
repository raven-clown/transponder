# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed

- Preserve every data line when forwarding multiline streaming events.

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
