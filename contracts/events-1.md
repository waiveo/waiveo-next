# Client Push / Watch Channel

**Contract:** events/1
**Version:** 1.0
**Status:** draft

## Scope

events/1 defines the platform's client push channel: the durable-event envelope, the catalog of platform-registered event schemas, the two wire bindings a subscriber uses to receive events live (WebSocket and Server-Sent Events), authentication for both, scope-node filtering, resumable delivery with explicit loss marking, and outbound webhook delivery. Where api/1 is the door a client sends commands through, events/1 is the door a client watches through — a subscriber never mutates state over this contract.

- In scope: the durable-event envelope and its generic fields; the platform-registered schema catalog (`entity.state_changed`, `automation.run`, `content.played`, `device.heartbeat`, `box.vitals`, `audit.event`) — fields, types, and when each is emitted; the naming rule that keeps platform schemas and pack-contributed schemas from colliding; the WS and SSE bindings; session/API key authentication for both; scope-node filtering; resumable delivery (the resume cursor and the gap signal); outbound webhook delivery (signing, retry, dead-letter).
- Out of scope: a pack's own `events` verb family — emitting a pack-declared event and receiving a dispatched one are `ctx/1` concerns, including payload validation against a pack's own declared schema; inbound webhook and ingest intake, a distinctly authenticated surface with its own route family (`api/1`); the Job resource and its state machine for long-running fleet operations (`api/1`); management-plane CRUD for registering a webhook endpoint or querying event history by page (`api/1`); the roles/scopes model that determines which scope nodes a principal may read — this contract consumes that as a given input, never redefines it.

## Definitions

- **ULID** — as defined in `manifest/1`: a 26-character Crockford-base32, time-sortable identifier.
- **Timestamp** — as defined in `rules/1`: an integer number of milliseconds since the Unix epoch (UTC).
- **Principal** — as defined in `api/1`: the authenticated caller a session or API key credential resolves to. This contract treats a principal only as an opaque identifier for authentication and scope-node visibility.
- **Scope node** — as defined in `api/1`: a node in the platform's placement tree; every envelope carries the scope node the event's subject resource is placed under.
- **Durable event** — one occurrence recorded by the platform under a registered or pack-declared schema, assigned a permanent ID and retained for a bounded window regardless of whether any subscriber is connected when it occurs.
- **Envelope** — the generic wrapper (Durable-event envelope) every durable event is delivered in, regardless of binding.
- **Registered schema** — one of the six platform-defined payload shapes this contract catalogs (Registered-schema catalog — general).
- **Pack-contributed schema** — an event name a pack declares via its manifest and emits through `ctx/1`; carried in the same envelope but not cataloged by this contract.
- **Subscriber** — a WS or SSE client with an open, authenticated events/1 connection.
- **Resume cursor** — the envelope `id` of the last event a subscriber successfully observed, supplied back on reconnect to continue from that point (Resume cursor).
- **Gap** — a signal marking a range of the event stream a subscriber will never receive (Loss markers).
- **Webhook endpoint** — a registered destination URL and signing secret this contract delivers matching durable events to; registration itself is a management-plane resource, out of this contract's scope.

## Normative requirements

### Versioning & transport surface

**[EVT-001]** events/1 MUST be reachable at the stable path `/events/v1`, independent of and never nested under the `/api/v1` prefix. Both the WS and SSE bindings (WS binding, SSE binding) are reachable at this same path, distinguished by the request's own shape: a WS `Upgrade` header selects the WS binding; an `Accept: text/event-stream` header with no `Upgrade` selects the SSE binding.

**[EVT-002]** Every payload this contract defines — a WS frame's body, an SSE `data:` field's content, a webhook delivery's request body — MUST be UTF-8 JSON.

**[EVT-003]** Within major version 1, evolution MUST be additive only: a new registered schema, a new optional envelope field, or a new optional field on an existing registered schema's payload MAY be introduced in a minor. An existing envelope field's meaning, an existing registered schema's already-published field, or the path (EVT-001) MUST NOT change within major version 1.

### Durable-event envelope

**[EVT-010]** Every event this contract delivers, over any binding, MUST be wrapped in one envelope carrying at least the fields below.

