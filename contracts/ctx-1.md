# Pack Runtime Protocol

**Contract:** ctx/1
**Version:** 1.0
**Status:** review

## Scope

ctx/1 defines the runtime protocol between the host and a running pack process: connection setup, framing, the hello/negotiate handshake, verb dispatch in both directions (pack→host requests, host→pack event/action dispatch), and the deprecation and error-handling contract that keeps a pack process portable across host implementations and stable over time.

- In scope: wire framing; the two named transport bindings and their timeout/reconnect/latency semantics; hello/negotiate; the verb-family catalog and the normative request/response shapes of `data`, `secrets`, `connections`, `assets`, `events`, `notify`, `health`, `http`, and `actions`; the verb deprecation/support-window policy; error taxonomy.
- Out of scope: manifest declaration shapes that drive verb behavior — capabilities, egress allowlist, data model (`manifest/1`); process supervision, sandboxing, and restart policy (a host concern, not a wire contract); the declarative UI grammar a pack's pages use (`ui-schema/1`); the automation rule vocabulary (`rules/1`); device-class semantics (a separate companion document); credential/certificate provisioning for the `network` binding (deployment configuration, not this wire contract).

## Definitions

- **Host** — the platform process (or its per-pack supervisor) that a ctx/1 connection is established with.
- **Pack process** — the running instance of a pack's bundled logic.
- **Frame** — one length-prefixed msgpack-encoded message on a ctx/1 connection.
- **Verb** — a named remote operation within a verb family, invoked request/response or fire-and-forget depending on its family.
- **Correlation ID** — the `id` field pairing a response frame to its request frame.
- **Trace ID** — the `trace_id` field threading one logical operation across host and pack logs (and, where the operation crosses further, relay and platform logs).
- **Principal** — a reference (ULID) to the identity on whose behalf a dispatched operation runs; the actor a pack attributes and authorizes an action's effects against. Carried today by `actions.invoke`'s `principal` field (CTX-110).
- **ULID** — as defined in manifest/1: a 26-character Crockford-base32, time-sortable identifier.

## Normative requirements

### Framing

**[CTX-001]** Every ctx/1 message MUST be encoded as one frame: a 4-byte unsigned big-endian length prefix followed by exactly that many bytes of msgpack-encoded payload. The length prefix counts the msgpack payload only, never itself, so a reader can allocate and frame without parsing.

**[CTX-002]** A frame's encoded payload MUST NOT exceed 16 MiB. A peer asked to send, or that receives, a frame whose payload would exceed this MUST fail the operation with a typed `FRAME_TOO_LARGE` error rather than truncate or fragment silently.

**[CTX-003]** Every frame's payload MUST be a msgpack map with at least the keys `type` (one of `request`, `response`, `event`, `error`), `id` (correlation ID, ULID), and `trace_id` (ULID). `request` and `event` frames additionally carry `verb` (string, `family.method` form) and `body` (map); `response` frames carry `body`; `error` frames carry `code` (string) and `message` (string) in place of `body`.

**[CTX-004]** A peer receiving a frame that fails to decode as msgpack, or whose payload does not satisfy CTX-003, MUST close the connection — after sending one `error` frame with code `MALFORMED_FRAME` if the transport still permits a write. It MUST NOT attempt to resynchronize mid-stream.

### Transport bindings

**[CTX-010]** ctx/1 is transport-agnostic above the framing layer (CTX-001–004); this contract defines exactly two named bindings, `local` and `network`. A conformant host implementation MUST support `local`; `network` support is specific to host deployments that run pack processes off-host from the connecting peer.

**[CTX-011]** **`local` binding:** the host listens on a filesystem unix domain socket and passes its path to the pack process via the `CTX_SOCKET_PATH` environment variable; the pack process MUST connect to it as a client. As a development-tooling alternative, the host MAY instead launch the pack as a direct child process communicating over its inherited stdin/stdout, signaled by `CTX_TRANSPORT=stdio` in place of `CTX_SOCKET_PATH`; framing (CTX-001–004) is identical over stdio.

**[CTX-012]** **`network` binding:** the host listens on a TLS endpoint; the pack process MUST connect to it as a client, authenticating with a client certificate whose material is provisioned to the pack process out-of-band (not specified by this contract). Both peers MUST validate the other's certificate against a trust anchor provisioned the same way. A plaintext (non-TLS) connection on this binding MUST be refused.

