#!/usr/bin/env bash
# Run a substrate binary through the path every platform must get right: start an engine process, see it
# ready, list it, serve the gateway, stop the engine and see it gone. The "engine" is Python's http.server
# (scripts/smoke/pyhttp.yaml), so this needs only Python and curl; it touches nothing outside a temp dir.
#
#   scripts/smoke.sh ./substrate
set -euo pipefail

bin="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
export SUBSTRATE_HOME="$work/home"
port=7795
serve_pid=""
cleanup() {
  "$bin" stop --all >/dev/null 2>&1 || true
  [ -n "$serve_pid" ] && kill "$serve_pid" 2>/dev/null || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT

step() { echo "== $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

step "version and doctor"
"$bin" version
"$bin" doctor >/dev/null

step "register a stand-in engine and a model"
"$bin" engine add "$here/smoke/pyhttp.yaml" --trust
mkdir -p "$work/model"
echo '{"architectures":["Qwen3ForCausalLM"]}' > "$work/model/config.json"
"$bin" model add smoke "$work/model" --engine pyhttp

step "start it and wait until it is ready"
"$bin" run smoke
"$bin" ps
"$bin" ps --json > "$work/ps.json"
grep -q '"status": "ready"' "$work/ps.json" || fail "instance is not ready: $(cat "$work/ps.json")"
url="$(sed -n 's/.*"base_url": "\(.*\)",/\1/p' "$work/ps.json")"
curl -fsS -o /dev/null "$url/" || fail "the engine does not answer at $url"

step "a second command still sees the same running process"
"$bin" ps --json > "$work/ps2.json"
grep -q '"status": "ready"' "$work/ps2.json" || fail "instance was lost between commands"

step "gateway serves and lists the instance"
"$bin" serve --port "$port" > "$work/serve.log" 2>&1 &
serve_pid=$!
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "http://127.0.0.1:$port/substrate/v1/whoami" 2>/dev/null && break
  sleep 0.2
done
# Responses go to a file first: grep -q stops reading at its first match, which curl reports as a failure.
curl -fsS -o "$work/instances.json" "http://127.0.0.1:$port/substrate/v1/instances"
grep -q '"smoke.pyhttp"' "$work/instances.json" || fail "gateway does not list the instance: $(cat "$work/serve.log")"
curl -fsS -o /dev/null "http://127.0.0.1:$port/admin/" || fail "admin UI is not served"
curl -fsS -o "$work/metrics.txt" "http://127.0.0.1:$port/metrics"
grep -q '^substrate_' "$work/metrics.txt" || fail "no metrics"
kill "$serve_pid" 2>/dev/null || true
wait "$serve_pid" 2>/dev/null || true
serve_pid=""

step "stop it and see it gone"
"$bin" stop smoke
[ "$("$bin" ps --json | tr -d '[:space:]')" = "[]" ] || fail "instance still listed after stop: $("$bin" ps --json)"
if curl -fsS -o /dev/null --max-time 3 "$url/" 2>/dev/null; then
  fail "the engine process still answers at $url after stop"
fi

echo "smoke test passed"