| field | type | required | notes |
|---|---|---|---|
| `id` | ULID | required | The event's own permanent identifier, assigned once at recording time. Doubles as the resume cursor value (Resume cursor). |
| `schema` | string | required | Names the payload shape (Registered-schema catalog — general). |
| `ts` | Timestamp | required | When the event was recorded. |
| `scope_node` | ULID | required | The scope node the event's subject resource is placed under; the sole input to scope-node filtering. |
| `trace_id` | ULID | required | The originating operation's trace ID, propagated from wherever the event was recorded. |
| `cost_class` | string | required | A short classification string used for backpressure/throttle accounting; its concrete vocabulary is maintained outside this contract. |
| `retention_class` | string | required | A short classification string governing how long the event is retained; its concrete vocabulary is maintained outside this contract. |
| `origin` | enum | required | One of `internal`, `relay`, `ingest` — which class of source recorded the event. |
| `origin_principal` | ULID | optional | The principal identified by `origin`, when one applies (e.g. the relay identity for `origin: relay`). Absent when the source carries no principal-shaped identity. |
| `payload` | object | required | The schema-specific fields (Registered-schema catalog — general); shape is determined by `schema`. |

**[EVT-011]** `id` values MUST be assigned in the platform's own recording order, so that comparing two IDs lexicographically also orders them by recording time — the same property `manifest/1`'s ULID definition already guarantees, relied on here for both delivery ordering (WS binding, SSE binding) and gap accounting (Loss markers).

**[EVT-012]** `scope_node` MUST be set at recording time to the scope node the event's subject resource — the entity, rule, screen, device, or principal the event is about — is itself placed under; an envelope with no defensible single subject (none exists among the registered schemas) is not addressed by this contract.

**[EVT-013]** An event whose `payload` does not validate against its `schema`'s field definition (Registered-schema catalog — general) MUST NOT be delivered over any binding this contract defines — a subscriber that receives an event MUST be able to trust its payload validates.

### Registered-schema catalog — general

**[EVT-020]** The catalog of platform-registered schemas — `entity.state_changed`, `automation.run`, `content.played`, `device.heartbeat`, `box.vitals`, `audit.event`, `variable.changed` — is additive-only: a new registered schema MAY be added by a later events/1 minor; a published schema's already-defined field MUST NOT change type or meaning, and MUST NOT be removed, within major version 1.

**[EVT-021]** A registered schema's `schema` value MUST be a bare `<domain>.<name>` string (lowercase, `.`-separated, no `/`). A pack-contributed schema's `schema` value is the pack-namespaced event name `manifest/1` MAN-090 defines (`<publisher>/<name>.<local-name>`), which always contains a `/` from its owning pack ID. These two forms are mutually exclusive by construction, so the registered and pack-contributed namespaces never collide.

**[EVT-022]** A pack-contributed event's `payload` validates against that pack's own declared `payloadSchema` (`manifest/1` MAN-090, enforced at emission by `ctx/1` CTX-070) — this contract defines no field shape for it and does not extend its own catalog to cover it. EVT-013's delivery guarantee still applies: an emission that failed `ctx/1`'s own validation never becomes a deliverable durable event.

### `entity.state_changed`

**[EVT-030]** The `entity.state_changed` schema publishes a transition observed on a device-plane entity. Its payload MUST validate against:

| field | type | notes |
|---|---|---|
| `entity_id` | ULID | The entity (`rules/1` Entity) that transitioned. |
| `device_id` | ULID | The parent device the entity belongs to. |
| `old_state` | string | The canonical state string before the transition. |
| `new_state` | string | The canonical state string after the transition; equal to `old_state` when this event reports an attribute-only change. |
| `attribute_change` | boolean | Whether any attribute value also changed on this same observation, independent of whether the canonical state string did (`rules/1` RUL-330). |
| `attributes_delta` | object | Present (possibly empty) whenever `attribute_change` is `true`; maps each changed attribute's name to `{old, new}`. Omitted when `attribute_change` is `false`. |

**[EVT-031]** An `entity.state_changed` event MUST be emitted when an entity's canonical state string changes, or when a `significant`-class attribute's value changes (`device-class-registry/1` REG-044); a change limited to `cosmetic`-class attributes MUST NOT alone trigger emission.

**[EVT-032]** This schema is a change feed, never a snapshot poll: it MUST be emitted once per qualifying observation (EVT-031), in the order those observations occurred, and MUST NOT be used to represent an entity's full current state — a subscriber reconstructing current state does so by folding the feed from a known baseline, not by treating any single event as authoritative of anything but that one transition.

### `automation.run`

**[EVT-040]** The `automation.run` schema publishes one automation rule evaluation's outcome. Its payload MUST validate against:

