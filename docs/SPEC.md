# substrate engine manifest — `substrate.engine/v1`

An inference engine joins substrate by publishing one YAML manifest. A
manifest declares how to find the engine, install it, launch it, check that
it is ready, translate the unified flags, and what it can serve. substrate
contains no engine-specific code. The builtin engines in
[`internal/spec/engines/`](../internal/spec/engines) are ordinary manifests
written against this spec.

```sh
substrate engine validate my-engine.yaml   # check against the spec
substrate engine add my-engine.yaml        # install into ~/.config/substrate/engines/
```

A user manifest whose `id` matches a builtin engine replaces that builtin.

## Server contract

An engine that substrate manages must:

1. Run in the foreground as one process tree, serving HTTP on the `{port}` it
   is given and the `{host}` it is given, if it accepts a host.
2. Answer `GET <health.path>` with **200 only when it can serve requests**.
   During loading it either answers non-200 or refuses connections.
3. Serve the OpenAI- or Anthropic-compatible paths that it lists in
   `capabilities.apis`.
4. Exit cleanly on `launch.stop_signal`. If it is still running after
   `launch.stop_timeout`, substrate kills the process group with SIGKILL.
5. Write its logs to stdout and stderr. substrate captures both in
   `~/.local/state/substrate/logs/<instance>.log`.

## Fields

```yaml
spec: substrate.engine/v1        # required, exact
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

install:                         # optional: `substrate engine install <id>`
  notes: shown before the steps
  steps:                         # each runs with `sh -c` in the data dir; stops at the first failure
    - git clone https://... "{data}/engines/my-engine"

launch:
  command: ["{bin}", --model, "{model}", --host, "{host}", --port, "{port}", --name, "{name}"]
  cwd: "{root}"                  # optional
  env: {PATH: "{bin_dir}:{env.PATH}"}  # optional
  stop_signal: SIGTERM           # SIGTERM (default) | SIGINT | SIGQUIT | SIGHUP
  stop_timeout: 30s              # default 30s

health:
  path: /health                  # required
  timeout: 10m                   # how long `run`/the gateway wait for readiness; default 10m

flags:                           # unified flag -> engine arguments/env; omitted = unsupported
  ctx: [--ctx-size, "{value}"]                 # shorthand for {args: [...]}
  gpus: {env: {CUDA_VISIBLE_DEVICES: "{value}"}}
  kv_cache_type:
    args: [--kv, "{value}"]
    values: {fp8: q8}                          # translate unified values; others pass through
  flash_attn: [--flash-attn]                   # bool flags: added only when true

capabilities:
  formats: [gguf]                # gguf | safetensors | strata-config
  quantizations: ["*"]           # checkpoint quant methods accepted ("*" = any): config.json quant_method (fp8, gptq, awq, ...)
                                 # or "mlx" for MLX checkpoints; GGUF file types are not checked
  apis: [chat, completions]      # chat | completions | embeddings | responses | messages | rerank
  features: [vision]             # free-form tags, shown in `engine show`

routing:
  priority: {gguf: 60}           # per format; the highest compatible installed engine wins
```

### Placeholders

| Placeholder | Value |
| --- | --- |
| `{bin}`, `{bin_dir}` | resolved entry point and its directory |
| `{home}`, `{data}`, `{state}` | home, substrate data dir, substrate state dir |
| `{model}` | local path, or the Hugging Face repo id when the model is not downloaded |
| `{model_path}` | local file or directory. If the command uses it, launching an undownloaded repo fails |
| `{name}` | served model name, which is the API `model` id |
| `{host}`, `{port}` | listen address and port |
| `{value}` | the flag value, inside `flags` only |
| `{env.NAME}` | the environment variable `NAME` |
| `{<var>}` | anything declared in `vars` |

Validation rejects an unknown placeholder, an unknown field, an unknown
format or API, a flag that is not in the unified vocabulary, a non-bool flag
without `{value}`, and a command without `{port}`.

## Unified flags

`substrate flags` prints the vocabulary and each engine's mapping.

| Key (manifest) | CLI | Type |
| --- | --- | --- |
| `ctx` | `--ctx` | int |
| `gpus` | `--gpus` | string |
| `tp` | `--tp` | int |
| `gpu_layers` | `--gpu-layers` | string |
| `parallel` | `--parallel` | int |
| `mem_fraction` | `--mem-fraction` | float (0,1] |
| `kv_cache_type` | `--kv-cache` | string (`f16`, `bf16`, `fp8`, `q8_0`, `q4_0`, or engine-specific) |
| `quantization` | `--quant` | string |
| `dtype` | `--dtype` | string |
| `api_key` | `--api-key` | string |
| `trust_remote_code` | `--trust-remote-code` | bool |
| `flash_attn` | `--flash-attn` | bool |
| `embeddings` | `--embeddings` | bool |

If a launch uses a flag that the engine does not map, the launch fails. Pass
`--lenient` to drop such flags with a warning, or pass engine arguments
verbatim after `--`. Adding a key to this vocabulary is a spec change.

## Local overrides

To change a manifest's behavior for one installation, edit
`~/.config/substrate/config.yaml` or run `substrate engine set` instead of
forking the manifest:

```yaml
engines:
  vllm:   {bin: ~/vllm/bin/vllm}
  strata:
    bin: /opt/Strata/.venv/bin/python
    vars: {server: tt/serve/server_tt.py}   # Tenstorrent port's server
    env: {STRATA_ENGINE_READY_S: "0"}
    args: [--idle-unload, "900"]            # appended to every launch
```
