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
  is open, and only on loopback. Binding to an address other than loopback
  requires at least one key, or `--insecure`, and warns while the gateway
  serves plain HTTP.
- **Keys.** Once any key exists, every request needs one, sent as
  `Authorization: Bearer` or `x-api-key`. Keys are stored only as SHA-256
  hashes in `~/.config/substrate/keys.yaml` (mode 0600). The master key comes
  from `$SUBSTRATE_API_KEY` or `serve --api-key-file F` (a file of mode
  0600), never from an argument. The client's key is never forwarded to an
  engine.
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
  unknown fields. Every change is recorded in the admin trail, and
  `gateway.disable_settings_api: true` refuses every change over HTTP.
- **Admin UI.** A client of the management API only; the key is kept in the
  browser tab (`sessionStorage`) and sent as a bearer token, and the page is
  sent with a strict Content-Security-Policy.
- **Data at rest.** Config and state directories are `0700`; keys, config,
  the usage database, the audit file and engine logs are `0600`. The audit
  file is hash-chained (`substrate audit verify`).

`substrate doctor` reports security findings for a given setup. The details
are in [docs/access.md](docs/access.md),
[docs/admin-api.md](docs/admin-api.md),
[docs/governance.md](docs/governance.md) and
[docs/operations.md](docs/operations.md).
