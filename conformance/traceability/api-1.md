# Traceability: api/1

One row per requirement ID `contracts/api-1.md` defines. Format: `conformance/traceability/README.md`.

**2026-07-26 re-drive note:** `conformance/drivers/api1` now mounts the LIVE,
HTTP-mounted `internal/app/api` handler (`api.New`) instead of calling the
convention libraries (`apihttp`/`apiselector`/`apijob`) directly — a
2026-07-26 audit found the prior driver certified those libraries, not the
shipped `/api/v1` surface. `covered` below still means exactly what
`README.md` defines ("a listed case exercises this requirement today") — it
is not a pass/fail signal, and this table has no third value for that. That
re-drive originally surfaced seven genuine, confirmed divergences between
frozen expectations and the live handler's actual behavior (wording/
status-code mismatches, two fixtures that violated a datamodel rule built
after they were frozen, and two Job-resource fields with no determinism
seam). Every one of those is closed and no driven case diverges today — see
`conformance/drivers/api1/driver_test.go`'s `expectedFailing` map, which is
empty and stays in place to make a NEW divergence loud.

**Data-subject export/delete update:** `POST /api/v1/workspace/export` and
`POST /api/v1/workspace/delete` are now mounted
(`internal/app/api/workspace.go`), so `API-121-valid-export-workspace-job` —
previously the one corpus case with no route to drive at all — is DRIVEN, and
the driver's pending set is empty. That case's `input` block gained the three
things the live handler needs and the frozen block never carried: the
authenticated `principal` whose id the Job's `created_by` is checked against,
that principal's `principal_role` (the operation is owner-only, so the role is
part of what makes the request valid), and the `workspace_state` org node whose
id is the Job's single target (API-123). The `expected` block is untouched. This
is the same class of input amendment two earlier commits already made to this
corpus so its cases could be driven against the real handler rather than
against the convention libraries.

`API-122` and `API-124` remain `TBD-wave1` deliberately. API-122's delete
operation IS implemented — it runs `security-model.md` SEC-121's destruction
path — and IS exercised end to end through the live mux: its owner-only
authorization, its `confirm_workspace_id` safety gate, and the destruction
itself. That exercise lives in `internal/app/api`'s own Go tests, and no case in
the FROZEN corpus drives it. `covered` here means "a listed case exercises this
requirement today", so claiming it without a corpus case would be exactly the
overclaim this column exists to prevent. API-124 is the section's own explicit
deferral of a fuller data-subject-request workflow and has nothing to cover.

| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| API-001 | `contracts/api-1.md#versioning--surface` | - | TBD-wave1 |
| API-002 | `contracts/api-1.md#versioning--surface` | - | TBD-wave1 |
| API-003 | `contracts/api-1.md#versioning--surface` | - | TBD-wave1 |
| API-010 | `contracts/api-1.md#error-shape` | `API-010-valid-simple-problem`, `API-013-valid-multi-field-validation-problem` | covered |
| API-011 | `contracts/api-1.md#error-shape` | `API-010-valid-simple-problem`, `API-013-valid-multi-field-validation-problem` | covered |
| API-012 | `contracts/api-1.md#error-shape` | - | TBD-wave1 |
| API-013 | `contracts/api-1.md#error-shape` | `API-013-valid-multi-field-validation-problem` | covered |
| API-013a | `contracts/api-1.md#error-shape` | `API-013-valid-multi-field-validation-problem` | covered |
| API-014 | `contracts/api-1.md#error-shape` | - | TBD-wave1 |
| API-015 | `contracts/api-1.md#error-shape` | `API-010-valid-simple-problem` | covered |
| API-016 | `contracts/api-1.md#error-shape` | `API-010-valid-simple-problem`, `API-013-valid-multi-field-validation-problem` | covered |
| API-020 | `contracts/api-1.md#optimistic-concurrency` | `API-023-invalid-if-match-conflict` | covered |
| API-021 | `contracts/api-1.md#optimistic-concurrency` | `API-022-invalid-if-match-missing`, `API-023-invalid-if-match-conflict` | covered |
| API-022 | `contracts/api-1.md#optimistic-concurrency` | `API-022-invalid-if-match-missing` | covered |
| API-023 | `contracts/api-1.md#optimistic-concurrency` | `API-023-invalid-if-match-conflict` | covered |
| API-024 | `contracts/api-1.md#optimistic-concurrency` | - | TBD-wave1 |
| API-025 | `contracts/api-1.md#optimistic-concurrency` | - | TBD-wave1 |
| API-030 | `contracts/api-1.md#keyset-pagination` | `API-032-valid-pagination-roundtrip` | covered |
| API-031 | `contracts/api-1.md#keyset-pagination` | - | TBD-wave1 |
| API-032 | `contracts/api-1.md#keyset-pagination` | `API-032-valid-pagination-roundtrip` | covered |
| API-033 | `contracts/api-1.md#keyset-pagination` | `API-032-valid-pagination-roundtrip` | covered |
| API-034 | `contracts/api-1.md#keyset-pagination` | `API-032-valid-pagination-roundtrip` | covered |
| API-035 | `contracts/api-1.md#keyset-pagination` | `API-035-invalid-cursor-foreign-resource` | covered |
| API-036 | `contracts/api-1.md#keyset-pagination` | `API-032-valid-pagination-roundtrip` | covered |
| API-040 | `contracts/api-1.md#label-selector-grammar` | `API-044-valid-selector-scope-subtree` | covered |
| API-041 | `contracts/api-1.md#label-selector-grammar` | `API-044-valid-selector-scope-subtree` | covered |
| API-042 | `contracts/api-1.md#label-selector-grammar` | `API-044-valid-selector-scope-subtree` | covered |
| API-043 | `contracts/api-1.md#label-selector-grammar` | `API-045-invalid-selector-malformed` | covered |
| API-044 | `contracts/api-1.md#label-selector-grammar` | `API-044-valid-selector-scope-subtree` | covered |
| API-045 | `contracts/api-1.md#label-selector-grammar` | `API-045-invalid-selector-malformed` | covered |
| API-046 | `contracts/api-1.md#label-selector-grammar` | - | TBD-wave1 |
| API-050 | `contracts/api-1.md#idempotency-key` | `API-052-valid-idempotency-replay` | covered |
| API-051 | `contracts/api-1.md#idempotency-key` | `API-052-valid-idempotency-replay`, `API-053-invalid-idempotency-key-reused-different-body` | covered |
| API-052 | `contracts/api-1.md#idempotency-key` | `API-052-valid-idempotency-replay` | covered |
| API-053 | `contracts/api-1.md#idempotency-key` | `API-053-invalid-idempotency-key-reused-different-body` | covered |
| API-054 | `contracts/api-1.md#idempotency-key` | - | TBD-wave1 |
| API-055 | `contracts/api-1.md#idempotency-key` | - | TBD-wave1 |
| API-056 | `contracts/api-1.md#idempotency-key` | - | TBD-wave1 |
| API-060 | `contracts/api-1.md#trace-id-propagation` | - | TBD-wave1 |
| API-061 | `contracts/api-1.md#trace-id-propagation` | - | TBD-wave1 |
| API-062 | `contracts/api-1.md#trace-id-propagation` | `API-010-valid-simple-problem` | covered |
| API-063 | `contracts/api-1.md#trace-id-propagation` | `API-063-valid-trace-id-propagated-into-durable-event` | covered |
| API-070 | `contracts/api-1.md#mcp-operation-tag-curation` | - | TBD-wave1 |
| API-071 | `contracts/api-1.md#mcp-operation-tag-curation` | - | TBD-wave1 |
| API-072 | `contracts/api-1.md#mcp-operation-tag-curation` | - | TBD-wave1 |
| API-080 | `contracts/api-1.md#evolution--deprecation-policy` | - | TBD-wave1 |
| API-081 | `contracts/api-1.md#evolution--deprecation-policy` | - | TBD-wave1 |
| API-082 | `contracts/api-1.md#evolution--deprecation-policy` | - | TBD-wave1 |
| API-083 | `contracts/api-1.md#evolution--deprecation-policy` | - | TBD-wave1 |
| API-084 | `contracts/api-1.md#evolution--deprecation-policy` | - | TBD-wave1 |
| API-090 | `contracts/api-1.md#security-override-convention` | - | TBD-wave1 |
| API-091 | `contracts/api-1.md#security-override-convention` | - | TBD-wave1 |
| API-092 | `contracts/api-1.md#security-override-convention` | - | TBD-wave1 |
| API-100 | `contracts/api-1.md#client-assignable-external_id` | `API-102-invalid-external-id-conflict` | covered |
| API-101 | `contracts/api-1.md#client-assignable-external_id` | `API-102-invalid-external-id-conflict`, `API-101-invalid-external-id-cross-kind-conflict` | covered |
| API-102 | `contracts/api-1.md#client-assignable-external_id` | `API-102-invalid-external-id-conflict` | covered |
| API-103 | `contracts/api-1.md#client-assignable-external_id` | - | TBD-wave1 |
| API-104 | `contracts/api-1.md#client-assignable-external_id` | - | TBD-wave1 |
| API-105 | `contracts/api-1.md#client-assignable-external_id` | - | TBD-wave1 |
| API-110 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | `API-111-valid-bulk-enable-202-job` | covered |
| API-111 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | `API-111-valid-bulk-enable-202-job` | covered |
| API-112 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | `API-111-valid-bulk-enable-202-job` | covered |
| API-113 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | `API-111-valid-bulk-enable-202-job` | covered |
| API-114 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | - | TBD-wave1 |
| API-115 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | - | TBD-wave1 |
| API-116 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | - | TBD-wave1 |
| API-117 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | - | TBD-wave1 |
| API-120 | `contracts/api-1.md#data-subject-export--delete` | `API-121-valid-export-workspace-job` | covered |
| API-121 | `contracts/api-1.md#data-subject-export--delete` | `API-121-valid-export-workspace-job` | covered |
| API-122 | `contracts/api-1.md#data-subject-export--delete` | - | TBD-wave1 |
| API-123 | `contracts/api-1.md#data-subject-export--delete` | `API-121-valid-export-workspace-job` | covered |
| API-124 | `contracts/api-1.md#data-subject-export--delete` | - | TBD-wave1 |