| field | type | notes |
|---|---|---|
| `rule_id` | ULID | The rule that was evaluated. |
| `rule_revision` | integer | The rule definition's revision at evaluation time. |
| `trigger_snapshot` | object | The trigger occurrence that started this evaluation, captured at the time it fired. |
| `condition_results` | array | One entry per evaluated condition, in declared order, with at least that condition's pass/fail outcome. |
| `action_outcomes` | array | One entry per attempted action, in execution order, with at least that action's outcome. |
| `mode_disposition` | enum | One of `ran`, `skipped`, `restarted` — the rule's mode-level disposition for this occurrence. |
| `misfire_caught` | boolean | Whether this occurrence's firing originated from a caught-up misfire (`rules/1` RUL-355) — orthogonal to `mode_disposition`, never a fourth disposition value (`rules/1` RUL-246). |

**[EVT-041]** `mode_disposition` MUST report `skipped` for an occurrence a rule's mode dropped without running any action, and `restarted` for one that canceled and replaced an in-flight run — never conflating either with a normal `ran` disposition. `misfire_caught` MUST be `true` for a deferred occurrence a rule's misfire policy ran later than scheduled (`rules/1` RUL-355), and `false` otherwise; it is an orthogonal marker of a firing's own origin, never a fourth `mode_disposition` value (`rules/1` RUL-246) — a caught-up misfire still resolves to `ran`, `skipped`, or `restarted` on its own merits, per whichever mode-evaluation outcome that firing would otherwise reach.

**[EVT-042]** An `automation.run` event's `origin` (Durable-event envelope) MUST reflect which evaluator produced it: `relay` for an edge-classified rule evaluated at the relay, `internal` for an app-side evaluation — the same schema serves both, distinguished only by `origin`.

**[EVT-043]** An `automation.run` event MUST be emitted for every rule occurrence reaching a mode disposition (EVT-041), including `skipped` — a rule that dropped an occurrence is exactly as observable through this schema as one that ran.

### `content.played`

**[EVT-050]** The `content.played` schema publishes one playback record for a screen. Its payload MUST validate against:

| field | type | notes |
|---|---|---|
| `asset_ref` | string | The played asset's content-addressed reference, a `sha256:` URI in the same form `ctx/1`'s `assets` family uses. |
| `screen_id` | ULID | The screen the asset played on. |
| `program_revision` | string | The compiled program revision the playback was assigned under. |
| `t_start` | Timestamp | When playback began. |
| `t_end` | Timestamp | When playback ended. |
| `cause` | enum | One of `scheduled`, `preempted`, `fallback`, `manual`, `loop` — why this playback occurred. |
| `completion` | enum | One of `completed`, `interrupted`, `skipped` — how it ended. |
| `power_evidence` | object, optional | `{power_state, source}` — device-power corroboration for the playback window, when the source device class supplies one. |

*draft-note: `cause`'s and `completion`'s member sets are fixed as MUST-level enums by the table above, not open; what remains a draft proposal is their provenance and `power_evidence`'s exact shape — this contract chose these specific members itself, without deriving them from any authoritative external source, so a future minor may still need to grow either enum additively (EVT-003) once real playback-engine behavior is surveyed.*

**[EVT-051]** A `content.played` event MUST be emitted once `t_end` is known for a given playback occurrence — this schema reports completed (or definitively ended) playback windows, never an in-progress one; a still-playing asset has not yet produced its event.

**[EVT-052]** `program_revision` MUST identify the exact compiled program revision in force at `t_start`, so a playback record remains attributable to a specific desired-state generation even after a later revision has since been applied.

### `device.heartbeat`

**[EVT-060]** The `device.heartbeat` schema publishes a device's periodic liveness/status report. Its payload MUST validate against:

| field | type | notes |
|---|---|---|
| `device_id` | ULID | The device this heartbeat is about. |
| `power_state` | string | The device's canonical power state, drawn from its device class's state vocabulary (`device-class-registry`). |
| `app_state` | string | The device's canonical foreground-app state, drawn from its device class's `app_type` attribute enum (`device-class-registry` REG-064). |
| `now_playing_content_id` | string, nullable | The content identifier currently playing on the device, or `null` when nothing is. |

**[EVT-061]** `power_state` MUST be a member of the reporting device's own device class's `states` (`device-class-registry` REG-020) — never a raw driver-reported value that hasn't been classified. `app_state` MUST be a member of the reporting device's own device class's `app_type` attribute enum (`device-class-registry` REG-064), matching `player/1`'s own PLY-121 — the registry carries no second, `states`-shaped list for foreground-app identity; only `app_type` expresses it.

*draft-note: this schema's emission cadence is not fixed by any normative source yet. Proposed: at least once per 60 seconds of connectivity, plus immediately on any `power_state` or `app_state` transition — subject to revision once real fleet data exists.*

### `box.vitals`

**[EVT-070]** The `box.vitals` schema publishes a relay's own physical/operational health snapshot. Its payload MUST validate against:

