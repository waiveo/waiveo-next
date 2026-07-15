# Management API Conventions

**Contract:** api/1
**Version:** 1.0
**Status:** draft

## Scope

api/1 defines the cross-cutting conventions every operation in `api/openapi.yaml` MUST follow: the error shape, optimistic concurrency, keyset pagination, idempotency, trace propagation, the `mcp:` operation-tag curation rule, the label-selector grammar, and the evolution/deprecation policy. `api/openapi.yaml` implements these conventions as reusable OpenAPI components and applies them to every operation; this document is their normative source — where the two disagree, this document governs.

- In scope: the Problem error shape and its error-code registry; revision/ETag/If-Match optimistic concurrency; keyset pagination (the opaque cursor grammar, referenced by name from other contracts); the label-selector grammar (equality, set-membership, existence, and scope-node subtree terms over labels); `Idempotency-Key` semantics on mutating POSTs; `Trace-Id` propagation; the `mcp:read`/`mcp:act` operation-tag curation rule; api/1's additive-evolution and deprecation policy.
- Out of scope: the per-resource business schema and endpoint list (`api/openapi.yaml`); the principal/role/session/audit model (a separate concern); the automation vocabulary a rule's `triggers`/`conditions`/`actions` fields hold (`rules/1`); the pack-runtime protocol an `execution: app-service` action dispatches through (`ctx/1`); the wire framing of `events/1`, api/1's sibling watch door.

## Definitions

- **ULID** — a 26-character Crockford-base32 identifier, lexicographically sortable by creation time, used for every resource ID this contract references.
- **Resource** — an object exposed by an api/1 operation with its own identity (`id`) and, if mutable, its own `revision`.
- **Revision** — a monotonically increasing per-resource version counter used for optimistic concurrency and change detection.
- **Principal** — the authenticated caller of a request: a human session or a service/API-key credential. This contract treats a principal only as an opaque identifier for scoping (Idempotency-Key) and audit; the principal/role model itself is defined elsewhere.
- **Problem** — the RFC 9457 `application/problem+json` document api/1 returns for every error response (Error shape).
- **Cursor** — the opaque continuation token a list operation returns for keyset pagination (Keyset pagination).
- **Selector** — a string in this contract's label-selector grammar (Label-selector grammar), accepted by every list operation's `selector` query parameter and by other contracts' selector-typed fields.
- **Scope node** — a node in the platform's org → site → group → screen tree; every resource this API exposes carries the ID of the scope node it is placed under.
- **List operation** — any GET operation whose success response is a page of zero or more resources (an `items` array plus a `cursor`).

## Normative requirements

### Versioning & surface

**[API-001]** Every api/1 operation MUST be reachable under the URL path prefix `/api/v1`; no unversioned or alternately-versioned path may alias it.

**[API-002]** A request or success-response body, when present, MUST be `application/json` UTF-8. An error-response body MUST be `application/problem+json` (Error shape).

**[API-003]** Every list operation MUST declare the pagination query parameters (Keyset pagination) and the `selector` query parameter (Label-selector grammar). A list operation's OpenAPI definition omitting either is nonconformant with this contract, not a case it exempts.

### Error shape

**[API-010]** Every error response MUST be a single JSON object conformant to RFC 9457, with at minimum the members `type`, `title`, `status`, and the extension members `code` and `trace_id` defined below.

**[API-011]** `code` MUST be one of the values in this contract's error-code registry (Error taxonomy table) or a value added by a later api/1 minor. A server MUST NOT emit a `code` value outside the registry.

**[API-012]** The error-code registry is additive-only: a published `code` value's meaning MUST NOT change, and MUST NOT be removed or repurposed, within major version 1.

**[API-013]** A response carrying more than one independent field-level validation failure MUST use `code: VALIDATION_FAILED` and MUST include the extension member `errors`: an array of `{field, code, message}` objects, one per failing field, in addition to the top-level `code`/`detail`.

