# EndpointMaxxing

One CLI and one endpoint for every inference engine. The command is `emx`.

Every engine launches differently. llama.cpp, buun-llama-cpp, vLLM, SGLang
and Strata each have their own binaries, flags, readiness checks and shutdown
behavior. EndpointMaxxing puts a common layer over them: `emx` launches local
inference engines and fronts them with a single OpenAI- and
Anthropic-compatible gateway with API keys, roles, quotas, usage accounting,
prompt policy and an audit trail.

It is part of [LocalMaxxing](https://localmaxxing.com): `emx` serves models,
[`lmx`](https://github.com/LottoLottoLotto/localmaxxing-cli) measures them.

It is for people and teams who run models on their own hardware (NVIDIA GPUs,
Tenstorrent cards) and want one stable endpoint for clients and agents,
instead of one launch script and one port per engine.

## Platforms

| System | Architectures | How it is tested |
|---|---|---|
| **Linux** | x86-64, arm64 | Unit tests, and live runs with real engines on NVIDIA and Tenstorrent hardware |
| **macOS** | Apple silicon, Intel | Unit tests and an engine start/serve/stop run on every change; not yet with real engines |
| **Windows** | x86-64, arm64 | Same as macOS |

Differences on macOS and Windows are listed in
[docs/operations.md](docs/operations.md#platforms). Building from source
requires **Go 1.26+**.

## Install

Pick one:

1. **Release binary.** Download the archive for your platform from
   [GitHub Releases](https://github.com/LottoLottoLotto/endpointmaxxing/releases):
   `emx_<version>_<os>_<arch>.tar.gz` for Linux and macOS (`darwin`),
   `.zip` for Windows. Check it against `checksums.txt` and put the binary on
   your `PATH`:

   ```sh
   sha256sum -c checksums.txt --ignore-missing     # macOS: shasum -a 256 -c checksums.txt --ignore-missing
   tar -xzf emx_*_linux_amd64.tar.gz
   install -m 0755 emx ~/.local/bin/emx
   ```

   On Windows, unpack the `.zip` and put `emx.exe` in a folder on
   `PATH`.

2. **`go install`:**

   ```sh
   go install github.com/LottoLottoLotto/endpointmaxxing/cmd/emx@latest
   ```

3. **Build from a clone:**

   ```sh
   git clone https://github.com/LottoLottoLotto/endpointmaxxing.git
   cd emx
   go build -o ~/.local/bin/emx ./cmd/emx
   ```

Then check the setup:

```sh
emx doctor
```

## Quick start

```sh
emx engines                                  # what is installed
emx engine set vllm --bin ~/vllm/bin/vllm    # point at an existing install
emx engine install llama.cpp                 # or let the manifest install it

emx model add qwen35-4b ~/models/Qwen3.5-4B-GGUF                   # -> llama.cpp
emx model add qwen3-0.6b ~/models/Qwen3-0.6B --mem-fraction 0.4    # -> vllm
emx route qwen3-0.6b                         # why that engine

emx run qwen35-4b --ctx 8192 --gpu-layers all --flash-attn
emx run qwen35-4b --engine buun-llama-cpp --name q4b-buun --kv-cache fp8
emx run org/model --dry-run                  # print the exact command without starting it
emx ps
emx logs qwen35-4b -f
emx stop --all

emx serve                                    # http://127.0.0.1:7777/v1
emx chat qwen3-0.6b "hello"                  # through the gateway when it runs
emx usage --by model --since 7d
```

## Features

- **One engine standard.** An engine joins emx through a YAML manifest
  ([`docs/SPEC.md`](docs/SPEC.md)). The manifest declares how to find,
  install, launch and health-check the engine, how it maps the unified flags,
  and what it can serve. emx has no engine-specific code
  ([Builtin engines](docs/engines.md#builtin-engines)).
- **Unified flags.** Options such as `--ctx`, `--gpus`, `--tp`,
  `--kv-cache fp8` and `--mem-fraction` are translated into each engine's own
  flags. If an engine has no equivalent for a flag, emx stops with an
  error instead of silently ignoring it.
- **Capability routing.** emx inspects each model: it reads GGUF
  metadata, checks a checkpoint's `config.json` for its quantization (GPTQ,
  FP8, AWQ, MLX and others), and recognizes Strata configs. It then picks the
  best installed engine that can load the model.
- **Lifecycle management.** `run`, `ensure`, `ps`, `stop`, `logs` and
  `health` work for every engine. Each instance runs in its own session,
  `stop` shuts down the whole process group, and the worker processes of a
  crashed engine are cleaned up so they don't keep holding GPU memory.
- **One endpoint.** `emx serve` provides a single OpenAI- and
  Anthropic-compatible gateway. It routes each request by its `model` field
  across every engine and accelerator, and starts registered models on
  demand ([Gateway](docs/gateway.md)).
- **Several machines.** Gateways on different machines form a mesh: a
  client can use any of them, and each request is served by a machine that
  has the model, started there if needed. One machine holds the keys
  ([Mesh](docs/mesh.md)).
- **GPU placement.** Before each start, emx checks free GPU memory,
  picks a GPU, and evicts the least recently used models if needed, so
  several models share the hardware without crashing for lack of memory
  ([Placement](docs/gateway.md#placement)). Tenstorrent chips are assigned
  whole and swapped between models
  ([Tenstorrent](docs/engines.md#tenstorrent)).
- **Production gateway.** Per-model concurrency limits with queueing,
  failover between replicas, upstream timeouts, health checks, automatic
  restart after crashes or hangs, per-client API keys with quotas, and
  Prometheus `/metrics`. `emx service` installs it as a systemd
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
- **Administration.** A management and settings API under `/emx/v1/`
  and a web admin UI at `/admin/` ([Management API](docs/admin-api.md)).
- **Built for agents.** Every command can print JSON, failures have stable
  exit codes, `emx commands --json` describes every command, and
  `emx mcp` serves the commands as MCP tools
  ([Agents](docs/admin-api.md#agents)).
- **Benchmarks.** `emx bench` measures a served model and publishes the
  result to LocalMaxxing ([Benchmarks](docs/benchmarks.md)).
- **Rent your models out.** With `lmx rent host`, a LocalMaxxing rental
  listing serves renters from this gateway over one outbound connection (no
  open ports), billed per token. Step-by-step guide, from install to payout:
  [Host a model on LocalMaxxing](https://www.localmaxxing.com/hosting)
  ([source](https://github.com/LottoLottoLotto/localmaxxing-cli/blob/main/cmd/lmx/skill/references/hosting.md);
  agents get it with `lmx skill`). The emx side: a delegated key per host
  agent ([Access](docs/access.md)) and `gateway.cache_isolation`
  ([Gateway](docs/gateway.md)).

## Documentation

| Document | Contents |
| --- | --- |
| [docs/engines.md](docs/engines.md) | builtin engines, Tenstorrent chips, locks and swapping, several models on one chip group |
| [docs/gateway.md](docs/gateway.md) | routing, limits, timeouts and health, placement, model aliases, configuration reload |
| [docs/mesh.md](docs/mesh.md) | several machines as one endpoint: setup, routing, shared keys, limits, security |
| [docs/access.md](docs/access.md) | keys, roles, quotas, output caps, teams, prices and budgets |
| [docs/governance.md](docs/governance.md) | prompt policy, classification, error kinds, data protection, retention, audit file, alerts and the admin trail |
| [docs/usage.md](docs/usage.md) | usage database tables, how tokens are counted, reporting commands |
| [docs/admin-api.md](docs/admin-api.md) | management API, settings API, admin UI, agents (exit codes, MCP) |
| [docs/operations.md](docs/operations.md) | files and the `config.yaml` reference, TLS, backup, metrics, systemd service, reliability |
| [docs/benchmarks.md](docs/benchmarks.md) | `emx bench` and LocalMaxxing |
| [docs/SPEC.md](docs/SPEC.md) | the engine manifest specification |

Project files: [CHANGELOG.md](CHANGELOG.md), [CONTRIBUTING.md](CONTRIBUTING.md),
[SECURITY.md](SECURITY.md).

## Security defaults

- **Loopback by default.** The gateway listens on `127.0.0.1:7777`. With no
  keys and no master key it is open (every caller is admin), only when it
  was started on loopback, and only for requests addressed to a loopback
  host that do not come from another web origin. Binding to another address
  requires at least one key, or `--insecure`, and warns while the gateway
  serves plain HTTP. Removing the last key does not open such a gateway.
- **Keys and roles.** Once any key exists, every request needs one. `user`
  keys do inference on the models they are allowed, `auditor` keys are
  read-only, `admin` keys do everything. Keys are stored only as SHA-256
  hashes; the master key comes from `$EMX_API_KEY` or a file of mode
  0600, never from an argument.
- **Guarded settings API.** Changes over HTTP are refused from anything but
  loopback unless the gateway serves HTTPS, must be JSON, cannot set host
  command lines, host-only flags or environment references, and are
  recorded in the admin trail. `gateway.disable_settings_api: true` turns
  them off.
- **Private files.** Config and state directories are `0700`; keys, config,
  the usage database, the audit file and engine logs are `0600`. Prompts and
  outputs are not stored unless `logging.bodies` is turned on. The audit
  file is hash-chained and can be keyed (`gateway.audit_key_file`).
- **Engines are not exposed.** The client's key is never forwarded to an
  engine, and only traffic through the gateway is metered and checked.
  Instance listings mask environment values and secret arguments.

`emx doctor` reports security findings for the current setup. Details:
[Keys and access](docs/access.md#keys-and-access),
[Settings API](docs/admin-api.md#settings-api), [TLS](docs/operations.md#tls),
[Data protection](docs/governance.md#data-protection). To report a
vulnerability, see [SECURITY.md](SECURITY.md).

## Project status

emx is pre-1.0: commands, flags, configuration keys, the management
API and the manifest spec may still change between releases. Changes are
listed in [CHANGELOG.md](CHANGELOG.md).
Contributions are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

emx is licensed under the Apache License, Version 2.0. See
[LICENSE](LICENSE) and [NOTICE](NOTICE).
