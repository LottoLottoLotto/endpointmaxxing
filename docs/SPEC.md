# EndpointMaxxing engine manifest — `emx.engine/v1`

An inference engine joins emx by publishing one YAML manifest. A
manifest declares how to find the engine, install it, launch it, check that
it is ready, translate the unified flags, and what it can serve. emx
contains no engine-specific code. The builtin engines in
[`internal/spec/engines/`](../internal/spec/engines) are ordinary manifests
written against this spec.

```sh
emx engine validate my-engine.yaml   # check against the spec
emx engine add my-engine.yaml        # list the commands the manifest makes emx run
emx engine add my-engine.yaml --trust   # install into ~/.config/emx/engines/
```

A manifest is a program. Its probe runs whenever engines are looked up
(`route`, `run`, `doctor`, the gateway starting a model), not only when that
engine is chosen, and its launch, prepare, cleanup and install commands run as
you. `engine add` therefore installs nothing until you pass `--trust`; without
it, it prints every command the manifest would run and the routing priority it
claims. Only add manifests you have read.

A user manifest whose `id` matches a builtin engine replaces that builtin.

## Server contract

An engine that emx manages must:

1. Run in the foreground as one process tree, serving HTTP on the `{port}` it
   is given and the `{host}` it is given, if it accepts a host.
2. Answer `GET <health.path>` with **200 only when it can serve requests**.
   During loading it either answers non-200 or refuses connections.
3. Serve the OpenAI- or Anthropic-compatible paths that it lists in
   `capabilities.apis`.
4. Exit cleanly on `launch.stop_signal`. If it is still running after
   `launch.stop_timeout`, emx kills the process group with SIGKILL.
5. Write its logs to stdout and stderr. emx captures both in
   `~/.local/state/emx/logs/<instance>.log`.

## Fields

```yaml
spec: emx.engine/v1        # required, exact
id: my-engine                    # required, [a-z0-9][a-z0-9._-]*
name: My Engine                  # required
description: one line            # optional
homepage: https://...            # optional

binary:                          # how to find the entry point
  env: MY_ENGINE_BIN             # env var holding the path; wins over candidates
  candidates:                    # tried in order; bare names via PATH; ~, $VAR, {data}, globs expand
    - my-server
    - "{data}/engines/my-engine/bin/my-server"
  probe: [--check]               # optional: candidate counts only if `<bin> --check` exits 0
  version: [--version]           # optional: last output line is the version

vars:                            # optional extra placeholders, expanded in order of name
  root: "{bin_dir}/.."

install:                         # optional: `emx engine install <id>`
  notes: shown before the steps
  steps:                         # each runs with `sh -c` in the data dir; stops at the first failure
    - git clone https://... "{data}/engines/my-engine"

launch:
  command: ["{bin}", --model, "{model}", --host, "{host}", --port, "{port}", --name, "{name}"]
  cwd: "{root}"                  # optional
  env: {PATH: "{bin_dir}:{env.PATH}"}  # optional
  stop_signal: SIGTERM           # SIGTERM (default) | SIGINT | SIGQUIT | SIGHUP
  stop_timeout: 30s              # default 30s
  api_key_env: MY_ENGINE_API_KEY # optional: pass the unified --api-key in this environment variable, not in the command
  prepare: [[mkdir, -p, "{cache}"]]                 # optional: run before each launch; failure aborts it
  cleanup: [[docker, rm, -f, "emx-{instance}"]] # optional: run after every stop and after a crash

accelerator:                     # optional; default {kind: nvidia}
  kind: tenstorrent              # nvidia | tenstorrent
  devices: 1                     # chips per instance (models override with --devices)
  lock: emx                # emx: the engine inherits the per-chip locks; engine: it takes them itself
  reset_on_kill: true            # tt-smi -r the chips after a forced stop or a crash

host:                            # optional: a multi-model host (see below)
  load: /admin/load
  unload: /admin/unload
  models: /admin/models
  load_timeout: 45m              # default 30m

health:
  path: /health                  # required
  timeout: 10m                   # how long `run`/the gateway wait for readiness; default 10m

context:                         # optional: how clients learn a model's context window
  path: /v1/models               # GET on the running engine, answering JSON
  field: data.0.max_model_len    # dotted path of the number in it; a number selects an array element
  split_by: parallel             # optional: the registered ctx is shared between this many slots

vision:                          # optional: how clients learn whether the loaded model takes images
  path: /props                   # GET on the running engine, answering JSON
  field: modalities.vision       # true/false, or a list of input kinds that names "image"

slots:                           # optional: how clients learn how many requests the engine works on at once
  path: /props                   # GET on the running engine, answering JSON
  field: total_slots             # the number in it
  # count: 1                     # instead of path and field: an engine that always serves this many

flags:                           # unified flag -> engine arguments/env; omitted = unsupported
  ctx: [--ctx-size, "{value}"]                 # shorthand for {args: [...]}
  gpus: {env: {CUDA_VISIBLE_DEVICES: "{value}", CUDA_DEVICE_ORDER: PCI_BUS_ID}}  # placement fills {value}
  kv_cache_type:
    args: [--kv, "{value}"]
    values: {fp8: q8}                          # translate unified values; others pass through
  flash_attn: [--flash-attn]                   # bool flags: added only when true

capabilities:
  formats: [gguf]                # gguf | safetensors | strata-config | flare-core
  quantizations: ["*"]           # checkpoint quant methods accepted ("*" = any): config.json quant_method (fp8, gptq, awq, ...)
                                 # or "mlx" for MLX checkpoints; GGUF file types are not checked
  apis: [chat, completions]      # chat | completions | embeddings | responses | messages | rerank
  features: [vision]             # free-form tags, shown in `engine show`

routing:
  priority: {gguf: 60}           # per format; the highest compatible installed engine wins

memory:                          # GPU memory need, for placement (default: weights, factor 1.1, overhead 1024)
  mode: weights                  # weights | fraction | exclusive | none
  factor: 1.1                    # weights: model file size × factor, split over the GPUs
  overhead_mib: 1536             # weights and fraction: added per GPU (KV cache, CUDA context, buffers)
  # default_fraction: 0.9        # fraction: share of each GPU claimed when --mem-fraction is unset
```

