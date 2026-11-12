# Gateway

[← README](../README.md) · [Documentation index](../README.md#documentation)

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

## Limits, timeouts, health

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

## Placement

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

## Model aliases

```sh
substrate alias set chat-default flash-next-iq3s   # clients ask for chat-default
substrate alias list
substrate alias rm chat-default
```

An alias (`aliases:` in `config.yaml`) is resolved before access checks,
routing and metering: the engine receives the real model name, the usage
record carries it, keys need access to the target, and `/v1/models` lists
the alias with `alias_for`. Responses say `X-Substrate-Alias`. Switching the
target takes effect within seconds through the config reload. An alias
shadows a registered model of the same name; aliases cannot point at
aliases.

## Configuration reload

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
