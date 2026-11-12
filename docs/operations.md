# Operations

[← README](../README.md) · [Documentation index](../README.md#documentation)

## Files

| Path | Contents |
| --- | --- |
| `~/.config/substrate/config.yaml` | `gateway` (incl. `tls`, `max_output_tokens`), `logging`, `prefer` (engine order per format), per-engine overrides (`bin`, `vars`, `env`, `args`, `memory`), `policy`, `classify`, `pricing`, `teams`, `aliases`, `alerts` |
| `~/.config/substrate/keys.yaml` | gateway API keys (hashes only, mode 0600) |
| `~/.config/substrate/models.yaml` | model registry: source, format, pinned engine, default flags, engine args |
| `~/.config/substrate/engines/*.yaml` | user manifests (new engines, or replacements for builtins) |
| `~/.local/state/substrate/instances/` | one JSON record per instance |
| `~/.local/state/substrate/logs/` | engine logs (kept after stop) |
| `~/.local/state/substrate/usage.db` | usage database |
| `~/.local/state/substrate/audit.jsonl` | audit log, when `logging.audit_file` is set |
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
  retention_days: 0      # >0 prunes usage records daily while the gateway runs
  audit_retention_days: 0  # >0 prunes the admin trail and rotated audit files (keep it longer)
  audit_max_mb: 100      # audit file rotation size
prefer:
  gguf: [buun-llama-cpp, llama.cpp]
engines:
  strata:                    # a Strata build that runs on a Tenstorrent P150
    accelerator: {kind: tenstorrent, devices: 1, lock: engine}
tenstorrent:
  lock: /var/tmp/tenstorrent/chip-{n}.lock   # every job on the card must use the same file
  locks: {0: /var/lock/my-other-tool.lock}   # per-chip overrides, to match existing scripts
  tt_smi: tt-smi
```

## TLS

```yaml
gateway:
    host: 0.0.0.0
    tls:
        cert: /etc/substrate/gateway.pem    # PEM chain
        key: /etc/substrate/gateway-key.pem
        client_ca: /etc/substrate/clients-ca.pem   # optional: require client certificates (mutual TLS)
        min_version: "1.2"                   # or "1.3"
```

or `substrate serve --tls-cert F --tls-key F [--tls-client-ca F]`. The
gateway then serves only HTTPS. Certificate and key files are re-read within
seconds of changing on disk, so a renewal (certbot, cert-manager) needs no
restart. `substrate chat` and `substrate reload` reach a TLS gateway by
trusting the system roots plus the configured certificate; under mutual TLS
they have no client certificate, and `chat` falls back to the engine.

## Backup

```sh
substrate backup --out /backups/substrate-$(date +%F).tar.gz [--audit] [--no-db]
```

The archive (mode 0600) holds `config/` (`config.yaml`, `models.yaml`,
`keys.yaml`, `engines/`) and `state/usage.db`, a consistent snapshot taken
with SQLite `VACUUM INTO` while the gateway keeps running; `--audit` adds the
audit log. To restore, stop the gateway and copy `config/` and `state/` back
into `~/.config/substrate` and `~/.local/state/substrate`. Key secrets are
only stored hashed, but the archive may hold prompts (`logging.bodies`):
keep it as private as the state directory.

## Metrics and service

`GET /metrics` (admin or auditor, or anyone with `metrics_public: true`) serves
Prometheus text:

- requests and tokens per model, engine and API;
- histograms of TTFT and request duration;
- instance status, in-flight requests and queue depth;
- GPU memory and utilization, and per-instance GPU memory and RSS;
- failovers, restarts, and dropped usage records;
- policy matches per rule (`substrate_policy_flags_total`), requests per prompt category
  (`substrate_requests_by_category_total`) and failures per error kind (`substrate_errors_total`);
- cost per model and team (`substrate_cost_total`), team budget use
  (`substrate_team_budget_used_ratio`), key expiry (`substrate_key_expiry_seconds`) and config reloads.

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
