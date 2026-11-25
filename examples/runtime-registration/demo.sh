#!/usr/bin/env bash
# Runtime plugin registration demo.
#
# Starts the compose stack with fake commands, then — without restarting the
# daemon — registers a LoRa receiver plugin, confirms it with the emulated
# device button, runs it, registers a generated calibration view on top of it,
# asks the (scripted) assistant about the capture, replaces the receiver with
# a revised version and finally removes both. Every step's ViewDocument is
# captured and rendered with `flipctl-tui --render-viewdoc`, the same way
# docs/evidence is produced.
#
# Requirements on the host: bash, curl, python3 and one of podman-compose,
# `docker compose` or `podman compose`.
#
# Environment:
#   COMPOSE       compose command to use (default: autodetect)
#   DEMO_SANDBOX  "bwrap" adds compose.bwrap.yaml so commands run under
#                 bubblewrap inside the container (default: auto/fallback)
#   DEMO_PAUSE    seconds to wait between steps, e.g. to watch the Web UI at
#                 http://localhost:8080 or `make tui` in another terminal
#   OUT           directory for captured ViewDocuments and renders
#   KEEP          set to 1 to leave the stack running afterwards
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HERE="$ROOT/examples/runtime-registration"
OUT="${OUT:-$HERE/out}"
BASE="${FLIPCTL_URL:-http://localhost:8080}"
PAUSE="${DEMO_PAUSE:-0}"
PROJECT="${COMPOSE_PROJECT_NAME:-flipctl-runtime-demo}"
cd "$ROOT"
mkdir -p "$OUT"

if [[ -z "${COMPOSE:-}" ]]; then
  if command -v podman-compose >/dev/null 2>&1; then COMPOSE="podman-compose"
  elif docker compose version >/dev/null 2>&1; then COMPOSE="docker compose"
  elif podman compose version >/dev/null 2>&1; then COMPOSE="podman compose"
  else echo "no compose implementation found; set COMPOSE" >&2; exit 1
  fi
fi
FILES=(-f docker-compose.yml -f "$HERE/compose.yaml")
if [[ "${DEMO_SANDBOX:-}" == "bwrap" ]]; then FILES+=(-f "$HERE/compose.bwrap.yaml"); fi
compose() { $COMPOSE -p "$PROJECT" "${FILES[@]}" "$@"; }

step=0
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
pause() { if [[ "$PAUSE" != "0" ]]; then sleep "$PAUSE"; fi; }
json() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1], {"d": d}))' "$1"; }