**[CTX-013]** On the `local` binding, the pack process MUST complete the hello/negotiate handshake (CTX-020–024) within 5 seconds of connecting; on the `network` binding, within 15 seconds (accounting for TLS handshake and network latency). A host that does not observe a completed handshake within the applicable window MUST close the connection and treat the attempt as a failed pack start.

**[CTX-014]** After a successful handshake, either peer MAY send a `control.ping` event frame on a connection otherwise idle for 30 seconds; the receiving peer MUST respond with `control.pong` within 10 seconds, or the sender MUST treat the connection as dead and close it.

**[CTX-015]** On the `network` binding only, a pack process whose connection drops after a successful handshake MUST attempt to reconnect with exponential backoff starting at 1 second, doubling on each attempt, capped at 30 seconds between attempts, retrying indefinitely until reconnection succeeds or the host's supervisor terminates the pack process. The `local` binding relies on process supervision (out of scope of this contract) rather than protocol-level reconnect.

**[CTX-016]** Each verb family carries a latency envelope a conformant host SHOULD meet for a healthy pack under nominal load: `data` and `entities` verbs SHOULD complete within 250ms; `secrets` and `connections` verbs SHOULD complete within 2 seconds (allowing for upstream credential refresh); `health.report` and `events.emit` SHOULD complete within 250ms. `assets.derive` is exempt — it is asynchronous by contract (CTX-061) and carries no round-trip budget. These are conformance-suite timing assertions, not hard protocol limits: a peer MUST NOT treat a slow-but-eventually-completed call as a protocol error.

*draft-note: the specific timeout, backoff, and latency-envelope numbers in CTX-013–016 are proposed defaults for review — they are not derived from measured hardware timing and should be revisited once real numbers exist.*

### Hello / negotiate

**[CTX-020]** The first frame on every newly connected ctx/1 connection MUST be sent by the pack process, with `verb: "control.hello"` and a body of `{manifest_id, manifest_version, ctx_range, feature_flags}`, where `ctx_range` is the `compat.ctx` version-range string from the pack's manifest (manifest/1 MAN-010) and `feature_flags` is the array from `compat.features` (MAN-012).

**[CTX-021]** The host's response MUST be a `control.hello-ack` frame with a body of `{negotiated_version, feature_flags, deprecated}`, where `negotiated_version` is the highest `ctx/1` minor version satisfying `ctx_range` that the host implements (as a `major.minor` string), `feature_flags` is the subset of the pack's requested flags the host grants, and `deprecated` is a map of verb name to `{deprecated_in, removed_in, message}` for every verb in the negotiated version that is currently deprecated.

**[CTX-022]** If no `ctx/1` version the host implements satisfies the pack's `ctx_range`, the host MUST respond with an `error` frame (code `INCOMPATIBLE_RANGE`) in place of `control.hello-ack`, then close the connection. This is the major-mismatch case (CTX-023).

**[CTX-023]** A **major** mismatch — the pack's `ctx_range` excludes every major version the host implements — MUST refuse per CTX-022. A **minor** mismatch — the range excludes the host's newest minor but includes an older one within an implemented major — MUST negotiate down to the highest mutually satisfying minor rather than refuse.

**[CTX-024]** Neither peer may send a frame with any `verb` other than `control.hello` / `control.hello-ack` before the handshake (CTX-020–021) completes; a peer that does MUST be treated as a protocol violation (`PROTOCOL_VIOLATION`) and the connection closed.

### Verb family catalog

**[CTX-030]** A conformant host MUST implement the following verb families, each namespacing its verbs as `<family>.<method>`: `data`, `config`, `secrets`, `connections`, `services`, `entities`, `assets`, `events`, `notify`, `health`, `http`, `log`, `schedule`, and `actions`. This contract specifies the normative request/response shape of `data`, `secrets`, `connections`, `assets`, `events`, `notify`, `health`, `http`, and `actions` below; the remaining families (`config`, `services`, `entities`, `log`, `schedule`) carry the common frame envelope (CTX-003) and are reserved for full specification in a future ctx/1 minor — a host MUST accept a hello/negotiate declaring baseline ctx/1 support without requiring the pack to exercise them.

