# Operations

[← README](../README.md) · [Documentation index](../README.md#documentation)

## Files

| Path | Contents |
| --- | --- |
| `~/.config/emx/config.yaml` | `gateway` (incl. `tls`, `max_output_tokens`, `audit_key_file`), `logging`, `prefer` (engine order per format), per-engine overrides (`bin`, `vars`, `env`, `args`, `memory`), `policy`, `classify`, `pricing`, `teams`, `aliases`, `alerts`, `mesh` |
| `~/.config/emx/keys.yaml` | gateway API keys (hashes only, mode 0600); on a mesh follower, the copy from `mesh.keys_from` |
| `~/.config/emx/mesh-shared.yaml` | a mesh follower's copy of the teams, prices and aliases ([Mesh](mesh.md)) |
| `~/.config/emx/models.yaml` | model registry: source, format, pinned engine, default flags, engine args |
| `~/.config/emx/engines/*.yaml` | user manifests (new engines, or replacements for builtins) |
| `~/.local/state/emx/instances/` | one JSON record per instance (mode 0600; the only place holding unmasked environment and arguments) |
| `~/.local/state/emx/logs/` | engine logs (kept after stop) |
| `~/.local/state/emx/usage.db` | usage database (schema version 7) |
| `~/.local/state/emx/audit.jsonl` | audit log, when `logging.audit_file` is set |
| `~/.local/state/emx/audit.jsonl.head` | the audit log's head anchor (see [Audit file integrity](governance.md#audit-file-integrity)) |
| `~/.local/share/emx/` | engines installed by `emx engine install` |

Set `EMX_HOME` to keep all of these under one directory.

The usage database is migrated when a newer emx first opens it.
Schema version 7 adds the columns `requests.node` and `requests.mesh_from`
([Mesh](mesh.md#limits-and-records)); an older emx binary refuses a database
that this version has migrated.

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
  max_body_mb: 64            # largest inference request body, MiB (1-256); larger bodies get 413
  max_buffered_mb: 1024      # request bodies held in memory by all in-flight requests, MiB (>= max_body_mb); beyond it 503
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
mesh:                        # absent: this gateway is on its own (see mesh.md)
  node: box1
  secret_file: mesh.secret   # >= 32 bytes, mode 0600, the same on every machine
  keys_from: box1            # the machine that holds keys, teams, prices and aliases
  peers:
    - {name: box2, url: https://box2.lan:7777}
  ca_file: ""                # extra CA for the peers' certificates
  interval: 5s
```

## TLS

```yaml
gateway:
    host: 0.0.0.0
    tls:
        cert: /etc/emx/gateway.pem    # PEM chain
        key: /etc/emx/gateway-key.pem
        client_ca: /etc/emx/clients-ca.pem   # optional: require client certificates (mutual TLS)
        min_version: "1.2"                   # or "1.3"
```

or `emx serve --tls-cert F --tls-key F [--tls-client-ca F]`. The
gateway then serves only HTTPS. Certificate and key files are re-read within
seconds of changing on disk, so a renewal (certbot, cert-manager) needs no
restart. `emx chat` and `emx reload` reach a TLS gateway by
trusting the system roots plus the configured certificate; under mutual TLS
they have no client certificate, and `chat` falls back to the engine.

## Backup

```sh
emx backup --out /backups/emx-$(date +%F).tar.gz [--audit] [--no-db]
```

The archive (mode 0600) holds `config/` (`config.yaml`, `models.yaml`,
`keys.yaml`, `engines/`) and `state/usage.db`, a consistent snapshot taken
with SQLite `VACUUM INTO` while the gateway keeps running; `--audit` adds the
audit log, its head anchor (`<audit file>.head`) and its pruning record
(`<audit file>.first`). `backup` never overwrites or removes a file that is
already at `--out`: it stops and leaves it as it is. The audit key file
(`gateway.audit_key_file`) is not in the archive: keep a copy of it
separately. To restore, stop the gateway and copy `config/` and `state/` back
into `~/.config/emx` and `~/.local/state/emx`. Key secrets are
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
- refused-key (401) requests over the stored-per-minute limit
  (`emx_auth_denials_unstored_total`, a counter; these are not in
  `emx_requests_total`) and request-body memory held by in-flight
  requests (`emx_request_body_buffered_bytes`, a gauge bounded by
  `gateway.max_buffered_mb`);
- policy matches per rule (`emx_policy_flags_total`), requests per prompt category
  (`emx_requests_by_category_total`) and failures per error kind (`emx_errors_total`);
- cost per model and team (`emx_cost_total`), team budget use
  (`emx_team_budget_used_ratio`), key expiry (`emx_key_expiry_seconds`) and config reloads.

The `model` label is `invalid` for requests that were not routed to a model
and whose model name is not a valid one, and `other` for unrouted model
names beyond the first 200 distinct ones (see
[Governance](governance.md#prompt-policy-classification-and-errors)).

```sh
emx service install --enable -- --port 7777   # systemd user unit; PATH captured from this shell
emx service status
emx service uninstall
loginctl enable-linger $USER                         # keep it running after logout
```

The unit uses `KillMode=process`, so restarting or upgrading the gateway
leaves running engines alone, and the new gateway picks them up again. On
SIGINT or SIGTERM, the gateway waits up to 30 seconds for in-flight streams
before exiting.

## Platforms

emx runs on Linux, macOS and Windows. Linux is where it is run with real
engines and accelerators; on macOS and Windows the tests and an engine
start/serve/stop run pass on every change, but real engines have not been
exercised there yet.

| | Linux | macOS | Windows |
|---|---|---|---|
| Processes of an engine are found by | session (`/proc`) | process group (`ps`) | process tree |
| Stopping an engine | the manifest's stop signal, then a kill after the stop timeout | same | always a forced termination of the process tree: Windows cannot deliver a signal to another process |
| File locks | `flock` | `flock` | `LockFileEx` |
| Owner-only files (0600/0700) | enforced, checked by `doctor` and for key files | same | not checked: access is governed by the ACL of your profile directory, where the files live by default |
| `emx engine install` | runs the manifest's shell steps | same | needs a POSIX `sh` on `PATH` (Git for Windows has one); otherwise install the engine yourself and `emx engine set <id> --bin <path>` |
| Tenstorrent engines | supported | not available | not available |

On Windows, keep `EMX_HOME` (if you set it) inside a directory only you
can read: keys, prompts and request records are stored there.

## Reliability

- **Concurrent use:** config and registry writes, and instance starts, take
  file locks, so parallel CLI calls, the gateway and MCP sessions don't
  overwrite each other.
- **Validation:** `config.yaml` and `models.yaml` are parsed strictly, so
  unknown keys and invalid unified flag values are errors.
- **Process tracking:**
  - Instances are tracked by PID together with the process start time, so a
    reused PID is never mistaken for an engine.
  - If an automatically chosen port is taken during startup, emx
    retries on a new port.
  - The previous three logs of each instance are kept as `<id>.log.1` to
    `.3`.
- **Secrets:** instance records are mode 0600. API keys are removed from
  `ps`/`run --dry-run` output, logs and events. `ps`, `inspect` and the
  management API also show every environment value and every
  secret-looking argument value as `***` (see
  [Management API](admin-api.md#management-api)); `start` events and
  instance log headers mask arguments the same way.
