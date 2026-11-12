# substrate

One CLI and one endpoint for every inference engine.

Every engine launches differently. llama.cpp, buun-llama-cpp, vLLM, SGLang
and Strata each have their own binaries, flags, readiness checks and shutdown
behavior. substrate puts a common layer over them: it launches local
inference engines and fronts them with a single OpenAI- and
Anthropic-compatible gateway with API keys, roles, quotas, usage accounting,
prompt policy and an audit trail.

It is for people and teams who run models on their own hardware (NVIDIA GPUs,
Tenstorrent cards) and want one stable endpoint for clients and agents,
instead of one launch script and one port per engine.

## Platforms

- **Linux** (x86-64, arm64) and **macOS** (Apple silicon) are supported.
- **Windows** is not supported natively (file locking and process-group
  handling are POSIX-only). Use WSL2.
- Building from source requires **Go 1.26+**.

## Install

Pick one:

1. **Release binary.** Download the archive for your platform from
   [GitHub Releases](https://github.com/LottoLottoLotto/substrate/releases)
   (`substrate_<version>_<os>_<arch>.tar.gz`), check it against
   `checksums.txt`, and put `substrate` on your `PATH`:

   ```sh
   sha256sum -c checksums.txt --ignore-missing     # macOS: shasum -a 256 -c checksums.txt --ignore-missing
   tar -xzf substrate_*_linux_amd64.tar.gz
   install -m 0755 substrate ~/.local/bin/substrate
   ```

2. **`go install`:**

   ```sh
   go install github.com/LottoLottoLotto/substrate/cmd/substrate@latest
   ```

3. **Build from a clone:**

   ```sh
   git clone https://github.com/LottoLottoLotto/substrate.git
   cd substrate
   go build -o ~/.local/bin/substrate ./cmd/substrate
   ```

Then check the setup:

```sh
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

## Features

- **One engine standard.** An engine joins substrate through a YAML manifest
  ([`docs/SPEC.md`](docs/SPEC.md)). The manifest declares how to find,
  install, launch and health-check the engine, how it maps the unified flags,
  and what it can serve. substrate has no engine-specific code
  ([Builtin engines](docs/engines.md#builtin-engines)).
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
  demand ([Gateway](docs/gateway.md)).
- **GPU placement.** Before each start, substrate checks free GPU memory,
  picks a GPU, and evicts the least recently used models if needed, so
  several models share the hardware without crashing for lack of memory
  ([Placement](docs/gateway.md#placement)). Tenstorrent chips are assigned
  whole and swapped between models
  ([Tenstorrent](docs/engines.md#tenstorrent)).
- **Production gateway.** Per-model concurrency limits with queueing,
  failover between replicas, upstream timeouts, health checks, automatic
  restart after crashes or hangs, per-client API keys with quotas, and
  Prometheus `/metrics`. `substrate service` installs it as a systemd
  service ([Operations](docs/operations.md)).
- **Access and cost control.** Keys with `user`, `auditor` and `admin` roles,
  expiry and rotation, teams, prices and monthly budgets
  ([Access](docs/access.md)).
- **Governance.** Local rule-based prompt policy and classification, a
  hash-chained audit file, retention and per-client erasure
  ([Governance](docs/governance.md)).
- **Usage logging.** The usage database records every request, its tokens
  and latency, engine start and stop events, and GPU and memory samples
  ([Usage data](docs/usage.md)).
- **Administration.** A management and settings API under `/substrate/v1/`
  and a web admin UI at `/admin/` ([Management API](docs/admin-api.md)).
- **Built for agents.** Every command can print JSON, failures have stable
  exit codes, `substrate commands --json` describes every command, and
  `substrate mcp` serves the commands as MCP tools
  ([Agents](docs/admin-api.md#agents)).
- **Benchmarks.** `substrate bench` measures a served model and publishes the
  result to LocalMaxxing ([Benchmarks](docs/benchmarks.md)).

## Documentation

| Document | Contents |
| --- | --- |
| [docs/engines.md](docs/engines.md) | builtin engines, Tenstorrent chips, locks and swapping, several models on one chip group |
| [docs/gateway.md](docs/gateway.md) | routing, limits, timeouts and health, placement, model aliases, configuration reload |
| [docs/access.md](docs/access.md) | keys, roles, quotas, output caps, teams, prices and budgets |
| [docs/governance.md](docs/governance.md) | prompt policy, classification, error kinds, data protection, retention, audit file, alerts and the admin trail |
| [docs/usage.md](docs/usage.md) | usage database tables, how tokens are counted, reporting commands |
| [docs/admin-api.md](docs/admin-api.md) | management API, settings API, admin UI, agents (exit codes, MCP) |
| [docs/operations.md](docs/operations.md) | files and the `config.yaml` reference, TLS, backup, metrics, systemd service, reliability |
| [docs/benchmarks.md](docs/benchmarks.md) | `substrate bench` and LocalMaxxing |
| [docs/SPEC.md](docs/SPEC.md) | the engine manifest specification |

Project files: [CHANGELOG.md](CHANGELOG.md), [CONTRIBUTING.md](CONTRIBUTING.md),
[SECURITY.md](SECURITY.md).

## Security defaults

- **Loopback by default.** The gateway listens on `127.0.0.1:7777`. With no
  keys and no master key it is open, and only on loopback. Binding to another
  address requires at least one key, or `--insecure`, and warns while the
  gateway serves plain HTTP.
- **Keys and roles.** Once any key exists, every request needs one. `user`
  keys do inference on the models they are allowed, `auditor` keys are
  read-only, `admin` keys do everything. Keys are stored only as SHA-256
  hashes; the master key comes from `$SUBSTRATE_API_KEY` or a file of mode
  0600, never from an argument.
- **Guarded settings API.** Changes over HTTP are refused from anything but
  loopback unless the gateway serves HTTPS, cannot set host command lines,
  and are recorded in the admin trail. `gateway.disable_settings_api: true`
  turns them off.
- **Private files.** Config and state directories are `0700`; keys, config,
  the usage database, the audit file and engine logs are `0600`. Prompts and
  outputs are not stored unless `logging.bodies` is turned on.
- **Engines are not exposed.** The client's key is never forwarded to an
  engine, and only traffic through the gateway is metered and checked.

`substrate doctor` reports security findings for the current setup. Details:
[Keys and access](docs/access.md#keys-and-access),
[Settings API](docs/admin-api.md#settings-api), [TLS](docs/operations.md#tls),
[Data protection](docs/governance.md#data-protection). To report a
vulnerability, see [SECURITY.md](SECURITY.md).

## Project status

substrate is pre-1.0: commands, flags, configuration keys, the management
API and the manifest spec may still change between releases. Changes are
listed in [CHANGELOG.md](CHANGELOG.md).
Contributions are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

substrate is licensed under the Apache License, Version 2.0. See
[LICENSE](LICENSE) and [NOTICE](NOTICE).
