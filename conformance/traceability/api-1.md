# Traceability: api/1

One row per requirement ID `contracts/api-1.md` defines. Format: `conformance/traceability/README.md`.

**2026-07-26 re-drive note:** `conformance/drivers/api1` now mounts the LIVE,
HTTP-mounted `internal/app/api` handler (`api.New`) instead of calling the
convention libraries (`apihttp`/`apiselector`/`apijob`) directly — a
2026-07-26 audit found the prior driver certified those libraries, not the
shipped `/api/v1` surface. `covered` below still means exactly what
`README.md` defines ("a listed case exercises this requirement today") — it
is not a pass/fail signal, and this table has no third value for that. Seven
of the eleven now-driven cases surfaced a genuine, confirmed divergence
between their frozen expectation and the live handler's actual behavior
(wording/status-code mismatches, two fixtures that violate a datamodel rule
built after they were frozen, and two Job-resource fields with no
determinism seam) — see `conformance/drivers/api1/driver_test.go`'s
`expectedFailing` map for the full list and reasons, and the Track A
follow-up issue this reconciliation files for resolving them. `API-120/121/
123` moved back to `TBD-wave1`: `API-121-valid-export-workspace-job` is the
one corpus case with no mounted route to drive at all (`/api/v1/workspace/
export` does not exist in `api.New`'s mux).

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
| API-035 | `contracts/api-1.md#keyset-pagination` | - | TBD-wave1 |
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
| API-063 | `contracts/api-1.md#trace-id-propagation` | - | TBD-wave1 |
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
| API-110 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | `API-111-valid-bulk-enable-202-job` | covered |
| API-111 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | `API-111-valid-bulk-enable-202-job` | covered |
| API-112 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | `API-111-valid-bulk-enable-202-job` | covered |
| API-113 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | `API-111-valid-bulk-enable-202-job` | covered |
| API-114 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | - | TBD-wave1 |
| API-115 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | - | TBD-wave1 |
| API-116 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | - | TBD-wave1 |
| API-117 | `contracts/api-1.md#fleet-mutating-operations--the-job-resource` | - | TBD-wave1 |
| API-120 | `contracts/api-1.md#data-subject-export--delete` | - | TBD-wave1 |
| API-121 | `contracts/api-1.md#data-subject-export--delete` | - | TBD-wave1 |
| API-122 | `contracts/api-1.md#data-subject-export--delete` | - | TBD-wave1 |
| API-123 | `contracts/api-1.md#data-subject-export--delete` | - | TBD-wave1 |
| API-124 | `contracts/api-1.md#data-subject-export--delete` | - | TBD-wave1 |
