# Management API, admin UI and agents

[← README](../README.md) · [Documentation index](../README.md#documentation)

## Management API

For agents that only speak HTTP, the gateway also offers a management API
under `/substrate/v1/`:

| Method | Path | Body or query | Result |
| --- | --- | --- | --- |
| GET | `/substrate/v1/instances` | | instances with status, in-flight requests, idle state, last use |
| POST | `/substrate/v1/ensure` | `{"model", "engine"?, "timeout"?}` | a ready instance (placed, evicting if needed) |
| POST | `/substrate/v1/stop` | `{"id"}` (instance id or model name) | waits for in-flight requests, then stops |
| GET | `/substrate/v1/usage` | `by`, `since`, `until`, `model`, `engine`, `instance`, `client`, `team`, `category`, `error_kind`, `errors`, `flagged`, `min_severity`, `rule` | aggregates (with cost) |
| GET | `/substrate/v1/requests` | same filters, plus `id` and `limit` | request records |
| GET | `/substrate/v1/events` | `since`, `until`, `model`, `engine`, `instance`, `kind` (`admin`, `exited`, `budget_alert`, ...), `limit` | lifecycle events, budget alerts, config reloads, admin trail |
| GET | `/substrate/v1/flags` | same filters | policy matches by rule and detector |
| GET | `/substrate/v1/errors` | same filters | failed requests by error kind |
| GET | `/substrate/v1/report` | same filters (`since` defaults to `24h`) | totals, cost, models, teams, clients, categories, flags, errors |
| GET | `/substrate/v1/keys` | | keys with role, team, expiry, status and last use (no secrets) |
| GET | `/substrate/v1/teams` | | team limits and their use this day and month |
| POST | `/substrate/v1/policy/test` | `{"text"}` or `{"body"}`, optionally `model`, `client`, `api` | category and flags, nothing sent to a model |
| GET / POST | `/substrate/v1/reload` | | last reload outcome / re-read the config now (400 with the error if it is invalid) |
| GET | `/substrate/v1/whoami` | | the caller's key name, role and team |

### Settings API

Admins change the same settings as `substrate key|team|price|alias|model`
over HTTP. Both go through one code path, so validation, warnings and the
admin trail are identical.

| Method | Path | Body | Result |
| --- | --- | --- | --- |
| POST | `/substrate/v1/keys` | `{"name", "role"?, "team"?, "models"?, "rpm"?, "tokens_per_day"?, "max_output_tokens"?, "expires"?}` | 201, the key and its `secret` (shown only now) |
| PATCH | `/substrate/v1/keys/{name}` | any of the fields above except `name`; `""` / `[]` clear `team` / `models`, `"expires": "never"` | the key |
| POST | `/substrate/v1/keys/{name}/rotate` | `{"grace"?: "24h"}` | the new `secret`; the old one works until `old_secret_valid_until` |
| POST | `/substrate/v1/keys/{name}/disable`, `/enable` | | refused at once / restored |
| DELETE | `/substrate/v1/keys/{name}` | | revoked |
| PUT / DELETE | `/substrate/v1/teams/{name}` | `{"tokens_per_day"?, "tokens_per_month"?, "budget_per_month"?, "alert_at"?, "models"?}` | creates or changes the team / removes it |
| GET | `/substrate/v1/prices` | | currency, default and per-model prices |
| PUT / DELETE | `/substrate/v1/prices/{model\|glob\|default}` | `{"prompt", "completion", "cached"?, "currency"?}` | sets / removes a price |
| GET | `/substrate/v1/aliases` | | alias → model |
| PUT / DELETE | `/substrate/v1/aliases/{alias}` | `{"model"}` | sets / removes an alias |
| GET | `/substrate/v1/models` | | the registry, with `source_missing` for entries whose files are gone |
| POST | `/substrate/v1/models` | `{"name", "source", "engine"?, "force"?, "flags"?, "vram_mib"?, "idle_unload"?, "max_concurrency"?, "devices"?}` | 201, the entry and its route |
| DELETE | `/substrate/v1/models/{name}` | | removed from the registry (running instances keep running) |

Changes to config.yaml and models.yaml apply at once; responses carry
`warnings` (an unconfigured team, a budget without prices, an alias to an
unregistered model). Reads need an admin or auditor key, changes an admin key.

Security:

- **Encrypted or local only.** Changes are refused (403) over plain HTTP
  from anything but loopback, because requests and answers carry key
  secrets. Serve HTTPS (`gateway.tls`) for remote administration; a
  TLS-terminating proxy on the same host connects over loopback.
- **No host command lines.** Engine arguments, manifest vars, container
  pre-args, engine binaries and environment are not settable over HTTP: they
  reach command lines and container options on the host. Use the CLI there.
  Unified flags are, since they are validated per type.
- **No self-lockout.** A key cannot delete, disable or demote itself; use
  another admin key or the CLI.
- **Strict input.** Bodies must be `application/json` (no form posts), at
  most 64 KiB, with no unknown fields. Answers are sent `Cache-Control:
  no-store`.
- **Audited.** Every change is an `admin` event, audit-log line and webhook
  naming the key and the client address (`... [api from 10.0.0.7]`); refused
  attempts are recorded as `auth_failed` requests.
- **Off switch.** `gateway.disable_settings_api: true` refuses every change
  over HTTP; reads stay.

## Admin UI

The gateway serves a web admin UI at `/admin/` (for example
`http://127.0.0.1:7777/admin/`) for people. Agents should use the CLI, MCP or
the management API instead.

- **A client of the management API only.** The page holds no data and has no
  routes of its own. Everything it shows or does is a `/substrate/v1` call
  with the key you sign in with, so the same role rules and admin trail apply.
- **Sign in** with an `admin` or `auditor` key (or the master key). The key is
  kept in that browser tab only (`sessionStorage`) and sent as a bearer token,
  so there is no cookie to forge from another site. An open gateway needs no
  key. `user` keys are refused.
- **Pages:** overview (totals, alerts, usage by model, client, team and
  category, flags, errors), models and instances (register, remove, start,
  stop), requests and events with filters, policy matches and prompt testing,
  keys and teams (create, edit, rotate, disable, delete), and config (aliases,
  prices, reload). Auditors see everything with every change button disabled.
- **New secrets** (a created or rotated key) are shown once, with a copy
  button; nothing stores them.
- The page is sent with a strict Content-Security-Policy (no inline script,
  no framing).

## Agents

- **Exit codes:**

  | Code | Meaning |
  | --- | --- |
  | 0 | ok |
  | 1 | error |
  | 2 | usage |
  | 3 | not_found |
  | 4 | timeout |
  | 5 | engine_failed |
  | 6 | conflict |
  | 7 | unsupported |
  | 8 | capacity |
  | 9 | forbidden |

  With `--json`, a failure prints `{"error": "...", "code": "not_found"}`.
- **Discovery:** `substrate commands --json` lists every command with its
  flags, their types and defaults, and the matching MCP tool name.
- **Idempotent start:** `substrate ensure <model>` returns a ready instance:
  an existing one, one that is still loading (it waits), or a newly started
  one. Call it before every use.
- **One-shot request:** `substrate chat <model> "prompt"` goes through the
  gateway when it is running. Otherwise it ensures an instance and calls it
  directly. Either way the request is metered.
- **MCP:** `substrate mcp` speaks MCP over stdio. Each command becomes a tool
  named `substrate_<command>`, for example `substrate_ensure`,
  `substrate_chat`, `substrate_usage` and `substrate_model_add`. Tool
  arguments mirror the command's flags, plus `args` for positional arguments
  and `engine_args` for arguments passed through to the engine.

  ```json
  { "mcpServers": { "substrate": { "command": "substrate", "args": ["mcp"] } } }
  ```

  For Claude Code: `claude mcp add substrate -- substrate mcp`.