| field | type | notes |
|---|---|---|
| `relay_id` | ULID | The relay this report is about. |
| `cpu_temp` | number | Degrees Celsius. |
| `throttled_flags` | array of strings | Zero or more active throttle/warning flags (e.g. an undervoltage-now or throttled-now condition), named rather than bit-packed so a new flag is an additive schema change, not a bit-layout change. |
| `undervoltage` | boolean | Whether undervoltage is presently detected. |
| `disk_headroom` | integer | Bytes of free space remaining on the relay's own operational storage. |
| `sd_health` | object, optional | Implementation-defined SD/storage-wear detail, where the platform is able to read one. |

*draft-note: this schema's emission cadence is not fixed by any normative source yet. Proposed: at least once per 5 minutes, plus immediately on any flag transition (`undervoltage` or a new `throttled_flags` member) — subject to revision once real fleet data exists.*

**[EVT-071]** `throttled_flags` MUST be an empty array, never absent, when no flag is active — a subscriber checks for emptiness, never for the field's presence.

### `audit.event`

**[EVT-080]** The `audit.event` schema publishes one platform audit record. Its payload MUST validate against:

| field | type | notes |
|---|---|---|
| `actor_principal` | ULID | The principal that performed the action. |
| `on_behalf_of` | ULID, optional | A second principal the action was performed for, when the actor acted in a delegated capacity; absent for a self-attributed action. |
| `action` | string | A short, stable identifier for what happened (e.g. `login.failure`, `pack.install`). |
| `target` | string | An opaque reference to the affected resource, in the form `<resource-type>:<id>`. |
| `result` | enum | One of `success`, `failure`. |

*draft-note: `action`'s and `target`'s exact string grammars are this contract's own proposal — nothing normative fixes them beyond naming the two fields.*

**[EVT-081]** An `audit.event` event MUST be emitted for at least each of the following, without exception:

- authentication events: login success, login failure, and lockout;
- enrollment/pairing grant creation and redemption;
- consent changes;
- session or API key issuance and revocation;
- trust-bundle or signing-key changes;
- entitlement changes;
- pack install, update, and remove;
- every privileged operation performed over a pack-host or relay-host administrative binding.

**[EVT-082]** `retention_class` (Durable-event envelope) on an `audit.event` event MUST be set to a value the platform's retention configuration treats as long-lived relative to the other registered schemas — an audit trail outliving the operational telemetry recorded alongside it is the property this field exists to express, even though this contract does not itself enumerate retention-class values or their durations.

**[EVT-083]** `result: failure` MUST still carry every other field (EVT-080) — a failed action is exactly as auditable as a successful one, never elided for having failed.

### `variable.changed`

**[EVT-084]** The `variable.changed` schema publishes one committed change to a platform-owned typed variable's value. Its payload MUST validate against:

| field | type | notes |
|---|---|---|
| `variable` | string | The changed variable's own name. |
| `old_value` | string, number, or boolean; nullable | The variable's value immediately before this change; `null` when the variable was previously unset. |
| `new_value` | string, number, or boolean; nullable | The variable's value immediately after this change; `null` when this change unsets it. |

**[EVT-085]** A `variable.changed` event MUST be emitted once per committed variable write, in write order. This is the durable event an `event`-kind trigger addresses by its platform-reserved unnamespaced name to fire a rule immediately on a variable's own value changing (`rules/1` RUL-080) — a distinct mechanism from a `variable` condition (`rules/1` RUL-150), which reads a variable's compile-time-closed value without itself needing this event.

### WS binding

**[EVT-090]** A WS connection to `/events/v1` MUST negotiate the subprotocol `events.v1+json`; a client offering no subprotocol or a different one MUST be refused at the WS handshake, before any application-level frame is exchanged.

**[EVT-091]** The first client-to-server WS message on a newly opened connection MUST be a `hello` frame: `{type: "hello", resume_from?, selector?, schemas?}` (Resume cursor, Scope-node filtering). A connection that sends any other frame first MUST be closed.

**[EVT-092]** The server's response MUST be a `hello-ack` frame: `{type: "hello-ack", resume_result}`, where `resume_result` is one of `fresh` (no `resume_from` supplied, or delivery is starting from connection time forward), `resumed` (delivery is continuing from exactly the requested point with nothing lost), or `gap` (Loss markers — an explicit `gap` frame immediately follows).

**[EVT-093]** Every delivered durable event over WS MUST be an `event` frame: `{type: "event", event: <the envelope, Durable-event envelope>}`.

**[EVT-094]** A loss signal over WS MUST be a `gap` frame, shaped and triggered exactly as Loss markers defines, delivered in-order at the point in the sequence the gap occurred (or, for a resume-time gap, immediately after `hello-ack`).

