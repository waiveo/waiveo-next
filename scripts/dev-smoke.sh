#!/usr/bin/env bash
# scripts/dev-smoke.sh — asserts the dev stack answers on both ports.
set -euo pipefail
fail() { echo "SMOKE FAIL: $1" >&2; exit 1; }
for port in 7400 7401; do
  body=$(curl -fsS -m 3 "http://127.0.0.1:${port}/healthz") || fail "no listener on :${port}"
  echo "$body" | grep -q '"status":"ok"' || fail ":${port} wrong payload: $body"
done
echo "SMOKE OK"
