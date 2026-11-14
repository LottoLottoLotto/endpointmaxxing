"""tt-multihost: one process owns a Tenstorrent chip group and keeps several models
resident in its DRAM, so switching between them costs nothing but the next request.

Each model is a tt-metal tt_transformers Transformer with its own paged KV cache and
Generator (its own decode trace); all of them share one open MeshDevice. Requests
for any model are executed one at a time on the device by a single worker thread
(the device has one command queue); prefill/decode of different models interleave
at request granularity.

HTTP (OpenAI-compatible subset + admin API used by emx):
  GET  /health                      200 once the device is open
  GET  /v1/models                   resident models
  POST /v1/chat/completions         stream and non-stream
  POST /v1/completions              stream and non-stream (raw prompt)
  GET  /admin/models                resident models with load time, DRAM and last use
  POST /admin/load    {"name", "path", "ctx"?}   load (evicting LRU models if DRAM is short)
  POST /admin/unload  {"name"}      release a model's weights, KV cache and traces
  GET  /admin/memory                device DRAM usage
"""

from __future__ import annotations

import argparse
import gc
import json
import os
import queue
import re
import sys
import threading
import time
import traceback
import uuid
from collections import defaultdict
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import torch
import ttnn
from loguru import logger
from transformers import AutoTokenizer

from models.tt_transformers.tt.common import PagedAttentionConfig, create_tt_model
from models.tt_transformers.tt.generator import Generator
from models.tt_transformers.tt.model_config import DecodersPrecision, ModelArgs

MESH_SHAPES = {"P150": (1, 1), "P300": (1, 2), "P150x4": (1, 4), "P150x8": (1, 8), "BHGLX": (8, 4),
               "N150": (1, 1), "N300": (1, 2), "T3K": (1, 8)}
BLOCK = 32  # paged-attention block size
NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")  # emx model names; also the cache dir name


from tt_multihost.guard import MAX_BODY_BYTES, BadRequest, host_allowed, is_json, template_kwargs  # noqa: E402


class Unloaded(Exception):
    """The model was unloaded while the request waited for the device (answered with 404)."""


def _find_grid(self, N):
    """ModelArgs.find_grid with the column limit taken from the opened device.

    Upstream hardcodes 12 columns on Blackhole; boards with an extra harvested column (11-column
    compute grid once dispatch takes a column) then shard decode residuals onto dispatch cores.
    """
    grid = getattr(self, "max_grid_size", None)
    max_rows = min(10, grid.y) if grid else 10
    max_cols = min(12, grid.x) if grid else 12
    for cores in sorted((k for k in range(1, max_rows * max_cols + 1) if N % k == 0), key=lambda k: abs(k - 32)):
        for rows in range(1, max_rows + 1):
            if cores % rows == 0 and cores // rows <= max_cols:
                return rows, cores // rows
    raise AssertionError(f"cannot split {N} tiles evenly over a {max_rows}x{max_cols} core grid")


ModelArgs.find_grid = _find_grid


def dram_used() -> tuple[int, int]:
    """(allocated, total) device DRAM bytes on the first device."""
    view = ttnn.get_memory_view(STATE.device, ttnn.BufferType.DRAM)
    alloc = view.total_bytes_allocated_per_bank * view.num_banks
    total = alloc + view.total_bytes_free_per_bank * view.num_banks
    return int(alloc), int(total)


@dataclass
class Slot:
    name: str
    path: str
    max_seq_len: int
    args: object
    model: object
    generator: Generator
    kv_cache: list
    page_table: torch.Tensor
    tokenizer: object
    dram_bytes: int
    load_seconds: float
    loaded_at: float = field(default_factory=time.time)
    last_used: float = field(default_factory=time.time)
    requests: int = 0
    unloaded: bool = False  # set once its device memory is released; queued requests then fail cleanly


class State:
    def __init__(self):
        self.device = None
        self.models: dict[str, Slot] = {}
        self.lock = threading.Lock()  # guards models (the worker is the only device user)
        self.work: queue.Queue = queue.Queue()
        self.opts = None


STATE = State()


# ---------------------------------------------------------------- device worker

def device_call(fn, *a, **kw):
    """Run fn on the device worker thread and wait for its result."""
    done = threading.Event()
    box = {}

    def job():
        try:
            box["value"] = fn(*a, **kw)
        except BaseException as e:  # noqa: BLE001 - reported to the caller
            box["error"] = e
            box["trace"] = traceback.format_exc()
        finally:
            done.set()

    STATE.work.put(job)
    done.wait()
    if "error" in box:
        logger.error(box["trace"])
        raise box["error"]
    return box.get("value")


