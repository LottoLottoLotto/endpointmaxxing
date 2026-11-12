# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's private
vulnerability reporting: on the
[repository](https://github.com/LottoLottoLotto/substrate), open the
**Security** tab and choose **Report a vulnerability**
([direct link](https://github.com/LottoLottoLotto/substrate/security/advisories/new)).

Do not open a public issue or pull request for a suspected vulnerability.

A useful report says which version (`substrate version`) and platform you
used, how the gateway was configured (listen address, TLS, keys and their
roles), and the steps or request that show the problem. Leave real key
secrets and prompts out of the report.

## Supported versions

Security fixes are made for the latest release only. Please check that the
problem still exists on the latest release before reporting.

## Scope

In scope:

- the gateway (`substrate serve`): authentication, roles, model access
  globs, quotas and budgets, request routing and proxying, TLS handling;
- the management and settings API under `/substrate/v1/` and the admin UI at
  `/admin/`;
- stored data: `keys.yaml`, the usage database, the audit file and its hash
  chain, backups, file permissions;
- the CLI and `substrate mcp`, where they cross a privilege boundary (for
  example secrets leaking into logs, events, `ps` output or command lines);
- engine manifests and how they turn configuration into host command lines.

Out of scope:

- vulnerabilities in the inference engines substrate launches (llama.cpp,
  vLLM, SGLang, Strata, tt-metal images and others): report those to their
  projects;
- a gateway deliberately run open on a public address with `--insecure`;
- clients that call an engine's port directly instead of the gateway (they
  bypass keys, policy and metering by design: keep engine ports private);
- actions by someone who already has the operator's OS account, an `admin`
  key or write access to the config and state directories;
- prompt policy detectors missing a particular secret format or phrasing
  (the policy is rule-based; send those as ordinary issues).

## Security model in brief

- **Open mode only on loopback.** With no keys and no master key, the gateway
  is open: every caller is admin. Whether it may run open is decided when
  `serve` starts: only on a loopback listen address, or with `--insecure`.
  Binding to an address other than loopback requires at least one key, or
  `--insecure`, and warns while the gateway serves plain HTTP. If the last
  key is removed from a gateway that was not permitted to run open, it
  answers 401 to everything until a key exists or it is restarted with a
  master key.
- **Open mode and browsers.** An open gateway answers 403 unless the request
  is addressed to `localhost`, `::1` or a `127.0.0.0/8` address (not
  required with `--insecure`), carries no `Origin` other than the gateway's
  own scheme, host and port (`null` is refused), and does not carry
  `Sec-Fetch-Site: cross-site`. A web page in the operator's browser, or a
  DNS name rebound to loopback, therefore cannot use it.
- **Keys.** Once any key exists, every request needs one, sent as
  `Authorization: Bearer` or `x-api-key`. Keys are stored only as SHA-256
  hashes in `~/.config/substrate/keys.yaml` (mode 0600). The master key comes
  from `$SUBSTRATE_API_KEY` or `serve --api-key-file F` (a file of mode
  0600), never from an argument. The client's key is never forwarded to an
  engine.
- **Engine keys.** An engine's own key (`--api-key`) is passed in the
  engine's environment where its manifest sets `launch.api_key_env` (builtin:
  `vllm`, `llama.cpp`, `buun-llama-cpp`). `sglang`, `strata` and `tt-vllm`
  still receive it on the command line, which every user of the host can
  read in the process list. Instance listings (`ps`, `inspect`,
  `/substrate/v1/instances`) show environment values and secret-looking
  argument values as `***`; the full values are only in the owner-only
  instance file.
- **Roles.** `user` keys: inference and `/v1/models`, limited to their (and
  their team's) model globs, plus their own usage and request records.
  `auditor` keys: read-only access to usage, requests, events, flags, errors,
  reports, keys, teams, `/metrics` and `policy/test`; no inference, and no
  starting, stopping or reloading. `admin` keys: everything.
- **Settings API guards.** Changes are refused (403) over plain HTTP from
  anything but loopback, because requests and answers carry key secrets.
  Engine arguments, manifest vars, container pre-args, engine binaries and
  environment are not settable over HTTP. A key cannot delete, disable or
  demote itself. Bodies must be `application/json`, at most 64 KiB, with no
  unknown fields; every management call with a body must be
  `application/json` (415 otherwise). Host-only flags (`trust_remote_code`)
  and `$` in a model source are refused. Every change is recorded in the
  admin trail, and
  `gateway.disable_settings_api: true` refuses every change over HTTP.
- **Admin UI.** A client of the management API only; the key is kept in the
  browser tab (`sessionStorage`) and sent as a bearer token, and the page is
  sent with a strict Content-Security-Policy.
- **Request handling.** Rate limits and budgets are checked before a body is
  read; bodies are bounded per request (`gateway.max_body_mb`) and in total
  (`gateway.max_buffered_mb`). A body must be one JSON object without
  repeated top-level fields, so the gateway and the engine read the same
  `model` and prompt. Model access and policy also apply to requests without
  a `model`. Blocking policy rules refuse a request whose text is too large
  to scan.
- **Data at rest.** Config and state directories are `0700`; keys, config,
  the usage database, the audit file and engine logs are `0600`.
- **Audit file.** Lines are hash-chained and numbered, and the chain's head
  is recorded in `<audit file>.head`. `substrate audit verify` finds an
  edited, inserted, removed or reordered line and a truncated or rewritten
  tail. With `gateway.audit_key_file` the chain is an HMAC-SHA256, so a file
  rewritten without the key does not verify. Not detected on the host
  alone: someone who can write both the audit file and its `.head` rolling
  both back to an earlier state, or truncating the file and deleting the
  anchor (a warning only); without a key, a consistent rewrite of the whole
  file. These are caught only by `audit verify --expect-head` /
  `--expect-count` with a head stored off the host. Someone who holds the
  audit key can produce lines that verify.

`substrate doctor` reports security findings for a given setup. The details
are in [docs/access.md](docs/access.md),
[docs/admin-api.md](docs/admin-api.md),
[docs/governance.md](docs/governance.md) and
[docs/operations.md](docs/operations.md).
