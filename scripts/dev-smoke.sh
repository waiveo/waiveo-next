#!/usr/bin/env bash
# scripts/dev-smoke.sh — asserts the dev stack (feeder + relay) answers /healthz,
# that the relay booted its edge-automation stack, AND that it booted its
# schedule resolver. Delegates health to the Go probe (scripts/devsmoke): the
# feeder serves an ed25519-leaf TLS cert that some system curl builds (macOS
# LibreSSL) cannot handshake, so the probe is Go — matching the all-Go,
# all-ed25519 stack. It owns readiness (retries each endpoint ~10s) and prints
# SMOKE OK / SMOKE FAIL. Then this script asserts the relay's own log recorded
# (a) the automation engine loading the signed edge_rules from desired state
# and (b) the schedule resolver (internal/relay/schedulehost, REL-065) having
# run its boot-time resolve — the relay logs both lines at boot, before it
# binds the listener the probe waits on, so they are always present by the
# time SMOKE OK prints.
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

# 3. Schedule resolver: the relay resolved the carried schedule section at boot
# (a governed screen's schedule-driven program, or the explicit no-governing-
# schedule line — either way, the resolver ran).
SCHEDULE_LINE="SCHEDULE RESOLVER OK"
if [ -f "$RELAY_LOG" ] && grep -q "$SCHEDULE_LINE" "$RELAY_LOG"; then
  echo "$(grep -m1 -o "$SCHEDULE_LINE.*" "$RELAY_LOG")"
else
  echo "SCHEDULE RESOLVER FAIL: relay did not log '$SCHEDULE_LINE' in $RELAY_LOG" >&2
  exit 1
fi

# 4. Authoring loop: author a schedule edit over the api/1 HTTP surface and assert
# the relay's resolved screen program changes within the re-pull interval — the
# whole loop (api -> store -> desired-state -> relay re-pull -> re-resolve) live
# over real HTTP. The probe prints AUTHORING LOOP OK, or exits non-zero.
RELAY_LOG="$RELAY_LOG" bash scripts/authoring-demo.sh

# 5. Content loop: upload an asset to the feeder's content origin and fetch it
# back from the url the server returns, verifying content-addressing (the fetched
# bytes hash to the server-computed asset_ref) — the upload -> content-addressed
# store -> direct-fetch loop a real screen walks, live over real HTTP. The relay
# is never in this data path (REL-140). The probe prints CONTENT LOOP OK, or
# exits non-zero.
go run ./scripts/contentloop