**[API-014]** A background or per-target operation that surfaces an error outside the direct request/response cycle (a fleet job's per-target failure, a webhook delivery failure) MUST type that error using a `code` from this same registry, never a parallel vocabulary.

**[API-015]** `instance`, when present, MUST be the request's own path, unmodified — an operator or client correlating a stored Problem document back to the endpoint that produced it MUST be able to do so from this field alone.

**[API-016]** `type` MUST be the literal string `about:blank`; `code` (API-011) is the sole machine-readable discriminant this version of api/1 defines. A later minor MAY mint dereferenceable `type` URIs without breaking this contract, since `about:blank` remains a legal value throughout.

### Optimistic concurrency

**[API-020]** Every mutable resource MUST carry a `revision` field (Revision) in its representation, and every response representing a single instance of it MUST carry a strong `ETag` response header derived solely from `revision`, so a client can treat `ETag` and `revision` as interchangeable.

**[API-021]** Every state-changing request against an existing mutable resource (an update or a delete, never a create) MUST carry an `If-Match` request header naming the `ETag` value the client last observed for that resource.

**[API-022]** A state-changing request against an existing mutable resource that omits `If-Match` MUST be rejected with `428 Precondition Required` / `code: IF_MATCH_REQUIRED`, without executing the write. No operation offers an unconditional-overwrite path.

**[API-023]** A state-changing request whose `If-Match` value does not equal the resource's current `ETag` MUST be rejected with `412 Precondition Failed` / `code: REVISION_CONFLICT`, without executing the write; the Problem document's `current_revision` extension member MUST carry the resource's current `revision` so the client can re-read and retry.

**[API-024]** A create operation (a POST minting a new resource) MUST NOT require `If-Match` and MUST NOT accept one — a resource that does not yet exist has no prior revision to condition on.

**[API-025]** A multi-resource bulk or import operation that internally performs a read-modify-write over several resources MUST apply API-021–023 per resource, and MUST report each resource's own outcome — including any `REVISION_CONFLICT` — individually in its response, rather than succeeding or failing the batch as one undifferentiated unit.

### Keyset pagination

**[API-030]** Every list operation MUST accept two query parameters: `cursor` (string, optional) and `limit` (integer, optional).

**[API-031]** `limit`'s server-applied default MUST be 50 and its server-enforced maximum MUST be 200. A client-supplied `limit` outside `[1, 200]` MUST be rejected with `code: VALIDATION_FAILED`, never silently clamped.

**[API-032]** A list operation's success response body MUST be a JSON object with an `items` array (the page's resources, in the operation's default order) and a `cursor` field: an opaque continuation token for the next page, or `null` when no further rows remain.

**[API-033]** `cursor`, when supplied on a request, MUST be treated as an opaque value previously issued by that same operation; a client MUST NOT construct, parse, or mutate one. A cursor string carries no meaning across different list operations or resource types, even where two cursor values happen to be byte-identical.

**[API-034]** A list operation's default order MUST be a stable keyset — sorted by the resource's ULID (creation order) unless the operation declares an explicit alternate sort key — so that paging via `cursor` neither skips nor repeats a row across pages absent a concurrent delete of an already-returned row.

**[API-035]** A `cursor` value that is malformed, expired, or was issued by a different operation or resource type MUST be rejected with `400 Bad Request` / `code: CURSOR_INVALID` — never silently treated as "start from the beginning."

**[API-036]** A `cursor` string MUST match `^[A-Za-z0-9_-]+$` so it round-trips through a query parameter without additional escaping. This is the one concrete cursor-token grammar api/1 defines; any other contract or field that names "the platform's keyset-pagination convention" refers to this grammar and MUST NOT restate or vary it.

### Label-selector grammar

**[API-040]** A `selector` query parameter, when present on a list operation, MUST restrict results to resources whose labels (and, for API-044, scope-node placement) satisfy every comma-separated term in the selector string. Terms are ANDed; this grammar has no OR operator and no grouping.

**[API-041]** Each term MUST be one of: equality (`key=value` or `key==value`), inequality (`key!=value`), set-membership (`key in (value[,value...])`), set-exclusion (`key notin (value[,value...])`), existence (`key`), non-existence (`!key`), or the scope-node subtree term (API-044).

**[API-042]** A label `key` MUST match `^([a-z0-9A-Z][a-z0-9A-Z.-]{0,251}/)?[a-z0-9A-Z][a-z0-9A-Z_.-]{0,62}$` — an optional DNS-subdomain-style prefix (at most 253 characters) followed by `/`, then a name segment (at most 63 characters). A `value` MUST match `^[a-z0-9A-Z][a-z0-9A-Z_.-]{0,62}$` or be empty. Neither charset admits `,`, `(`, `)`, `=`, `!`, or whitespace, so no term or value ever needs escaping.

**[API-043]** Whitespace immediately inside a set-membership or set-exclusion term's parentheses MUST be tolerated (trimmed) by the server. Whitespace anywhere else in a term MUST be rejected with `code: SELECTOR_INVALID`.

**[API-044]** The term `scope_node subtree <ulid>` MUST restrict results to resources placed at the named scope node or at any descendant of it. The ordinary equality form applied to the reserved key `scope_node` (`scope_node=<ulid>` / `scope_node==<ulid>`) MUST restrict to that exact node only, with no subtree expansion.

**[API-045]** A selector string that fails to parse under API-041–044, or names a `scope_node` value that is not a syntactically valid ULID, MUST be rejected with `400 Bad Request` / `code: SELECTOR_INVALID`, identifying the offending term in `detail`.

**[API-046]** This grammar is the platform's sole normative definition of a label selector. Any field elsewhere that accepts a "label selector" string — including a `recipients_selector` parameter and the selector form of an `EntityRef` — MUST accept exactly this grammar, unmodified; a reference to "the platform's label-selector grammar" from another contract resolves to this section.

### Idempotency-Key

**[API-050]** A mutating POST operation (creating a resource, invoking a pack action, submitting a fleet job, or any other non-idempotent-by-default write) MUST accept an optional `Idempotency-Key` request header: a client-generated opaque string, 1–255 characters.

**[API-051]** An `Idempotency-Key` is scoped to the tuple (authenticated principal, HTTP method, request path). The identical key value presented by a different principal, or against a different method or path, MUST be treated as an unrelated, fresh request — never as a replay.

**[API-052]** The server MUST retain, for at least 24 hours from first use, the mapping from an `Idempotency-Key` scope (API-051) to a content hash of the original request body and the original complete response (status and body). A repeat request presenting the same key and a body whose hash matches MUST return the retained response verbatim, without re-executing the operation's side effects.

**[API-053]** A repeat request presenting the same `Idempotency-Key` scope with a body whose hash does not match the original MUST be rejected with `409 Conflict` / `code: IDEMPOTENCY_KEY_REUSED`, and MUST NOT execute.

**[API-054]** A request presenting an `Idempotency-Key` whose original request is still executing (no stored response yet) MUST be rejected with `409 Conflict` / `code: IDEMPOTENCY_KEY_IN_PROGRESS`, rather than executing concurrently or blocking until the original completes.

**[API-055]** Once an `Idempotency-Key` scope's retention window (API-052) elapses, the same key value MAY be reused and MUST execute as a fresh request — the server is not required to remember it indefinitely.

**[API-056]** An action's own idempotency classification (whether the underlying operation is safe to execute more than once without a key) is independent of `Idempotency-Key` support: the header guarantees at-most-once execution per key regardless of the underlying operation's own retry-safety, so a client MAY use it to make even a not-safely-retryable operation (`manifest/1`'s `idempotencyClass: not-idempotent`) safe to retry at the client's own request layer.

### Trace-ID propagation

**[API-060]** Every api/1 request MAY carry a `Trace-Id` request header. Every api/1 response, success or error, MUST carry a `Trace-Id` response header: the request's own value, if it validated (API-061), or a freshly server-generated one otherwise.

**[API-061]** A supplied `Trace-Id` value MUST be accepted only if it is 20–36 characters long and matches `^[A-Za-z0-9-]+$` (a Crockford-base32 ULID and a hyphenated UUID both satisfy this). A value failing this check MUST be discarded and replaced with a freshly server-generated ULID; the request MUST still proceed — an invalid `Trace-Id` is never itself a request error.

**[API-062]** Every Problem response body (Error shape) MUST echo the response's `Trace-Id` value in its own `trace_id` extension member, so the header and the body agree and either alone is sufficient for correlating a report back to a request.

**[API-063]** When a request causes work in another component (a relay-bound command, a durable event, a background job), the server MUST propagate the same `Trace-Id` value into that component's own record of the work, so one value correlates the request across every component it touched.

### `mcp:` operation-tag curation

**[API-070]** An operation intended for exposure as an MCP tool MUST carry exactly one of the two curation tags `mcp:read` (no side effect a retry could double-apply) or `mcp:act` (mutates state). An operation carrying neither tag MUST NOT be exposed as an MCP tool.

**[API-071]** The set of `mcp:read`/`mcp:act`-tagged operations in the OpenAPI document is the sole input to MCP tool generation; no separate allowlist or denylist exists. Removing both tags from an operation, or removing the operation itself, retires its generated tool at the next generation.

**[API-072]** An operation tagged `mcp:act` that is a POST MUST also accept `Idempotency-Key` (Idempotency-Key), so that an MCP client's own retry-on-timeout behavior cannot double-apply a mutating tool call.

### Evolution & deprecation policy

**[API-080]** Within major version 1, api/1 evolution MUST be additive only: a new operation, a new optional request field, or a new response field MAY be introduced in any minor. An existing required request field, an existing response field's type, an existing operation's path, or an existing error `code`'s meaning MUST NOT change within major version 1 — such a change requires a new major version and a new path prefix (`/api/v2`).

**[API-081]** A field, parameter, or operation being phased out MUST first be marked `deprecated: true` at its OpenAPI location for at least one full minor version before its removal date is set.

**[API-082]** Once a removal date is set for a deprecated field, parameter, or operation, every response touching it MUST carry a `Deprecation` response header (RFC 9745, the deprecation date) and a `Sunset` response header (RFC 8594, the removal date). Removal MUST NOT occur before the `Sunset` date has passed.

**[API-083]** A generated client built against api/1's current minor, and a generated client built against the immediately preceding minor, MUST both continue to function correctly against the current server — the server MUST NOT require a client to be on the latest minor.

**[API-084]** A breaking change (API-080) MUST ship as a new major-version path prefix served concurrently with the prior major for a stated overlap window, never as an in-place change to an existing major's behavior.

## Wire shapes

```json
// Problem — the shared error shape
{
  "type": "about:blank",
  "title": "Not Found",
  "status": 404,
  "detail": "No scope node exists with this identifier.",
  "instance": "/api/v1/scope-nodes/01J8Z3K4N5P6Q7R8S9T0V1W2X9",
  "code": "NOT_FOUND",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X4"
}
```

```json
// Problem — multi-field validation failure (API-013)
{
  "type": "about:blank",
  "title": "Validation Failed",
  "status": 400,
  "code": "VALIDATION_FAILED",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X5",
  "errors": [
    { "field": "kind", "code": "ENUM_MISMATCH", "message": "must be one of org, site, group, screen" },
    { "field": "name", "code": "TOO_SHORT", "message": "must be at least 1 character" }
  ]
}
```

```json
// A paginated list response envelope, page 1 of 2 (Keyset pagination)
{
  "items": [{ "id": "01J8Z3K4N5P6Q7R8S9T0V1W2X3" }],
  "cursor": "01J8Z3K4N5P6Q7R8S9T0V1W2X3"
}
```

```json
// The same list operation, final page — cursor is null (API-032)
{
  "items": [],
  "cursor": null
}
```

```http
# A state-changing request without If-Match (API-022)
PATCH /api/v1/scope-nodes/01J8Z3K4N5P6Q7R8S9T0V1W2X3 HTTP/1.1
Content-Type: application/json

{"name": "Renamed Site"}
```

```http
# The same request, correctly conditioned (API-021)
PATCH /api/v1/scope-nodes/01J8Z3K4N5P6Q7R8S9T0V1W2X3 HTTP/1.1
Content-Type: application/json
If-Match: "3"
Idempotency-Key: 8f14e45f-ceea-4b3e-8c1e-1a1b2c3d4e5f
Trace-Id: 01J8Z3K4N5P6Q7R8S9T0V1W2X4

{"name": "Renamed Site"}
```

## Negotiation

api/1 has no connection-time handshake — it is negotiated once, structurally, via the URL path prefix (API-001) and enforced continuously via the evolution policy (Evolution & deprecation policy):

- **Version selection** — a client selects a major version by the path prefix it calls (`/api/v1`); there is no header-based version negotiation.
- **Minor-version skew** — within major version 1, a client generated against the current minor and one generated against the immediately preceding minor both continue to work against the current server (API-083); this is a static guarantee of the additive-only rule (API-080), not a runtime negotiation step.
- **Deprecation ahead of removal** — a deprecated field, parameter, or operation is marked `deprecated: true` at least one minor before a removal date is set (API-081), and carries `Deprecation`/`Sunset` response headers once that date is set (API-082) — a client can detect an approaching removal without out-of-band notice.
- **Major-version overlap** — a breaking change ships as a new path prefix served alongside the prior major for a stated overlap window (API-084); a client migrates by changing its path prefix, not by renegotiating a connection.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `VALIDATION_FAILED` | The request body or a query parameter failed schema or business validation. | no — fix the request |
| `UNAUTHENTICATED` | No valid principal was presented. | no — re-authenticate |
| `FORBIDDEN` | The principal is authenticated but not authorized for this operation. | no |
| `NOT_FOUND` | No resource exists at the identifier named by the request. | no |
| `REVISION_CONFLICT` | `If-Match` did not equal the resource's current `ETag`/`revision`. | yes — re-read and retry with the fresh revision |
| `IF_MATCH_REQUIRED` | A state-changing request against an existing resource omitted `If-Match`. | yes — retry with `If-Match` set |
| `CURSOR_INVALID` | The supplied `cursor` was malformed, expired, or issued by a different operation. | yes — retry without a cursor |
| `SELECTOR_INVALID` | The supplied `selector` failed to parse under the label-selector grammar. | yes — retry with a corrected selector |
| `IDEMPOTENCY_KEY_REUSED` | The same `Idempotency-Key` scope was presented with a different request body. | no — use a new key or the original body |
| `IDEMPOTENCY_KEY_IN_PROGRESS` | The same `Idempotency-Key` scope's original request has not yet completed. | yes — after a short backoff |
| `RATE_LIMITED` | The principal exceeded its rate limit. | yes — after the stated backoff |
| `INTERNAL` | An unclassified server-side failure. | yes — with backoff |
| `UNAVAILABLE` | The server or a dependency it needs is temporarily unable to serve the request. | yes — with backoff |

## Conformance notes

- Traceability map: `conformance/traceability/api-1.md` — maps every `API-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/api-1/` — one JSON case file per `case-id` referenced from the traceability map.
- `api/openapi.yaml` is this contract's machine-readable companion: it implements every component named above (`Problem`, the pagination parameters, the `selector` parameter, `If-Match`, `Idempotency-Key`, `Trace-Id`) and applies them to the `scope-nodes` and `automations` resource families end to end; every other resource family is a path stub pending a later minor.
- The 24-hour Idempotency-Key retention window (API-052) and the additive-evolution/deprecation timeline (Evolution & deprecation policy) are both time-dependent properties; corpus cases exercise the request/response shapes these rules produce, not elapsed real time — retention-window and deprecation-timeline behavior are exercised against an injectable clock in a driver harness, not a static corpus.
- The principal/role/session model that `Principal` (Definitions) and `FORBIDDEN`/`UNAUTHENTICATED` (Error taxonomy) presuppose is out of this contract's scope (Scope) and is not exercised by this corpus; cases that need a principal treat one as a given, opaque input.
