# Governance: policy, audit and data protection

[← README](../README.md) · [Documentation index](../README.md#documentation)

## Prompt policy, classification and errors

Every request the gateway proxies is inspected before it reaches an engine.
The inspection is local and rule-based: no prompt leaves the machine, and no
model is involved.

- **Policy flags.** `policy.rules` match the prompt (system, user, assistant
  and tool text, from any API; also tool definitions, tool-call names and
  arguments, tool results, and rerank `query` and `documents`) against
  built-in detectors, keywords (whole words, case-insensitive) or RE2
  patterns. Each match is recorded on the
  request with its rule, detector, severity, count and a redacted sample
  (`AK****************LE`); the matched text itself is never stored, unless
  `logging.flagged_bodies` keeps flagged bodies for review.
- **Blocking.** A rule with `action: block` refuses the request with 400 and
  `error_kind` `policy_blocked`. The error names the rule, not the matched
  text, and the engine never sees the request.
- **Requests too large to inspect.** If a request's text exceeds
  `policy.scan_max_bytes` and a rule with `action: block` applies to it
  (its model, client and API), the request is refused with 400 "request too
  large to inspect", `error_kind` `policy_blocked`, and a flag with detector
  `too_large`. Where only flagging rules apply, the text beyond the limit is
  not scanned.
- **Exceptions.** `allow` (per rule, or `policy.allow` for every rule)
  ignores individual matches that contain an allowed keyword or match an
  allowed pattern: test card numbers, the company's own e-mail domain.
  `except_models` and `except_clients` exempt models or API keys from a rule.
  `unless` skips a rule for prompts containing a term; whoever writes the
  prompt controls that, so use it against false positives, never to exempt
  users from a block (use `except_clients` for that).
- **Categories.** The last user turn is labelled with a task category
  (`coding`, `math`, `translation`, `summarization`, `data_analysis`,
  `writing`, `extraction`, `reasoning`, `question_answering`, else
  `general`) by keyword and pattern scores. `classify.categories` replaces
  the built-in set.
- **Error kinds.** Every failure gets an `error_kind`: `auth_failed`,
  `forbidden`, `rate_limited`, `budget_exceeded`, `policy_blocked`, `bad_request`,
  `model_not_found`, `no_capacity`, `engine_start_failed`,
  `engine_unreachable`, `upstream_timeout`, `engine_error`,
  `engine_rejected` or `client_disconnected` (`substrate errors --kinds`).
  Refused API keys are recorded too, with the caller's address, at most 60
  `auth_failed` (401) records per minute in the usage database and the audit
  file. Further ones are only counted, in
  `substrate_auth_denials_unstored_total`, and are not part of
  `substrate_requests_total`.
- **Recorded values.** Text the client chooses is bounded before it is
  stored: `path`, `user_agent` and `client` are cut to 256 bytes, `error` to
  1024. A request that was not routed to a model and whose model name is not
  a valid one (letters, digits, `. _ : -`, at most 128 characters) is
  recorded with model `invalid` in usage records, metric labels and the
  audit file. In metrics, the model names of requests that never reached an
  engine get their own series only for the first 200 distinct names; later
  ones are counted under model `other`.
- **Audit log.** `logging.audit_file` appends one JSON line per request,
  refused key, administrative change and alert (`type` `request`,
  `access_denied`, `admin` or the alert kind; no bodies) for a SIEM or log
  shipper. Lines are hash-chained and the file rotates itself (see
  [Audit file integrity](#audit-file-integrity)); it is also reopened when
  an outside tool moves it away.

The defaults flag credentials (`secrets`, high), card, IBAN and social
security numbers (`financial`, high), e-mail addresses and phone numbers
(`contact_info`, low) and prompt-injection phrasing (`prompt_injection`,
medium), and block nothing. Rule and category changes apply within seconds
(see [Configuration reload](gateway.md#configuration-reload)).

```yaml
policy:
    enabled: true
    scan_max_bytes: 1048576          # prompt text scanned per request; beyond it, blocking rules refuse the request
    allow:                           # matches every rule ignores
        patterns: ['@mycorp\.com$']
    rules:
        - name: secrets
          severity: high              # low, medium, high, critical
          detectors: [api_key, aws_access_key, private_key, jwt, password_assignment]
        - name: codenames
          action: block               # flag (default) or block
          severity: critical
          keywords: ["Project Falcon"]
          patterns: ['\bFALCON-\d{4}\b']
          models: [qwen3.8-27b-tt]    # optional scope: models, clients, apis (exact names)
          except_clients: [legal-team]
        - name: financial
          detectors: [credit_card, iban]
          allow:
              keywords: ["4111 1111 1111 1111"]   # test card
          unless:
              keywords: ["PCI training"]
classify:
    enabled: true
    default: general
    categories:                       # optional; replaces the built-in set
        - name: legal
          keywords: [contract, liability, NDA]
logging:
    flagged_bodies: false
    audit_file: audit.jsonl           # relative to ~/.local/state/substrate
```

`substrate policy show` lists the rules, categories and detectors.
`substrate policy test "text"` (or `--file`, `--body request.json`) shows
what a prompt would be labelled and flagged with, without sending it.
Requests recorded before this feature have no category or flags, and their
failures are reported as `unclassified`.

## Data protection

- **Private files.** The config and state directories are `0700`, and keys,
  config, the usage database (with its WAL), the audit file and engine logs
  are `0600`. Directories and files left wider by an older version are
  tightened on the next run; `doctor` reports anything it could not fix.
- **Deletion leaves no copy.** The usage database runs with
  `secure_delete`, so pruned or erased rows are overwritten, and the WAL is
  truncated after every prune or erasure.
- **Retention.** `logging.retention_days` prunes usage records (requests,
  bodies, lifecycle events, samples) daily; `logging.audit_retention_days`
  separately prunes the admin trail and rotated audit files, so the record of
  who changed what can outlive usage data. Both default to 0 (keep
  everything); typical policies are 30-90 days of usage and a year or more of
  audit. `doctor` warns when audit retention is the shorter one.
- **Erasure.** `substrate prune --client NAME` (or `DELETE
  /substrate/v1/clients/{name}/data`, admin) deletes all of one client's
  request records and bodies. The erasure itself is recorded in the admin
  trail; the audit file keeps its lines, as the tamper-evident record.
- **Tamper-evident audit file.** Lines are hash-chained across rotated
  files, the chain's head is recorded beside the file, and the chain can be
  keyed (`gateway.audit_key_file`); `substrate audit verify` checks it. See
  [Audit file integrity](#audit-file-integrity). The file rotates at
  `logging.audit_max_mb` (default 100) to `audit-<UTC time>.jsonl`.

`usage` also reports average TTFT, average duration, and decode speed in
tokens per second. Decode speed is completion tokens divided by generation
time (total time minus TTFT), over metered streams. `--csv` prints any table
as CSV; `report --csv` is one table with a `SECTION` column and raw numbers.
`doctor` also checks the setup: plain HTTP on a public address, public
metrics, prompt storage without retention, keys without expiry, expiring or
unused for 90 days, keys in undefined teams, many admin keys, budgets
without prices and aliases to unregistered models. It lists registered
models whose files no longer exist (`/v1/models` marks them
`source_missing`, and the admin UI offers no Start for them).

Only traffic that passes through `substrate serve` or `substrate chat` is
metered. A client that calls an engine's port directly bypasses the meter.
GPU and memory samples are taken only while the gateway runs.

### Audit file integrity

- **Record format.** Each audit line starts with `"prev"` (the chain hash of
  the line before, across rotated files) and `"seq"` (its number in the
  chain, starting at 1; lines written by older versions have no `seq`).
  With an audit key it also carries `"mac"`. `substrate audit verify` (and
  `doctor`) finds an edited, inserted, removed or reordered line; lines from
  before chaining are reported and anchor the chain.
- **Head anchor.** After every line the writer records the chain head in
  `<audit file>.head` (mode 0600), for example
  `{"file":"audit.jsonl","count":1234,"hash":"…"}`. `audit verify` fails
  (exit 6) if the file ends before that record or no longer contains it: the
  tail was truncated or rewritten, or an older copy was restored. If the
  anchor is missing (the file was last written by an older version, or the
  anchor was deleted), verification passes with a warning, and the next
  write creates it. A file that is ahead of its anchor is accepted. If lines
  are removed and the gateway keeps writing, the next line is chained from
  the anchor, so `audit verify` keeps reporting `chain broken` at that
  point.
- **Keyed chain.** `gateway.audit_key_file` (default: unset, an unkeyed
  SHA-256 chain) is the path of a file holding a text secret of at least 32
  bytes (surrounding whitespace is ignored); relative paths are under the
  config directory:

  ```sh
  openssl rand -hex 32 > ~/.config/substrate/audit.key && chmod 600 ~/.config/substrate/audit.key
  ```

  The file must not be accessible by group or others. `substrate serve`
  refuses to start if it is missing, group- or other-accessible, or shorter
  than 32 bytes; `substrate doctor` reports that as `audit_key`. With a key,
  each line's chain hash is an HMAC-SHA256 stored in the line as `mac`, so
  someone who rewrites the file without the key cannot produce lines that
  verify. Verification with a key fails if a keyed line does not match (it
  was edited, or the key is wrong), if unkeyed lines follow keyed ones, or
  if the newest line is not keyed. Right after a key is configured on an
  existing audit file, `audit verify` and `doctor` therefore report "the
  newest line is not keyed" until the first line is written with the key.
  The key is read when the gateway starts or the setting changes. To change
  the key, move the old audit files away and start a new file. Keep a copy
  of the key off the host, to verify copies of the audit files there.
- **Verifying.**

  ```sh
  substrate audit verify [--key-file F] [--expect-head HASH [--expect-count N]] [--expect-first N]
  ```

  | Flag | Effect |
  | --- | --- |
  | `--key-file F` | audit key file, mode 0600 (default: `gateway.audit_key_file`); verification fails if the chain has keyed lines and no key is given |
  | `--expect-head HASH` | a head hash printed by an earlier verify and stored elsewhere (SIEM, ticket); fails (exit 6) unless the chain still contains the line with that hash, that is, nothing up to it was removed or rewritten. The chain may have grown since |
  | `--expect-count N` | default 0 (not checked). With `--expect-head`: the line must be record N. Alone: fails if the chain ends before record N |
  | `--expect-first N` | default 0 (not checked). The first record number printed by an earlier verify: fails if the chain now starts later, that is, old lines were removed since |

  **Missing oldest lines.** A chain that does not start at record 1 is what
  retention leaves when it prunes old rotated files, and also what deleting
  the oldest lines leaves. Retention therefore records the oldest record it
  kept in `<audit file>.first`; verify fails when the chain starts later than
  that, or starts after record 1 without such a record.

  On success it prints `head: count N hash H (first record kept: F)`, where
  `count` is the head line's `seq`; warnings are printed as `warning: ...`.
  `--json` adds `head` (`file`, `count`, `hash`), `first`, `keyed`, `warnings`
  and `expected_head_count`.
  If the file holding a recorded head has been pruned by audit retention,
  `--expect-head` fails: record heads more often than
  `logging.audit_retention_days`. `doctor` reports a broken chain but not
  the warnings.
- **What it does not detect.** Someone who can write both the audit file
  and its `.head` can roll both back to an earlier state, or truncate the
  file and delete the anchor (a warning only). Without a key, they can also
  rewrite the whole file with a consistent chain, or delete the oldest lines
  and rewrite `.first` to match. Each of these is caught only by
  `--expect-head` / `--expect-count` / `--expect-first` with the printed
  values stored off the host, so record the printed head regularly (or ship the file, or the
  `admin` webhook, to a SIEM). Someone who also holds the audit key can
  produce lines that verify.

## Alerts and the admin trail

```yaml
alerts:
    webhooks:
        - url: https://hooks.slack.com/services/T000/B000/XXXX
          format: text                     # {"text": ...}: Slack, Teams, Mattermost, Google Chat
          kinds: [budget_alert, engine_exited, key_expiring]
        - url: https://siem.example.com/ingest
          headers: {Authorization: Bearer ...}
          kinds: [admin]                   # json: {kind, detail, actor, time, host}
```

- **Kinds:** `budget_alert`, `config_reload_failed`, `key_expiring` (keys
  expiring within 7 days or expired in the last 7, once a day per key),
  `engine_exited`, `engine_start_failed` and `admin`; no `kinds` means all.
  Deliveries time out after 5 seconds and are retried once; failures are
  logged with the URL's path (its secret) removed.
- **Admin trail:** every administrative change is recorded as an `admin`
  event (`substrate events`), an audit-log line (`type` `admin`) and an
  `admin` webhook, with who made it: `key add/set/rotate/disable/enable/rm`,
  `team set/rm`, `price set/rm`, `alias set/rm`, `model add/rm`, `engine set`
  (actor: `$SUBSTRATE_ACTOR`, else the OS user), and the management API's
  `ensure`, `stop` and `reload` (actor: the API key). Edits to `config.yaml`
  itself show up as `config_reloaded` events naming the changed sections.
- **Request ids:** the engine receives the gateway's request id as
  `X-Request-Id`, so engine logs can be matched to `substrate requests --id`.
  A client's own `X-Request-Id` is stored as `client_request_id`, and
  `--id` matches either (see [Requests](gateway.md#requests)).