**[CTX-031]** Every verb call MUST carry the correlation ID (CTX-003 `id`) and trace ID (CTX-003 `trace_id`); a host or pack that forwards work triggered by a verb call to another subsystem (e.g. a device command, a durable event) MUST propagate the same `trace_id` into that subsystem's own record of the work.

### `data` family

**[CTX-040]** `data.query(collection, filter?, sort?, cursor?, limit?, revision?, state?)` MUST be scoped to collections the calling pack declared in its own `dataModel.collections` (manifest/1 MAN-051); a request naming any other pack's collection MUST fail with `COLLECTION_NOT_OWNED`. `filter` accepts the typed operators `eq`, `ne`, `lt`, `lte`, `gt`, `gte`, `in`, `contains`, composed with `and`/`or`. `state` restricts results to `draft` or `published` rows on collections declaring `lifecycle: draft-publish` (manifest/1 MAN-052); `revision`, when present, restricts to rows whose current `revision` exactly matches (a read-after-write confirmation use, not a historical-version query — this contract keeps no row history). `cursor`, when present, MUST be treated as an opaque continuation token: the pack MUST NOT construct, parse, or infer meaning from its contents, only pass back a value the host itself returned. A response's own `cursor` field (Wire shapes) carries the token for the next page, and is `null` once no further rows remain.

*draft-note: this contract does not mint its own cursor grammar — the concrete token format aligns with the platform's keyset-pagination convention authored in `api/v1`, referenced here rather than redefined. Confirm the reference once api/v1 lands.*

**[CTX-041]** `data.write(collection, entity_id?, fields, expected_revision?)` creates a row (when `entity_id` is omitted) or updates one. An update MUST supply `expected_revision`; the host MUST reject the write with `REVISION_CONFLICT` if the row's current `revision` does not match. No unconditional-overwrite path exists.

**[CTX-042]** `data.aggregate(collection, op, field?, groupBy?, filter?)` MUST support `op` values `count`, `sum`, `min`, `max` (`field` required for all but `count`). The response body MUST be `{value}` — the scalar aggregate result — when `groupBy` is absent, or `{groups}` — an array of `{key, value}` objects, one per distinct value of the `groupBy` field among the filtered rows — when `groupBy` is present. Joins across collections and raw query languages are explicitly not offered by this verb or any other in the `data` family — a pack composing data from multiple collections MUST denormalize at write time.

### `secrets` and `connections` families

**[CTX-050]** `secrets.require(name)` returns a versioned opaque handle `{handle, version}` for a secret the pack declared a need for; it MUST NOT return raw secret material. A pack resolves the handle to a usable value only via the specific verb that consumes it (e.g. `http.request` accepting a `secretHandle` field in place of a literal credential) — raw secret bytes MUST NOT appear in any `data`, `log`, or `event` verb body.

**[CTX-051]** `connections.get(provider)` returns `{token, state, expires_at}` for a connection declared in the pack's manifest (`connections`, manifest/1 MAN-055); the host owns credential refresh. A pack MUST treat the returned token as short-lived and MUST NOT cache it past `expires_at`.

**[CTX-052]** The host MUST emit a `connections.state_changed` event (see `events` family) whenever a connection's `state` transitions (e.g. `connected` → `expired` → `revoked`), so a pack does not need to poll `connections.get` to detect loss of access.

### `assets` family

**[CTX-060]** `assets.put(bytes_ref)` and `assets.ref(hash)` are synchronous: `put` returns a content-addressed `sha256:` URI once the host has durably stored the content; `ref` resolves an existing `sha256:` URI to its current metadata (`{size, contentType, createdAt}`) or fails with `NOT_FOUND`.

**[CTX-061]** `assets.derive(profile, inputs, params?)` MUST be asynchronous: the call returns immediately with `{job_id}` (a ULID), and the derived output is delivered exclusively via a subsequent `events` frame (`assets.derive_complete`, body `{job_id, output_ref}` or `{job_id, error}`) — never as the `assets.derive` call's own response body. `inputs` MUST be an array of `sha256:` URIs already resolvable via `assets.ref`; the host MUST reject a request containing any input not yet content-addressed.

**[CTX-062]** The `assets.derive` request and its resulting job envelope MUST NOT contain secret handles, live upstream URLs, or any field not resolvable to a `sha256:` URI already at rest — a derive job's inputs are exactly the enumerated content-addressed objects, nothing fetched ad hoc during the job.