# Registration writes run inside the container so they come from loopback:
# by default flipctld refuses registration from any other address.
local_api() { # METHOD PATH [manifest-file]
  if [[ $# -ge 3 ]]; then
    compose exec -T flipctld curl -sS -X "$1" -H 'Content-Type: application/yaml' --data-binary @- -w '\nHTTP %{http_code}\n' "http://localhost:8080$2" <"$3"
  else
    compose exec -T flipctld curl -sS -X "$1" -w '\nHTTP %{http_code}\n' "http://localhost:8080$2"
  fi
}

view() { curl -fsS "$BASE/api/v1/sessions/$SID/view"; }

# act ACTION_ID [PARAMS_JSON]: invoke a semantic session action on the current view.
act() {
  local doc payload params="${2:-}"
  [[ -n "$params" ]] || params='{}'
  doc="$(view)"
  payload="$(printf '%s' "$doc" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print(json.dumps({"actionId": sys.argv[1], "viewId": d["viewId"], "revision": d["revision"], "params": json.loads(sys.argv[2])}))
' "$1" "$params")"
  curl -fsS -X POST -H 'Content-Type: application/json' --data "$payload" "$BASE/api/v1/sessions/$SID/actions" >/dev/null
}

# wait_for PY_EXPR: poll the current view until the expression over d is true.
wait_for() {
  for _ in $(seq 1 100); do
    if view | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if eval(sys.argv[1]) else 1)' "$1"; then return 0; fi
    sleep 0.2
  done
  echo "timed out waiting for: $1" >&2
  return 1
}

# snap NAME: save the current ViewDocument and render it as the TUI does.
snap() {
  step=$((step + 1))
  local f
  f="$OUT/$(printf '%02d' "$step")-$1"
  view >"$f.json"
  compose exec -T flipctld sh -c 'cat >/tmp/view.json && flipctl-tui --render-viewdoc /tmp/view.json --render-width "${RENDER_WIDTH:-140}" --render-height 0 --render-color plain' <"$f.json" >"$f.txt"
  cat "$f.txt"
  pause
}

PLUGINS_PY=$(cat <<'PY'
import json, sys
d = json.load(sys.stdin)
print("registry v%s, sandbox=%s" % (d["registryVersion"], d["sandbox"]))
for p in d["plugins"]:
    granted = p.get("granted") if p["state"] == "active" else p["capabilities"]
    caps = ", ".join(k if v is True else "%s=%s" % (k, ",".join(v)) for k, v in (granted or {}).items()) or "none"
    print("  %-38s %-6s %-9s %-7s caps: %s  (%s)" % (p["id"], p.get("version", ""), p["tier"], p["state"], caps, p["source"]))
PY
)
ARCHIVE_PY=$(cat <<'PY'
import json, sys
for a in json.load(sys.stdin).get("archived", []):
    print("  %s v%s (owner %s)" % (a["id"], a["version"], a["owner"]))
PY
)
plugins() { curl -fsS "$BASE/api/v1/plugins" | python3 -c "$PLUGINS_PY"; }

cleanup() {
  if [[ "${KEEP:-0}" != "1" ]]; then compose down --remove-orphans >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

say "Starting flipctld (compose project $PROJECT, $COMPOSE)"
compose up --build -d flipctld >"$OUT/compose-up.log" 2>&1 || { tail -20 "$OUT/compose-up.log"; exit 1; }
for _ in $(seq 1 60); do curl -fsS "$BASE/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$BASE/readyz" >/dev/null
SID="$(curl -fsS -X POST "$BASE/api/v1/sessions" | json 'd["sessionId"]')"
echo "Observer session $SID. Web UI: $BASE  TUI: go run ./cmd/flipctl-tui -addr $BASE"
plugins
snap home-before

say "Registration from outside the container is refused by default"
probe="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/yaml' --data-binary @"$HERE/lora-rx.yaml" "$BASE/api/v1/plugins/field.lora-rx")"
echo "PUT from host -> HTTP $probe (expected 403: registration is localhost-only)"
if [[ "$probe" != "403" ]]; then
  echo "note: this client reached the daemon over loopback; cleaning up" >&2
  local_api DELETE /api/v1/plugins/field.lora-rx >/dev/null || true
fi

say "Register field.lora-rx v0.1.0 at runtime (declares /dev/ttyLoRa0)"
local_api PUT /api/v1/plugins/field.lora-rx "$HERE/lora-rx.yaml"
wait_for 'any(b["id"] == "pending-registrations" for b in d["blocks"])'
snap pending-lora-rx

say "Press the (emulated) device button to confirm"
act device.confirm_registration '{"pluginId":"field.lora-rx"}'
wait_for 'any(b["id"] == "ephemeral-apps" for b in d["blocks"])'
snap home-with-lora-rx
plugins

say "Run the new plugin"
act app.open '{"appId":"field.lora-rx"}'
snap lora-rx-form
act job.start '{"appId":"field.lora-rx"}'
wait_for 'd["viewId"].endswith("-done")'
snap lora-rx-done
curl -fsS "$BASE/api/v1/jobs/j-1" | python3 -c 'import json,sys; j=json.load(sys.stdin); print("job %s (%s v%s) ran with sandbox=%s" % (j["id"], j["appId"], j["pluginVersion"], j["result"].get("sandbox", "none")))'

say "Register a generated calibration view that composes the receiver and the assistant"
act navigation.back
local_api PUT /api/v1/plugins/assistant.c-demo.lora-calibration "$HERE/lora-calibration.yaml"
wait_for 'any(b["id"] == "pending-registrations" for b in d["blocks"])'
snap pending-calibration
act device.confirm_registration '{"pluginId":"assistant.c-demo.lora-calibration"}'
wait_for 'sum(len(b.get("items", [])) for b in d["blocks"] if b["id"] == "ephemeral-apps") == 2'
snap home-with-calibration

say "Capture packets from the calibration view, then ask the assistant"
act app.open '{"appId":"assistant.c-demo.lora-calibration"}'
act view.capture '{"appId":"assistant.c-demo.lora-calibration","action":"capture"}'
wait_for 'any(b["kind"] == "progress" for b in d["blocks"])'
snap calibration-capturing
wait_for 'any(b["kind"] == "table" for b in d["blocks"]) and not any(b["kind"] == "progress" for b in d["blocks"])'
act view.advise '{"appId":"assistant.c-demo.lora-calibration","action":"advise"}'
wait_for 'any(b["id"] == "assistant-reply" for b in d["blocks"])'
snap calibration-answer

say "Replace field.lora-rx with v0.2.0 (same capabilities: applied immediately)"
act navigation.back
local_api PUT /api/v1/plugins/field.lora-rx "$HERE/lora-rx-v2.yaml"
act app.open '{"appId":"field.lora-rx"}'
wait_for 'any(f["id"] == "sf" for b in d["blocks"] if b["kind"] == "form" for f in b["fields"])'
snap lora-rx-v2-form
plugins

say "Remove both plugins"
act navigation.back
local_api DELETE /api/v1/plugins/assistant.c-demo.lora-calibration
local_api DELETE /api/v1/plugins/field.lora-rx
wait_for 'not any(b["id"] == "ephemeral-apps" for b in d["blocks"])'
snap home-after
plugins
echo
echo "Archived (restorable with POST /api/v1/plugins/{id}/restore):"
curl -fsS "$BASE/api/v1/plugins?archived=1" | python3 -c "$ARCHIVE_PY"
echo
echo "Daemon log (registration and sandbox lines):"
compose logs flipctld 2>&1 | grep -E 'plugin (registered|removed)|sandbox=|WITHOUT' | head -20 || true
say "Done. Captured views are in ${OUT#"$ROOT/"}"
