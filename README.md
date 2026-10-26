# substrate

One CLI and one endpoint for every inference engine.

Every engine launches differently. llama.cpp, buun-llama-cpp, vLLM, SGLang
and Strata each have their own binaries, flags, readiness checks and shutdown
behavior. substrate puts a common layer over them:

- **One engine standard.** An engine joins substrate through a YAML manifest
  ([`docs/SPEC.md`](docs/SPEC.md)). The manifest declares how to find,
  install, launch and health-check the engine, how it maps the unified flags,
  and what it can serve. substrate has no engine-specific code.
- **Unified flags.** Options such as `--ctx`, `--gpus`, `--tp`,
  `--kv-cache fp8` and `--mem-fraction` are translated into each engine's own
  flags. If an engine has no equivalent for a flag, substrate stops with an
  error instead of silently ignoring it.
- **Capability routing.** substrate inspects each model: it reads GGUF
  metadata, checks a checkpoint's `config.json` for its quantization (GPTQ,
  FP8, AWQ, MLX and others), and recognizes Strata configs. It then picks the
  best installed engine that can load the model.
- **Lifecycle management.** `run`, `ensure`, `ps`, `stop`, `logs` and
  `health` work for every engine. Each instance runs in its own session,
  `stop` shuts down the whole process group, and the worker processes of a
  crashed engine are cleaned up so they don't keep holding GPU memory.
- **One endpoint.** `substrate serve` provides a single OpenAI- and
  Anthropic-compatible gateway. It routes each request by its `model` field
  across every engine and accelerator, and starts registered models on
  demand.
