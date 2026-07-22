.PHONY: dev dev-up dev-down smoke
# Repo-local run dir (git-ignored): pidfiles + the built binaries live here, so teardown
# is exact (by PID, not `pkill -f`) and nothing lands in a shared /tmp.
RUNDIR := $(CURDIR)/.dev
FEEDER_BIN := $(RUNDIR)/waiveo-feeder
RELAY_BIN := $(RUNDIR)/waiveo-relay

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