### `events` family

**[CTX-070]** `events.emit(name, payload)` publishes a durable event; `name` MUST match a `contributes.automation.events` entry the pack declared (manifest/1 MAN-090), and `payload` MUST validate against that entry's `payloadSchema`. An unnamed or schema-invalid emit MUST fail with `EVENT_NOT_DECLARED` or `PAYLOAD_SCHEMA_INVALID` respectively — never be silently dropped.

**[CTX-071]** The host dispatches events **to** a pack (host→pack direction) as `event`-type frames whose `verb` matches a family the pack subscribed to at hello time (via `feature_flags` or an explicit `events.subscribe` call); the pack MUST NOT be sent events it never subscribed to.

### `notify` family

**[CTX-080]** `notify.send(template, recipients_selector, params)` requests the host deliver a notification through a platform-owned transport — the pack never opens its own transport-level socket (SMTP, webhook, push); egress scoping (manifest/1 MAN-030–032) grants HTTP egress, not raw transport access. `recipients_selector` MUST use the platform's label-selector grammar; `template` MUST reference a locale-catalog entry bundled with the pack.

**[CTX-081]** `notify.send` is fire-and-forget from the pack's perspective: the response acknowledges only that the request was accepted for delivery, not that delivery succeeded. Delivery outcome, retry, and failure surfacing are handled outside this contract.

### `health` family

**[CTX-090]** `health.report(status, detail?)` reports the pack's own health; `status` MUST be one of `healthy`, `degraded`, `degraded-critical`. A pack MUST send an initial `health.report(healthy)` within 60 seconds of completing hello/negotiate, and MUST send a report within 10 minutes of any transition into `degraded-critical`. The host MAY treat an absent initial report past 60 seconds as `degraded` for supervision purposes.

**[CTX-091]** `detail`, when present, MUST be a `{code, message}` object identifying the degraded subsystem; `code` values are pack-defined strings, opaque to the host beyond display and aggregation into its own operator-facing health surfaces.

### Backpressure signal

**[CTX-092]** ctx/1 reserves a host→pack backpressure/throttle signal — a `control.backpressure` event frame (host → pack), reusing the same `control.*` connection-level verb namespace `control.hello`/`control.ping` already establish (Hello / negotiate, Transport bindings) — that a conformant host MAY dispatch to tell a pack to slow or pause its own outbound call rate. A pack that does not act on it is not in protocol violation for v1: this requirement reserves only the verb name and its direction; its full body shape is left to a future ctx/1 minor, the same additive-reservation treatment CTX-030 already gives the `config`/`services`/`entities`/`log`/`schedule` verb families.

*draft-note: once specified, `control.backpressure`'s throttle scope is expected to key on the same `cost_class` classification `events/1`'s durable-event envelope already carries (`events/1` Durable-event envelope) — the vocabulary this platform uses elsewhere to classify queued, throttleable work, which would include a host's own queued `assets.derive` jobs (CTX-061). This note states the expected scoping axis only; it defines no new field on `assets.derive` or any other verb today, and the signal's own concrete body shape remains reserved to the future minor CTX-092 describes.*

### `http` family

**[CTX-100]** `http.request(method, url, headers?, body?, secretHandle?)` is the pack's only sanctioned network egress path. The host MUST reject any `url` whose host does not match an entry in the pack's manifest `egress` allowlist (manifest/1 MAN-030) with `EGRESS_NOT_ALLOWLISTED`, and MUST re-validate the allowlist match on every redirect hop rather than only the initial URL.

**[CTX-101]** Before dispatching `http.request`, the host MUST resolve the target hostname and reject the call with `EGRESS_DENIED_IP_CLASS` if the resolved address falls in a loopback, RFC 1918, link-local, or unique-local (ULA) range — regardless of whether the allowlist matched by hostname or IP-literal. This check MUST repeat against every redirect hop's resolved address, not only the original request.

**[CTX-102]** `http.request` MUST pin the DNS resolution used for the allowlist/IP-class check (CTX-100–101) to the connection actually opened — a re-resolution between check and connect that could substitute a different address MUST NOT be possible.

### `actions` family (host → pack dispatch)

