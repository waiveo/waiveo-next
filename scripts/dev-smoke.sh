#!/usr/bin/env bash
# scripts/dev-smoke.sh — asserts the dev stack answers on both ports.
# Owns readiness: retries each port for ~10s so it does not depend on a fixed
# start-up sleep in the Makefile (cold Node / freshly-built Go can be slow).
set -euo pipefail
fail() { echo "SMOKE FAIL: $1" >&2; exit 1; }

probe() {
  local port=$1 body deadline=$(( SECONDS + 10 ))
  while :; do
    if body=$(curl -fsS -m 3 "http://127.0.0.1:${port}/healthz" 2>/dev/null); then
      echo "$body" | grep -q '"status":"ok"' || fail ":${port} wrong payload: $body"
      return 0
    fi
    [ "$SECONDS" -ge "$deadline" ] && fail "no listener on :${port} after ~10s"
    sleep 0.25
  done
}

for port in 7400 7401; do probe "$port"; done
echo "SMOKE OK"
