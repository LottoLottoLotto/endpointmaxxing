# Engines

[← README](../README.md) · [Documentation index](../README.md#documentation)

Engines join substrate through YAML manifests; the manifest format is
specified in [SPEC.md](SPEC.md). This page lists the builtin engines and
describes how Tenstorrent accelerators are handled.

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

## Tenstorrent

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
substrate model add lfm2.5-24b ~/models/lfm2.5-24b --engine tt-vllm \
  --var image=my-tt-image:latest --var cache=$HOME/.cache/tt/lfm2.5-24b --ctx 32768 \
  --pre-arg -e --pre-arg MY_SETTING=1 -- --block-size 64 --enable-prefix-caching ...
```

  - `my-tt-image:latest` stands for an image you build yourself from
    [tt-inference-server](https://github.com/tenstorrent/tt-inference-server);
    substrate does not ship one.
  - `--var` sets the engine's placeholders: `image` (required), `cache`
    (tensor cache, which must be specific to the checkpoint), `kernel_cache`,
    and `mesh` (`MESH_DEVICE`, for example `P150x4` for multi-chip models).
  - `--pre-arg` adds docker options before the image name.
  - Arguments after `--` go to vLLM.

`substrate doctor` lists each chip with its lock file, who holds it, and
which processes have it open. The device column of `substrate ps` shows
`tt:<chips>`.

### Several models on one chip group

Swapping a model onto a chip takes 30 seconds to minutes. The builtin
`tt-multihost` engine avoids that for models tt-metal's `tt_transformers`
supports (Llama 3.x, Qwen2.5, Qwen3, Mistral, ...): one container owns the
chips and keeps several models loaded in device memory, so switching between
them costs nothing.

```sh
substrate engine set tt-multihost --var image=my-tt-image:latest --var models=$HOME/models/tt
substrate model add qwen2.5-3b ~/models/tt/Qwen2.5-3B-Instruct --engine tt-multihost
substrate model add qwen2.5-coder-0.5b ~/models/tt/Qwen2.5-Coder-0.5B-Instruct --engine tt-multihost
```

- The image is again one you build from
  [tt-inference-server](https://github.com/tenstorrent/tt-inference-server).
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
