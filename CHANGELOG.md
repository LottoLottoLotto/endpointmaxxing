# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-11-16

### Added

- `gateway.cache_isolation`: clients cannot see each other's prompt-cache
  hits, with any engine. Cached-token counts in responses are lowered to
  what the client's own earlier prompts explain (the engine's count is kept
  as `cached_reported`), and answers are held back for the time computing
  the rest of the prompt takes, so another client's cache does not make
  them start sooner. See docs/gateway.md.
- Delegating keys (`emx key add --delegate`): a key that acts for clients of
  its own names them in `X-Emx-Client`; each is recorded as `sub_client`
  and is its own cache domain. Mesh calls carry it.
- The usage database moves to schema version 8 (`sub_client`,
  `cached_reported`).
- `/v1/models` shows each model's `emx price` as `emx.price` to admin and
  auditor keys, so a rental listing can be priced from it (`lmx rent
  publish`).

## [0.2.1] - 2026-11-15

### Added

- Mesh routing compares how soon each machine would answer: requests ahead
  per slot over the model's measured decode speed there, so a fast GPU is
  chosen over an idle CPU.
- A machine that answers 5xx (it could not start the model, say) is
  followed by the next machine that has the model.
- `X-Emx-Node` on an inference request names the machine to serve it.
- Management calls take `?node=NAME` to act on another machine, relayed
  with the caller's role and checked again there. New commands: `emx mesh
  status`, `ps`, `run`, `stop`, `usage` (`emx mesh` alone was the status).
- Changes to keys, teams, prices and aliases can be made through any
  machine's API; it takes them to the machine that holds them. That
  machine tells the others to copy at once instead of at their next poll.
- `entered=1` / `emx usage --entered` leaves out forwarded requests, so
  machine totals add up; `emx mesh usage` sums them.
- Admin UI: a Mesh page with each machine's status, instances and usage.

### Fixed

- A relayed management call or forwarded request to a peer that accepts
  the connection but never answers no longer waits forever: relayed calls
  stop after a minute (model starts excepted), forwarded requests after
  `gateway.upstream_header_timeout`.
- `emx service install` suggested `loginctl enable-linger` when lingering
  was already on.

- The resource sampler logged the same GPU listing error every sample on a
  machine where `nvidia-smi` is installed but has no working driver; it is
  logged once until it changes.

## [0.2.0] - 2026-11-14

### Added

- Mesh: gateways on several machines form one endpoint (`mesh` in
  `config.yaml`, [docs/mesh.md](docs/mesh.md)). A client can send any
  request to any machine. The request is served by this machine or by a peer
  that has the model ready (the least busy first), or started on one that
  can start it. `/v1/models` lists the models of every machine, with
  `emx.nodes`. Calls between machines are signed with a shared secret
  (HMAC-SHA256, 60 s window, no replays).
- One machine holds the keys, teams, prices and aliases; the others copy
  them and refuse local changes (409). Daily token quotas and team budgets
  count use on every machine.
- `emx mesh` and `GET /emx/v1/mesh` show the machines, whether they answer,
  and what each serves. `emx doctor` checks the mesh secret and warns about
  plain HTTP between machines.
- Request records have `node` (where a forwarded request was served) and
  `mesh_from` (where it came from); `emx usage --by node`. The usage
  database moves to schema version 7.

### Fixed

- Admin UI: the State column of the models page showed
  `[object HTMLSpanElement]` instead of the status of running instances.

## [0.1.5] - 2026-11-14

### Security

- An engine key registered with a model (`flags.api_key`) is no longer
  returned by `GET /emx/v1/models` (readable by auditors), `emx models --json`
  or the MCP model list; it is shown as `***`.
- `emx run --dry-run` masks keys and tokens in the plan (also ones that come
  from the model's registration or the engine's `env`); through MCP every
  environment value is masked.
- Secrets an engine prints into its own log (SGLang logs its arguments) are
  replaced in `emx logs`, in the error a client gets when a start fails, in
  event records, and in the log file once the engine has stopped.
- SGLang's API key is passed in `SGLANG_API_KEY` instead of on its command
  line.

### Fixed

- The gateway no longer restarts a model that `emx stop` (run from another
  process) stopped on purpose: it could notice the engine gone before the
  stop had finished and treat it as a crash, leaving an engine running that
  nothing tracked.

## [0.1.4] - 2026-11-14

### Added

- Strata models report their concurrency: a running Strata is asked
  (`total_slots`), and a stopped one reads the `"parallel"` setting of its
  config (one request at a time without it).

### Fixed

- A Strata model whose weight files were deleted is reported as
  `files missing` and left out of `/v1/models`; only the config file was
  checked before.

## [0.1.3] - 2026-11-14

### Added

- `/v1/models` tells clients how many requests share each model
  (`emx.concurrency`: `mode` single or shared, how many run at once, the
  engine's slots, the gateway's limit, and how many are running and queued
  now). Engine manifests say how to learn it with a `slots` block. `emx models`
  shows a PARALLEL column and the admin UI an "At once" column.

## [0.1.2] - 2026-11-14

### Added

- `/v1/models` says more about each model: whether it takes images
  (`architecture.input_modalities`), the longest reply (`limits.max_output_tokens`),
  and under `emx`: the request formats it serves, its last load time, its
  measured speed over 30 days, and the hardware it needs with whether that is
  free now. Engine manifests can say where to read image support with a
  `vision` block.

## [0.1.1] - 2026-11-14

### Added

- `/v1/models` reports each model's context window (`max_model_len` and
  `context_length`): what the running engine reports, else what the model is
  registered to start with. Engine manifests say where to read it with the new
  `context` block.
- `emx models` shows CONTEXT and STATUS columns (`--json`: `context`,
  `unavailable`); the admin UI shows the context too.

### Changed

- `/v1/models` no longer lists registered models that cannot be started (files
  missing, or no installed engine serves them) or aliases pointing at them;
  `?unavailable=1` includes them with the reason. The `emx.source_missing`
  field is replaced by `emx.unavailable`.

### Fixed

- A binary built with `go install ...@<version>` reports that version instead
  of `dev`.

## [0.1.0] - 2026-11-13

The first public release.

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
- `emx serve`: one OpenAI- and Anthropic-compatible gateway
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
  audit file with `emx audit verify`.
- Management API and settings API under `/emx/v1/`, a web admin UI at
  `/admin/`, alert webhooks and an admin trail of every administrative
  change.
- Agent support: JSON output, stable exit codes, `emx commands --json`
  and `emx mcp` (every command as an MCP tool).
- Operations: TLS with certificate reload and optional mutual TLS,
  Prometheus `/metrics`, `emx service` (systemd user unit),
  `emx backup` and `emx doctor`.
- `emx bench`: measures a served model and publishes results to
  LocalMaxxing through `lmx`.

### Changed

Entries marked "breaking" can break existing clients.

- Breaking: every POST, PUT, PATCH or DELETE under `/emx/v1/` with a
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
  `X-Emx-Request-Id` and `X-Request-Id`; the client's value is stored
  as `client_request_id`, and `requests?id=` matches either.
- Breaking: instance output (`GET /emx/v1/instances`, the `ensure`
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
  counted in the new metric `emx_auth_denials_unstored_total`. New
  gauge `emx_request_body_buffered_bytes`.
- Usage database schema version 6 (`requests.client_request_id`); older
  binaries refuse a migrated database.
- Manifest field `launch.api_key_env`; `vllm` (`VLLM_API_KEY`), `llama.cpp`
  and `buun-llama-cpp` (`LLAMA_API_KEY`) now receive the engine key in the
  environment. `emx flags --json` reports `host_only` per flag.
- `emx key rm` and `key disable` warn on stderr when no usable key is
  left.
- `emx backup --audit` also archives the audit head anchor.
- A missing model source is reported as written
  (`~/x/m.gguf: no such file or directory`).

### Security

- `emx mcp` no longer offers inputs that choose what the host runs or
  reads (`engine add`, `engine set`, engine arguments, `--var`, `--pre-arg`,
  `--trust-remote-code`, file-path flags) unless started with `--allow-host`.
  Breaking for agents that used them.
- `emx engine add` lists the commands a manifest makes emx run
  and installs it only with `--trust`. Breaking: scripts must add the flag.
- `bench run` passes the model's API key to `lmx` in `LMX_MODEL_API_KEY`
  instead of on its command line; an `lmx` without that feature is refused
  unless `--key-in-args` is given.
- Request-body memory is counted as bytes arrive and one key may hold at most
  half of `gateway.max_buffered_mb`, so idle uploads or one key cannot make
  the gateway answer 503 to everyone.
- The `tt-multihost` server accepts only `enable_thinking` and `thinking` in
  `chat_template_kwargs` (a client can no longer supply a chat template),
  requires `Content-Type: application/json` on POST, answers only when
  addressed by IP address or as localhost, and caps bodies at 64 MiB.
- `emx logs` no longer reads files outside the logs directory.
- A model file with endlessly nested metadata arrays is refused instead of
  crashing the process; names taken from model files are cut to 64 printable
  characters.
- Human-readable output shows control characters as `\xNN` instead of
  sending them to the terminal, and `--csv` output prefixes cells that a
  spreadsheet would run as formulas with an apostrophe.
- `emx backup` never removes or overwrites an existing file at `--out`.
- Tenstorrent chip locks: a symbolic link in a lock's place is not followed,
  an unreadable lock counts as held, and a chip reset holds the chip's lock.
- `audit verify` fails when the oldest lines were deleted (retention records
  what it removed in `<audit file>.first`); new `--expect-first`.
- Windows: stopping an engine whose main process already exited no longer
  reaches processes that later reused its process id.
- Release workflow: actions pinned to commits, tag names validated and passed
  through the environment.
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
- Settings API: `POST /emx/v1/models` refuses host-only flags
  (`trust_remote_code`) and `$` in `source`.
- Instance ids are validated (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`), so
  `stop` and lookups cannot leave the instances directory.
- Engine API keys no longer appear on the command line for `vllm`,
  `llama.cpp`, `buun-llama-cpp`, `strata` and `tt-vllm`; `sglang` still
  receives `--api-key` there.
- Audit file: lines carry `seq`, the chain head is recorded in
  `<audit file>.head`, and `gateway.audit_key_file` keys the chain
  (HMAC-SHA256). `emx audit verify` detects a truncated or rewritten
  tail and gained `--key-file`, `--expect-head` and `--expect-count`.
- Webhook delivery failures are logged without the URL's path.

[Unreleased]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.1.5...v0.2.0
[0.1.5]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/LottoLottoLotto/endpointmaxxing/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/LottoLottoLotto/endpointmaxxing/releases/tag/v0.1.0
