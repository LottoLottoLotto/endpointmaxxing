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

### Management API

For agents that only speak HTTP, the gateway also offers a management API
under `/substrate/v1/`:

| Method | Path | Body or query | Result |
| --- | --- | --- | --- |
| GET | `/substrate/v1/instances` | | instances with status, in-flight requests, idle state, last use |
| POST | `/substrate/v1/ensure` | `{"model", "engine"?, "timeout"?}` | a ready instance (placed, evicting if needed) |
| POST | `/substrate/v1/stop` | `{"id"}` (instance id or model name) | waits for in-flight requests, then stops |
| GET | `/substrate/v1/usage` | `by`, `since`, `until`, `model`, `engine`, `instance`, `client` | aggregates |
| GET | `/substrate/v1/requests` | same filters, plus `limit` | request records |
| GET | `/substrate/v1/events` | same filters, plus `limit` | lifecycle events |

### Keys and access

```sh
substrate key add alice --models 'qwen*' --rpm 60 --tokens-per-day 2000000   # prints the key once
substrate key add ops --role admin
substrate key list
substrate key rm alice
```

- **Open mode:** with no keys and no master key, the gateway is open, and
  only on loopback.
- **Once any key exists,** every request needs one, sent as
  `Authorization: Bearer` or `x-api-key`.
- **`user` keys:** inference and `/v1/models`, limited to their model globs,
  plus `/substrate/v1/usage` and `/requests` filtered to their own records.
- **`admin` keys:** everything, including the management API, `/events` and
  `/metrics`. The master key from `--api-key` or `$SUBSTRATE_API_KEY` is an
  admin key.
- **Quotas:** going over `rpm` or `tokens-per-day` returns 429. Usage is
  attributed to the key's name, which the client can't forge.
- **Storage:** keys are stored only as SHA-256 hashes in
  `~/.config/substrate/keys.yaml` (mode 0600).
- **Exposure:** binding to an address other than loopback requires at least
  one key, or `--insecure`.
- **Engine keys:** the client's key is never forwarded. The engine receives
  its own key, set with `--api-key` at launch.

### Metrics and service

`GET /metrics` (admin, or anyone with `metrics_public: true`) serves
Prometheus text:

- requests and tokens per model, engine and API;
- histograms of TTFT and request duration;
- instance status, in-flight requests and queue depth;
- GPU memory and utilization, and per-instance GPU memory and RSS;
- failovers, restarts, and dropped usage records.

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
| `requests` | gateway request, or `substrate chat` request | model, instance, engine, API, stream, status, error, client, user agent, remote address, prompt/completion/cached/reasoning tokens, usage source, finish reason, autostart, queue ms (wait for a ready instance, including autostart), TTFT ms, duration ms, bytes in and out |
| `bodies` | request, when `logging.bodies` is on | request and response bodies (capped at `body_max_bytes`) |
| `events` | lifecycle change | `start` (redacted argv), `ready` (load time), `start_failed`, `stop` (uptime), `exited` (crash, with the log tail and the number of orphaned workers killed) |
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
substrate usage --by model|engine|instance|client|api|day|hour|total --since 24h
substrate requests --since 1h --model qwen3-0.6b       # recent requests
substrate requests --id req_...                          # one request, with bodies if captured
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