def device_worker():
    while True:
        job = STATE.work.get()
        job()


# ---------------------------------------------------------------- loading

def _load(name: str, path: str, max_seq_len: int) -> Slot:
    if name in STATE.models:
        return STATE.models[name]
    o = STATE.opts
    blocks = max(1, max_seq_len // BLOCK)
    # tt_transformers reads the model location and its converted-weight cache from the environment.
    os.environ["HF_MODEL"] = path
    os.environ["TT_CACHE_PATH"] = str(Path(o.cache) / name)
    # A captured trace replays into scratch addresses the allocator considers free; weights or KV caches
    # allocated after a capture could land there and be overwritten. Drop every resident model's traces
    # first; each re-captures on its next decode (well under a second).
    for other in STATE.models.values():
        _release_traces(other.generator)
    before, _ = dram_used()
    t0 = time.time()
    args, model, kv_cache, _ = create_tt_model(
        STATE.device,
        instruct=True,
        max_batch_size=1,
        optimizations=lambda a: DecodersPrecision.performance(a.n_layers, a.model_name),
        max_seq_len=max_seq_len,
        paged_attention_config=PagedAttentionConfig(block_size=BLOCK, max_num_blocks=blocks),
        dtype=ttnn.bfloat8_b,
    )
    generator = Generator([model], [args], STATE.device, tokenizer=args.tokenizer)
    page_table = torch.arange(blocks, dtype=torch.int32).reshape(1, blocks)
    tokenizer = AutoTokenizer.from_pretrained(path)
    slot = Slot(name=name, path=path, max_seq_len=max_seq_len, args=args, model=model, generator=generator,
                kv_cache=[kv_cache], page_table=page_table, tokenizer=tokenizer, dram_bytes=0, load_seconds=0)
    # Compile and capture traces now, so the first request is fast.
    _generate(slot, tokenizer.encode("Hello"), max_tokens=2, temperature=0.0, top_p=1.0, emit=None)
    after, _ = dram_used()
    slot.dram_bytes = max(0, after - before)
    slot.load_seconds = time.time() - t0
    STATE.models[name] = slot
    logger.info(f"loaded {name} from {path} in {slot.load_seconds:.1f}s, {slot.dram_bytes / 2**30:.2f} GiB DRAM")
    return slot


def _unload(name: str) -> bool:
    slot = STATE.models.pop(name, None)
    if slot is None:
        return False
    _release_traces(slot.generator)
    for layer in slot.model.layers:
        for t in getattr(layer.attention, "layer_past", None) or []:
            try:
                ttnn.deallocate(t)
            except Exception:  # noqa: BLE001 - already freed
                pass
    slot.unloaded = True
    slot.generator = slot.model = slot.kv_cache = None
    del slot
    gc.collect()
    ttnn.synchronize_device(STATE.device)
    logger.info(f"unloaded {name}")
    return True


def _release_traces(gen: Generator) -> None:
    """Release gen's device traces and forget them, so it captures fresh ones when next used."""
    gen.__del__()
    for attr in ("trace_id_prefill", "trace_inputs_prefill", "trace_output_prefill", "trace_id_prefill_sampling",
                 "trace_input_prefill_sampling", "trace_output_prefill_sampling", "trace_ids_decode",
                 "trace_inputs_decode", "trace_output_decode"):
        setattr(gen, attr, defaultdict(lambda: None))
    if hasattr(gen, "_bucket_trace_store"):
        gen._bucket_trace_store = {}


def load(name: str, path: str, max_seq_len: int) -> Slot:
    """Load name, evicting least recently used models while device DRAM is short."""
    with STATE.lock:
        if name in STATE.models:
            return STATE.models[name]
    need = estimate_bytes(path, max_seq_len)
    while True:
        used, total = device_call(dram_used)
        if total - used >= need + STATE.opts.dram_reserve:
            break
        with STATE.lock:
            victims = sorted(STATE.models.values(), key=lambda s: s.last_used)
        if not victims:
            break  # nothing to evict: try anyway, the allocator decides
        logger.info(f"evicting {victims[0].name} (LRU) to fit {name}: need {need / 2**30:.2f} GiB, "
                    f"free {(total - used) / 2**30:.2f} GiB")
        device_call(_unload, victims[0].name)
    return device_call(_load, name, path, max_seq_len)


def estimate_bytes(path: str, max_seq_len: int) -> int:
    """bfloat8 weights are ~half of bf16 safetensors; KV cache (bf8) per token is added."""
    weights = sum(f.stat().st_size for f in Path(path).glob("*.safetensors")) // 2
    try:
        cfg = json.loads((Path(path) / "config.json").read_text())
        kv_heads = cfg.get("num_key_value_heads", cfg.get("num_attention_heads", 8))
        head_dim = cfg.get("head_dim") or cfg["hidden_size"] // cfg["num_attention_heads"]
        kv = 2 * cfg["num_hidden_layers"] * kv_heads * head_dim * max_seq_len  # ~1 byte per value in bf8
    except Exception:  # noqa: BLE001 - unknown layout: weights only
        kv = 0
    return int(weights * 1.1 + kv)


# ---------------------------------------------------------------- generation

def _sample(logits: torch.Tensor, temperature: float, top_p: float) -> int:
    logits = logits.reshape(-1).float()
    if temperature <= 0:
        return int(torch.argmax(logits).item())
    probs = torch.softmax(logits / temperature, dim=-1)
    if top_p < 1.0:
        sorted_p, idx = torch.sort(probs, descending=True)
        keep = torch.cumsum(sorted_p, dim=-1) - sorted_p < top_p
        probs = torch.zeros_like(probs).scatter(0, idx[keep], sorted_p[keep])
        probs = probs / probs.sum()
    return int(torch.multinomial(probs, 1).item())


def _generate(slot: Slot, prompt_ids: list[int], max_tokens: int, temperature: float, top_p: float, emit,
              stop_ids: set[int] | None = None) -> tuple[list[int], str]:
    """Prefill prompt_ids and decode up to max_tokens; emit(token_id) per token. Runs on the device thread."""
    if slot.unloaded:
        raise Unloaded(f"model {slot.name!r} was unloaded")
    room = slot.max_seq_len - len(prompt_ids) - 1
    if room <= 0:
        raise BadRequest(f"prompt of {len(prompt_ids)} tokens exceeds the {slot.max_seq_len}-token context")
    max_tokens = min(max_tokens, room)
    gen = slot.generator
    # Clear the previous request's cache so nothing leaks between users of a slot.
    for layer in slot.model.layers:
        k, v = layer.attention.layer_past
        ttnn.mul(k, 0, output_tensor=k)
        ttnn.mul(v, 0, output_tensor=v)
    gen.prev_page_table = None
    tokens = torch.tensor([prompt_ids], dtype=torch.int64)
    logits = gen.prefill_forward_text(tokens, page_table=slot.page_table, kv_cache=slot.kv_cache,
                                      prompt_lens=[len(prompt_ids)], enable_trace=False)
    if isinstance(logits, tuple):
        logits = logits[0]
    stop_ids = stop_ids if stop_ids is not None else _stop_ids(slot)
    out: list[int] = []
    tok = _sample(logits[0], temperature, top_p)
    pos = torch.tensor([len(prompt_ids)])
    reason = "length"
    for i in range(max_tokens):
        if tok in stop_ids:
            reason = "stop"
            break
        out.append(tok)
        if emit:
            emit(tok)
        if i == max_tokens - 1:
            break
        res = gen.decode_forward(torch.tensor([[tok]]), pos, page_table=slot.page_table, kv_cache=slot.kv_cache,
                                 enable_trace=STATE.opts.trace, reset_batch=(i == 0))
        logits = res[0] if isinstance(res, tuple) else res
        tok = _sample(logits[0], temperature, top_p)
        pos += 1
    slot.last_used = time.time()
    slot.requests += 1
    return out, reason


def _stop_ids(slot: Slot) -> set[int]:
    ids = set()
    tk = slot.tokenizer
    if tk.eos_token_id is not None:
        ids.add(tk.eos_token_id)
    for t in ("<|im_end|>", "<|eot_id|>", "<|endoftext|>", "<end_of_turn>"):
        i = tk.convert_tokens_to_ids(t)
        if isinstance(i, int) and i >= 0 and i != tk.unk_token_id:
            ids.add(i)
    return ids


# ---------------------------------------------------------------- HTTP

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # quiet access log
        pass

    def _json(self, status: int, obj) -> None:
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _error(self, status: int, msg: str, typ: str = "invalid_request_error") -> None:
        self._json(status, {"error": {"message": msg, "type": typ}})

    def _body(self) -> dict:
        n = int(self.headers.get("Content-Length") or 0)
        if n > MAX_BODY_BYTES:
            raise BadRequest(f"body of {n} bytes exceeds {MAX_BODY_BYTES}")
        return json.loads(self.rfile.read(n) or b"{}")

    def _refused(self) -> bool:
        """Answers and returns True for a request a web page could have forged (see guard.py)."""
        if not host_allowed(self.headers.get("Host")):
            self._error(403, "this server answers only when addressed by IP address or as localhost", "permission_error")
            return True
        if self.command == "POST" and not is_json(self.headers.get("Content-Type")):
            self._error(415, "send Content-Type: application/json")
            return True
        return False

    def do_GET(self):
        if self._refused():
            return None
        if self.path == "/health":
            return self._json(200 if STATE.device is not None else 503, {"status": "ok"})
        if self.path == "/v1/models":
            with STATE.lock:
                data = [{"id": n, "object": "model", "created": int(s.loaded_at), "owned_by": "tt-multihost",
                         "max_model_len": s.max_seq_len} for n, s in STATE.models.items()]
            return self._json(200, {"object": "list", "data": data})
        if self.path == "/admin/models":
            with STATE.lock:
                data = [{"name": n, "path": s.path, "max_seq_len": s.max_seq_len, "dram_bytes": s.dram_bytes,
                         "load_seconds": round(s.load_seconds, 2), "loaded_at": s.loaded_at,
                         "last_used": s.last_used, "requests": s.requests} for n, s in STATE.models.items()]
            return self._json(200, data)
        if self.path == "/admin/memory":
            used, total = device_call(dram_used)
            return self._json(200, {"dram_allocated_bytes": used, "dram_total_bytes": total})
        return self._error(404, f"no route {self.path}", "not_found_error")

    def do_POST(self):
        if self._refused():
            return None
        try:
            body = self._body()
        except Exception as e:  # noqa: BLE001
            return self._error(400, f"body is not JSON: {e}")
        try:
            if self.path == "/admin/load":
                name, path = body.get("name"), body.get("path")
                if not name or not path:
                    return self._error(400, 'body must be {"name", "path", "ctx"?}')
                if not NAME_RE.match(name):
                    return self._error(400, f"name {name!r} must match {NAME_RE.pattern}")
                if not (Path(path) / "config.json").is_file():
                    return self._error(400, f"{path} has no config.json inside the host (is it under the "
                                            "directory the host mounts, vars.models?)")
                s = load(name, path, int(body.get("ctx") or STATE.opts.max_seq_len))
                return self._json(200, {"name": s.name, "dram_bytes": s.dram_bytes, "load_seconds": s.load_seconds})
            if self.path == "/admin/unload":
                ok = device_call(_unload, body.get("name", ""))
                return self._json(200 if ok else 404, {"unloaded": ok})
            if self.path in ("/v1/chat/completions", "/v1/completions"):
                return self._complete(body, chat=self.path.endswith("chat/completions"))
        except BadRequest as e:
            return self._error(400, str(e))
        except Unloaded as e:  # unloaded meanwhile: the client (emx) loads it again
            return self._error(404, str(e), "not_found_error")
        except Exception as e:  # noqa: BLE001 - surface engine errors to the client
            logger.error(traceback.format_exc())
            return self._error(500, f"{type(e).__name__}: {e}", "api_error")
        return self._error(404, f"no route {self.path}", "not_found_error")

    def _complete(self, body: dict, chat: bool):
        name = body.get("model")
        with STATE.lock:
            slot = STATE.models.get(name)
            if slot is None and len(STATE.models) == 1 and not name:
                slot = next(iter(STATE.models.values()))
        if slot is None:
            return self._error(404, f"model {name!r} is not loaded (POST /admin/load)", "not_found_error")
        tk = slot.tokenizer
        if chat:
            kwargs = template_kwargs(body.get("chat_template_kwargs"))
            text = tk.apply_chat_template(body.get("messages", []), add_generation_prompt=True, tokenize=False, **kwargs)
            ids = tk.encode(text, add_special_tokens=False)
        else:
            ids = tk.encode(body.get("prompt", ""))
        max_tokens = int(body.get("max_tokens") or body.get("max_completion_tokens") or 256)
        temperature = float(body.get("temperature", 0.7))
        top_p = float(body.get("top_p", 1.0))
        rid = ("chatcmpl-" if chat else "cmpl-") + uuid.uuid4().hex[:24]
        created = int(time.time())
        if not body.get("stream"):
            out, reason = device_call(_generate, slot, ids, max_tokens, temperature, top_p, None)
            text = tk.decode(out, skip_special_tokens=True)
            choice = ({"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": reason}
                      if chat else {"index": 0, "text": text, "finish_reason": reason})
            return self._json(200, {"id": rid, "object": "chat.completion" if chat else "text_completion",
                                    "created": created, "model": slot.name, "choices": [choice],
                                    "usage": {"prompt_tokens": len(ids), "completion_tokens": len(out),
                                              "total_tokens": len(ids) + len(out)}})
        # Stream: tokens flow from the device thread through a queue to this connection.
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        q: queue.Queue = queue.Queue()
        result = {}

        def run():
            try:
                result["out"], result["reason"] = device_call(_generate, slot, ids, max_tokens, temperature, top_p, q.put)
            except Exception as e:  # noqa: BLE001
                result["error"] = str(e)
            q.put(None)

        threading.Thread(target=run, daemon=True).start()
        kind = "chat.completion.chunk" if chat else "text_completion"

        def send(obj):
            self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n")
            self.wfile.flush()

        produced: list[int] = []
        sent_text = ""
        while True:
            tok = q.get()
            if tok is None:
                break
            produced.append(tok)
            text = tk.decode(produced, skip_special_tokens=True)
            delta, sent_text = text[len(sent_text):], text
            if not delta or delta.endswith("\ufffd"):
                continue  # wait for a complete UTF-8 character
            choice = {"index": 0, "delta": {"content": delta}, "finish_reason": None} if chat else \
                {"index": 0, "text": delta, "finish_reason": None}
            send({"id": rid, "object": kind, "created": created, "model": slot.name, "choices": [choice]})
        if "error" in result:
            send({"error": {"message": result["error"], "type": "api_error"}})
        else:
            fin = {"index": 0, "delta": {}, "finish_reason": result["reason"]} if chat else \
                {"index": 0, "text": "", "finish_reason": result["reason"]}
            send({"id": rid, "object": kind, "created": created, "model": slot.name, "choices": [fin]})
            if (body.get("stream_options") or {}).get("include_usage"):
                send({"id": rid, "object": kind, "created": created, "model": slot.name, "choices": [],
                      "usage": {"prompt_tokens": len(ids), "completion_tokens": len(produced),
                                "total_tokens": len(ids) + len(produced)}})
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True


def open_device(o):
    shape = MESH_SHAPES.get(o.mesh, (1, 1))
    if shape[0] * shape[1] > 1:
        ttnn.set_fabric_config(ttnn.FabricConfig.FABRIC_1D)
    # tt_transformers sizes its core grids for the dispatch placement tt-metal's own harness uses:
    # dispatch on a column on Blackhole (tests/scripts/common.py get_updated_device_params).
    axis = ttnn.DispatchCoreAxis.COL if ttnn.device.is_blackhole() else None
    STATE.device = ttnn.open_mesh_device(ttnn.MeshShape(*shape), dispatch_core_config=ttnn.DispatchCoreConfig(None, axis),
                                         trace_region_size=o.trace_region_size, l1_small_size=o.l1_small_size,
                                         num_command_queues=1)
    STATE.device.enable_program_cache()
    logger.info(f"opened mesh {o.mesh} {shape}; DRAM {dram_used()[1] / 2**30:.1f} GiB")


def main(argv=None):
    p = argparse.ArgumentParser(prog="tt-multihost")
    p.add_argument("--host", default="0.0.0.0")
    p.add_argument("--port", type=int, default=8000)
    p.add_argument("--mesh", default=os.environ.get("MESH_DEVICE", "P150"))
    p.add_argument("--cache", default=os.environ.get("TT_CACHE_ROOT", "/cache"))
    p.add_argument("--max-seq-len", type=int, default=4096)
    p.add_argument("--trace", action=argparse.BooleanOptionalAction, default=True, help="trace decode (per model)")
    p.add_argument("--trace-region-size", type=int, default=256 << 20)
    p.add_argument("--l1-small-size", type=int, default=24576)
    p.add_argument("--dram-reserve", type=int, default=1 << 30, help="bytes kept free when loading")
    p.add_argument("--preload", action="append", default=[], help="name=path to load at start (repeatable)")
    o = p.parse_args(argv)
    STATE.opts = o
    threading.Thread(target=device_worker, daemon=True, name="device").start()
    device_call(open_device, o)
    for spec in o.preload:
        name, _, path = spec.partition("=")
        load(name, path, o.max_seq_len)
    srv = ThreadingHTTPServer((o.host, o.port), Handler)
    srv.daemon_threads = True
    logger.info(f"tt-multihost listening on {o.host}:{o.port}")
    try:
        srv.serve_forever()
    finally:
        with STATE.lock:
            names = list(STATE.models)
        for n in names:
            device_call(_unload, n)
        device_call(lambda: ttnn.close_mesh_device(STATE.device))


if __name__ == "__main__":
    sys.exit(main())
