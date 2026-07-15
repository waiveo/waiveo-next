# api/1 codegen

Two generated clients come from `api/openapi.yaml`: TypeScript (for the web
UI) and Go (for the `waiveo` CLI and MCP server, per
`contracts/api-1.md`/principle 1). Both are pinned, both run in CI, and both
run locally in this repo's normal toolchain (Node per `.node-version`, Go
per `go.mod`) — neither needs a network call beyond the initial dependency
fetch.

## TypeScript — `openapi-typescript`

Pinned exact in `package.json` (`openapi-typescript` `7.13.0`, `typescript`
`5.9.3` — `5.9.3` rather than the newer `typescript@7.x` line because
`openapi-typescript@7.13.0` declares a `typescript@^5.x` peer dependency;
`5.9.3` is the newest release satisfying it). Lockfile (`package-lock.json`,
`lockfileVersion: 3`) is committed; `npm ci --ignore-scripts` installs from
it exactly, no `postinstall` scripts required.

```sh
cd scripts/codegen
npm ci --ignore-scripts
npm run generate        # generate:ts, then typecheck
```

- `generate:ts` runs `openapi-typescript ../../api/openapi.yaml -o
  ../../api/gen/ts/api.d.ts` — a pure schema→types transform, no I/O beyond
  reading the spec and writing the one file.
- `typecheck` runs `tsc --noEmit -p tsconfig.json` over
  `typecheck/smoke.ts`, which imports the generated `paths`/`components`
  types and constructs a handful of real values against them (both
  exemplar resources, the `Problem` shape, a paginated list envelope, the
  `Idempotency-Key` header on the automation run action). A schema-breaking
  edit to `api/openapi.yaml` — a renamed/removed field, a widened/narrowed
  type, a dropped response code — fails this compile, not just "the
  generator didn't crash." (Verified: temporarily assigning an
  off-registry `ErrorCode` value in `smoke.ts` produces a real `tsc`
  error; reverted before commit.)

`api/gen/ts/api.d.ts` is committed. CI regenerates it and runs
`git diff --exit-code` before type-checking, so an `api/openapi.yaml` edit
landed without regenerating fails the build on drift, not just on a type
error.

## Go — `oapi-codegen`

Pinned via `go.mod`'s `tool` directive (Go 1.24+), not a floating
`go install`: `go get -tool
github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.2` added a
`tool` line to `go.mod` and the full resolved graph to `go.sum` — the
same reproducibility a lockfile gives the npm side, using the mechanism
this repo already has at its root rather than a second tool (no
`tools.go` shim, no separate version file).

```sh
go tool oapi-codegen -config scripts/codegen/oapi-codegen.yaml api/openapi.yaml
go build ./...
go vet ./...
```

`scripts/codegen/oapi-codegen.yaml` generates `types,client` (matching
principle 1 — Go gets a generated *client*, since the server itself is
Node) into `api/gen/go/api.gen.go`, package `apiv1`. The generated client
also needs `github.com/oapi-codegen/runtime` at runtime; that is a normal
(non-tool) `require` in `go.mod`, added the same way any other imported
package would be.

`api/gen/go/api.gen.go` is committed. CI regenerates it and runs
`git diff --exit-code` before `go vet`, mirroring the TS drift check.

### A real tool limitation, and how it's worked around

`oapi-codegen` v2.7.2 (current latest as of this writing) does not fully
support OpenAPI 3.1 yet (tracked upstream:
<https://github.com/oapi-codegen/oapi-codegen/issues/373> — its own CLI
prints a warning to this effect on every run). Concretely: it cannot
resolve a JSON Schema 3.1-style nullable union (`type: ["string", "null"]`
or `type: ["integer", "null"]`), and errors out (`unhandled Schema type:
&[string null]`) on every field using one — which, correctly, `api/openapi.yaml`
uses for every genuinely-nullable field (`Cursor`, `ScopeNode.parent_id`,
`ScopeNode.account_state`, `Automation.max`, ...), since that is the
correct 3.1 idiom and the legacy 3.0 `nullable: true` keyword is rejected
outright by `@redocly/cli`'s structural validator under `openapi: 3.1.0`
(`Property "nullable" is not expected here`) — so downgrading the schema
to work around the code generator was not an option without breaking the
other, spec-valid, generator.

The fix used here is `oapi-codegen`'s own escape hatch: the `x-go-type`
vendor extension on each affected field (e.g. `x-go-type: "*string"` on
`Cursor`), which tells `oapi-codegen` what Go type to emit for that node
directly, bypassing the code path that doesn't yet understand a 3.1 null
union. `x-go-type` is an `x-`-prefixed vendor extension, so it is inert to
every other consumer — `@redocly/cli` ignores it, and `openapi-typescript`
ignores it too (it independently produces the correct `string | null` /
`number | null` from the same 3.1-native `type` array). The OpenAPI
document itself stays fully, idiomatically 3.1; only the Go generation
step needed a hint. When `oapi-codegen` closes #373, the `x-go-type` lines
can be removed without changing anything else.

## What runs where

Both steps run locally in this repo's normal toolchain and in CI
(`.github/workflows/pr-tier.yml`, `merge-tier.yml`), after the contract
validators. Neither step is CI-only: this repo's dev machine had both a
working Node and a working Go toolchain, so both were exercised and
verified end to end while authoring this contract (`npm ci
--ignore-scripts && npm run generate`; `go tool oapi-codegen ... && go
build ./... && go vet ./...`), not merely wired and hoped for. CI is still
the system of record for both — it runs on a clean checkout with pinned
action SHAs, which is the authoritative "does this actually work"
signal, not this machine's state.

## Regenerating after an `api/openapi.yaml` change

```sh
(cd scripts/codegen && npm ci --ignore-scripts && npm run generate)
go tool oapi-codegen -config scripts/codegen/oapi-codegen.yaml api/openapi.yaml
go build ./... && go vet ./...
git add api/gen scripts/codegen/package-lock.json go.mod go.sum
```

If `api/openapi.yaml`'s dependency surface changes (a new/removed schema,
a new nullable field needing its own `x-go-type`), regenerate both before
committing — CI's drift check will otherwise fail the build, which is the
intended behavior, not a false positive.
