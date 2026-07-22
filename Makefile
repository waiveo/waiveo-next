.PHONY: dev dev-up dev-down smoke web-dev web-check web-build web-sse-check
# Repo-local run dir (git-ignored): pidfiles + the built binaries live here, so teardown
# is exact (by PID, not `pkill -f`) and nothing lands in a shared /tmp.
RUNDIR := $(CURDIR)/.dev
FEEDER_BIN := $(RUNDIR)/waiveo-feeder
RELAY_BIN := $(RUNDIR)/waiveo-relay

# The web console (web/) is a self-contained npm project built with Vite; the
# production bundle is embedded into the feeder binary (internal/app/webui/dist).
WEB_DIR := $(CURDIR)/web
WEB_EMBED_DIR := $(CURDIR)/internal/app/webui/dist

# Bring the stack up, smoke it, and ALWAYS tear it down (success or failure) — exit with
# the smoke result so `make dev` is a clean, self-cleaning check.
dev: dev-up
	@bash scripts/dev-smoke.sh; rc=$$?; $(MAKE) --no-print-directory dev-down; exit $$rc

# Idempotent: tear down any prior instance first, then start fresh and record PIDs.
# Wave 1: no app component yet (Wave 2) — dev is feeder + relay only.
dev-up: dev-down
	@mkdir -p $(RUNDIR)
	@# Fresh enrollment each run: the connection handshake (REL-030) verifies the
	@# relay's channel binding against the enrollment key the (fresh-per-process,
	@# in-memory-CA) feeder recorded at enroll, so a relay identity persisted
	@# against a prior feeder process could no longer bind. Clearing it keeps the
	@# self-contained dev check deterministic across repeated runs.
	@rm -rf $(RUNDIR)/relay-identity
	@# Fresh app store each run too: the authoring-loop demo (scripts/authoring-demo.sh)
	@# edits the seeded schedule, so starting from the pristine seed keeps the live
	@# demonstration deterministic across repeated runs (the feeder re-seeds an empty
	@# store at boot). The WAL/SHM sidecars are cleared alongside the db file.
	@rm -f $(RUNDIR)/feeder-store.db $(RUNDIR)/feeder-store.db-wal $(RUNDIR)/feeder-store.db-shm
	@go build -o $(FEEDER_BIN) ./cmd/waiveo-feeder
	@go build -o $(RELAY_BIN) ./cmd/waiveo-relay
	@{ $(FEEDER_BIN) >$(RUNDIR)/feeder.log 2>&1 & echo $$! > $(RUNDIR)/feeder.pid; }
	@# WAIVEO_RELAY_DEMO_OBSERVE drives one synthetic screen-on at boot so the demo
	@# edge rule fires end to end (its automation.run rides the telemetry channel to
	@# the app event log) — the live input the observability probe reads back. The
	@# real device-state source is deferred hardware, so the dev stack synthesizes it.
	@{ WAIVEO_RELAY_DEMO_OBSERVE=1 $(RELAY_BIN) >$(RUNDIR)/relay.log 2>&1 & echo $$! > $(RUNDIR)/relay.pid; }

smoke:
	@bash scripts/dev-smoke.sh

dev-down:
	@[ -f $(RUNDIR)/feeder.pid ] && kill $$(cat $(RUNDIR)/feeder.pid) 2>/dev/null || true
	@[ -f $(RUNDIR)/relay.pid ] && kill $$(cat $(RUNDIR)/relay.pid) 2>/dev/null || true
	@rm -f $(RUNDIR)/feeder.pid $(RUNDIR)/relay.pid $(RUNDIR)/feeder.log $(RUNDIR)/relay.log

# --- Web console ------------------------------------------------------------
# Assumes deps are installed (`cd web && npm ci`). The Vite dev server proxies
# /api + /events + /content to the running Go feeder (see web/vite.config.ts).
web-dev:
	@npm --prefix $(WEB_DIR) run dev

# The web gate: typecheck (tsc --noEmit) + lint (eslint) + unit tests (vitest run).
web-check:
	@npm --prefix $(WEB_DIR) run check

# Live SSE-through-Vite-proxy check (Task 2 regression guard). Brings the
# feeder+relay stack up, boots the real Vite dev server against it, asserts the
# /events/v1 live stream reaches a client THROUGH the proxy (a fresh subscribe
# opens its 200 text/event-stream headers immediately; a recorded event's bytes
# stream through), then ALWAYS tears the stack down. Guards the http-proxy-3
# header-flush hook in web/vite.config.ts — without it a fresh SSE subscribe
# hangs with no `open`, silently stranding every `live` binding on its static
# value. Assumes web deps are installed (`cd web && npm ci`).
web-sse-check: dev-up
	@node $(WEB_DIR)/scripts/sse-proxy-probe.mjs; rc=$$?; $(MAKE) --no-print-directory dev-down; exit $$rc

# Production build into web/dist, then refresh the embedded copy the feeder
# serves from (internal/app/webui/dist) so a subsequent `make dev` / feeder build
# embeds the real SPA. The built output is git-ignored; the committed `.gitkeep`
# sentinel (recreated below after the copy wipes it) is what keeps
# `//go:embed all:dist` compiling — and the Go-string placeholder shell is what
# keeps `go build ./...` serving a valid shell — when this has not been run.
web-build:
	@npm --prefix $(WEB_DIR) run build
	@rm -rf $(WEB_EMBED_DIR)
	@mkdir -p $(WEB_EMBED_DIR)
	@cp -R $(WEB_DIR)/dist/. $(WEB_EMBED_DIR)/
	@touch $(WEB_EMBED_DIR)/.gitkeep
