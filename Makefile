.PHONY: dev dev-up dev-down smoke
# Repo-local run dir (git-ignored): pidfiles + the built relay stub live here, so teardown
# is exact (by PID, not `pkill -f`) and nothing lands in a shared /tmp.
RUNDIR := $(CURDIR)/.dev
RELAY_BIN := $(RUNDIR)/waiveo-relay-stub

# Bring the stack up, smoke it, and ALWAYS tear it down (success or failure) — exit with
# the smoke result so `make dev` is a clean, self-cleaning check.
dev: dev-up
	@bash scripts/dev-smoke.sh; rc=$$?; $(MAKE) --no-print-directory dev-down; exit $$rc

# Idempotent: tear down any prior instance first, then start fresh and record PIDs.
dev-up: dev-down
	@mkdir -p $(RUNDIR)
	@cd scripts/devstub/relay && go build -o $(RELAY_BIN) .
	@{ $(RELAY_BIN) & echo $$! > $(RUNDIR)/relay.pid; }
	@{ node scripts/devstub/app/server.mjs & echo $$! > $(RUNDIR)/app.pid; }

smoke:
	@bash scripts/dev-smoke.sh

dev-down:
	@[ -f $(RUNDIR)/relay.pid ] && kill $$(cat $(RUNDIR)/relay.pid) 2>/dev/null || true
	@[ -f $(RUNDIR)/app.pid ] && kill $$(cat $(RUNDIR)/app.pid) 2>/dev/null || true
	@rm -f $(RUNDIR)/relay.pid $(RUNDIR)/app.pid
