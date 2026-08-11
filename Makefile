.PHONY: dev dev-up dev-down dev-key smoke web-dev web-check web-build web-sse-check web-e2e example-pack player-sideload
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
	@# 0700, not the umask default. The feeder PRINTS its one-time first-boot
	@# setup code on every unclaimed boot (SEC-120's "present it"), and the
	@# redirects below send that stdout to $(RUNDIR)/feeder.log. That code mints
	@# `owner` at the root scope and is redeemable over an UNAUTHENTICATED route,
	@# so a world-readable log inside a world-readable directory hands the box to
	@# any local reader. The directory gate is what closes it — the shell's `>`
	@# creates the log at the umask default and nothing here can change that.
	@mkdir -p $(RUNDIR) && chmod 700 $(RUNDIR)
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
	@# The dev API key every HTTP probe presents. Run BEFORE the feeder starts, so
	@# the two never write the auth database at once, and re-run every dev-up
	@# because it is idempotent: a key that still names a live session is left
	@# exactly as it is. See scripts/devcred's package doc.
	@$(MAKE) --no-print-directory dev-key
	@{ $(FEEDER_BIN) >$(RUNDIR)/feeder.log 2>&1 & echo $$! > $(RUNDIR)/feeder.pid; }
	@# This relay binds loopback (the WAIVEO_RELAY_LISTEN default), which is what
	@# keeps `make dev` from multicasting: discovery reads its default off the
	@# listen address, so a dev run does not M-SEARCH the LAN the laptop is on and
	@# does not probe whatever answers. There is deliberately no opt-out flag here
	@# to forget — see discoveryEnabled in cmd/waiveo-relay/main.go.
	@# WAIVEO_RELAY_DEMO_OBSERVE drives one synthetic screen-on at boot so the demo
	@# edge rule fires end to end (its automation.run rides the telemetry channel to
	@# the app event log) — the live input the observability probe reads back. The
	@# real device-state source is deferred hardware, so the dev stack synthesizes it.
	@{ WAIVEO_RELAY_DEMO_OBSERVE=1 $(RELAY_BIN) >$(RUNDIR)/relay.log 2>&1 & echo $$! > $(RUNDIR)/relay.pid; }

# Provision (or re-confirm) the local dev API key the HTTP probes present to the
# running feeder. `dev-up` runs this for you; run it by hand when driving the
# probes outside make, or after deleting the key file. It never prints the key —
# only where it wrote and what that key is authorized to do. The whole story
# (what authority, why, and where the file lives) is in ONE place:
# scripts/devcred's package doc.
dev-key:
	@go run ./scripts/devkey

smoke:
	@bash scripts/dev-smoke.sh

# Build the in-repo declarative example pack (examples/packs/menu-board) into a
# distributable zip artifact — a manifest, two ui-schema/1 page documents, and a
# locale catalog, nothing executable. The bytes are identical to what `make dev`'s
# pack smoke and the end-to-end test install over the real POST /api/v1/packs (one
# source of truth: examples/packs). The output lands in the git-ignored run dir.
example-pack:
	@mkdir -p $(RUNDIR) && chmod 700 $(RUNDIR)
	@go run ./scripts/examplepack -out $(RUNDIR)/menu-board.pack.zip

# Push the current player-v3 build onto the screen fleet (scripts/fleetsideload).
# The roster defaults to $WAIVEO_RELAY_ECP_TARGETS — the same screens the relay
# drives — and credentials come from the dev-lab env file; neither is ever
# spelled here. Override anything by passing flags: `make player-sideload
# SIDELOAD_ARGS="-devices hanger=192.0.2.51"`. Start with -dry-run.
#
# Not part of `make dev`: this touches real hardware.
player-sideload:
	@go run ./scripts/fleetsideload $(SIDELOAD_ARGS)

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

# The real click-through gate (Wave 4b, Task 5): the anti-regression proving a
# user-facing control actually worked, not just rendered. Build the SPA into the
# feeder embed, build the example-pack zip the spec installs, bring the FULL stack
# up (feeder + relay serving the real built SPA), then drive Chromium headless
# through the actual console — New -> fill -> Save (a row appears AND lands in the
# pack-data API), select -> edit -> Save (persists), Delete (gone), and every core
# nav item's heading — and ALWAYS tear the stack down, exiting with the spec result.
# A dead control (a button that renders but does nothing) fails this where a
# render-only unit test stays green. Playwright is DEV-ONLY (Apache-2.0), never in
# the production bundle; the Chromium binary is a one-time `cd web && npx playwright
# install chromium` (cached in the OS, never committed). Assumes web deps installed
# (`cd web && npm ci`). Sub-makes keep the build/pack/up order deterministic.
web-e2e:
	@$(MAKE) --no-print-directory web-build
	@$(MAKE) --no-print-directory example-pack
	@$(MAKE) --no-print-directory dev-up
	@PW_PACK_ZIP=$(RUNDIR)/menu-board.pack.zip npm --prefix $(WEB_DIR) run e2e; rc=$$?; $(MAKE) --no-print-directory dev-down; exit $$rc
