# Access: keys, teams, prices and budgets

[← README](../README.md) · [Documentation index](../README.md#documentation)

## Keys and access

```sh
emx key add alice --team research --models 'qwen*' --rpm 60 --expires 90d   # prints the key once
emx key add ops --role admin
emx key add compliance --role auditor
emx key list                       # role, team, expiry, last use, status
emx key set alice --expires 2027-01-31 --tokens-per-day 2000000 --max-output-tokens 4096
emx key rotate alice --grace 24h   # new secret printed once; the old one works for 24h
emx key disable alice              # refused at once, history kept (key enable alice)
emx key rm alice
```

- **Open mode:** with no keys and no master key, the gateway is open: every
  caller is admin. Whether it may run open is decided when `serve` starts:
  only on a loopback listen address, or with `--insecure`. An open gateway
  answers 403 unless the request
  - is addressed to `localhost`, `::1` or a `127.0.0.0/8` address in its
    `Host` header (`--insecure` lifts this rule only);
  - carries no `Origin` other than the gateway's own scheme, host and port
    (`Origin: null` is refused);
  - does not carry `Sec-Fetch-Site: cross-site`.

  So a web page open in the operator's browser cannot use an open gateway.
- **Removing the last key:** `emx key rm` and `emx key disable`
  print a warning on stderr when no usable key is left. A gateway that was
  not permitted to run open (a non-loopback address without `--insecure`)
  does not become open: it answers 401 to everything until a key exists or
  it is restarted with a master key.
- **Once any key exists,** every request needs one, sent as
  `Authorization: Bearer` or `x-api-key`.
- **`user` keys:** inference and `/v1/models`, limited to their (and their
  team's) model globs, plus `/emx/v1/usage` and `/requests` filtered to
  their own records.
- **`auditor` keys:** read-only: every client's usage and requests, events,
  flags, errors, reports, keys, teams, `/metrics` and `policy/test`. No
  inference, and no starting, stopping or reloading.
- **`admin` keys:** everything. The master key (`$EMX_API_KEY`, or
  `serve --api-key-file F` for a file of mode 0600; never an argument, since
  other users can read command lines) is an admin key.
- **Lifecycle:** expired and disabled keys get 401, recorded under the key's
  name (`error_kind` `auth_failed`). `key rotate` replaces the secret; the old
  one keeps working until `--grace` ends. `key list` shows when each key was
  last used, from the usage database; Prometheus exports
  `emx_key_expiry_seconds`.
  At most 60 `auth_failed` (401) records are stored per minute; see
  [Governance](governance.md#prompt-policy-classification-and-errors).
- **Quotas:** going over `rpm` or `tokens-per-day` (or a team limit) returns
  429 with `Retry-After` set to when the limit frees up (the oldest request
  leaving the minute window; local midnight or the first of the month for
  budgets). Every inference response carries OpenAI-style
  `x-ratelimit-limit-requests` / `-remaining-requests` / `-reset-requests`
  and `x-ratelimit-limit-tokens` / `-remaining-tokens` / `-reset-tokens` for
  the tightest key or team limit, so SDKs back off on their own. Usage is
  attributed to the key's name, which the client can't forge.
- **Output caps:** `gateway.max_output_tokens` and a key's
  `--max-output-tokens` cap each request's `max_tokens` /
  `max_completion_tokens` (Anthropic `max_tokens`, Responses
  `max_output_tokens`); the lower one wins, and a request without a limit gets
  the cap. The response says `X-Emx-Max-Output-Tokens` when it applied.
- **Storage:** keys are stored only as SHA-256 hashes in
  `~/.config/emx/keys.yaml` (mode 0600).
- **Exposure:** binding to an address other than loopback requires at least
  one key or a master key, or `--insecure`, and warns while the gateway
  serves plain HTTP. `--insecure` allows open mode on that address and lets
  open-mode requests be addressed to any host name.
- **Engine keys:** the client's key is never forwarded. The engine receives
  its own key, set with `--api-key` at launch or registered with the model.
  Every builtin engine gets it in its environment (`launch.api_key_env`); an
  engine whose manifest maps it to an argument gets it on its command line,
  where the host's process list shows it (see
  [Builtin engines](engines.md#builtin-engines)). A registered key is kept in
  `models.yaml` (owner-only) and shown as `***` everywhere else: listings,
  `run --dry-run`, `emx logs`, error messages and event records. Typing
  `--api-key VALUE` puts the value on *your* command line and in your shell
  history; registering through the settings API (`POST /emx/v1/models`)
  avoids that.

## Teams, prices and budgets

```sh
emx price set 'qwen3.8-*' --prompt 0.40 --completion 1.20 --cached 0.10   # per million tokens
emx price set default --prompt 0.20 --completion 0.60 --currency EUR
emx team set research --tokens-per-month 500000000 --budget-per-month 2000 --alert-at 0.8 --models 'qwen*'
emx key set alice --team research
emx team list                       # each limit's use this day/month
emx usage --by team --since 30d     # tokens and cost per team
```

- **Prices** are per million tokens: an exact model name wins over the
  longest matching glob, then `default`. Cached prompt tokens use `cached`
  when set, else the prompt price. Local models have no invoice: the price
  is whatever the operator charges back (hardware, power) or the hosted
  equivalent. Every request records its `cost` (none when unpriced or
  unmetered); `report` and `usage` sum it.
- **Teams** group keys (`key add/set --team`). A team's `tokens_per_day`,
  `tokens_per_month` and `budget_per_month` (cost) are checked before each
  request: once used up, requests get 429 with `error_kind`
  `budget_exceeded` until the next local day or month. Limits are checked
  before a request and charged after it, so the request that crosses a limit
  completes. A team's `models` globs narrow what its keys may use.
- **Alerts:** crossing `alert_at` (default 80%) of any limit logs a line and
  records a `budget_alert` event, once per limit and period; Prometheus
  exports `emx_team_budget_used_ratio` for alerting rules.
