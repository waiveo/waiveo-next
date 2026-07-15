.PHONY: dev dev-up dev-down smoke
dev: dev-up smoke

dev-up:
	@cd scripts/devstub/relay && go build -o /tmp/waiveo-relay-stub . && (/tmp/waiveo-relay-stub &)
	@(node scripts/devstub/app/server.mjs &)
	@sleep 1

smoke:
	@bash scripts/dev-smoke.sh

dev-down:
	@pkill -f waiveo-relay-stub || true
	@pkill -f "devstub/app/server.mjs" || true
