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
	@go build -o $(FEEDER_BIN) ./cmd/waiveo-feeder
	@go build -o $(RELAY_BIN) ./cmd/waiveo-relay
	@{ $(FEEDER_BIN) & echo $$! > $(RUNDIR)/feeder.pid; }
	@{ $(RELAY_BIN) & echo $$! > $(RUNDIR)/relay.pid; }

smoke:
	@bash scripts/dev-smoke.sh

dev-down:
	@[ -f $(RUNDIR)/feeder.pid ] && kill $$(cat $(RUNDIR)/feeder.pid) 2>/dev/null || true
	@[ -f $(RUNDIR)/relay.pid ] && kill $$(cat $(RUNDIR)/relay.pid) 2>/dev/null || true
	@rm -f $(RUNDIR)/feeder.pid $(RUNDIR)/relay.pid
