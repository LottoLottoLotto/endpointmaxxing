# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

The initial public feature set, to be released as 0.1.0.

### Added

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

[Unreleased]: https://github.com/LottoLottoLotto/substrate/commits/main
