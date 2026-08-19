# Traceability: data-model/1

One row per requirement ID `contracts/data-model-1.md` defines. Format: `conformance/traceability/README.md`.

**Variables (DAT-130–DAT-138):** every row is still `TBD-wave1`, and the reason
has CHANGED — read it before assuming this section is unbuilt.

The section is no longer text ahead of its implementation. The variables
resource family has landed: a `variables` store kind and table, the api/1 CRUD
family (`api/openapi.yaml` `/variables`), the DAT-134/135 resolution
(`internal/datamodel/variable.go`), the DAT-137 event
(`internal/events/variable_changed.go` +
`internal/app/api/variables.go`), and the two `rules/1` halves it exists to
serve — the `variable` condition's environment and the `variable_write` action's
sink (`internal/app/api/automations_exec.go`). All three published field-level
codes are emitted, and their group has been deleted from
`conformance/unimplemented-error-codes.json`.

What is still absent is a CORPUS CASE, which is what this column measures. The
rules are exercised by Go tests instead — `internal/datamodel/variable_test.go`
(DAT-131a/132/133/134/135/136 including the per-name and delete-re-exposes
cases), `internal/app/api/variables_e2e_test.go` (the same rules over real HTTP,
plus DAT-137's three events) — and a Go test is deliberately NOT a corpus case,
so naming one here would be the overclaim this column exists to prevent. These
rows go to `covered` when the data-model/1 driver grows cases for them.

Two of these will want a driver rather than a corpus case even then. DAT-135 —
resolution is per NAME, not as a unit — is a statement about what a lookup does
NOT do, and the case for it is two variables set at different depths, which
proves today's implementation separates them rather than that no future one
couples them. DAT-138 forbids storing secrets as variables and no case can
observe a rule about what an operator ought not to put in a field; it is
enforceable only by a review of what the platform itself writes there, plus a
warning wherever an operator would type one.

**DAT-138 lost its only operator-facing warning on 2026-08-19, and the write
paths it warns about are all still live.** Recorded here because the honest
state of this row is worse than "TBD-wave1" implies. The warning used to sit on
the Variables console page — a DAT-138-tagged panel reading *"Don't put
passwords, API keys or tokens here. A variable is readable by every automation,
is published in full on the activity feed each time it changes, and is visible
to anyone who can see the scope node it sits at."* That page
(`web/src/routes/variables/`) was deleted with the console strip that moved the
variables UI toward the `waiveo/system` pack
(`plans/2026-08-11-capability-ownership-map.md` §2.2 group E). **Do not go
looking for that file; it is gone.** What was NOT deleted is anything it
mitigated:

- `/variables` and `/variables/{variable_id}` remain a full api/1 CRUD family
  (`api/openapi.yaml`, `internal/app/api/variables.go`, mounted at
  `internal/app/api/api.go:378`), and the console's typed module is still wired
  (`web/src/api/resources.ts`).
- `rules/1` `variable_write` (RUL-220) is LIVE on the app peer, not a no-op —
  `internal/rules/eval/action.go` dispatches it through `VariableSink`, and its
  own comment records that it was removed from the no-op default arm precisely
  so the app peer performs it.
- The store is unencrypted and the value is republished on `variable.changed`
  (DAT-137), so the disclosure the warning described is unchanged.

So DAT-138 currently has **zero enforcement of any kind** — no corpus case, no
driver, no schema check, and now no warning on any surface. The prohibition is
stated only in `contracts/data-model-1.md` and in the api/1 spec commentary for
the variables family. Re-authoring the operator-facing half belongs to whichever
surface owns variables next (the `waiveo/system` pack per the map, or any core
page that reintroduces the family); the api/1 `description` fields for
`createVariable`/`updateVariable` are the other candidate and are deliberately
untouched here, because editing them regenerates `api/gen/ts/api.d.ts` and the
Go client and would have to clear the codegen drift gate in the same change.
Whoever closes this row should close that gap first, not merely add a case.

**Where the three VARIABLE_* codes are raised, and where they are not.** Same
shape as the DEVICE_IDENTITY_INCOMPLETE note below, and recorded for the same
reason. `VariableCreate` declares `name` with DAT-131a's `pattern` and `value`
as a three-scalar `oneOf`, so the request-body schema refuses a bad name or a
non-scalar value before the row validator runs: over HTTP those two arrive as a
422 VALIDATION_FAILED naming the field, not as `VARIABLE_NAME_INVALID` /
`VARIABLE_VALUE_INVALID`. The codes are what the row validator raises on the
path a request body does not pass through — and unlike DEVICE_IDENTITY_INCOMPLETE
that path is a LIVE one rather than a seed or a migration: a rule's
`variable_write` action (RUL-220) writes straight to the store, and its refusal
carries the code into the run report an operator reads
(`TestVariableWriteRefusalsCarryTheirPublishedCodes`).
`VARIABLE_NAME_DUPLICATE` is the one of the three that DOES reach an api/1
response with its published code, because the uniqueness rule is decided inside
the write transaction, past every schema check.

**Where DEVICE_IDENTITY_INCOMPLETE is raised, and where it is not.** A reader
tracing this code from data-model/1's Field-level error register should not go
looking for it in an api/1 response, because it does not appear in one.
`AdoptedDeviceCreate` declares `driver` and `native_id` as `required` with
`minLength: 1`, so the request-body schema refuses an absent or empty value
before the row validator runs — for BOTH fields, not only `driver`. The code is
what that validator raises on the paths a request body never passes through: an
id migration, a seed, a snapshot build.

This is recorded rather than resolved. Making it reachable over HTTP would mean
weakening a declared schema so that a second refusal could fire, which buys a
reader nothing and costs a caller the clearer of the two answers.

| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| DAT-001 | `contracts/data-model-1.md#scope-node-tree` | `DAT-001-valid-scope-node-tree` | covered |
| DAT-001a | `contracts/data-model-1.md#scope-node-tree` | - | TBD-wave1 |
| DAT-002 | `contracts/data-model-1.md#scope-node-tree` | `DAT-001-valid-scope-node-tree`, `DAT-002-invalid-create-scope-node-under-nonexistent-parent` | covered |
| DAT-003 | `contracts/data-model-1.md#scope-node-tree` | `DAT-001-valid-scope-node-tree` | covered |
| DAT-004 | `contracts/data-model-1.md#scope-node-tree` | `DAT-001-valid-scope-node-tree`, `DAT-004a-valid-screen-and-device-identity-rows` | covered |
| DAT-004a | `contracts/data-model-1.md#scope-node-tree` | - | TBD-wave1 |
| DAT-004b | `contracts/data-model-1.md#scope-node-tree` | - | TBD-wave1 |
| DAT-004c | `contracts/data-model-1.md#screen-program-override` | - | TBD-wave1 |
| DAT-004d | `contracts/data-model-1.md#screen-program-override` | - | TBD-wave1 |
| DAT-005 | `contracts/data-model-1.md#resource-row-baseline` | `DAT-001-valid-scope-node-tree` | covered |
| DAT-005a | `contracts/data-model-1.md#resource-row-baseline` | `DAT-005b-invalid-identity-rows-missing-baseline` | covered |
| DAT-005b | `contracts/data-model-1.md#resource-row-baseline` | `DAT-004a-valid-screen-and-device-identity-rows`, `DAT-005b-invalid-identity-rows-missing-baseline` | covered |
| DAT-006 | `contracts/data-model-1.md#resource-row-baseline` | `DAT-001-valid-scope-node-tree`, `DAT-005b-invalid-identity-rows-missing-baseline`, `DAT-006-invalid-create-row-at-nonexistent-scope-node` | covered |
| DAT-007 | `contracts/data-model-1.md#resource-row-baseline` | - | TBD-wave1 |
| DAT-008 | `contracts/data-model-1.md#resource-row-baseline` | `DAT-001-valid-scope-node-tree` | covered |
| DAT-010 | `contracts/data-model-1.md#org-node-account-state-and-entitlements` | `DAT-010-valid-org-account-states` | covered |
| DAT-011 | `contracts/data-model-1.md#org-node-account-state-and-entitlements` | `DAT-010-valid-org-account-states` | covered |
| DAT-012 | `contracts/data-model-1.md#org-node-account-state-and-entitlements` | - | TBD-wave1 |
| DAT-013 | `contracts/data-model-1.md#org-node-account-state-and-entitlements` | `DAT-010-valid-org-account-states` | covered |
| DAT-014 | `contracts/data-model-1.md#org-node-account-state-and-entitlements` | - | TBD-wave1 |
| DAT-020 | `contracts/data-model-1.md#referential-integrity-attachment-and-deletion` | `DAT-020-invalid-delete-scope-node-carrying-a-child` | covered |
| DAT-021 | `contracts/data-model-1.md#referential-integrity-attachment-and-deletion` | `DAT-021-invalid-delete-scope-node-a-row-is-placed-at` | covered |
| DAT-022 | `contracts/data-model-1.md#referential-integrity-attachment-and-deletion` | `DAT-022-invalid-delete-org-scope-node` | covered |
| DAT-030 | `contracts/data-model-1.md#time-as-data` | `DAT-033-valid-screen-tz-override` | covered |
| DAT-031 | `contracts/data-model-1.md#time-as-data` | `DAT-033-valid-screen-tz-override` | covered |
| DAT-032 | `contracts/data-model-1.md#time-as-data` | `DAT-033-valid-screen-tz-override` | covered |
| DAT-033 | `contracts/data-model-1.md#time-as-data` | `DAT-033-valid-screen-tz-override` | covered |
| DAT-034 | `contracts/data-model-1.md#time-as-data` | `DAT-033-valid-screen-tz-override` | covered |
| DAT-040 | `contracts/data-model-1.md#scheduling-core-playlist` | - | TBD-wave1 |
| DAT-041 | `contracts/data-model-1.md#scheduling-core-playlist` | - | TBD-wave1 |
| DAT-042 | `contracts/data-model-1.md#scheduling-core-playlist` | - | TBD-wave1 |
| DAT-043 | `contracts/data-model-1.md#scheduling-core-cast` | - | TBD-wave1 |
| DAT-050 | `contracts/data-model-1.md#scheduling-core-schedule` | `DAT-121-valid-misfire-catchup-vs-skip-by-kind`, `DAT-053-valid-screen-schedule-precedence-over-site` | covered |
| DAT-051 | `contracts/data-model-1.md#scheduling-core-schedule` | `DAT-051-valid-site-schedule-cascades-to-screen`, `DAT-111-valid-layered-daypart-holiday-over-base`, `DAT-053-valid-equal-priority-specificity-nearer-node-wins`, `DAT-075-valid-masked-preset-fires-on-fall-through` | covered |
| DAT-052 | `contracts/data-model-1.md#scheduling-core-schedule` | `DAT-118-valid-terminal-default-blank` | covered |
| DAT-053 | `contracts/data-model-1.md#scheduling-core-schedule` | `DAT-053-valid-screen-schedule-precedence-over-site`, `DAT-111-valid-layered-daypart-holiday-over-base`, `DAT-053-valid-equal-priority-specificity-nearer-node-wins`, `DAT-053-valid-equal-priority-same-node-lowest-id-wins`, `DAT-075-valid-masked-preset-fires-on-fall-through` | covered |
| DAT-060 | `contracts/data-model-1.md#scheduling-core-validity-window` | - | TBD-wave1 |
| DAT-061 | `contracts/data-model-1.md#scheduling-core-validity-window` | - | TBD-wave1 |
| DAT-062 | `contracts/data-model-1.md#scheduling-core-validity-window` | - | TBD-wave1 |
| DAT-070 | `contracts/data-model-1.md#scheduling-core-daypart` | `DAT-074-valid-daypart-display-power-off` | covered |
| DAT-071 | `contracts/data-model-1.md#scheduling-core-daypart` | `DAT-071-invalid-daypart-time-out-of-range`, `DAT-071-invalid-daypart-time-24-boundary`, `DAT-071-invalid-daypart-phantom-weekday`, `DAT-071-invalid-daypart-empty-days` | covered |
| DAT-072 | `contracts/data-model-1.md#scheduling-core-daypart` | `DAT-074-valid-daypart-display-power-off` | covered |
| DAT-073 | `contracts/data-model-1.md#scheduling-core-daypart` | `DAT-074-valid-daypart-display-power-off`, `DAT-073-invalid-within-schedule-daypart-overlap-rejected`, `DAT-073-invalid-midnight-wrap-collides-next-weekday` | covered |
| DAT-074 | `contracts/data-model-1.md#scheduling-core-daypart` | `DAT-074-valid-daypart-display-power-off` | covered |
| DAT-075 | `contracts/data-model-1.md#scheduling-core-daypart` | `DAT-074-valid-daypart-display-power-off`, `DAT-075-valid-masked-preset-fires-on-fall-through`, `DAT-075-valid-fall-back-boundary-refires-preset`, `DAT-075-valid-apply-carries-baseline-until-the-bound-rows-change` | covered |
| DAT-076 | `contracts/data-model-1.md#scheduling-core-daypart` | `DAT-074-valid-daypart-display-power-off`, `DAT-121-valid-misfire-catchup-vs-skip-by-kind` | covered |
| DAT-077 | `contracts/data-model-1.md#scheduling-core-daypart` | `DAT-073-invalid-within-schedule-daypart-overlap-rejected`, `DAT-073-invalid-midnight-wrap-collides-next-weekday` | covered |
| DAT-080 | `contracts/data-model-1.md#scheduling-core-fallback` | - | TBD-wave1 |
| DAT-081 | `contracts/data-model-1.md#scheduling-core-fallback` | `DAT-117-valid-layered-fallback-through-precedence` | covered |
| DAT-082 | `contracts/data-model-1.md#scheduling-core-fallback` | - | TBD-wave1 |
| DAT-090 | `contracts/data-model-1.md#scheduling-core-preset-batch` | `DAT-092-valid-preset-batch-partial-failure` | covered |
| DAT-091 | `contracts/data-model-1.md#scheduling-core-preset-batch` | `DAT-092-valid-preset-batch-partial-failure` | covered |
| DAT-092 | `contracts/data-model-1.md#scheduling-core-preset-batch` | `DAT-092-valid-preset-batch-partial-failure` | covered |
| DAT-093 | `contracts/data-model-1.md#scheduling-core-preset-batch` | `DAT-092-valid-preset-batch-partial-failure` | covered |
| DAT-094 | `contracts/data-model-1.md#scheduling-core-preset-batch` | `DAT-121-valid-misfire-catchup-vs-skip-by-kind` | covered |
| DAT-100 | `contracts/data-model-1.md#platform-ownership` | `DAT-101-invalid-pack-owned-scheduler-rejected` | covered |
| DAT-101 | `contracts/data-model-1.md#platform-ownership` | `DAT-101-invalid-pack-owned-scheduler-rejected` | covered |
| DAT-102 | `contracts/data-model-1.md#platform-ownership` | `DAT-101-invalid-pack-owned-scheduler-rejected` | covered |
| DAT-110 | `contracts/data-model-1.md#dayparting-evaluation` | `DAT-051-valid-site-schedule-cascades-to-screen`, `DAT-119-valid-dst-fall-back-window-holds-through-repeat`, `DAT-119-valid-dst-spring-forward-window-begins-first-real-instant` | covered |
| DAT-111 | `contracts/data-model-1.md#dayparting-evaluation` | `DAT-111-valid-layered-daypart-holiday-over-base`, `DAT-053-valid-screen-schedule-precedence-over-site`, `DAT-053-valid-equal-priority-specificity-nearer-node-wins`, `DAT-053-valid-equal-priority-same-node-lowest-id-wins`, `DAT-075-valid-masked-preset-fires-on-fall-through`, `DAT-119-valid-dst-spring-forward-window-begins-first-real-instant` | covered |
| DAT-112 | `contracts/data-model-1.md#display-power-and-the-playback-lease` | `DAT-074-valid-daypart-display-power-off` | covered |
| DAT-113 | `contracts/data-model-1.md#display-power-and-the-playback-lease` | `DAT-074-valid-daypart-display-power-off` | covered |
| DAT-114 | `contracts/data-model-1.md#display-power-and-the-playback-lease` | - | TBD-wave1 |
| DAT-115 | `contracts/data-model-1.md#display-power-and-the-playback-lease` | `DAT-074-valid-daypart-display-power-off` | covered |
| DAT-116 | `contracts/data-model-1.md#display-power-and-the-playback-lease` | `DAT-074-valid-daypart-display-power-off` | covered |
| DAT-117 | `contracts/data-model-1.md#dayparting-evaluation` | `DAT-117-valid-layered-fallback-through-precedence` | covered |
| DAT-118 | `contracts/data-model-1.md#dayparting-evaluation` | `DAT-118-valid-terminal-default-blank` | covered |
| DAT-119 | `contracts/data-model-1.md#dayparting-evaluation` | `DAT-119-valid-dst-fall-back-window-holds-through-repeat`, `DAT-119-valid-dst-spring-forward-window-begins-first-real-instant`, `DAT-075-valid-fall-back-boundary-refires-preset` | covered |
| DAT-120 | `contracts/data-model-1.md#schedule-side-misfire-default` | `DAT-121-valid-misfire-catchup-vs-skip-by-kind` | covered |
| DAT-121 | `contracts/data-model-1.md#schedule-side-misfire-default` | `DAT-121-valid-misfire-catchup-vs-skip-by-kind` | covered |
| DAT-122 | `contracts/data-model-1.md#schedule-side-misfire-default` | `DAT-121-valid-misfire-catchup-vs-skip-by-kind` | covered |
| DAT-123 | `contracts/data-model-1.md#schedule-side-misfire-default` | `DAT-121-valid-misfire-catchup-vs-skip-by-kind` | covered |
| DAT-130 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-131 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-131a | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-132 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-133 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-134 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-135 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-136 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-137 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
| DAT-138 | `contracts/data-model-1.md#variables` | - | TBD-wave1 |
