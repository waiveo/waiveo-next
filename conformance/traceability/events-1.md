# Traceability: events/1

One row per requirement ID `contracts/events-1.md` defines. Format: `conformance/traceability/README.md`.

**2026-07-26 re-drive note:** `conformance/drivers/events1` now mounts the
LIVE `internal/app/eventingest` (POST `/telemetry/v1/push`) and
`internal/app/eventsse` (GET `/events/v1`) handlers wherever a real producer
or transport path exists, rather than calling `internal/events` directly —
the same 2026-07-26 audit that flagged `api/1`'s driver found this driver had
never touched a single `internal/app/` package either. Unlike `api/1`,
re-driving against the live handlers surfaced no genuine corpus-vs-code
divergence: every driven case still passes. Two schemas (`audit.event`,
and the internal-origin `automation.run` case) remain driven directly
against `internal/events`, because no producer or HTTP endpoint anywhere in
the tree constructs either — `eventingest` is the only envelope-constructing
endpoint, and it is relay-telemetry-scoped (always `origin: relay`); see
`conformance/drivers/events1/driver.go`'s package doc for the full list of
what is driven through which path and why. EVT-100/102/103/104 moved to
`covered`: they were previously untested at the transport level (the old
driver never opened a real SSE connection); `EVT-091-valid-hello-fresh-
subscribe` and `EVT-140-valid-resume-with-gap` now drive a real
`httptest.Server`-backed SSE connection end to end.

**Scope-node filtering note:** `EVT-120-valid-scope-filtered-subscription` and
`EVT-101-valid-sse-selector-and-schemas` drive the live `GET /events/v1` handler
with a REAL scope-node tree and a REAL principal bound at one site (a seeded
`authtest` fixture), so EVT-101/120–124 are answered by the shipping
visible-set computation rather than by a driver-side model of it. Both close the
Hub before driving, which makes the delivered set observable synchronously: the
handler writes and flushes its whole resolved backlog before reaching the live
select, and that select returns at once on the closed done channel — no timer
decides when a stream has finished. EVT-123's live-tail half is additionally
driven by `internal/app/eventsse`'s own tests, which watch two differently-bound
principals across one interleaved append sequence.

EVT-012 stays `TBD-wave1` on purpose. These cases FILTER on the envelope's
`scope_node`; none of them proves a PRODUCER sets it to the subject resource's
own placement, and marking the row covered off a case that supplies `scope_node`
as input would claim the requirement is met when it is not.

There is now one producer that genuinely derives it: the api/1 surface's audit
middleware (`internal/app/api/audit.go`) reads each mutating request's subject
row and files that request's `audit.event` at the subject's own placement, which
`internal/app/api`'s own tests drive end to end — two differently-bound
principals watching one live `/events/v1` stream, each receiving only the record
whose subject sits in their subtree. That is a Go-package case, not a
conformance corpus case, so it does not populate the case-id column; the row
moves when a corpus case drives a producer. The relay telemetry ingest
(`internal/app/eventingest`) still stamps the site node on every record it
reconstructs, which is correct for its own traffic rather than an outstanding
EVT-012 defect: `relay/1` does not carry `audit.event` over the telemetry
channel at all (`internal/relay/telemetry/schema.go`), so no audit record is
ever placed by that path.

| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| EVT-001 | `contracts/events-1.md#versioning--transport-surface` | - | TBD-wave1 |
| EVT-002 | `contracts/events-1.md#versioning--transport-surface` | - | TBD-wave1 |
| EVT-003 | `contracts/events-1.md#versioning--transport-surface` | - | TBD-wave1 |
| EVT-010 | `contracts/events-1.md#durable-event-envelope` | `EVT-010-valid-entity-state-changed` | covered |
| EVT-011 | `contracts/events-1.md#durable-event-envelope` | - | TBD-wave1 |
| EVT-012 | `contracts/events-1.md#durable-event-envelope` | - | TBD-wave1 |
| EVT-013 | `contracts/events-1.md#durable-event-envelope` | `EVT-010-valid-entity-state-changed`, `EVT-013-invalid-unregistered-schema-payload`, `EVT-013-invalid-registered-schema-malformed-payload` | covered |
| EVT-020 | `contracts/events-1.md#registered-schema-catalog--general` | - | TBD-wave1 |
| EVT-021 | `contracts/events-1.md#registered-schema-catalog--general` | `EVT-013-invalid-unregistered-schema-payload` | covered |
| EVT-022 | `contracts/events-1.md#registered-schema-catalog--general` | `EVT-013-invalid-unregistered-schema-payload` | covered |
| EVT-030 | `contracts/events-1.md#entitystate_changed` | `EVT-010-valid-entity-state-changed` | covered |
| EVT-031 | `contracts/events-1.md#entitystate_changed` | `EVT-010-valid-entity-state-changed` | covered |
| EVT-032 | `contracts/events-1.md#entitystate_changed` | - | TBD-wave1 |
| EVT-040 | `contracts/events-1.md#automationrun` | `EVT-040-valid-automation-run` | covered |
| EVT-041 | `contracts/events-1.md#automationrun` | `EVT-040-valid-automation-run`, `EVT-041-valid-automation-run-skipped-internal`, `EVT-041-valid-automation-run-restarted`, `EVT-041-valid-automation-run-misfire-caught` | covered |
| EVT-042 | `contracts/events-1.md#automationrun` | `EVT-040-valid-automation-run`, `EVT-041-valid-automation-run-skipped-internal` | covered |
| EVT-043 | `contracts/events-1.md#automationrun` | `EVT-040-valid-automation-run`, `EVT-041-valid-automation-run-skipped-internal`, `EVT-041-valid-automation-run-restarted`, `EVT-041-valid-automation-run-misfire-caught` | covered |
| EVT-050 | `contracts/events-1.md#contentplayed` | `EVT-050-valid-content-played` | covered |
| EVT-051 | `contracts/events-1.md#contentplayed` | `EVT-050-valid-content-played` | covered |
| EVT-052 | `contracts/events-1.md#contentplayed` | `EVT-050-valid-content-played` | covered |
| EVT-060 | `contracts/events-1.md#deviceheartbeat` | `EVT-060-valid-device-heartbeat` | covered |
| EVT-061 | `contracts/events-1.md#deviceheartbeat` | `EVT-060-valid-device-heartbeat` | covered |
| EVT-070 | `contracts/events-1.md#boxvitals` | `EVT-070-valid-box-vitals` | covered |
| EVT-071 | `contracts/events-1.md#boxvitals` | `EVT-070-valid-box-vitals` | covered |
| EVT-080 | `contracts/events-1.md#auditevent` | `EVT-080-valid-audit-login-failure` | covered |
| EVT-081 | `contracts/events-1.md#auditevent` | `EVT-080-valid-audit-login-failure` | covered |
| EVT-082 | `contracts/events-1.md#auditevent` | - | TBD-wave1 |
| EVT-083 | `contracts/events-1.md#auditevent` | `EVT-080-valid-audit-login-failure` | covered |
| EVT-084 | `contracts/events-1.md#variablechanged` | - | TBD-wave1 |
| EVT-085 | `contracts/events-1.md#variablechanged` | - | TBD-wave1 |
| EVT-090 | `contracts/events-1.md#ws-binding` | `EVT-091-valid-hello-fresh-subscribe` | covered |
| EVT-091 | `contracts/events-1.md#ws-binding` | `EVT-091-valid-hello-fresh-subscribe` | covered |
| EVT-092 | `contracts/events-1.md#ws-binding` | `EVT-091-valid-hello-fresh-subscribe`, `EVT-140-valid-resume-with-gap` | covered |
| EVT-093 | `contracts/events-1.md#ws-binding` | - | TBD-wave1 |
| EVT-094 | `contracts/events-1.md#ws-binding` | `EVT-140-valid-resume-with-gap` | covered |
| EVT-095 | `contracts/events-1.md#ws-binding` | - | TBD-wave1 |
| EVT-096 | `contracts/events-1.md#ws-binding` | - | TBD-wave1 |
| EVT-100 | `contracts/events-1.md#sse-binding` | `EVT-091-valid-hello-fresh-subscribe`, `EVT-140-valid-resume-with-gap` | covered |
| EVT-101 | `contracts/events-1.md#sse-binding` | `EVT-101-valid-sse-selector-and-schemas` | covered |
| EVT-102 | `contracts/events-1.md#sse-binding` | `EVT-134-invalid-resume-from-malformed`, `EVT-140-valid-resume-with-gap` | covered |
| EVT-103 | `contracts/events-1.md#sse-binding` | `EVT-140-valid-resume-with-gap` | covered |
| EVT-104 | `contracts/events-1.md#sse-binding` | `EVT-140-valid-resume-with-gap`, `EVT-142-valid-mid-stream-buffer-exceeded-gap` | covered |
| EVT-105 | `contracts/events-1.md#sse-binding` | - | TBD-wave1 |
| EVT-105a | `contracts/events-1.md#sse-binding` | - | TBD-wave1 |
| EVT-110 | `contracts/events-1.md#authentication` | - | TBD-wave1 |
| EVT-111 | `contracts/events-1.md#authentication` | - | TBD-wave1 |
| EVT-112 | `contracts/events-1.md#authentication` | - | TBD-wave1 |
| EVT-113 | `contracts/events-1.md#authentication` | - | TBD-wave1 |
| EVT-114 | `contracts/events-1.md#authentication` | - | TBD-wave1 |
| EVT-120 | `contracts/events-1.md#scope-node-filtering` | `EVT-120-valid-scope-filtered-subscription`, `EVT-101-valid-sse-selector-and-schemas`, `EVT-142-valid-mid-stream-buffer-exceeded-gap` | covered |
| EVT-121 | `contracts/events-1.md#scope-node-filtering` | `EVT-091-valid-hello-fresh-subscribe`, `EVT-101-valid-sse-selector-and-schemas` | covered |
| EVT-122 | `contracts/events-1.md#scope-node-filtering` | `EVT-101-valid-sse-selector-and-schemas` | covered |
| EVT-123 | `contracts/events-1.md#scope-node-filtering` | `EVT-120-valid-scope-filtered-subscription` | covered |
| EVT-124 | `contracts/events-1.md#scope-node-filtering` | `EVT-101-valid-sse-selector-and-schemas` | covered |
| EVT-130 | `contracts/events-1.md#resume-cursor` | `EVT-134-invalid-resume-from-out-of-scope` | covered |
| EVT-131 | `contracts/events-1.md#resume-cursor` | `EVT-134-invalid-resume-from-malformed` | covered |
| EVT-132 | `contracts/events-1.md#resume-cursor` | `EVT-091-valid-hello-fresh-subscribe` | covered |
| EVT-133 | `contracts/events-1.md#resume-cursor` | - | TBD-wave1 |
| EVT-134 | `contracts/events-1.md#resume-cursor` | `EVT-134-invalid-resume-from-malformed`, `EVT-134-invalid-resume-from-out-of-scope` | covered |
| EVT-134a | `contracts/events-1.md#resume-cursor` | `EVT-134-invalid-resume-from-out-of-scope`, `EVT-142-valid-mid-stream-buffer-exceeded-gap` | covered |
| EVT-135 | `contracts/events-1.md#resume-cursor` | - | TBD-wave1 |
| EVT-140 | `contracts/events-1.md#loss-markers` | `EVT-140-valid-resume-with-gap`, `EVT-142-valid-mid-stream-buffer-exceeded-gap` | covered |
| EVT-141 | `contracts/events-1.md#loss-markers` | `EVT-140-valid-resume-with-gap` | covered |
| EVT-142 | `contracts/events-1.md#loss-markers` | `EVT-142-valid-mid-stream-buffer-exceeded-gap` | covered |
| EVT-142a | `contracts/events-1.md#loss-markers` | `EVT-142a-invalid-deferral-exceeds-its-bound` | TBD-wave1 |
| EVT-143 | `contracts/events-1.md#loss-markers` | `EVT-140-valid-resume-with-gap`, `EVT-142-valid-mid-stream-buffer-exceeded-gap` | covered |
| EVT-144 | `contracts/events-1.md#loss-markers` | - | TBD-wave1 |
| EVT-150 | `contracts/events-1.md#webhook-delivery` | - | TBD-wave1 |
| EVT-151 | `contracts/events-1.md#webhook-delivery` | `EVT-151-valid-webhook-delivery-signed` | covered |
| EVT-152 | `contracts/events-1.md#webhook-delivery` | - | TBD-wave1 |
| EVT-153 | `contracts/events-1.md#webhook-delivery` | - | TBD-wave1 |
| EVT-154 | `contracts/events-1.md#webhook-delivery` | - | TBD-wave1 |
| EVT-155 | `contracts/events-1.md#webhook-delivery` | - | TBD-wave1 |
| EVT-156 | `contracts/events-1.md#webhook-delivery` | `EVT-151-valid-webhook-delivery-signed` | covered |
| EVT-157 | `contracts/events-1.md#webhook-delivery` | `EVT-151-valid-webhook-delivery-signed` | covered |
| EVT-158 | `contracts/events-1.md#webhook-delivery` | - | TBD-wave1 |

`EVT-142a` stays `TBD-wave1` even though it now cites a case, because the
requirement has three separable clauses and the corpus measures one of them.

- **Bounded deferral** — measured by `EVT-142a-invalid-deferral-exceeds-its-bound`.
  An implementation whose deferral is effectively unbounded previously left this
  whole driver green while the package's own Go tests failed; it now fails here
  too, which is the gap that case was written to close.
- **Defer rather than name an outside id or drop the marker** — exercised by
  `EVT-142-valid-mid-stream-buffer-exceeded-gap`, whose `to_id` is resolved over
  the whole log rather than the visible set and whose `from_id` comes from the
  last delivered id rather than the watermark.
- **A deferral MUST NOT hold the subscriber's delivery position** — measured by
  NOTHING in the corpus. An implementation that freezes the position while a
  marker is pending leaves every case here green. It is pinned by
  `TestSSE_DeferredGapDoesNotRescanTheRetainedTail` in `internal/app/eventsse`,
  which is real evidence but is not the evidence this column reports, and the
  clause matters: freezing the position is the tail-rescan amplifier that got an
  earlier attempt reverted.

Marking the row `covered` on the strength of one clause would be the kind of
claim this table exists to prevent, so it cites what it has and says what it
lacks.
