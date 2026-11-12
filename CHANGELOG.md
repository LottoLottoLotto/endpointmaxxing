# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

The initial public feature set, to be released as 0.1.0.

### Added

- Release binaries for Linux, macOS and Windows (amd64 and arm64), built and
  run on each system by the release workflow before publishing.

- Engine manifests: engines join through a YAML manifest
  ([docs/SPEC.md](docs/SPEC.md)) that declares how to find, install, launch
  and health-check them; builtin manifests for `llama.cpp`, `buun-llama-cpp`,
  `vllm`, `sglang`, `strata`, `tt-vllm` and `tt-multihost`.
- Unified flags (`--ctx`, `--gpus`, `--tp`, `--kv-cache`, `--mem-fraction`,
  ...) translated into each engine's own flags, with an error when an engine
  has no equivalent.
- Capability routing: GGUF metadata, checkpoint quantization and Strata
  configs are inspected to pick the best installed engine for a model.
- Lifecycle commands for every engine: `run`, `ensure`, `ps`, `stop`, `logs`,
  `health`, with whole-process-group shutdown and cleanup of orphaned
  workers.
- `substrate serve`: one OpenAI- and Anthropic-compatible gateway
  (`/v1/chat/completions`, `/v1/completions`, `/v1/responses`,
  `/v1/messages`, `/v1/embeddings`, `/v1/rerank`) that routes by `model`,
  starts registered models on demand and fails over between replicas.
- Gateway limits and health: per-model concurrency with queueing, upstream
  timeouts, health checks, automatic restart, idle unload, model aliases and
  configuration reload without restart.
- GPU placement: memory estimates, GPU choice and least-recently-used
  eviction, with `memory`, `exclusive` and `off` modes.
- Tenstorrent support: whole-chip assignment, shared per-chip lock files,
  swapping between models, and the `tt-multihost` engine that keeps several
  models loaded on one chip group.
- API keys with `user`, `auditor` and `admin` roles, model globs, expiry,
  rotation with a grace period, disable/enable, request and token quotas with
  rate-limit headers, and output-token caps.
- Teams, prices and budgets: per-model prices, per-request cost, team token
  and cost limits, and budget alerts.
- Usage database (SQLite): requests with tokens, latency and cost, lifecycle
  events, GPU and instance memory samples, with `usage`, `requests`,
  `report`, `flagged`, `errors`, `events`, `samples` and `prune` commands and
  CSV output.
- Prompt policy and classification: local rule-based detectors, keywords and
  patterns that flag or block requests, task categories, and stable error
  kinds.
- Data protection: owner-only files, `secure_delete`, separate usage and
  audit retention, per-client erasure, and a hash-chained, self-rotating
  audit file with `substrate audit verify`.
- Management API and settings API under `/substrate/v1/`, a web admin UI at
  `/admin/`, alert webhooks and an admin trail of every administrative
  change.
- Agent support: JSON output, stable exit codes, `substrate commands --json`
  and `substrate mcp` (every command as an MCP tool).
- Operations: TLS with certificate reload and optional mutual TLS,
  Prometheus `/metrics`, `substrate service` (systemd user unit),
  `substrate backup` and `substrate doctor`.
- `substrate bench`: measures a served model and publishes results to
  LocalMaxxing through `lmx`.

### Changed

Entries marked "breaking" can break existing clients.

- Breaking: every POST, PUT, PATCH or DELETE under `/substrate/v1/` with a
  body must send `Content-Type: application/json` (415 otherwise), including
  `ensure`, `stop`, `reload` and `policy/test`.
- Breaking: inference request bodies must be exactly one JSON object;
  repeated top-level fields and non-lower-case spellings of `model`,
  `messages`, `prompt`, `input`, `stream`, `stream_options`, `tools`,
  `system`, `instructions`, `query` and `documents` get 400.
- Breaking: with `gateway.stream_usage` on (default), the final usage chunk
  is always sent on streamed chat and completions, even when the client sent
  `stream_options.include_usage: false`.
- Breaking: inference request bodies are limited to `gateway.max_body_mb`
  (default 64 MiB, was 256 MiB; 413 beyond it).
- Breaking: a client's `X-Request-Id` is no longer the usage record's id.
  The gateway always generates the id and returns it as
  `X-Substrate-Request-Id` and `X-Request-Id`; the client's value is stored
  as `client_request_id`, and `requests?id=` matches either.
- Breaking: instance output (`GET /substrate/v1/instances`, the `ensure`
  response, `ps`, `inspect`) shows every `env` value and secret-looking
  argument values as `***`.
- Rate limits and budgets are checked before the request body is read, and
  every request of a key counts toward `rpm`, including ones refused later.
- A request without `model` is subject to the model globs and model-scoped
  policy rules of the only ready model, and the engine receives that name.
- New `gateway.max_buffered_mb` (default 1024): memory for all in-flight
  request bodies; beyond it 503 with `Retry-After: 1`. Bodies must arrive
  within 5 minutes.
- Stored `path`, `user_agent` and `client` are cut to 256 bytes, `error` to
  1024; unrouted requests with an invalid model name are recorded as model
  `invalid`; metrics count unrouted model names beyond the first 200 as
  `other`.
- At most 60 `auth_failed` (401) records are stored per minute; the rest are
  counted in the new metric `substrate_auth_denials_unstored_total`. New
  gauge `substrate_request_body_buffered_bytes`.
- Usage database schema version 6 (`requests.client_request_id`); older
  binaries refuse a migrated database.
- Manifest field `launch.api_key_env`; `vllm` (`VLLM_API_KEY`), `llama.cpp`
  and `buun-llama-cpp` (`LLAMA_API_KEY`) now receive the engine key in the
  environment. `substrate flags --json` reports `host_only` per flag.
- `substrate key rm` and `key disable` warn on stderr when no usable key is
  left.
- `substrate backup --audit` also archives the audit head anchor.
- A missing model source is reported as written
  (`~/x/m.gguf: no such file or directory`).

### Security

- Open mode is decided when `serve` starts (loopback listen address or
  `--insecure`): removing the last key from any other gateway makes it
  answer 401 instead of opening it.
- Open-mode requests must be addressed to a loopback host (unless
  `--insecure`) and must not come from another origin (`Origin`,
  `Sec-Fetch-Site: cross-site`); 403 otherwise.
- Policy rules also scan tool definitions, tool-call arguments, tool results
  and rerank `query` / `documents`; a request too large to scan
  (`policy.scan_max_bytes`) is refused when a blocking rule applies
  (detector `too_large`).
- Settings API: `POST /substrate/v1/models` refuses host-only flags
  (`trust_remote_code`) and `$` in `source`.
- Instance ids are validated (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`), so
  `stop` and lookups cannot leave the instances directory.
- Engine API keys no longer appear on the command line for `vllm`,
  `llama.cpp` and `buun-llama-cpp`; `sglang`, `strata` and `tt-vllm` still
  receive `--api-key` there.
- Audit file: lines carry `seq`, the chain head is recorded in
  `<audit file>.head`, and `gateway.audit_key_file` keys the chain
  (HMAC-SHA256). `substrate audit verify` detects a truncated or rewritten
  tail and gained `--key-file`, `--expect-head` and `--expect-count`.
- Webhook delivery failures are logged without the URL's path.

[Unreleased]: https://github.com/LottoLottoLotto/substrate/commits/main