### Memory modes

| Mode | Need per GPU | Use for |
| --- | --- | --- |
| `weights` | file size × `factor` ÷ GPUs + `overhead_mib` | engines that allocate what they load (llama.cpp) |
| `fraction` | `--mem-fraction` (or `default_fraction`) × GPU size + `overhead_mib` | engines that preallocate a share (vLLM, SGLang) |
| `exclusive` | the whole GPU; other emx instances on it are evicted | engines that fill free memory (Strata on CUDA, Hyperflare) |
| `none` | nothing; placement is skipped | CPU-only builds, non-NVIDIA accelerators |

After an instance becomes ready, emx measures its real GPU footprint.
For `weights` mode, the largest measurement from the last 30 days replaces
the estimate. For `fraction` mode, the larger of the measurement and the
formula is used. `model add --vram-mib N` overrides everything. A single
install can override the block in `config.yaml`, under
`engines.<id>.memory`.

### Placeholders

| Placeholder | Value |
| --- | --- |
| `{bin}`, `{bin_dir}` | resolved entry point and its directory |
| `{home}`, `{data}`, `{state}` | home, emx data dir, emx state dir |
| `{model}` | local path, or the Hugging Face repo id when the model is not downloaded |
| `{model_path}` | local file or directory. If the command uses it, launching an undownloaded repo fails |
| `{name}` | served model name, which is the API `model` id |
| `{host}`, `{port}` | listen address and port |
| `{instance}` | the instance id (e.g. for container names) |
| `{devices}`, `{device_count}` | the Tenstorrent chips placement assigned (`0,1`) and how many |
| `{assets}` | where a builtin engine's shipped files are written before each launch (`{data}/assets/<id>`) |
| `{value}` | the flag value, inside `flags` only |
| `{env.NAME}` | the environment variable `NAME` |
| `{<var>}` | anything declared in `vars`. Install overrides and models (`model add --var K=V`) replace values, and vars may reference each other. A var declared `""` must be set per model (per engine, with `engine set --var`, for a multi-model host). |

`{pre_args}`, as a whole element of `launch.command`, is replaced by the
model's `--pre-arg` list. It is used for options that must sit
mid-command, such as docker options before the image name.

`launch.api_key_env` names the environment variable the engine reads its API
key from. When set, the unified `--api-key` value is passed to the engine in
that variable and is not placed on the command line, where the host's
process list would show it; it takes precedence over `flags.api_key`. It
must be a valid environment variable name (`[A-Za-z_][A-Za-z0-9_]*`) and is
not allowed in a multi-model host manifest. Of the builtin engines, `vllm`
uses `VLLM_API_KEY`, `llama.cpp` and `buun-llama-cpp` use `LLAMA_API_KEY`,
`strata` uses `STRATA_API_KEY`, and `tt-vllm` uses `VLLM_API_KEY` (its docker
command has `-e VLLM_API_KEY`, which forwards the variable into the
container without putting its value on a command line). `sglang` takes its key
only as an argument, so its manifest launches it with a line of Python
(`python -c ...`) that reads `SGLANG_API_KEY` and appends `--api-key` to
SGLang's arguments inside the process: an engine without an environment
variable for its key can be wrapped the same way. `hyperflare` takes its key
only from a file: its manifest launches it through bash, which reads
`FLARE_API_KEY` and passes `--api-key-file <(printf %s "$key")`, a pipe only
the engine can read.