**[EVT-095]** Either peer MAY send a `ping` frame `{type: "ping"}` on a connection otherwise idle for 30 seconds; the receiving peer MUST respond with `pong` within 10 seconds or the sender MUST treat the connection as dead and close it. A server closing a connection this way MUST use the `IDLE_TIMEOUT` code (Error taxonomy).

**[EVT-096]** A server-initiated close MUST carry a WS close reason naming one of this contract's error-taxonomy codes (Error taxonomy), so a client's reconnect logic can distinguish an authentication failure from a slow-consumer disconnect from an ordinary idle timeout.

### SSE binding

**[EVT-100]** An SSE request to `/events/v1` (`Accept: text/event-stream`, no `Upgrade` header) MUST respond `200` with `Content-Type: text/event-stream` and then stream events for the connection's lifetime; SSE offers no client-to-server frame after this initial request.

**[EVT-101]** `selector` and `schemas` (Scope-node filtering, Registered-schema catalog — general) MUST be accepted as query parameters on the initial SSE request, since SSE has no later opportunity to supply them.

**[EVT-102]** `resume_from` MUST be accepted as a query parameter on the initial SSE request. On any reconnect, a `Last-Event-ID` request header, when present, MUST take precedence over a `resume_from` query parameter — this is what lets a browser's native reconnect (which resends `Last-Event-ID` automatically from the last `id:` field it saw) resume correctly with no page-level bookkeeping.

**[EVT-103]** Every delivered durable event over SSE MUST be framed as an SSE event whose `event:` field is `event`, whose `id:` field is the envelope's `id`, and whose `data:` field is the envelope itself (Durable-event envelope), serialized as one JSON line.

**[EVT-104]** A loss signal over SSE MUST be framed as an SSE event whose `event:` field is `gap`, whose `id:` field is the gap's `to_id` (Loss markers — so a subsequent native reconnect's `Last-Event-ID` lands exactly at the resumed point), and whose `data:` field is the gap payload.

**[EVT-105]** The server MAY send an SSE `retry:` field to hint the client's native reconnect delay; this contract does not otherwise define SSE-level negotiation — connection setup that WS performs via `hello`/`hello-ack` (EVT-091–092) is, on SSE, entirely carried by the initial request's query parameters and the stream's first event.

### Authentication

**[EVT-110]** A WS upgrade request MUST be authenticated by exactly one of: the platform session cookie, carried automatically as an ordinary same-origin request header; or an `Authorization: Bearer <api-key>` header, for a non-browser client.

**[EVT-111]** An SSE request MUST be authenticated by exactly one of: the platform session cookie, for a browser's native `EventSource` (which cannot set custom headers); or an `Authorization: Bearer <api-key>` header, for a client library that supports one.

**[EVT-112]** An API key or session credential MUST NOT be accepted as a query-string parameter on either binding — a token that could ride a URL is a token that leaks into server access logs and intermediate proxies.

**[EVT-113]** A request that fails authentication on either binding MUST be rejected before any upgrade or stream begins, with an HTTP error response in `api/1`'s Problem shape (API-010) and the `AUTH_REQUIRED` code (Error taxonomy) — never with a WS/SSE-level frame, since no session has been established yet to frame one over.

**[EVT-114]** A session or API key credential's revocation MUST terminate every open events/1 connection authenticated by it within a bounded delay, not merely block future connections.

*draft-note: the exact bound in EVT-114 is not fixed by any normative source yet. Proposed: within 60 seconds of revocation taking effect.*

### Scope-node filtering

**[EVT-120]** A subscriber's default visible set — absent a narrowing `selector` — MUST be exactly the scope nodes its principal can read, computed the same way any other api/1-governed read is scoped; an event whose `scope_node` (Durable-event envelope) falls outside that set MUST NOT be delivered.

**[EVT-121]** A `hello`/query-parameter `selector`, when present, MUST use `api/1`'s label-selector grammar (API-040–046), including the scope-node subtree term (API-044), and MUST only narrow the default visible set (EVT-120) — it MUST NOT be able to widen delivery to a scope node the principal cannot otherwise read.

**[EVT-122]** A `selector` term that resolves, wholly or in part, outside the principal's readable scope-node set MUST simply match nothing for that term, exactly as an ordinary empty-result filter would — it MUST NOT be surfaced as an error. Treating an out-of-reach scope node as an error would let a selector probe for the existence of scope nodes the principal cannot read.

**[EVT-123]** Scope-node filtering (EVT-120–122) MUST be enforced server-side, per event, at delivery time — a subscriber's own claimed `selector` is never the sole boundary a server relies on.