**[CTX-110]** The host dispatches a pack action invocation (manifest/1 MAN-100) as a `request`-type frame with `verb: "actions.invoke"` and body `{action_name, params, principal}`; the pack's response body MUST be `{result}`, or an `error` frame. This is the one direction in the `actions` family — packs never call `actions.invoke` on the host. CTX-112 states the exception when automation invokes an `execution: relay-command` action.

**[CTX-111]** An action whose manifest declaration marks it `not-idempotent` (manifest/1 MAN-103) MUST NOT be automatically retried by the host on timeout or connection loss; an `automationCallable: true` action invoked from automation carries the same idempotency contract as one invoked via the management API.

**[CTX-112]** When automation invokes an action declared `execution: relay-command` (manifest/1 MAN-091), the relay executes it directly as a device command; the host MUST NOT dispatch it as an `actions.invoke` frame for that invocation. `actions.invoke` (CTX-110) is used for that same action when it is invoked via the management API or as an `execution: app-service` automation action (manifest/1 MAN-091).

### Deprecation & support window

**[CTX-120]** A verb MUST NOT be removed from a ctx/1 host implementation less than **two ctx/1 minor versions** after it was first marked `deprecated_in`, **and** less than **six months** after that minor shipped — whichever bound is longer.

**[CTX-121]** A pack whose declared `compat.ctx` range falls behind the host's negotiated version (CTX-021, CTX-023) MUST continue running with a typed operator-facing warning; the host MUST NOT refuse to start an already-installed pack solely for using a deprecated verb still inside its support window. Only a fresh install of that pack version MAY be blocked once its range no longer intersects the host's implemented majors at all.

**[CTX-122]** A verb past its `removed_in` version MUST respond `VERB_REMOVED` rather than silently no-op or repurpose the verb name.

## Wire shapes

```json
// Frame (msgpack payload shown as JSON for readability)
{
  "type": "request",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2X3",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X4",
  "verb": "data.query",
  "body": { "collection": "forecasts", "limit": 20 }
}
```

```json
// control.hello (pack -> host, frame zero on every connection)
{
  "type": "request",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2X5",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X5",
  "verb": "control.hello",
  "body": {
    "manifest_id": "acme/weather-widget",
    "manifest_version": "1.2.0",
    "ctx_range": ">=1.0 <2.0",
    "feature_flags": []
  }
}
```

```json
// control.hello-ack (host -> pack)
{
  "type": "response",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2X5",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X5",
  "body": {
    "negotiated_version": "1.3",
    "feature_flags": [],
    "deprecated": {
      "data.legacyQuery": { "deprecated_in": "1.2", "removed_in": "2.0", "message": "use data.query" }
    }
  }
}
```

```json
// data.query request
{
  "type": "request",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y1",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y1",
  "verb": "data.query",
  "body": {
    "collection": "forecasts",
    "cursor": "opaque-token-returned-by-a-prior-data.query-response",
    "limit": 20
  }
}
```

```json
// data.query response
{
  "type": "response",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y1",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y1",
  "body": {
    "rows": [
      {
        "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y2",
        "revision": 1,
        "lifecycle_state": "published",
        "scope_node": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
        "labels": [],
        "template_ref": null,
        "params": null,
        "location": "west",
        "summary": "Sunny"
      }
    ],
    "cursor": null
  }
}
```

```json
// data.aggregate request (scalar)
{
  "type": "request",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y3",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y3",
  "verb": "data.aggregate",
  "body": {
    "collection": "forecasts",
    "op": "count"
  }
}
```

```json
// data.aggregate response (scalar)
{
  "type": "response",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y3",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y3",
  "body": {
    "value": 42
  }
}
```

```json
// data.aggregate request (groupBy)
{
  "type": "request",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "verb": "data.aggregate",
  "body": {
    "collection": "forecasts",
    "op": "count",
    "groupBy": "location"
  }
}
```

```json
// data.aggregate response (groupBy)
{
  "type": "response",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "body": {
    "groups": [
      { "key": "west", "value": 3 },
      { "key": "east", "value": 1 }
    ]
  }
}
```

```json
// assets.derive request
{
  "type": "request",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2X6",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X6",
  "verb": "assets.derive",
  "body": {
    "profile": "html_bundle-to-png",
    "inputs": ["sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85"],
    "params": { "width": 1920, "height": 1080 }
  }
}
```