`context` tells the gateway how to report a model's context window in
`/v1/models`, so that a client does not have to guess it. For a running
instance the gateway requests `path` on the engine once and reads the number at
`field`. For a registered model that is not running it uses the registered
`ctx` flag, divided by the `split_by` flag's value where the engine shares one
context between parallel slots. The builtin `llama.cpp` and `buun-llama-cpp`
read `/props` (`default_generation_settings.n_ctx`, split by `parallel`);
`vllm` and `tt-vllm` read `max_model_len` from `/v1/models`, and so does
`hyperflare`; `strata` reads `meta.n_ctx` there. An engine without `context`
reports the registered `ctx`
only.

`slots` tells clients how many requests the engine works on at the same time,
which the gateway reports as `emx.concurrency` in `/v1/models`. Engines pick
their own number when none is given at launch, so a running instance is asked
(`path`, `field`); the builtin `llama.cpp` and `buun-llama-cpp` read
`total_slots` from `/props`, and so does `strata` (whose stopped models report the
`"parallel"` setting of their Strata config, one request at a time without it). An engine that always serves a fixed number says
so with `count` (a single-user runtime: `slots: {count: 1}`). `hyperflare` reads
`flare.max_concurrency` from `/v1/models`. Without `slots`,
the registered `parallel` flag is reported, and nothing when it is not set.

`vision` works the same way for image input: the gateway requests `path` on a
running instance and reads `field`, which is either true/false or a list of
input kinds. The builtin `llama.cpp` and `buun-llama-cpp` read
`modalities.vision` from `/props`; `strata` reads
`architecture.input_modalities` from `/v1/models`, and `hyperflare`
`flare.input_modalities` there. For a model that is not
running, and for engines without `vision`, the gateway looks at the model's
own files (a Hugging Face `config.json` with a `vision_config` section).

Validation rejects an unknown placeholder, an unknown field, an unknown
format or API, a flag that is not in the unified vocabulary, a non-bool flag
without `{value}`, a `launch.api_key_env` that is not an environment
variable name, and a command without `{port}`.

### Multi-model hosts

An engine with a `host` block keeps several models loaded in one process. It
runs as one instance per engine (the instance id is the engine id), launched
without a model, and serves every model routed to the engine:

| Request | Body | Answer |
| --- | --- | --- |
| `POST <host.load>` | `{"name": "<served name>", "path": "<local model dir>", "ctx": <tokens, 0 = default>}` | 200 once the model serves; the host may unload other models to make room |
| `POST <host.unload>` | `{"name": "<served name>"}` | 200, or 404 when the model isn't loaded |
| `GET <host.models>` | | `[{"name": "<served name>", ...}]`: the models loaded now |

The gateway routes a request to the host when `models` lists the requested
model, and calls `load` otherwise. Requests carry the served name in their
`model` field. Because the launch serves no single model, it may not use
`{model}`, `{model_path}`, `{name}` or `{pre_args}`, and `flags` must be
empty and `launch.api_key_env` unset. Each load sends the model's `--ctx`.

## Unified flags

`emx flags` prints the vocabulary and each engine's mapping. With
`--json`, each flag carries `host_only`, and an engine using
`launch.api_key_env` shows its `api_key` mapping as
`{"env": {"VLLM_API_KEY": "{value}"}}`.

| Key (manifest) | CLI | Type | Host only |
| --- | --- | --- | --- |
| `ctx` | `--ctx` | int | no |
| `gpus` | `--gpus` | string | no |
| `tp` | `--tp` | int | no |
| `gpu_layers` | `--gpu-layers` | string | no |
| `parallel` | `--parallel` | int | no |
| `mem_fraction` | `--mem-fraction` | float (0,1] | no |
| `kv_cache_type` | `--kv-cache` | string (`f16`, `bf16`, `fp8`, `q8_0`, `q4_0`, or engine-specific) | no |
| `quantization` | `--quant` | string | no |
| `dtype` | `--dtype` | string | no |
| `api_key` | `--api-key` | string | no |
| `trust_remote_code` | `--trust-remote-code` | bool | yes |
| `flash_attn` | `--flash-attn` | bool | no |
| `embeddings` | `--embeddings` | bool | no |

A host-only flag (`host_only`) lets its value choose code the engine runs or
files it reads on the host. Only the CLI on the host sets it; the settings
API refuses it (see [Settings API](admin-api.md#settings-api)). Every flag
in the vocabulary states the property.

If a launch uses a flag that the engine does not map, the launch fails. Pass
`--lenient` to drop such flags with a warning, or pass engine arguments
verbatim after `--`. Adding a key to this vocabulary is a spec change.

## Local overrides

To change a manifest's behavior for one installation, edit
`~/.config/emx/config.yaml` or run `emx engine set` instead of
forking the manifest:

```yaml
engines:
  vllm:   {bin: ~/vllm/bin/vllm}
  strata:
    bin: /opt/Strata/.venv/bin/python
    vars: {server: tt/serve/server_tt.py}   # Tenstorrent port's server
    env: {STRATA_ENGINE_READY_S: "0"}
    args: [--idle-unload, "900"]            # appended to every launch
    accelerator: {kind: tenstorrent, lock: engine}   # this build runs on Tenstorrent chips
```