**[EVT-124]** An optional `schemas` filter (Registered-schema catalog — general), when present in `hello` or as a query parameter, MUST restrict delivery to events whose `schema` is a member of the supplied list, applied as an additional restriction alongside scope-node filtering, never in place of it.

### Resume cursor

**[EVT-130]** `resume_from`, where supplied, MUST be a previously observed envelope `id` (Durable-event envelope) — the exact value of some earlier `event.id` this same principal received, not a server-minted opaque token.

**[EVT-131]** A `resume_from` value MUST match `^[A-Za-z0-9_-]+$` (`api/1` API-036) so it round-trips through a WS `hello` field or an SSE query parameter without extra escaping — this is the one property `resume_from` shares with `api/1`'s keyset-pagination cursor. It otherwise differs from that cursor: a `resume_from` value is a transparent event ID a client is expected to persist and reason about (e.g. compare, log), never an opaque token a client must treat as unparseable.

**[EVT-132]** Omitting `resume_from` MUST start delivery fresh — no backlog, only events recorded from connection time forward — and MUST report `resume_result: fresh` (EVT-092).

**[EVT-133]** A `resume_from` value still within the platform's retention window MUST resume delivery with every eligible event recorded after it, in `id` order, with neither a gap nor a duplicate, and MUST report `resume_result: resumed`.

**[EVT-134]** A `resume_from` value that is syntactically malformed (EVT-131), or that names an `id` the platform never recorded, MUST be rejected with `RESUME_FROM_INVALID` (Error taxonomy) before any event is delivered — it MUST NOT be treated as equivalent to an omitted `resume_from`.

**[EVT-135]** Delivery over this contract is at-least-once: a subscriber MAY observe the same `id` more than once across a reconnect boundary and MUST treat `id` as its deduplication key — this contract does not guarantee exactly-once delivery, only gap-free-or-marked delivery (Loss markers) and a stable, comparable ordering key.

### Loss markers

**[EVT-140]** A gap — whether at resume time or mid-stream — MUST be represented by one shape: `{from_id, to_id, reason}`, where `from_id` is the subscriber's own last-known point (the requested `resume_from`, or the last `id` successfully delivered before a mid-stream gap; `null` only when no such point exists), `to_id` is the `id` delivery resumes at, and `reason` is one of `retention_expired` or `buffer_exceeded`.

**[EVT-141]** `reason: retention_expired` MUST be used when a supplied `resume_from` is older than the platform's retention window — the requested point is no longer reconstructible, so `to_id` is the oldest `id` the platform can still deliver.

**[EVT-142]** `reason: buffer_exceeded` MUST be used when a connected subscriber falls far enough behind live delivery that the server drops undelivered events to catch it up rather than buffer them unboundedly. A server choosing between dropping-and-gapping a slow subscriber versus disconnecting it outright MUST gap (not disconnect) whenever it can still write to the connection; it MUST disconnect with `SLOW_CONSUMER_DISCONNECTED` (Error taxonomy) only once writes to that connection are themselves backed up past a bounded timeout.

**[EVT-143]** Silent loss is forbidden: a server MUST NOT drop any eligible event from a subscriber's stream without a corresponding `gap` covering it — every discontinuity a subscriber experiences is either absent (EVT-133) or explicitly marked (EVT-140–142), never simply missing.

**[EVT-144]** This gap shape — `{from_id, to_id, reason}` — is specific to a subscriber stream, where the envelope `id` (Durable-event envelope) is the natural bound. The platform applies the same underlying loss-marking pattern to its other buffered channels — a bounded range plus a reason, never a silent drop — but not always this exact shape: a relay's own offline telemetry-buffer queue, for instance, marks its overflow with `{from_seq, to_seq, dropped_counts_by_schema, reason}`, a marker defined by `relay/1` that shares this shape's from/to/reason spine but is extended with per-schema drop counts and sequence-based, not `id`-based, bounds. A subscriber or operator MUST NOT assume a different buffered channel's own loss marker is this same three-field shape — only the bounded-range-plus-reason pattern is common to both.

*draft-note: EVT-142's exceeded-writes timeout bound is not fixed by any normative source yet. Proposed: 30 seconds of persistently backed-up writes before disconnecting.*

### Webhook delivery

**[EVT-150]** This section governs outbound delivery only — the platform pushing a durable event to an operator-registered external URL. It is unrelated to inbound webhook or ingest intake, a distinctly authenticated surface (its own route family and a signed-request or ingest-token scheme) that this contract does not define. A webhook endpoint's own registration (URL, subscribed schemas, scope-node selector) is a management-plane resource this contract does not define either; this section governs delivery mechanics given a registered endpoint.

