#!/usr/bin/env bash
# scripts/authoring-demo.sh — the live authoring-loop demonstration for `make dev`.
# Against the already-running feeder + relay, it authors a schedule edit over the
# api/1 HTTP surface and asserts the relay's resolved screen program changes within
# the re-pull interval — the whole loop (api -> store -> desired-state -> relay
# re-pull -> schedulehost re-resolve) exercised end to end over real HTTP.
#
# The work is a Go probe (scripts/authoringdemo), not curl: the feeder serves its
# api over an ed25519-leaf TLS cert some system curl builds (macOS LibreSSL) cannot
# handshake, so a curl edit would spuriously fail against a healthy server. On
# success the probe prints AUTHORING LOOP OK; this wrapper exits with its status.
set -euo pipefail
cd "$(dirname "$0")/.."

go run ./scripts/authoringdemo