```json
// assets.derive_complete (host -> pack, event — the async completion of the request above)
{
  "type": "event",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2X7",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X6",
  "verb": "assets.derive_complete",
  "body": {
    "job_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X6",
    "output_ref": "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
  }
}
```

```json
// actions.invoke request (host -> pack)
{
  "type": "request",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y5",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y5",
  "verb": "actions.invoke",
  "body": {
    "action_name": "refresh",
    "params": {},
    "principal": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z6"
  }
}
```

```json
// actions.invoke response (pack -> host)
{
  "type": "response",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y5",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y5",
  "body": {
    "result": { "status": "ok" }
  }
}
```

```json
// error frame
{
  "type": "error",
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2X8",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X8",
  "code": "EGRESS_NOT_ALLOWLISTED",
  "message": "host api.other.example is not in this pack's egress allowlist"
}
```

## Negotiation

manifest/1's `compat.ctx` range and the host's implemented `ctx/1` minors are reconciled at connection time via `control.hello` / `control.hello-ack` (CTX-020–021):

- **Major mismatch** — no host-implemented major satisfies the pack's range: the host MUST refuse with `INCOMPATIBLE_RANGE` and close the connection (CTX-022, CTX-023).
- **Minor mismatch** — the pack's range excludes the host's newest minor but admits an older one: the host MUST negotiate down and report that older minor as `negotiated_version` (CTX-023).
- **Deprecation ahead of removal** — a verb the negotiated version still includes but has begun deprecating is listed in `hello-ack.deprecated`; the pack MAY continue calling it until `removed_in`, subject to the two-minors-and-six-months floor (CTX-120).
- **Support window** — an installed pack whose declared range trails the host's current minor keeps running with a typed warning rather than being stopped (CTX-121); only a fresh install of that pack version is blocked once its range no longer intersects the host's implemented majors at all.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `FRAME_TOO_LARGE` | A frame's msgpack payload exceeds 16 MiB. | no |
| `MALFORMED_FRAME` | A frame failed to decode as msgpack or did not satisfy the minimum envelope shape. | no |
| `INCOMPATIBLE_RANGE` | No host-implemented ctx/1 major satisfies the pack's declared `ctx_range`. | no |
| `PROTOCOL_VIOLATION` | A verb frame was sent before hello/negotiate completed, or another ordering rule was broken. | no |
| `COLLECTION_NOT_OWNED` | A `data` verb named a collection the calling pack did not declare in its own manifest. | no |
| `REVISION_CONFLICT` | `data.write`'s `expected_revision` did not match the row's current revision. | yes — re-read and retry with the fresh revision |
| `EVENT_NOT_DECLARED` | `events.emit` used a `name` not present in the pack's `contributes.automation.events`. | no |
| `PAYLOAD_SCHEMA_INVALID` | An `events.emit` payload failed its declared `payloadSchema`. | no |
| `EGRESS_NOT_ALLOWLISTED` | `http.request`'s target host (initial or post-redirect) is not in the manifest `egress` allowlist. | no |
| `EGRESS_DENIED_IP_CLASS` | `http.request`'s resolved address is loopback, RFC 1918, link-local, or ULA. | no |
| `VERB_REMOVED` | The called verb is past its `removed_in` version. | no |
| `NOT_FOUND` | `assets.ref` was called with a `sha256:` URI the host has no record of. | no |

## Conformance notes

- Traceability map: `conformance/traceability/ctx-1.md` — maps every `CTX-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/ctx-1/` — one JSON case file per `case-id` referenced from the traceability map.
- The `network` binding's certificate-provisioning mechanism is deployment configuration, not specified here; conformance cases exercise framing, handshake, and verb semantics against the `local` binding as the reference transport — both bindings share the same framing and verb layer by construction (CTX-010), so binding choice is not itself a source of behavioral variance the corpus needs to double up on.
- Timing-dependent behavior (CTX-013–016's timeouts/backoff) is exercised against an injectable/fake clock in the driver harness, not wall-clock sleeps in a static corpus — the JSON cases here assert frame shape and sequencing, not elapsed time.
- `config`, `services`, `entities`, `log`, and `schedule` verb families (CTX-030) carry no normative wire shape yet beyond the common envelope; they are out of corpus scope until a future ctx/1 minor specifies them.
