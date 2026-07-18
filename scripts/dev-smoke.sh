#!/usr/bin/env bash
# scripts/dev-smoke.sh — asserts the dev stack (feeder + relay) answers /healthz.
# Delegates to the Go probe (scripts/devsmoke): the feeder serves an ed25519-leaf
# TLS cert that some system curl builds (macOS LibreSSL) cannot handshake, so the
# probe is Go — matching the all-Go, all-ed25519 stack. It owns readiness (retries
# each endpoint ~10s) and prints SMOKE OK / SMOKE FAIL.
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./scripts/devsmoke
