#!/usr/bin/env bash
# scripts/dev-smoke.sh — asserts the dev stack (feeder + relay) answers /healthz
# AND that the relay booted its edge-automation stack. Delegates health to the Go
# probe (scripts/devsmoke): the feeder serves an ed25519-leaf TLS cert that some
# system curl builds (macOS LibreSSL) cannot handshake, so the probe is Go —
# matching the all-Go, all-ed25519 stack. It owns readiness (retries each endpoint
# ~10s) and prints SMOKE OK / SMOKE FAIL. Then this script asserts the relay's own
# log recorded the automation engine loading the signed edge_rules from desired
# state (Wave-1 Phase-2 relay-automation integration) — the relay logs that line
# at boot, before it binds the listener the probe waits on, so it is always present
# by the time SMOKE OK prints.
set -euo pipefail
cd "$(dirname "$0")/.."

# 1. Health: feeder + relay answer /healthz (prints SMOKE OK, or exits non-zero).
go run ./scripts/devsmoke

# 2. Automation stack: the relay compiled + loaded the desired-state edge_rules.
RELAY_LOG="${RELAY_LOG:-.dev/relay.log}"
AUTOMATION_LINE="automation engine loaded:"
if [ -f "$RELAY_LOG" ] && grep -q "$AUTOMATION_LINE" "$RELAY_LOG"; then
  echo "AUTOMATION STACK OK ($(grep -m1 -o "$AUTOMATION_LINE.*" "$RELAY_LOG"))"
else
  echo "AUTOMATION STACK FAIL: relay did not log '$AUTOMATION_LINE' in $RELAY_LOG" >&2
  exit 1
fi