- **GPU placement.** Before each start, substrate checks free GPU memory,
  picks a GPU, and evicts the least recently used models if needed, so
  several models share the hardware without crashing for lack of memory
  ([Placement](#placement)).
- **Production gateway.** Per-model concurrency limits with queueing,
  failover between replicas, upstream timeouts, health checks, automatic
  restart after crashes or hangs, per-client API keys with quotas, and
  Prometheus `/metrics`. `substrate service` installs it as a systemd
  service.
- **Usage logging.** The usage database records every request, its tokens
  and latency, engine start and stop events, and GPU and memory samples
  ([Usage data](#usage-data)).
- **Built for agents.** Every command can print JSON, failures have stable
  exit codes, `substrate commands --json` describes every command, and
  `substrate mcp` serves the commands as MCP tools ([Agents](#agents)).

## Install

```sh
go build -o ~/.local/bin/substrate ./cmd/substrate
substrate doctor
```

## Quick start

```sh
substrate engines                                  # what is installed
substrate engine set vllm --bin ~/vllm/bin/vllm    # point at an existing install
substrate engine install llama.cpp                 # or let the manifest install it

substrate model add qwen35-4b ~/models/Qwen3.5-4B-GGUF                   # -> llama.cpp
substrate model add qwen3-0.6b ~/models/Qwen3-0.6B --mem-fraction 0.4    # -> vllm
substrate route qwen3-0.6b                         # why that engine

substrate run qwen35-4b --ctx 8192 --gpu-layers all --flash-attn
substrate run qwen35-4b --engine buun-llama-cpp --name q4b-buun --kv-cache fp8
substrate run org/model --dry-run                  # print the exact command without starting it
substrate ps
substrate logs qwen35-4b -f
substrate stop --all

substrate serve                                    # http://127.0.0.1:7777/v1
substrate chat qwen3-0.6b "hello"                  # through the gateway when it runs
substrate usage --by model --since 7d
```

## Gateway

`substrate serve` handles `/v1/chat/completions`, `/v1/completions`,
`/v1/responses`, `/v1/messages`, `/v1/embeddings` and `/v1/rerank`:

- Requests go to a ready instance whose name matches the request's `model`,
  whatever engine or accelerator serves it. When several instances serve the
  same model, requests rotate between them.
- If an instance can't be reached, the request moves to another replica,
  provided nothing has been sent to the client yet.
- A request for a registered model with no running instance starts that
  model, through [placement](#placement). The start keeps going even if the
  client that triggered it disconnects. Starts of different models run in
  parallel; starts of the same model are serialized.
- A request without a `model` goes to the only ready instance, if exactly one
  is ready.
- A request for an API the engine doesn't implement is rejected before
  anything is loaded.
- `model add`, `engine set` and `key add`/`key rm` take effect without
  restarting the gateway.

### Limits, timeouts, health

| Setting | Default | Effect |
| --- | --- | --- |
| `max_concurrency` | 0 (unlimited) | in-flight requests per model; a model can override it (`model add --max-concurrency`) |
| `max_queue` | 64 | requests that may wait for a slot; beyond that the gateway answers 429 with `Retry-After` |
| `queue_timeout` | 5m | longest wait for a slot (then 504) |
| `upstream_header_timeout` | 10m | longest wait for an engine's first response, prefill included |
| `upstream_idle_timeout` | 5m | aborts a stream when the engine sends nothing for this long |
| `health_interval` | 15s | ready instances are probed; after 3 consecutive failures an instance gets no more traffic |
| `restart_crashed` | true | instances the gateway started are restarted after a crash or 3 failed health checks, at most 3 times in 10 minutes |
| `idle_unload` | 0 (never) | stops evictable instances idle this long; a model can override it (`model add --idle-unload 15m`) |

### Placement

Every launch, whether from the gateway, `run`, `ensure`, `chat` or MCP,
goes through placement unless you pass `--force`:

1. **Estimate the need.** In order of precedence:
   - a declared size (`model add --vram-mib`);
   - the manifest's `fraction` mode: vLLM and SGLang claim
     `--mem-fraction` of each GPU;
   - the largest footprint substrate measured for this model and engine in
     the last 30 days (recorded each time an instance becomes ready);
   - the `weights` rule: file size × factor + overhead.

   `exclusive` engines take whole GPUs, and `none` engines use no NVIDIA
   memory at all (for example Strata built for a Tenstorrent card).
2. **Choose GPUs.** The free memory per GPU (`nvidia-smi`) minus what
   instances still loading have reserved. `--tp N` needs N GPUs; `--gpus`
   pins them. The chosen GPUs are passed as `CUDA_VISIBLE_DEVICES`, with
   `CUDA_DEVICE_ORDER=PCI_BUS_ID` so the numbers match `nvidia-smi`.
3. **Evict if needed.** If nothing fits, the gateway stops evictable
   instances: idle ones first, then the least recently used, and only as
   many as necessary. It waits for their in-flight requests to finish first.
   Owners in `evict_owners` (default `gateway`) are evictable; models you
   started yourself are not. The CLI and MCP never evict: they fail with
   code `capacity` and name what would have to stop.

Placement modes (`gateway.placement` or `serve --placement`):

- `memory` (the default) is the behavior above.
- `exclusive` runs one model per GPU.
- `off` skips all checks.

`substrate run <model> --dry-run` shows the decision, for example
`placement: GPU 0, 9830 MiB each (0.40 of each GPU)`.

### Tenstorrent

Today's Tenstorrent runtimes require each chip to be owned by one process.
So substrate treats chips as whole devices, and a model on a busy chip waits
for its turn to swap in instead of sharing the chip:

- **Chip assignment.** A Tenstorrent engine's definition declares
  `accelerator: {kind: tenstorrent}`. Each instance gets whole chips: one by
  default, or `model add --devices N`.
  - Multi-chip models get neighboring chips, aligned to the chip count. On a
    32-chip Galaxy, an 8-chip model gets a whole tray.
  - The chosen chips go to the engine as `{devices}`, for example
    `TT_VISIBLE_DEVICES`.
- **Sharing the card with other jobs.** substrate uses the same per-chip
  lock files as other jobs on the card (`tenstorrent.lock`, with per-chip
  overrides in `tenstorrent.locks`). It also treats a chip as busy while
  `/proc/driver/tenstorrent/<n>/pids` lists any process.
  - Docker-based engines (`lock: substrate`) inherit the lock, so it lives
    exactly as long as the engine.
  - Engines that lock the chip themselves (`lock: engine`, such as Strata's
    `tt-engine.sh`) are only checked before starting.
- **Swapping.** When a model needs chips another model holds, its requests
  wait. The resident model gives up its chips only when one of these is true:
  - it has been loaded for at least `gateway.swap.min_residency` (default
    30s) and has no requests in flight or queued;
  - it has held the chips for `gateway.swap.max_residency` (default 10m)
    while other models wait.

  Waiting models swap in first come, first served. A request that waits
  longer than `gateway.swap.wait_timeout` (default 15m) gets a 504 with
  `Retry-After` set to the model's last measured load time. Chips held by
  another team's job are waited for in the same way.
- **Cleanup.** After a forced stop or a crash, substrate removes the container
  (`launch.cleanup`) and resets the chips (`tt-smi -r`).
- **Models.** The builtin `tt-vllm` engine runs one tt-metal vLLM image per
  model:

```sh
substrate model add lfm2.5-24b /var/tmp/lfm25/quant/ckpt --engine tt-vllm \
  --var image=lfm25-p150:dev-m12 --var cache=/var/tmp/lfm25/cache --ctx 32768 \
  --pre-arg -e --pre-arg LFM_FAST_DECODE=1 -- --block-size 64 --enable-prefix-caching ...
```

  - `--var` sets the engine's placeholders: `image` (required), `cache`
    (tensor cache, which must be specific to the checkpoint), `kernel_cache`,
    and `mesh` (`MESH_DEVICE`, for example `P150x4` for multi-chip models).
  - `--pre-arg` adds docker options before the image name.
  - Arguments after `--` go to vLLM.

`substrate doctor` lists each chip with its lock file, who holds it, and
which processes have it open. The device column of `substrate ps` shows
`tt:<chips>`.

#### Several models on one chip group

Swapping a model onto a chip takes 30 seconds to minutes. The builtin
`tt-multihost` engine avoids that for models tt-metal's `tt_transformers`
supports (Llama 3.x, Qwen2.5, Qwen3, Mistral, ...): one container owns the
chips and keeps several models loaded in device memory, so switching between
them costs nothing.

```sh
substrate engine set tt-multihost --var image=lfm25-p150:dev-m12 --var models=/var/tmp/tt-multihost/models
substrate model add qwen2.5-3b /var/tmp/tt-multihost/models/Qwen2.5-3B-Instruct --engine tt-multihost
substrate model add qwen2.5-coder-0.5b /var/tmp/tt-multihost/models/Qwen2.5-Coder-0.5B-Instruct --engine tt-multihost
```

- The first request for a model starts the host (when it isn't running) and
  loads the model into it. Later requests for any loaded model go straight to
  the host.
- A model's first load converts its weights (minutes); later loads read the
  converted copy from `vars.cache` and take seconds.
- When device memory runs short, the host unloads its least recently used
  model. `substrate stop <model>` unloads one; `substrate stop tt-multihost`
  stops the host.
- `vars.models` is a directory holding every hosted checkpoint, mounted
  read-only at the same path. `--ctx` sets a model's context (default
  `vars.ctx`, 4096). `vars.mesh` sets the chip group shape (`P150x4`, ...).
- To other Tenstorrent models the host is one instance: it swaps chips with
  them under the rules above.
- `substrate ps` shows the models the host holds.

Measured on one P150: a warm host start plus first load takes about 7s, a warm
load 1–3s, and switching between loaded models adds no time to a request.

### Management API

For agents that only speak HTTP, the gateway also offers a management API
under `/substrate/v1/`:

| Method | Path | Body or query | Result |
| --- | --- | --- | --- |
| GET | `/substrate/v1/instances` | | instances with status, in-flight requests, idle state, last use |
| POST | `/substrate/v1/ensure` | `{"model", "engine"?, "timeout"?}` | a ready instance (placed, evicting if needed) |
| POST | `/substrate/v1/stop` | `{"id"}` (instance id or model name) | waits for in-flight requests, then stops |
| GET | `/substrate/v1/usage` | `by`, `since`, `until`, `model`, `engine`, `instance`, `client`, `team`, `category`, `error_kind`, `errors`, `flagged`, `min_severity`, `rule` | aggregates (with cost) |
| GET | `/substrate/v1/requests` | same filters, plus `limit` | request records |
| GET | `/substrate/v1/events` | same filters, plus `limit` | lifecycle events, budget alerts, config reloads |
| GET | `/substrate/v1/flags` | same filters | policy matches by rule and detector |
| GET | `/substrate/v1/errors` | same filters | failed requests by error kind |
| GET | `/substrate/v1/report` | same filters (`since` defaults to `24h`) | totals, cost, models, teams, clients, categories, flags, errors |
| GET | `/substrate/v1/keys` | | keys with role, team, expiry, status and last use (no secrets) |
| GET | `/substrate/v1/teams` | | team limits and their use this day and month |
| POST | `/substrate/v1/policy/test` | `{"text"}` or `{"body"}`, optionally `model`, `client`, `api` | category and flags, nothing sent to a model |
| GET / POST | `/substrate/v1/reload` | | last reload outcome / re-read the config now (400 with the error if it is invalid) |

### Keys and access

```sh
substrate key add alice --team research --models 'qwen*' --rpm 60 --expires 90d   # prints the key once
substrate key add ops --role admin
substrate key add compliance --role auditor
substrate key list                       # role, team, expiry, last use, status
substrate key set alice --expires 2027-01-31 --tokens-per-day 2000000
substrate key rotate alice --grace 24h   # new secret printed once; the old one works for 24h
substrate key disable alice              # refused at once, history kept (key enable alice)
substrate key rm alice
```

- **Open mode:** with no keys and no master key, the gateway is open, and
  only on loopback.
- **Once any key exists,** every request needs one, sent as
  `Authorization: Bearer` or `x-api-key`.
- **`user` keys:** inference and `/v1/models`, limited to their (and their
  team's) model globs, plus `/substrate/v1/usage` and `/requests` filtered to
  their own records.
- **`auditor` keys:** read-only: every client's usage and requests, events,
  flags, errors, reports, keys, teams, `/metrics` and `policy/test`. No
  inference, and no starting, stopping or reloading.
- **`admin` keys:** everything. The master key from `--api-key` or
  `$SUBSTRATE_API_KEY` is an admin key.
- **Lifecycle:** expired and disabled keys get 401, recorded under the key's
  name (`error_kind` `auth_failed`). `key rotate` replaces the secret; the old
  one keeps working until `--grace` ends. `key list` shows when each key was
  last used, from the usage database; Prometheus exports
  `substrate_key_expiry_seconds`.
- **Quotas:** going over `rpm` or `tokens-per-day` returns 429. Usage is
  attributed to the key's name, which the client can't forge.
- **Storage:** keys are stored only as SHA-256 hashes in
  `~/.config/substrate/keys.yaml` (mode 0600).
- **Exposure:** binding to an address other than loopback requires at least
  one key, or `--insecure`, and warns while the gateway serves plain HTTP.
- **Engine keys:** the client's key is never forwarded. The engine receives
  its own key, set with `--api-key` at launch.

### TLS

```yaml
gateway:
    host: 0.0.0.0
    tls:
        cert: /etc/substrate/gateway.pem    # PEM chain
        key: /etc/substrate/gateway-key.pem
        client_ca: /etc/substrate/clients-ca.pem   # optional: require client certificates (mutual TLS)
        min_version: "1.2"                   # or "1.3"
```

or `substrate serve --tls-cert F --tls-key F [--tls-client-ca F]`. The
gateway then serves only HTTPS. Certificate and key files are re-read within
seconds of changing on disk, so a renewal (certbot, cert-manager) needs no
restart. `substrate chat` and `substrate reload` reach a TLS gateway by
trusting the system roots plus the configured certificate; under mutual TLS
they have no client certificate, and `chat` falls back to the engine.

### Teams, prices and budgets

```sh
substrate price set 'qwen3.8-*' --prompt 0.40 --completion 1.20 --cached 0.10   # per million tokens
substrate price set default --prompt 0.20 --completion 0.60 --currency EUR
substrate team set research --tokens-per-month 500000000 --budget-per-month 2000 --alert-at 0.8 --models 'qwen*'
substrate key set alice --team research
substrate team list                       # each limit's use this day/month
substrate usage --by team --since 30d     # tokens and cost per team
```

- **Prices** are per million tokens: an exact model name wins over the
  longest matching glob, then `default`. Cached prompt tokens use `cached`
  when set, else the prompt price. Local models have no invoice: the price
  is whatever the operator charges back (hardware, power) or the hosted
  equivalent. Every request records its `cost` (none when unpriced or
  unmetered); `report` and `usage` sum it.
- **Teams** group keys (`key add/set --team`). A team's `tokens_per_day`,
  `tokens_per_month` and `budget_per_month` (cost) are checked before each
  request: once used up, requests get 429 with `error_kind`
  `budget_exceeded` until the next local day or month. Limits are checked
  before a request and charged after it, so the request that crosses a limit
  completes. A team's `models` globs narrow what its keys may use.
- **Alerts:** crossing `alert_at` (default 80%) of any limit logs a line and
  records a `budget_alert` event, once per limit and period; Prometheus
  exports `substrate_team_budget_used_ratio` for alerting rules.

### Configuration reload

The gateway re-reads `config.yaml` and `models.yaml` within two seconds of a
change, on SIGHUP (`systemctl --user reload substrate`), on `substrate
reload`, or `POST /substrate/v1/reload`. Requests in flight finish under the
configuration they started with. An invalid file is refused: the running
configuration stays, the error is logged once and recorded as a
`config_reload_failed` event, and `substrate_config_reload_failures_total`
counts it. Policy, classification, prices, teams, limits, timeouts (except
`upstream_header_timeout`), placement, swap and logging options apply at
once; the listen address, TLS settings (certificates still renew),
`logging.enabled`, `upstream_header_timeout`, the health and sample intervals
and the `tenstorrent` section apply at the next start. Keys (`keys.yaml`) apply
on the next request, as they always have.

### Metrics and service

`GET /metrics` (admin or auditor, or anyone with `metrics_public: true`) serves
Prometheus text:

- requests and tokens per model, engine and API;
- histograms of TTFT and request duration;
- instance status, in-flight requests and queue depth;
- GPU memory and utilization, and per-instance GPU memory and RSS;
- failovers, restarts, and dropped usage records;
- policy matches per rule (`substrate_policy_flags_total`), requests per prompt category
  (`substrate_requests_by_category_total`) and failures per error kind (`substrate_errors_total`);
- cost per model and team (`substrate_cost_total`), team budget use
  (`substrate_team_budget_used_ratio`), key expiry (`substrate_key_expiry_seconds`) and config reloads.

```sh
substrate service install --enable -- --port 7777   # systemd user unit; PATH captured from this shell
substrate service status
substrate service uninstall
loginctl enable-linger $USER                         # keep it running after logout
```

The unit uses `KillMode=process`, so restarting or upgrading the gateway
leaves running engines alone, and the new gateway picks them up again. On
SIGINT or SIGTERM, the gateway waits up to 30 seconds for in-flight streams
before exiting.

## Usage data

Everything is stored in `~/.local/state/substrate/usage.db`, an SQLite
database in WAL mode. You can query it directly with any SQLite client.

| Table | One row per | Contents |
| --- | --- | --- |
| `requests` | gateway request, `substrate chat` request, or refused API key | model, instance, engine, API, stream, status, error, error kind, client, team, user agent, remote address, prompt/completion/cached/reasoning tokens, usage source, finish reason, autostart, queue ms (wait for a ready instance, including autostart), TTFT ms, duration ms, bytes in and out, prompt category, policy flags (JSON) and their highest severity, cost |
| `bodies` | request, when `logging.bodies` is on (or `logging.flagged_bodies` and the request was flagged) | request and response bodies (capped at `body_max_bytes`) |
| `events` | lifecycle change or gateway event | `start` (redacted argv), `ready` (load time), `start_failed`, `stop` (uptime), `exited` (crash, with the log tail and the number of orphaned workers killed), `budget_alert`, `config_reloaded`, `config_reload_failed` |
| `gpu_samples` | GPU per sample | memory used/total, utilization, power |
| `instance_samples` | instance per sample | GPU memory and RSS summed over the engine's whole process session, process count |

### How tokens are counted

The `usage_source` column says where each count came from:

- **`usage`**: the engine's own usage report. This covers OpenAI chat and
  completions (JSON and streams), the Responses API, Anthropic Messages
  (input tokens from `message_start`, output tokens from `message_delta`) and
  embeddings.
- **`timings`**: llama.cpp's `timings` block, used when the engine sends no
  usage report.
- **`chunks`**: an estimate from the number of streamed content events, used
  when the engine reports nothing.
- An empty `usage_source` means the request was not metered. These requests
  appear in the `UNMETERED` column of `substrate usage`.

For streamed chat and completions requests, the gateway adds
`stream_options.include_usage` to the request, so engines send a final usage
chunk and streamed tokens are counted exactly. Turn this off with
`gateway.stream_usage: false`. Clients can tag their traffic with an
`X-Substrate-Client` header (or the request's `user` field) to attribute
usage.

### Commands

```sh
substrate usage --by model|engine|instance|client|team|api|category|error_kind|flag_severity|status|day|hour|total --since 24h
substrate requests --since 1h --model qwen3-0.6b       # recent requests
substrate requests --flagged --min-severity high       # flagged requests (also --rule, --category, --errors, --error-kind)
substrate requests --id req_...                          # one request, with bodies if captured
substrate report --since 7d                              # totals, models, clients, categories, flags, errors
substrate flagged --since 7d                             # policy matches by rule and detector
substrate errors --since 7d                              # failures by kind (`--kinds` lists them)
substrate events --since 7d                              # starts, load times, crashes
substrate samples [--gpu] --since 1h                     # memory over time
substrate prune --older-than 90d
```

`usage` also reports average TTFT, average duration, and decode speed in
tokens per second. Decode speed is completion tokens divided by generation
time (total time minus TTFT), over metered streams.

Only traffic that passes through `substrate serve` or `substrate chat` is
metered. A client that calls an engine's port directly bypasses the meter.
GPU and memory samples are taken only while the gateway runs.

### Prompt policy, classification and errors

Every request the gateway proxies is inspected before it reaches an engine.
The inspection is local and rule-based: no prompt leaves the machine, and no
model is involved.

- **Policy flags.** `policy.rules` match the prompt (system, user, assistant
  and tool text, from any API) against built-in detectors, keywords (whole
  words, case-insensitive) or RE2 patterns. Each match is recorded on the
  request with its rule, detector, severity, count and a redacted sample
  (`AK****************LE`); the matched text itself is never stored, unless
  `logging.flagged_bodies` keeps flagged bodies for review.
- **Blocking.** A rule with `action: block` refuses the request with 400 and
  `error_kind` `policy_blocked`. The error names the rule, not the matched
  text, and the engine never sees the request.
- **Exceptions.** `allow` (per rule, or `policy.allow` for every rule)
  ignores individual matches that contain an allowed keyword or match an
  allowed pattern: test card numbers, the company's own e-mail domain.
  `except_models` and `except_clients` exempt models or API keys from a rule.
  `unless` skips a rule for prompts containing a term; whoever writes the
  prompt controls that, so use it against false positives, never to exempt
  users from a block (use `except_clients` for that).
- **Categories.** The last user turn is labelled with a task category
  (`coding`, `math`, `translation`, `summarization`, `data_analysis`,
  `writing`, `extraction`, `reasoning`, `question_answering`, else
  `general`) by keyword and pattern scores. `classify.categories` replaces
  the built-in set.
- **Error kinds.** Every failure gets an `error_kind`: `auth_failed`,
  `forbidden`, `rate_limited`, `budget_exceeded`, `policy_blocked`, `bad_request`,
  `model_not_found`, `no_capacity`, `engine_start_failed`,
  `engine_unreachable`, `upstream_timeout`, `engine_error`,
  `engine_rejected` or `client_disconnected` (`substrate errors --kinds`).
  Refused API keys are recorded too, with the caller's address.
- **Audit log.** `logging.audit_file` appends one JSON line per request and
  refused key (`type` `request` or `access_denied`; no bodies) for a SIEM or
  log shipper. It is reopened when rotated, so logrotate needs no
  `copytruncate`.

The defaults flag credentials (`secrets`, high), card, IBAN and social
security numbers (`financial`, high), e-mail addresses and phone numbers
(`contact_info`, low) and prompt-injection phrasing (`prompt_injection`,
medium), and block nothing. Rules and categories are read when the gateway
starts.

```yaml
policy:
    enabled: true
    scan_max_bytes: 1048576          # prompt text scanned per request
    allow:                           # matches every rule ignores
        patterns: ['@mycorp\.com$']
    rules:
        - name: secrets
          severity: high              # low, medium, high, critical
          detectors: [api_key, aws_access_key, private_key, jwt, password_assignment]
        - name: codenames
          action: block               # flag (default) or block
          severity: critical
          keywords: ["Project Falcon"]
          patterns: ['\bFALCON-\d{4}\b']
          models: [qwen3.8-27b-tt]    # optional scope: models, clients, apis (exact names)
          except_clients: [legal-team]
        - name: financial
          detectors: [credit_card, iban]
          allow:
              keywords: ["4111 1111 1111 1111"]   # test card
          unless:
              keywords: ["PCI training"]
classify:
    enabled: true
    default: general
    categories:                       # optional; replaces the built-in set
        - name: legal
          keywords: [contract, liability, NDA]
logging:
    flagged_bodies: false
    audit_file: audit.jsonl           # relative to ~/.local/state/substrate
```

`substrate policy show` lists the rules, categories and detectors.
`substrate policy test "text"` (or `--file`, `--body request.json`) shows
what a prompt would be labelled and flagged with, without sending it.
Requests recorded before this feature have no category or flags, and their
failures are reported as `unclassified`.

## Agents

- **Exit codes:**

  | Code | Meaning |
  | --- | --- |
  | 0 | ok |
  | 1 | error |
  | 2 | usage |
  | 3 | not_found |
  | 4 | timeout |
  | 5 | engine_failed |
  | 6 | conflict |
  | 7 | unsupported |
  | 8 | capacity |
  | 9 | forbidden |

  With `--json`, a failure prints `{"error": "...", "code": "not_found"}`.
- **Discovery:** `substrate commands --json` lists every command with its
  flags, their types and defaults, and the matching MCP tool name.
- **Idempotent start:** `substrate ensure <model>` returns a ready instance:
  an existing one, one that is still loading (it waits), or a newly started
  one. Call it before every use.
- **One-shot request:** `substrate chat <model> "prompt"` goes through the
  gateway when it is running. Otherwise it ensures an instance and calls it
  directly. Either way the request is metered.
- **MCP:** `substrate mcp` speaks MCP over stdio. Each command becomes a tool
  named `substrate_<command>`, for example `substrate_ensure`,
  `substrate_chat`, `substrate_usage` and `substrate_model_add`. Tool
  arguments mirror the command's flags, plus `args` for positional arguments
  and `engine_args` for arguments passed through to the engine.

  ```json
  { "mcpServers": { "substrate": { "command": "substrate", "args": ["mcp"] } } }
  ```

  For Claude Code: `claude mcp add substrate -- substrate mcp`.

## Reliability

- **Concurrent use:** config and registry writes, and instance starts, take
  file locks, so parallel CLI calls, the gateway and MCP sessions don't
  overwrite each other.
- **Validation:** `config.yaml` and `models.yaml` are parsed strictly, so
  unknown keys and invalid unified flag values are errors.
- **Process tracking:**
  - Instances are tracked by PID together with the process start time, so a
    reused PID is never mistaken for an engine.
  - If an automatically chosen port is taken during startup, substrate
    retries on a new port.
  - The previous three logs of each instance are kept as `<id>.log.1` to
    `.3`.
- **Secrets:** instance records are mode 0600. API keys are removed from
  `ps`/`run --dry-run` output, logs and events.

## Files

| Path | Contents |
| --- | --- |
| `~/.config/substrate/config.yaml` | `gateway`, `logging`, `prefer` (engine order per format), per-engine overrides (`bin`, `vars`, `env`, `args`, `memory`) |
| `~/.config/substrate/keys.yaml` | gateway API keys (hashes only, mode 0600) |
| `~/.config/substrate/models.yaml` | model registry: source, format, pinned engine, default flags, engine args |
| `~/.config/substrate/engines/*.yaml` | user manifests (new engines, or replacements for builtins) |
| `~/.local/state/substrate/instances/` | one JSON record per instance |
| `~/.local/state/substrate/logs/` | engine logs (kept after stop) |
| `~/.local/state/substrate/usage.db` | usage database |
| `~/.local/state/substrate/audit.jsonl` | audit log, when `logging.audit_file` is set |
| `~/.local/share/substrate/` | engines installed by `substrate engine install` |

Set `SUBSTRATE_HOME` to keep all of these under one directory.

Here is a `config.yaml` with every setting at its default:

```yaml
gateway:
  host: 127.0.0.1
  port: 7777
  autostart: true
  placement: memory          # memory | exclusive | off
  evict_owners: [gateway]    # whose instances placement and idle unload may stop
  idle_unload: 0s
  restart_crashed: true
  health_interval: 15s
  max_concurrency: 0
  max_queue: 64
  queue_timeout: 5m
  upstream_header_timeout: 10m
  upstream_idle_timeout: 5m
  metrics_public: false
  stream_usage: true
  swap:
    min_residency: 30s       # a model keeps its chips at least this long once ready
    max_residency: 10m       # ...and yields after this long when others wait, even with its own demand
    wait_timeout: 15m        # longest a request waits to be swapped in
logging:
  enabled: true
  bodies: false          # true stores prompts and outputs
  body_max_bytes: 1048576
  sample_interval: 30s   # 0s disables GPU/memory sampling
  retention_days: 0      # >0 prunes daily while the gateway runs
prefer:
  gguf: [buun-llama-cpp, llama.cpp]
engines:
  strata:                    # a Strata build that runs on a Tenstorrent P150
    accelerator: {kind: tenstorrent, devices: 1, lock: engine}
tenstorrent:
  lock: /var/tmp/tenstorrent/chip-{n}.lock   # every job on the card must use the same file
  locks: {0: /var/tmp/gemma4-12b/device.lock} # per-chip overrides, to match existing scripts
  tt_smi: tt-smi
```

## Builtin engines

| Engine | Formats | Notes |
| --- | --- | --- |
| `llama.cpp` | gguf | `llama-server` |
| `buun-llama-cpp` | gguf, safetensors | VBR/TurboQuant/TCQ KV cache; `--kv-cache vbr` is passed through |
| `vllm` | safetensors (gguf at low priority) | the venv's `bin/` is put on PATH (ninja for JIT kernels) |
| `sglang` | safetensors (gguf at low priority) | entry point is a python that can import sglang |
| `strata` | strata-config | the model is the `strata-<model>.json` that Strata's setup writes; a Tenstorrent build sets `engines.strata.accelerator: {kind: tenstorrent, lock: engine}` |
| `tt-vllm` | safetensors (opt-in with `--engine tt-vllm`) | tt-metal vLLM in docker, one image per model (`--var image=...`), whole Tenstorrent chips |
| `tt-multihost` | safetensors, bf16 (opt-in with `--engine tt-multihost`) | one tt-metal container keeps several `tt_transformers` models loaded on its chips; its server ships with substrate (`engine set tt-multihost --var image=... --var models=...`) |