**[EVT-151]** Every delivery attempt MUST be an HTTP `POST` to the endpoint's registered URL with the envelope (Durable-event envelope) as the JSON body, and MUST carry three headers: `X-Waiveo-Delivery-Id` (a ULID identifying this logical delivery, stable across its own retries), `X-Waiveo-Timestamp` (the delivery attempt's Unix timestamp in seconds), and `X-Waiveo-Signature` (an HMAC-SHA256, keyed by the endpoint's own signing secret, computed over the ASCII string `<X-Waiveo-Timestamp>.<raw JSON body>`).

**[EVT-152]** A receiver SHOULD reject a delivery whose `X-Waiveo-Timestamp` is further from the receiver's own clock than a stated replay window, and SHOULD deduplicate by `X-Waiveo-Delivery-Id` across retries of the same logical delivery — both are receiver-side implementation guidance, not enforced by the sender.

*draft-note: EVT-152's replay window is not fixed by any normative source yet. Proposed: 5 minutes.*

**[EVT-153]** A delivery attempt that does not receive a `2xx` response within a bounded request timeout MUST be retried with exponential backoff, capped, up to a bounded maximum number of attempts.

*draft-note: EVT-153's exact numbers are not fixed by any normative source yet. Proposed: request timeout 10 seconds; backoff starting at 30 seconds, doubling, capped at 1 hour; give up after 15 attempts spread over roughly 24 hours.*

**[EVT-154]** An endpoint that exhausts EVT-153's retry budget for **consecutive** deliveries past a bounded failure count MUST be transitioned to `disabled` and MUST raise an operator-facing signal; a `disabled` endpoint receives no further delivery attempts until an operator re-enables it.

*draft-note: EVT-154's consecutive-failure count is not fixed by any normative source yet. Proposed: 10 consecutive fully-exhausted deliveries.*

**[EVT-155]** An endpoint's own delivery progress MUST be tracked the same way a WS/SSE subscription tracks its own (Resume cursor): as a `last_delivered_id`. Re-enabling a `disabled` endpoint, or resuming an endpoint recovering from transient failures, MUST resume delivery from `last_delivered_id` under the exact same retention-window gap behavior (Loss markers) a WS/SSE reconnect uses — a webhook endpoint is, from the delivery log's perspective, just another subscriber.

**[EVT-156]** Webhook delivery is at-least-once (EVT-135 applies identically): a receiver MUST be able to rely on `X-Waiveo-Delivery-Id` for its own idempotent handling of a redelivered event.

**[EVT-157]** Deliveries to one endpoint MUST be attempted in `id` order, and a later event's delivery MUST NOT be attempted ahead of an earlier one still within its own retry budget — an endpoint observes the same monotonic ordering a WS/SSE subscriber does.

**[EVT-158]** An endpoint's signing secret MUST be rotatable without a delivery gap: the platform MUST accept a signature computed under either the current or the immediately prior secret for a stated overlap window after rotation, so a receiver has time to adopt the new secret before the old one stops working.

*draft-note: EVT-158's overlap window is not fixed by any normative source yet. Proposed: 24 hours.*

## Wire shapes

```json
// Durable-event envelope — entity.state_changed
{
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y7",
  "schema": "entity.state_changed",
  "ts": 1752537600000,
  "scope_node": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y7",
  "cost_class": "telemetry",
  "retention_class": "telemetry-standard",
  "origin": "relay",
  "origin_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2Y8",
  "payload": {
    "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y9",
    "device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
    "old_state": "idle",
    "new_state": "playing",
    "attribute_change": true,
    "attributes_delta": {
      "active_app_id": { "old": null, "new": "slidecast" }
    }
  }
}
```

```json
// Durable-event envelope — automation.run
{
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2YB",
  "schema": "automation.run",
  "ts": 1752537601000,
  "scope_node": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YB",
  "cost_class": "automation",
  "retention_class": "telemetry-standard",
  "origin": "relay",
  "payload": {
    "rule_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YC",
    "rule_revision": 4,
    "trigger_snapshot": { "kind": "state", "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y9", "to": "playing" },
    "condition_results": [{ "kind": "state", "passed": true }],
    "action_outcomes": [{ "kind": "device-command", "status": "ok" }],
    "mode_disposition": "ran",
    "misfire_caught": false
  }
}
```

```json
// Durable-event envelope — audit.event
{
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2YD",
  "schema": "audit.event",
  "ts": 1752537602000,
  "scope_node": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YD",
  "cost_class": "audit",
  "retention_class": "audit-long",
  "origin": "internal",
  "payload": {
    "actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
    "on_behalf_of": null,
    "action": "login.failure",
    "target": "principal:01J8Z3K4N5P6Q7R8S9T0V1W2YE",
    "result": "failure"
  }
}
```

```json
// WS: client hello (frame zero)
{ "type": "hello", "resume_from": "01J8Z3K4N5P6Q7R8S9T0V1W2Y6", "schemas": ["entity.state_changed", "automation.run"] }
```

```json
// WS: server hello-ack, then a gap (the requested resume_from had already aged out of retention)
{ "type": "hello-ack", "resume_result": "gap" }
```

```json
// WS: the gap frame following the hello-ack above
{ "type": "gap", "from_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y6", "to_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "reason": "retention_expired" }
```

```
# SSE: an event frame followed by a gap frame
event: event
id: 01J8Z3K4N5P6Q7R8S9T0V1W2Y7
data: {"id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y7","schema":"box.vitals", "...":"..."}

event: gap
id: 01J8Z3K4N5P6Q7R8S9T0V1W2Y9
data: {"from_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y7","to_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y9","reason":"buffer_exceeded"}

```

```http
# Outbound webhook delivery
POST /operator-endpoint HTTP/1.1
Host: hooks.example.com
Content-Type: application/json
X-Waiveo-Delivery-Id: 01J8Z3K4N5P6Q7R8S9T0V1W2YF
X-Waiveo-Timestamp: 1752537603
X-Waiveo-Signature: 5f6e...a1b2

{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2YB","schema":"automation.run","...":"..."}
```

## Negotiation

- **Version selection** — a client selects the major version by the path it calls (`/events/v1`, EVT-001); there is no header-based version negotiation, matching `api/1`'s own approach.
- **Connection setup** — WS performs a lightweight `hello`/`hello-ack` exchange (EVT-091–092) as the first frames on every connection; SSE carries the same information (selector, schemas, resume point) on its one initial request (EVT-101–102), since it offers no later client-to-server frame.
- **Minor-version skew** — within major version 1, a new optional envelope field or a new registered schema is additive (EVT-003); a subscriber built against an older minor continues to receive events it understands and MAY ignore a `schema` or envelope field it does not recognize.
- **Resume across a reconnect** — `resume_from` (Resume cursor) is the sole continuity mechanism across a dropped connection; there is no separate session-resumption token distinct from the last observed event ID.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `AUTH_REQUIRED` | No valid session or API key principal was presented on the WS upgrade or SSE request. | yes — after re-authenticating |
| `SELECTOR_INVALID` | The supplied `selector` failed to parse under `api/1`'s label-selector grammar. | yes — retry with a corrected selector |
| `RESUME_FROM_INVALID` | The supplied `resume_from` was malformed or names an `id` the platform never recorded. | yes — retry without `resume_from`, or with a previously observed `id` |
| `SLOW_CONSUMER_DISCONNECTED` | The connection's writes stayed backed up past the bounded timeout after a buffer-exceeded gap. | yes — reconnect with `resume_from` |
| `WEBHOOK_ENDPOINT_DISABLED` | A delivery was attempted against an endpoint already auto-disabled after exhausting its consecutive-failure budget. | no — an operator must re-enable the endpoint |
| `IDLE_TIMEOUT` | A WS connection missed a `ping`/`pong` round trip and was closed as dead (EVT-095). | yes — reconnect with `resume_from` |
| `INTERNAL` | An unclassified server-side failure. | yes — with backoff |
| `UNAVAILABLE` | The server or a dependency it needs is temporarily unable to serve the request. | yes — with backoff |

## Conformance notes

- Traceability map: `conformance/traceability/events-1.md` — maps every `EVT-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/events-1/` — one JSON case file per `case-id` referenced from the traceability map.
- The registered-schema catalog's emission cadence for `device.heartbeat` and `box.vitals` (EVT-060, EVT-070 draft-notes), the webhook retry/backoff numbers (EVT-153–154, EVT-158), and the slow-consumer and revocation timeouts (EVT-114, EVT-142) are all draft-note proposals pending real measurement; corpus cases exercise the shapes and orderings these rules produce, not elapsed real time — timing behavior is exercised against an injectable clock in a driver harness, not wall-clock sleeps in a static corpus.
- The roles/scopes model that determines a principal's readable scope-node set (Scope-node filtering) is out of this contract's scope and is not exercised by this corpus; cases that need one treat a principal's readable set as a given, opaque input.
- Webhook endpoint registration (URL, secret provisioning, subscribed-schema selection) is a management-plane resource this contract does not define; corpus cases exercise delivery mechanics against an already-registered endpoint, taken as a given input.
