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
  Anthropic-compatible gateway. It routes each request by its `model` field,
  starts registered models on demand, and swaps models on a shared GPU.
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

- Requests go to a ready instance whose name matches the request's `model`.
  When several instances serve the same model, requests rotate between them.
- A request for a registered model with no running instance starts that
  model. The start keeps going even if the client that triggered it
  disconnects.
- With `swap` on (the default), starting a model first stops the other
  models the gateway itself started, once their in-flight requests finish.
  Models started from the CLI or by MCP are never stopped this way.
- A request without a `model` goes to the only ready instance, if exactly one
  is ready.
- A request for an API the engine doesn't implement is rejected with a clear
  error.
- `model add` and `engine set` take effect without restarting the gateway.

The gateway also offers a management API under `/substrate/v1/` for agents
that only speak HTTP:

| Method | Path | Body or query | Result |
| --- | --- | --- | --- |
| GET | `/substrate/v1/instances` | | instances with status and in-flight requests |
| POST | `/substrate/v1/ensure` | `{"model", "engine"?, "timeout"?}` | a ready instance (reused or started) |
| POST | `/substrate/v1/stop` | `{"id"}` (instance id or model name) | waits for in-flight requests, then stops |
| GET | `/substrate/v1/usage` | `by`, `since`, `until`, `model`, `engine`, `instance`, `client` | aggregates |
| GET | `/substrate/v1/requests` | same filters, plus `limit` | request records |
| GET | `/substrate/v1/events` | same filters, plus `limit` | lifecycle events |

On security and shutdown:

- Binding to an address other than loopback requires `--api-key` or
  `$SUBSTRATE_API_KEY`; `--insecure` overrides this.
- The client's key is never forwarded. The engine receives its own key, set
  with `--api-key` at launch.
- On SIGINT or SIGTERM, the gateway waits up to 30 seconds for in-flight
  streams before it exits. Engines keep running after the gateway stops.

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
| `~/.config/substrate/config.yaml` | `gateway`, `logging`, `prefer` (engine order per format), per-engine overrides |
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
  swap: true
  stream_usage: true
logging:
  enabled: true
  bodies: false          # true stores prompts and outputs
  body_max_bytes: 1048576
  sample_interval: 30s   # 0s disables GPU/memory sampling
  retention_days: 0      # >0 prunes daily while the gateway runs
prefer:
  gguf: [buun-llama-cpp, llama.cpp]
```

## Builtin engines

| Engine | Formats | Notes |
| --- | --- | --- |
| `llama.cpp` | gguf | `llama-server` |
| `buun-llama-cpp` | gguf, safetensors | VBR/TurboQuant/TCQ KV cache; `--kv-cache vbr` is passed through |
| `vllm` | safetensors (gguf at low priority) | the venv's `bin/` is put on PATH (ninja for JIT kernels) |
| `sglang` | safetensors (gguf at low priority) | entry point is a python that can import sglang |
| `strata` | strata-config | the model is the `strata-<model>.json` that Strata's setup writes |
