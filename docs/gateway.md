# Gateway

[← README](../README.md) · [Documentation index](../README.md#documentation)

`emx serve` handles `/v1/chat/completions`, `/v1/completions`,
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
- A request without a `model` goes to the only ready model. The key's and
  team's model globs and model-scoped policy rules apply to that model, and
  the engine receives it as `model`. If not exactly one model is ready, the
  request gets 400.
- A request for an API the engine doesn't implement is rejected before
  anything is loaded.

`GET /v1/models` is what a client or an agent harness reads to choose a model.
It lists the models that can answer: running instances, and registered models
that could be started.

- **Context size.** Each entry carries the model's context window in tokens as
  `max_model_len` and `context_length` (the two names clients look for), when
  it is known. For a running model it is what the engine reports. For a
  registered model that is not running it is the registered `--ctx`, divided by
  `--parallel` for engines that share one context between their slots
  (llama.cpp: `--ctx 16384 --parallel 4` offers 4,096 tokens per request), else
  the context declared with `emx bench config --context-length`. Without any of
  these the fields are absent, and the client falls back to its own default: set
  `--ctx` when you register a model that agents will use.
- **What else each entry says.** Every field is absent when the gateway does
  not know it, so a client falls back to its own default.

  | Field | Meaning | Where it comes from |
  | --- | --- | --- |
  | `limits.max_output_tokens` | the most one reply can hold | `gateway.max_output_tokens` and the caller key's own cap, within the context |
  | `architecture.input_modalities` | `["text"]` or `["text", "image"]` | the running engine's own report; for a stopped Hugging Face checkpoint, its `config.json` (a `vision_config` section, unless `language_model_only`) |
  | `emx.images` | the same as a true/false | |
  | `emx.apis` | request formats the model's engine serves (`chat`, `completions`, `responses`, `messages`, `embeddings`, `rerank`) | the instance, else the engine manifest |
  | `emx.load_seconds` | how long the model took to become ready the last time it was started | the gateway's event records |
  | `emx.decode_tok_per_sec` | average generation speed of one request over the last 30 days of real traffic (several clients at once share the engine, which lowers it) | the usage records |
  | `emx.concurrency` | how many requests share the model: `mode` (`single`: one request at a time, each at the engine's full speed; `shared`: several at once, more in total and less for each), `serves` (how many run at the same time before others wait), `parallel` (the engine's slots), `limit` (the gateway's `max_concurrency` for the model, when set), and `active` / `queued` right now | the running engine's report, else the registered `--parallel`; `mode` and `serves` are absent when neither is known |
  | `emx.price` | the model's `emx price`: `prompt`, `completion` and `cached` per 1M tokens, and `currency`; shown to admin and auditor keys only (`lmx rent publish` reads it to price rental listings) | `pricing` in config.yaml |
  | `emx.hardware` | running: `{kind, loaded: true, gpus or chips}`. Stopped: `kind` (`gpu` or `tenstorrent`), `chips` it needs or `need_mib` (declared `vram_mib`, else the peak measured in 30 days), `free_mib` on the roomiest GPU as last sampled, and `free_now` | placement |
  | `emx.weights` | which weights answer: `status` (`hashing`, `ready` or `error`), and when ready the `id` (the sha256 of the lines `<sha256> <bytes>\n` of the weight files, sorted by sha256: equal for every copy of the same weights, whatever the file names) and `files` (`path` relative to the model's directory, `bytes`, `sha256`, `git_sha1` for files under 10 MiB, and `weight`); `origin` (`{repo, revision}`) when known; `error` says which file could not be read | the model's files (below) |

  `free_now: false` does not mean a request fails: the gateway may make room by
  stopping an idle model it started, and it waits for a busy Tenstorrent chip.
  It tells a client that the first answer will take longer.
- **One model, two ways to serve it.** Concurrency is fixed when a model
  starts, so to offer both the fastest single-user speed and a shared version,
  register the model twice (`emx model add m-solo ... --parallel 1` and
  `emx model add m-shared ... --parallel 8`). Clients pick by name, and
  `emx.concurrency.mode` tells them which is which. Switching between the two
  restarts the model unless the hardware holds both.
- **Unavailable models are left out.** A registered model whose files are gone,
  or that no installed engine can serve, is not listed, and neither is an alias
  that points at one. `emx models` shows them with the reason in its STATUS
  column, and `GET /v1/models?unavailable=1` (used by the admin UI) includes
  them with `emx.unavailable`.
- **Which weights answer.** `emx.weights` fingerprints the model's files the
  way Hugging Face does: the sha256 of each weight file (`.gguf`,
  `.safetensors`, `.bin`, `.pt`, `.pth`), which Hugging Face publishes as a
  large file's `lfs.oid`, and the git blob SHA-1 of each file under 10 MiB, its
  `oid` for small files. A directory source lists its weights, configs
  (`*.json`), tokenizers, chat templates (`*.jinja`), `*.txt` and `*.model`
  files, including subdirectories; READMEs, hidden files, images and files over
  100 GiB are left out. A split GGUF lists all its shards, a Strata config the
  GGUF files it names. The gateway hashes the files of every registered model
  in the background, one file at a time, when it starts and every minute after
  (hashing a 20 GB file takes tens of seconds), and reports `status: hashing`
  until done. The hashes are kept in `weights.json` in the state directory by
  path, size and modification time: a restart does not hash again, a replaced
  file is hashed again. `origin` is the repo and commit of a source inside the
  Hugging Face cache (`.../models--<org>--<name>/snapshots/<commit>/...`, or a
  repo id source found there). For files downloaded elsewhere, say where they
  came from with `origin: <org>/<name>[@<revision>]` on the model in
  `models.yaml`; it is reported as written. `lmx rent publish` sends the
  fingerprint, and the rental site checks it against Hugging Face.
- `model add`, `engine set` and `key add`/`key rm` take effect without
  restarting the gateway.

## Requests

- **Order of checks.** A key's `rpm` and the token and cost budgets of the
  key and its team are checked before the body is read, so a key over its
  limits gets 429 without the gateway buffering anything. Every request of a
  key counts toward `rpm`, including ones refused later (for example 403 for
  a model outside the key's globs).
- **Body size.** A body larger than `gateway.max_body_mb` (default 64 MiB,
  allowed 1-256) gets 413; a declared `Content-Length` over the limit is
  refused without reading. All in-flight requests together may hold
  `gateway.max_buffered_mb` (default 1024 MiB, at least `max_body_mb`) of
  request bodies in memory, and one key at most half of that (but always one
  body of the allowed size); a request that would exceed either gets 503 with
  `Retry-After: 1` (`error_kind` `no_capacity`). Memory is counted as the
  bytes arrive, so a client that announces a large body and sends nothing
  holds nothing. The body must arrive within 5 minutes.
- **One JSON object.** The body must be exactly one JSON object, with
  nothing after it. A top-level field given more than once is refused with
  400. `model`, `messages`, `prompt`, `input`, `stream`, `stream_options`,
  `tools`, `system`, `instructions`, `query` and `documents` are accepted
  only in lower case (`"Model"` gets 400), so the gateway and the engine
  always read the same value. `user` is read only in lower case.
- **Stream usage.** With `gateway.stream_usage` on (the default), streamed
  chat and completions requests are sent to the engine with
  `stream_options.include_usage: true`, whatever the client sent. The client
  therefore always receives the final usage chunk, even if it asked for
  `false`.
- **Request ids.** The gateway generates the id of every request and
  returns it as `X-Emx-Request-Id` and `X-Request-Id`; the engine
  receives the same id as `X-Request-Id`, and an engine's own `X-Request-Id`
  response header is dropped. An `X-Request-Id` sent by the client (its
  first 128 characters) is stored with the record as `client_request_id`
  and need not be unique. `emx requests --id` and
  `GET /emx/v1/requests?id=` match either id.

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
| `max_body_mb` | 64 | largest request body on inference endpoints, in MiB (1-256); larger bodies get 413 |
| `max_buffered_mb` | 1024 | request-body memory held by all in-flight requests together, in MiB (at least `max_body_mb`); beyond it the gateway answers 503 with `Retry-After: 1` |
| `cache_isolation` | false | clients cannot see each other's prompt-cache hits (below) |

### Prompt-cache isolation

Engines reuse the computed start of a prompt they have seen before (prefix
caching), whoever sent it. With several clients on one model, that leaks:
a client can learn that someone recently sent a prompt starting the way its
own does, from the cached-token count in its usage, and from how fast the
answer starts. `gateway.cache_isolation: true` closes both, for every engine,
without the engine's help:

- The gateway remembers, per model and client, the last 16 prompts each
  client sent, with the prompt tokens the engine counted. For a new request,
  the client's own earlier prompts explain at most so many cached tokens:
  all of an earlier prompt the new one continues (the next turn of a chat, a
  longer completions prompt), or the share of one it starts the same way.
- Cached-token counts in the response (`cached_tokens`,
  `cache_read_input_tokens`) are lowered to that. The client sees, and is
  billed for, only the cache its own prompts left. The request record keeps
  the engine's count as `cached_reported`. In Anthropic-style usage, whose
  `input_tokens` leave out cache reads, what is taken off
  `cache_read_input_tokens` is added to `input_tokens`, so the prompt's total
  stays right and the difference does not show the hit.
- Engine fields that tell the same another way are removed from responses,
  JSON and stream chunks alike: llama.cpp's `timings` (`prompt_n`, `cache_n`,
  `prompt_ms` and the rates), `prompt_progress` and `tokens_cached`. Requests
  lose the llama.cpp options `return_progress` and `timings_per_token`.
  Responses that carry none of these fields pass byte for byte apart from the
  lowered counts; those that do are re-encoded (keys in sorted order).
- The answer's headers and first byte are held back for the time the engine
  takes to compute the part of the prompt the client's own cache does not
  explain, at the model's measured prompt speed. An answer sped up by another
  client's cache starts no sooner than one computed in full; a client's own
  cache hits stay fast. The speed is learned from requests the engine
  computed without cache; until a model has 3 of them, answers are not held.

A client is a key, or the client a delegating key acts for (see
[Access](access.md)): a rental host agent passes each renter as its own
client. `/v1/models` shows `emx.cache_isolation: true` when it is on.

Isolation costs the speed-up of cache hits between clients. Two settings
give it back where nothing leaks:

- **Common prefixes.** Once a prompt prefix has come from
  `gateway.cache_share_after` different clients (default 5; 0 never), it
  counts as every client's own: its cached tokens and its speed-up reach
  everyone. A popular agent's system prompt or a shared tool definition
  qualifies quickly; a hit on it says only that several clients use it. A
  prefix one client sent never does, however often it sends it. The rest of
  the prompt stays isolated. Someone who controls that many clients can make
  a prefix common, so raise the number where clients are cheap to create.
- **Clients that trust each other.** A key with `cache_shared`
  (`emx key add|set --cache-shared`) is one cache domain with all the clients
  it acts for. A delegating key can also name a group per request
  (`X-Emx-Cache-Group`); requests in one group share their cache and are
  still recorded per client. `lmx rent host --shared-cache <listing>` does
  that for one rental listing's renters.

## Placement

Every launch, whether from the gateway, `run`, `ensure`, `chat` or MCP,
goes through placement unless you pass `--force`:

1. **Estimate the need.** In order of precedence:
   - a declared size (`model add --vram-mib`);
   - the manifest's `fraction` mode: vLLM and SGLang claim
     `--mem-fraction` of each GPU;
   - the largest footprint emx measured for this model and engine in
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

`emx run <model> --dry-run` shows the decision, for example
`placement: GPU 0, 9830 MiB each (0.40 of each GPU)`.

## Model aliases

```sh
emx alias set chat-default flash-next-iq3s   # clients ask for chat-default
emx alias list
emx alias rm chat-default
```

An alias (`aliases:` in `config.yaml`) is resolved before access checks,
routing and metering: the engine receives the real model name, the usage
record carries it, keys need access to the target, and `/v1/models` lists
the alias with `alias_for`. Responses say `X-Emx-Alias`. Switching the
target takes effect within seconds through the config reload. An alias
shadows a registered model of the same name; aliases cannot point at
aliases.

## Configuration reload

The gateway re-reads `config.yaml` and `models.yaml` within two seconds of a
change, on SIGHUP (`systemctl --user reload emx`), on `emx
reload`, or `POST /emx/v1/reload`. Requests in flight finish under the
configuration they started with. An invalid file is refused: the running
configuration stays, the error is logged once and recorded as a
`config_reload_failed` event, and `emx_config_reload_failures_total`
counts it. Policy, classification, prices, teams, limits, timeouts (except
`upstream_header_timeout`), placement, swap and logging options apply at
once; the listen address, TLS settings (certificates still renew),
`logging.enabled`, `upstream_header_timeout`, the health and sample intervals
and the `tenstorrent` section apply at the next start. Keys (`keys.yaml`) apply
on the next request, as they always have.
