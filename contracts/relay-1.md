# Relay Protocol

**Contract:** relay/1
**Version:** 1.0
**Status:** draft

## Scope

relay/1 defines the protocol between an enrolled relay and its app peer: connection-time identity and version negotiation, per-relay certificate enrollment, the pull of generation-numbered desired state, the upstream delivery of durable telemetry, the device plane's candidate-record and command surface, the relay's authority to issue player-facing credentials on pairing-grant redemption, and the clock-trust exchange that lets time-based evaluation survive an unattested clock. A relay is a LAN gateway between its app peer and the devices/screens on its own LAN — it carries control, authentication, automation, and program references; it is never a store, and never a path content bytes travel through (Gateway posture).

- In scope: relay enrollment (certificate issuance, in-band renewal, the hardened expired-certificate re-enrollment path, relay identity); the hello/negotiate handshake (protocol-version negotiation, capability flags, the channel-binding signature, site binding, subnet metadata, clock-state exchange); the desired-state pull protocol (generation numbering and hashing, idempotent apply, enrollment-anchored trust, the consolidated snapshot's sections including the reserved workflow-generation slot); the telemetry-upstream channel (batch upload, cursor acknowledgment, the loss marker, durable-class vs. latest-only delivery, which registered schemas ride this channel); the device plane's candidate-record and device-command surface; the inputs the relay's player-credential authority acts on (pending pairing grants and revocation state riding desired state), the pairing code it displays for grant redemption, and the reserved reverse-claim identity fields; the clock-trust exchange (the persisted floor, the bounded time hint, the hardened re-enrollment path); multi-relay identity disambiguation.
- Out of scope: the compiled edge-rule vocabulary and evaluation semantics, including a compiled generation's own internal rule shape (`rules/1`); the device-class registry's state/attribute/command vocabulary (`device-class-registry/1`); the pack manifest fields a device or automation contribution is declared with (`manifest/1`); the player/1 handshake, playback lease, and content-fetch mechanics, and the wire messages a relay uses to issue a player its certificate and channel token — a screen fetches content bytes directly from its content origin, never through a relay/1 message, and player-facing credential issuance is a distinct contract this one only supplies inputs to (`player/1`); the registered telemetry schemas' own payload shapes — this contract states which schemas ride the telemetry-upstream channel and how, `events/1` defines their fields; asset storage, caching, or any storage-capacity negotiation for content — a relay holds no asset store to negotiate over (Gateway posture); the reverse-claim flow's own onboarding UX, claim-code redemption, and the appliance that carries it out — this contract reserves only the identity fields that keep that flow additive (Enrollment); the platform's scheduling/dayparting data model's own row shapes (a separate concern; this contract carries that data as an opaque desired-state section, never redefines its fields).

## Definitions

- **ULID** — as defined in `manifest/1`: a 26-character Crockford-base32, time-sortable identifier.
- **Timestamp** — as defined in `rules/1`: an integer number of milliseconds since the Unix epoch (UTC).
- **Relay** — the enrolled LAN-gateway peer this contract's protocol runs between. A relay serves exactly one site.
- **App peer** — the party a relay enrolls against, pulls desired state from, and pushes telemetry to; this contract treats it as the fixed remote end of every message it defines. A relay is always the connecting party; the app peer never dials a relay.
- **Site** — the scope node a relay is bound to at enrollment; a relay's `hello` carries that site's effective identity data (Hello / negotiate).
- **Screen** — a scope-node-attached display this contract's `screen_programs` section assigns a compiled program to. A screen's own runtime behavior, pairing, and content-fetch mechanics are `player/1`'s concern, out of scope here.
- **Entity** — as defined in `rules/1`: a device-plane object exposing a canonical state string and typed attributes, addressed by `entity_id`.
- **Device** — a physical or virtual thing a relay controls, identified by the tuple `(site, driver, native_id)` (Desired-state snapshot sections); a device exposes one or more Entities.
- **Generation** — as defined in `rules/1`: a versioned, hash-identified snapshot of compiled edge rules and other desired state. `rules/1` defers this snapshot's own transport, signing, and pull mechanics to this contract.
- **Desired state** — the complete content of one generation, as the consolidated snapshot shape below enumerates it (Desired-state snapshot sections).
- **Enrollment** — the process by which a relay obtains its per-relay certificate and is bound to a site (Enrollment).
- **Proof-of-possession signature** — a signature computed with a certificate's own private key over a value binding a fresh, single-use challenge nonce to a specific request, proving the signer holds that certificate's private key rather than merely having presented or observed the certificate's own public serial (Expired-certificate re-enrollment).
- **Channel-binding signature** — a signature the relay computes over an app-peer-supplied value at connection time, proving peer identity at the application layer independent of the transport's own mutual-TLS handshake (Hello / negotiate).
- **Pairing grant** — a short-lived, redemption-scoped authorization a relay redeems on a screen's behalf (Player-credential authority); minted by the platform's own grant mechanism, out of this contract's scope beyond the record shape it rides in desired state.
- **Candidate record** — a discovered-but-not-yet-adopted device observation the relay reports upstream (Device plane).
- **Clock trust state** — `trusted` or `untrusted`: the relay's own local attestation of whether its current time is verified. `rules/1` (RUL-370) depends on this contract's definition of the attestation mechanism.
- **Clock floor** — the relay's persisted, monotonically non-decreasing best-known-time lower bound (Clock trust).
- **Loss marker** — the bounded-range-plus-reason shape the telemetry-upstream channel uses to mark an unrecoverable gap in delivery (Loss marker); `events/1` EVT-144 references this contract's own shape by name.

## Normative requirements

### Versioning & transport surface

**[REL-001]** relay/1 MUST be reachable at a stable path on the app peer, upgraded to a persistent bidirectional connection; a relay is always the connecting party, opening a fresh connection after every disconnect — the app peer never dials out to a relay.

**[REL-002]** Every relay/1 message, in every connection state (the pre-enrollment bootstrap exchange or a post-enrollment connection), MUST be UTF-8 JSON: one message per underlying connection frame.

**[REL-003]** Once enrolled, a relay's connection to its app peer MUST be mutually authenticated: opened over TLS, both peers presenting and validating a certificate against a trust anchor established at enrollment (Enrollment). A relay and app peer running on the same host MAY connect over a loopback address; otherwise the relay dials outbound to the address it learned at enrollment (Enrollment).

**[REL-004]** Within major version 1, evolution MUST be additive only: a new optional message field, a new capability flag, or a new desired-state snapshot section MAY be introduced in a minor. An existing message field's meaning, an existing snapshot section's already-published shape, or the connection path (REL-001) MUST NOT change within major version 1.

**[REL-005]** Every relay/1 message exchanged after enrollment completes MUST carry `relay_id`. The sole exception is the enrollment exchange itself (Enrollment), where `relay_id` does not yet exist to carry.

**[REL-006]** A request/response-shaped relay/1 message pair MUST share one correlation `id`. Where a request represents work traceable to a single originating operation elsewhere in the platform — a device command dispatched from an operator action, for instance — it MUST carry that operation's own `trace_id`, so one identifier correlates the operation across the app peer, the relay, and any durable record the operation eventually produces.

**[REL-007]** A typed refusal this contract's Error taxonomy defines, when not already carried as the `error` field of an existing ack message (e.g. `state.ack`, `device.command_result`), MUST be sent as a top-level error frame: `{type: "error", id, trace_id, code, message}` — `id` the correlation id of the request being refused, when the refused exchange has one; `trace_id` that request's own `trace_id`, when it carried one (REL-006); `code` and `message` exactly as the Error taxonomy entry being raised defines. This shape is aligned with `ctx/1`'s own frame envelope (`ctx/1` CTX-003).

### Enrollment

**[REL-010]** A relay holding no valid certificate for its app peer MUST reach enrollment only through a distinct bootstrap exchange authenticated by possession of a claim credential (REL-011) — server-authenticated TLS, since the relay does not yet hold a client certificate to authenticate itself with. This is the sole exchange in this contract that proceeds without an already-enrolled identity.

**[REL-011]** A claim credential MUST carry at least `claim_token`. When the relay and its app peer are not co-located, the claim credential MUST additionally carry `app_endpoint` and `trust_pin` — the address the relay dials and the server-certificate trust datum it validates that address against before completing enrollment. A co-located (loopback) deployment MAY leave both implicit. This contract inherits, rather than itself secures, the trust of whatever out-of-band channel delivers a claim credential's `claim_token` and `trust_pin` to a relay ahead of enrollment (REL-010) — a claim credential intercepted or substituted in transit before it reaches the relay is indistinguishable, from this contract's own bootstrap exchange, from a legitimately delivered one.

**[REL-012]** The enrollment request MUST carry `{claim_token, csr}`, where `csr` is a certificate signing request over a keypair the relay generates and retains the private half of. On success, the app peer's response MUST carry `{relay_id, cert, not_before, not_after, desired_state_verification_key}` — `relay_id` is this relay's permanent identity going forward, `cert` is the issued per-relay certificate, and `desired_state_verification_key` is the app peer's own public key for verifying every subsequent desired-state snapshot (Idempotent apply & enrollment-anchored trust).

**[REL-013]** A `claim_token` MUST be single-use: a second enrollment attempt presenting an already-redeemed token MUST be refused with a typed error (Error taxonomy), never silently accepted as a second enrollment of the same relay. Issuance, scope, and TTL of a claim token are governed by the platform's own grant mechanism, outside this contract.

**[REL-014]** `relay_id`, once assigned by a successful enrollment (REL-012), MUST persist across every subsequent in-band certificate renewal (REL-015) and every expired-certificate re-enrollment (Expired-certificate re-enrollment) under that same enrollment relationship. `relay_id` MUST NOT be derived or recomputed from a certificate's own serial number — a renewal or re-enrollment issues a new serial under the same, unchanged `relay_id`.

**[REL-015]** An enrolled relay MUST be able to renew its certificate in-band, over its existing authenticated connection, ahead of expiry and without presenting a fresh claim credential: `{csr}` in, `{cert, not_before, not_after}` out, under the same `relay_id` (REL-014).

**[REL-016]** The app peer MUST check a presented certificate's revocation status at every connection attempt, not only at the time of its own issuance; a revoked certificate MUST be refused (`CERT_REVOKED`, Error taxonomy) regardless of whether it otherwise remains within its validity window.

**[REL-017]** A relay re-pointed to a different app peer — or completing an expired-certificate re-enrollment (Expired-certificate re-enrollment) — MUST treat that event as a fresh enrollment relationship for trust purposes: it discards and replaces its persisted `desired_state_verification_key` (REL-012) with the one delivered by the new enrollment response, and MUST NOT continue verifying subsequent snapshots against the key from before that event.

**[REL-018]** Re-pointing an already-adopted relay to a different app peer MUST assign a fresh `relay_id` under that new peer and MUST restart generation numbering (Desired-state pull) from that peer's own baseline — a relay's `relay_id` and generation sequence are scoped to one enrollment relationship, never carried across to a new one.

**[REL-019]** A relay's principal MAY additionally carry a second, factory-embedded credential distinct from its per-deployment enrollment certificate (REL-012), reserved for a claim flow in which the relay dials an app peer before being bound to any site. This contract reserves the field on the relay's identity and a distinct unclaimed connection state for it, but does not define the claim-code redemption flow, the console or display surface that carries a code between a relay and its owner, or the appliance that executes this flow (Scope) — a relay implementing only REL-010–018's claim-token path remains fully conformant.

### Expired-certificate re-enrollment

**[REL-020]** A relay presenting a certificate that has expired, but was never revoked, for a known `relay_id` MAY re-enroll without a fresh claim credential (REL-011), provided REL-021, REL-022, and this path's proof-of-possession requirement (REL-026–029) all hold. Completion of this path MAY proceed without operator interaction, subject to a deployment-configured policy governing whether automatic completion is permitted for a given relay.

**[REL-021]** The app peer MUST evaluate eligibility for this path by checking the presented certificate's serial against its own issuance record for that `relay_id` — never against a revocation list, from which an old-enough revoked entry may already have aged out. The presented certificate MUST be the most-recently-issued certificate on record for that `relay_id`.

**[REL-022]** A presented expired certificate that is superseded (a newer certificate has since been issued for that `relay_id`) or that was revoked MUST be refused outright (`CERT_EXPIRED_INELIGIBLE`, Error taxonomy) and MUST NOT be granted re-enrollment through this path — a superseded-or-revoked certificate presented this way is a credential-integrity signal worth surfacing operator-side; refusal here MUST raise a typed operator-facing condition through the platform's own alerting mechanism, outside this contract's own scope.

**[REL-023]** This path MUST succeed without requiring the relay's own clock to be trusted (Clock trust): both REL-021's "most-recently-issued" check and the certificate's own expired status are evaluated using the app peer's trusted time and issuance record, never anything the relay's own untrusted clock asserts.

**[REL-024]** A successful re-enrollment through this path MUST issue a fresh certificate under the same `relay_id` (REL-014) and MUST re-anchor the relay's persisted desired-state verification key (REL-017) exactly as an ordinary re-enrollment does.

**[REL-025]** This path MUST be rate-limited per `relay_id` (`RE_ENROLL_RATE_LIMITED`, Error taxonomy once exceeded) — bounding how often it may be exercised within a given window — so it cannot be used to force repeated identity churn against a single relay identity.

**[REL-026]** Before granting re-enrollment through this path, the app peer's bootstrap listener MUST issue a fresh, single-use challenge nonce of at least 128 bits, unique to that connection attempt — the same `challenge` message shape REL-030 defines for a post-enrollment connection, sent instead over this path's bootstrap exchange (REL-010).

**[REL-027]** The `renew` request presented through this path MUST carry `pop_signature`: proof that the relay holds the private key of the certificate it is presenting for re-enrollment (REL-021), computed with that certificate's own private key — never the fresh keypair `csr` was generated from — over a value that binds together the challenge nonce (REL-026) and that same request's own `csr`, so a captured signature cannot be replayed against a substituted CSR.

**[REL-028]** The app peer MUST verify `pop_signature` against the public key on record for the presented certificate's serial in its own issuance record (REL-021) before completing re-enrollment through this path. A presented serial that matches the issuance record is necessary but never sufficient by itself: REL-021's serial check and this signature verification MUST both pass.

**[REL-029]** A re-enrollment attempt through this path whose `pop_signature` is absent, malformed, or fails REL-028's verification MUST be refused with a typed error (`RE_ENROLL_POP_INVALID`, Error taxonomy) in place of `renew-ack` — regardless of whether REL-021 and REL-022's own checks would otherwise have passed.

### Hello / negotiate

**[REL-030]** Immediately upon opening an authenticated post-enrollment connection (REL-003), before either peer sends any other message, the app peer MUST send a `challenge` message carrying a fresh, single-use nonce of at least 128 bits, unique to that connection attempt.

**[REL-031]** The relay's `hello` — the first message it sends on that connection — MUST carry `{relay_id, protocol_version, features, site_binding, subnet_metadata, clock_state, channel_binding_signature}`.

**[REL-032]** `channel_binding_signature` MUST be a signature, computed with the relay's own enrollment private key (Enrollment), over the nonce the app peer supplied in `challenge` (REL-030). The app peer MUST verify this signature against the relay's enrollment-learned public key before accepting `hello`, and MUST refuse the connection (`CHANNEL_BINDING_INVALID`, Error taxonomy) on verification failure — regardless of whether the connection's own mutual-TLS handshake already succeeded. Transport-level authentication alone MUST NOT be treated as sufficient proof of peer identity.

**[REL-033]** `protocol_version` MUST be a `major.minor` string. If the app peer implements no minor of the relay's declared major, this is a **major mismatch**: `hello` MUST be refused with a typed error (`PROTOCOL_VERSION_UNSUPPORTED`, Error taxonomy) in place of `hello-ack`, and the connection closed. Otherwise, `hello-ack`'s `negotiated_version` MUST be the highest minor the app peer implements that is `<=` the relay's declared minor.

**[REL-034]** An app peer implementing relay/1 minor `M` MUST also implement minor `M-1` of the same major for exactly this negotiation (N−1 compatibility) — so a relay running one minor behind its app peer's current always finds a satisfying `negotiated_version` under REL-033, never a major-mismatch refusal solely for lagging by one minor.

**[REL-035]** `features` MUST be an array of capability-flag strings the relay supports. The app peer's `hello-ack` MUST report `features` as the subset it also supports; a flag the app peer does not recognize MUST simply be excluded from that subset, never cause `hello` to be refused.

**[REL-036]** `site_binding` MUST carry at least `{scope_node, tz, lat, long}` — the site this relay is bound to, and that site's effective timezone and coordinates, evaluated exactly as `rules/1`'s time-based and sun-based triggers and conditions require. The app peer's `hello-ack` reports its own current record of these values; a relay MUST treat `hello-ack`'s copy as authoritative going forward for this connection, updating anything it had cached from a prior connection or from desired state (Desired-state snapshot sections).

**[REL-037]** `subnet_metadata` MUST carry at least the relay's own canonical advertised address — the same address it advertises in its own discovery/pairing responses. A relay MUST send a fresh `hello` on its next connection after this address changes, so the app peer's own records never silently drift from a multi-homed relay's actual current address.

**[REL-038]** `clock_state` MUST carry `{state, source}` where `state` is `trusted` or `untrusted` (Clock trust) and `source` is an implementation-defined short string naming the relay's current time source; the app peer MUST accept an unrecognized `source` value as forward-compatible data, never a validation failure.

**[REL-039]** The app peer's `hello-ack` MUST carry `{relay_id, negotiated_version, features, site_binding, deprecated}`, where `deprecated` is a map of message-type name to `{deprecated_in, removed_in, message}` for every message type in the negotiated version currently deprecated (possibly empty). Neither peer may send any message other than `challenge`/`hello`/`hello-ack` before this exchange completes; a peer that does MUST be treated as a protocol violation (`PROTOCOL_VIOLATION`, Error taxonomy) and the connection closed.

**[REL-040]** The challenge nonce REL-030 requires MUST be derived from the TLS exporter keying material of that specific connection (RFC 5705; the TLS 1.3 exporter-derived channel-binding construction, RFC 9266) rather than an application-level value chosen independently of the TLS session. A plain per-connection random value not derived this way MUST NOT be used to satisfy REL-030: deriving the nonce from the exporter is what lets `channel_binding_signature` (REL-032) cryptographically bind to the exact TLS channel `hello` arrives on, rather than to a value that merely rode over it, which a TLS-terminating intermediary could otherwise relay unchanged.

**[REL-041]** The app peer MUST look up the enrollment-learned public key it verifies `channel_binding_signature` against (REL-032) by the connection's own mTLS-authenticated client-certificate identity (REL-003) — never by the self-asserted `hello.relay_id` (REL-031). If the connection's mTLS-authenticated identity and `hello.relay_id` do not name the same relay, the app peer MUST refuse the connection with a typed error (`RELAY_IDENTITY_MISMATCH`, Error taxonomy) rather than proceed using either identity alone.

### Desired-state pull

**[REL-050]** Desired state moves downstream by pull only: the relay requests it with a `state.pull` message; the app peer never sends an unsolicited snapshot. `state.pull` MAY carry `since_generation` — the relay's own last-applied generation number — letting the app peer answer efficiently when nothing has changed.

**[REL-051]** The app peer's response to `state.pull` MUST be exactly one of: `state.unchanged {generation}`, when `since_generation` already names the current generation; or `state.snapshot {generation, hash, signature, sections}` (Desired-state snapshot sections; `signature`'s signed scope is REL-075), otherwise. `state.snapshot` MAY additionally carry `signed_with_key`, identifying which key the signature was computed with, for diagnostic purposes only (REL-075).

**[REL-052]** `generation` MUST be a per-relay monotonically increasing integer, assigned by the app peer. A relay MUST NOT accept a `state.snapshot` whose `generation` is lower than its own last-applied generation. A `generation` equal to its own last-applied generation — a redelivery of an already-applied snapshot, for instance after a lost acknowledgment — MUST be accepted and handled under REL-070's no-op rule, exactly as a higher `generation` carrying identical `sections` content is.

**[REL-053]** `hash` MUST be computed over the complete, canonicalized content of `sections` (Desired-state snapshot sections), such that two snapshots with byte-identical `sections` content produce the same `hash` regardless of `generation` number, and any difference in `sections` content produces a different `hash`.

**[REL-054]** After applying a snapshot, the relay MUST acknowledge with `state.ack {applied_generation, error, divergence_reason}`, where `error` and `divergence_reason` are present only when the apply did not fully succeed (Error taxonomy for `error`; `divergence_reason` is an implementation-defined short string describing a partial or inconsistent apply outside this contract's own error vocabulary).

**[REL-055]** The relay MUST persist `{generation, hash}` for its last successfully applied snapshot to durable local storage, so that on restart it resumes evaluating its last-known desired state without first contacting its app peer (Idempotent apply & enrollment-anchored trust).

**[REL-056]** A generation swap MUST be applied atomically from the perspective of anything evaluating against it: an evaluation in progress at the moment of a swap completes entirely against either the prior generation or the new one, never a mix of fields from each.

### Desired-state snapshot sections

**[REL-060]** `sections` MUST be an object carrying exactly the following keys, every one of them present in every snapshot (an empty array or an explicit empty placeholder where a site currently has nothing to populate a section with, never an omitted key): `screen_programs`, `edge_rules`, `device_inventory`, `schedule`, `revocation_and_site`, `pairing_grants`, `workflow_generation`.

**[REL-061]** `screen_programs` MUST be an array of `{screen_id, program_revision, priority, display, content}` — `priority` one of `scheduled` or `preempt`, mirroring `player/1`'s own Lease `priority` field (`player/1` PLY-100); `display` one of `content` or `blank`, mirroring `player/1`'s own Lease `display` field (`player/1` PLY-093) — and `content` an array of signed content references `{asset_ref, url, expires_at}`, `asset_ref` a content-addressed `sha256:` URI in the same form `ctx/1`'s `assets` family uses. This contract carries `content`, `priority`, and `display` alike opaquely from app peer to relay to screen; it does not define how a screen resolves or fetches content, or adopts a priority class or display state, itself (`player/1`, Scope). Carrying `priority` here is what lets a `preempt`-priority assignment reach a relay's screen through its own offline-cached last-applied snapshot (Idempotent apply & enrollment-anchored trust) without requiring the relay's app-peer connection to be live at the moment a screen needs it; carrying `display` here is what lets a `blank` assignment reach a relay's screen the same way — riding the same offline-cached snapshot, so a blank assignment survives a WAN outage exactly as a preemption does.

**[REL-062]** `edge_rules` MUST be `{rules_minor_version, rules}` — `rules_minor_version` a `major.minor` string (mirroring REL-033's `protocol_version` format) naming the `rules/1` minor this generation was compiled against (`rules/1` Negotiation), `rules` an array of `rules/1` CompiledRuleEntry objects (`rules/1` Wire shapes), unmodified and unreinterpreted by this contract. A relay presented a generation whose `rules_minor_version` major component names a `rules/1` major it does not implement MUST refuse to apply that generation (`RULES_MAJOR_UNSUPPORTED`, Error taxonomy) rather than evaluate it under a mismatched vocabulary — the comparison is against the major component only, exactly as REL-033's own protocol-version negotiation compares majors before minors; the refusal `rules/1`'s own Negotiation section requires an executor to make, carried out here at this contract's transport layer.

**[REL-063]** `device_inventory` MUST be an object `{devices, pack_match_patterns}`. `devices` MUST be an array of adopted-device entries, each `{device_id, driver, native_id, poll_cadence_seconds, entities}`, where `entities` is an array of `{entity_id, device_class, enabled, hidden, display_name, category}` — `category` one of `primary` or `diagnostic`. A `device_id`'s identity is the tuple `(site, driver, native_id)`: re-adopting the same physical device for the same site under a different relay MUST resolve to the same `device_id`.

**[REL-064]** `pack_match_patterns` MUST be an array of the discovery-match patterns declared by every currently installed pack's device contribution (`manifest/1` MAN-070/071) — the patterns a relay watches for during discovery independent of what is already adopted.

**[REL-065]** `schedule` MUST carry the platform's scheduling-core data (playlist, daypart, validity-window, fallback, preset-batch, and display-power rows) for this site, keyed by scope node. This contract requires only that the section is present and travels under the same generation/hash/idempotent-apply rules as every other section; the row shapes themselves are that scheduling core's own content, not redefined here.

**[REL-066]** `revocation_and_site` MUST carry `{revoked, site_effective}`: `revoked` an array of opaque identifier strings the relay's player-credential authority (Player-credential authority) and connection-acceptance logic MUST treat as revoked even while disconnected from its app peer; `site_effective` a persisted copy of this site's `{tz, lat, long}` (mirroring `hello`'s `site_binding`, REL-036) so a relay's dayparting and sun/time evaluation remain correct across a restart without first completing a fresh `hello`.

**[REL-067]** `pairing_grants` MUST be an array of pending pairing-grant records (Player-credential authority) — grants minted against this site whose redemption the relay is authoritative for while connected or not.

**[REL-068]** `workflow_generation` is RESERVED: every snapshot MUST carry this key with an empty placeholder value in this version, and a relay MUST accept and structurally ignore its content without requiring any specific semantics from it. Its content shape is reserved for a future relay/1 minor and is not otherwise defined by this version.

### Idempotent apply & enrollment-anchored trust

**[REL-070]** A relay applying a `state.snapshot` whose `hash` equals its own persisted last-applied `hash` (REL-055) MUST treat the apply as a no-op: it MUST NOT re-run any apply-time side effect, and per `rules/1` RUL-381's own "changed" test, no in-flight rule run is canceled on account of it — this holds regardless of whether `generation` itself advanced.

**[REL-071]** Before applying any section of a `state.snapshot`, the relay MUST verify the snapshot's signature against its persisted `desired_state_verification_key` (Enrollment) — never against the platform's software-artifact trust bundle, an unrelated trust root this contract does not consume.

**[REL-072]** A snapshot that fails REL-071's verification MUST be rejected outright (`SNAPSHOT_SIGNATURE_INVALID`, Error taxonomy): no section is applied, partially or wholly, and the relay continues operating under its own last-applied generation. The relay's `state.ack` for a rejected snapshot MUST report `error` rather than an advanced `applied_generation`.

**[REL-073]** The relay's persisted `desired_state_verification_key` MUST be stored beside its persisted `{generation, hash}` (REL-055), so that both survive a power cycle and remain usable for offline verification without contacting the app peer.

**[REL-074]** A relay MUST discard its previously persisted `desired_state_verification_key` and adopt the one delivered by a fresh enrollment response (REL-012) whenever re-enrollment occurs (REL-017) — a snapshot signed under a key from before that re-enrollment MUST NOT verify against the newly persisted key and MUST be rejected under REL-072.

**[REL-075]** `state.snapshot`'s `signature` field MUST be present on every snapshot and MUST be computed over a canonicalization that includes `generation` together with `hash` — `{generation, hash, ...}`, never `hash` alone — so that a snapshot's signature is bound to the specific generation number it was issued under: relabeling an old, validly-signed snapshot under a higher `generation` changes the signed content and fails REL-071's verification. `signed_with_key`, when present, identifies which key the app peer used only for diagnostic purposes; it MUST NOT be treated as authoritative in place of REL-071's own verification against the relay's persisted `desired_state_verification_key`.

### Telemetry upstream

**[REL-090]** The relay MUST buffer telemetry entries durably while disconnected from its app peer, and upload buffered entries in batches once connected: `telemetry.push {entries, loss_markers}`, where each entry is `{seq, schema, payload}` — `schema` one of the registered `events/1` schemas this section names (REL-095), `payload` that schema's own field shape, unmodified — and `loss_markers` is an array of Loss marker records (Loss marker), present (possibly empty) on every push.

**[REL-091]** `seq` MUST be a per-relay monotonically increasing integer, assigned by the relay at the moment it records the entry — distinct from, and never reset by, a reconnect, an app-peer restart, or a generation swap.

**[REL-092]** The app peer MUST acknowledge receipt with `telemetry.ack {ack_through_seq, loss_markers_acked}` — `ack_through_seq` the highest ordinary-entry `seq` received, `loss_markers_acked` an array of `{from_seq, to_seq}` pairs identifying which delivered loss markers were received. The relay MUST NOT discard any buffered entry whose `seq` exceeds `ack_through_seq`; it MAY discard entries at or below that value once acknowledged.

**[REL-093]** Every `content.played`, `automation.run`, and `entity.state_changed` entry (per `events/1`'s own registered-schema catalog) is **durable-class**: the relay MUST retain and eventually deliver every such entry, or explicitly mark its loss (Loss marker) — it MUST NOT be silently coalesced or superseded by a later entry.

**[REL-094]** Every `device.heartbeat` and `box.vitals` entry is **latest-only**: because each such entry is itself a periodic current-status snapshot rather than a discrete occurrence, the relay MAY discard a buffered but not-yet-delivered entry of either schema for a given subject once a newer entry of the same schema and subject has been recorded, without that discard counting as loss under Loss marker — the newer entry already supersedes everything the discarded one would have reported.

*draft-note: REL-094's latest-only classification for `device.heartbeat`/`box.vitals` (as against durable-class for the other three registered schemas, REL-093) is this contract's own proposed reconciliation of a periodic-snapshot schema with a bounded offline buffer; it is not dictated by any normative source beyond the registered schemas' own field shapes (`events/1`), which already describe both as point-in-time status reports rather than change-feed entries. Confirm before this contract leaves draft.*

**[REL-095]** The five schemas named in REL-093–094 are exactly the `events/1` registered schemas this channel carries; this contract defines no field shape of its own for any of them — `events/1` is their sole normative source.

**[REL-096]** The relay's durable buffer MUST be bounded; once full, the overflow policy MUST be drop-oldest — discarding the lowest-`seq` unacknowledged durable-class entries first to make room for new ones — and MUST produce a loss marker (Loss marker) accounting for exactly what was dropped. A latest-only entry discarded under REL-094 MUST NOT itself be counted in that marker's `dropped_counts_by_schema`.

**[REL-097]** The relay MUST retry an unacknowledged batch with backoff across reconnects; entries already covered by a received `ack_through_seq` MUST NOT be re-sent.

### Loss marker

**[REL-100]** A loss marker MUST be shaped exactly `{from_seq, to_seq, dropped_counts_by_schema, reason}`: `from_seq` the lowest `seq` dropped, `to_seq` the highest `seq` dropped, `dropped_counts_by_schema` an object mapping each affected `schema` name to how many of its entries were dropped in this range, and `reason` a short string naming why. This shape is `events/1` EVT-144's own named exception to its subscriber-stream gap shape (`{from_id, to_id, reason}`) — this contract, not `events/1`, is this shape's normative source.

**[REL-101]** `reason` MUST be `buffer_exceeded` for every loss marker this contract's overflow policy (REL-096) produces; this contract defines no other `reason` value.

**[REL-102]** A loss marker MUST be delivered to the app peer in a `telemetry.push`'s `loss_markers` array at the next opportunity after the loss it describes occurred; a single push MAY carry more than one marker alongside its ordinary `entries`. The relay MUST continue re-sending a not-yet-acknowledged loss marker on every subsequent push until its own `{from_seq, to_seq}` pair appears in some received `loss_markers_acked` (REL-092).

**[REL-103]** Silent loss is forbidden: the relay MUST NOT allow a durable-class entry (REL-093) to be dropped from its buffer without a corresponding loss marker accounting for it — every durable-class discontinuity the app peer observes is either absent or explicitly marked, never simply missing.

**[REL-104]** `dropped_counts_by_schema` MUST count only durable-class entries (REL-093); a latest-only entry discarded under REL-094's supersession rule MUST NOT appear in any loss marker's counts, since REL-094 does not classify that discard as loss.

**[REL-105]** The loss-marker object itself MUST carry exactly the four fields REL-100 names, no more — sharing `events/1` EVT-144's from/to/reason spine (not its exact 3-field subscriber-stream shape) is what lets a consumer handling both channels' loss markers recognize the common bounded-range-plus-reason pattern, without conflating this contract's own extended, sequence-keyed shape with EVT-144's own three-field, id-keyed one. Reliable delivery of the marker itself (REL-102's resend-until-acknowledged rule) is carried entirely by the telemetry-ack envelope's own `loss_markers_acked` field (REL-092), never by adding a sequence field to the marker shape itself.

### Device plane

**[REL-110]** The relay MUST report its currently known candidate set — devices its own discovery has observed but that are not (or no longer) adopted — to the app peer as `device.candidates {candidates}`, `candidates` an array of `{match, provenance, status, ignored_until, first_seen, last_seen}`. `match` MUST use one of the discovery-match forms `manifest/1` MAN-071 defines. `provenance` MUST be `discovered` or `manual`. `status` MUST be one of `pending`, `adopted`, `ignored`; `ignored_until` MUST be present — a Timestamp or the literal `forever` — if and only if `status` is `ignored`, and MUST be absent otherwise.

**[REL-111]** `device.candidates` is a full-set report of the relay's current view, not an event log: the app peer MUST treat each report as replacing its prior view of this relay's candidate set, keyed by `match`, rather than as a delta to fold in.

**[REL-112]** The app peer dispatches a resolved device operation as `device.command {entity_id, command, params}` — `entity_id` already resolved to one specific adopted entity (`rules/1` Entity targeting resolves any selector or device-class filter before a command reaches this contract; relay/1 accepts only a single, already-resolved `entity_id`). The relay MUST respond `device.command_result {ok, error}`.

**[REL-113]** A `device.command` whose `command` does not resolve against the target entity's device class's own command vocabulary (`device-class-registry/1` REG-052) MUST be rejected with a typed error result (`COMMAND_UNRESOLVED`, Error taxonomy); the relay MUST NOT attempt an unresolved command against the physical device.

**[REL-114]** `device.command`'s `params` MAY carry credential material scoped to that single dispatch. Such material MUST NOT be written to any durable store — including the relay's own operational database and its persisted desired state — and MUST NOT appear in any log output; an implementation carries it only in memory for the dispatch's duration.

**[REL-115]** A `device.command` MUST be serialized per target device: the relay MUST NOT dispatch a second command to the same `device_id` while an earlier one to that same device is still outstanding — it queues or refuses the conflicting attempt rather than reordering or interleaving delivery to one physical device.

### Player-credential authority

**[REL-120]** A relay is the sole issuer of player certificates and channel tokens for its own site's screens, and the sole verifier of a screen's per-connection credential; this contract does not define the player/1-facing messages that issuance and verification ride on (`player/1`, Scope) — this section defines only the inputs this contract's own channels deliver those decisions from.

**[REL-121]** A pairing-grant record (`pairing_grants`, REL-067) MUST carry at least `{grant_id, purpose, resulting_principal_kind, ttl, redemption_mode, issued_at}` — `redemption_mode` one of `one-time` or `multi`. This record is a specialization of `security-model/1`'s own canonical grant shape (`security-model/1` SEC-030), carrying only the subset of fields a relay itself must enforce, never a competing grant shape of its own.

**[REL-122]** A pairing grant delivered via `pairing_grants` MUST remain redeemable by the relay for the whole of its `ttl` even while the relay is disconnected from its app peer: the relay's own last-applied snapshot (Idempotent apply & enrollment-anchored trust) is authoritative for redemption eligibility until a newer generation supersedes it.

**[REL-123]** The relay MUST enforce `revocation_and_site.revoked` (REL-066) against every player-certificate issuance, every channel-token issuance, and every per-connection credential verification it performs, using its own last-synced copy while disconnected — a revocation the relay has not yet pulled is not yet enforceable, but a synced one MUST be enforced regardless of connectivity.

**[REL-124]** Every pairing-grant redemption the relay performs MUST be reported upstream at the next telemetry or connection opportunity, so the platform's own audit record can reflect a redemption that occurred while disconnected (the audit record's own shape is out of this contract's scope).

**[REL-125]** REL-019's reserved reverse-claim identity fields are this section's concern only insofar as a relay using that path is, before its redemption completes, bound to no site and therefore has no `pairing_grants` or `revocation_and_site` to enforce; this contract defines nothing further about that path (Enrollment, Scope).

**[REL-126]** A pairing code a relay displays for redemption (`player/1` Pairing redemption, PLY-024) MUST encode the relay's own dial address, a `grant_selector`, and a `fingerprint_commitment` — `grant_selector` a value the relay resolves against `pairing_grants` (REL-121) to the specific `pairing_grant` record the code names; `fingerprint_commitment` a truncated hash the relay computes over its own current trust-anchor public key (`player/1` Out-of-band cert authentication). The relay never receives `fingerprint_commitment` back from a player, and MUST NOT store it as, or treat it as part of, any redemption state — its role in `player/1`'s commitment-verified trust path (`player/1` PLY-052) ends at correctly computing and displaying it; this contract defines no verb for a relay to receive, check, or otherwise consume a `fingerprint_commitment` from a player.

### Clock trust

**[REL-130]** A relay MUST persist a clock floor: the latest time value it has ever verified independently or previously observed as current. On restart, it MUST NOT adopt a wall-clock reading earlier than this persisted floor.

**[REL-131]** While `clock_state` is `untrusted` (Hello / negotiate), every time-based evaluation this contract's desired state feeds (`rules/1` time/time_pattern/sun triggers and conditions, per `rules/1` RUL-370) MUST use the persisted floor rather than an unverified wall-clock reading; this contract defines the floor's persistence and update rules (this section), `rules/1` defines how an evaluation consumes them.

**[REL-132]** The clock floor MUST advance only from a time value the relay can verify independent of an unauthenticated claim. An app peer's `clock.hint` (REL-133) MUST NOT by itself advance the floor.

*draft-note: the specific independently-verifiable time source(s) an implementation uses to advance the floor (e.g. a network time-authentication protocol, or a signed timestamp verified against the relay's own trust state) are not fixed by any normative source yet; this contract requires only that whatever source is used is independently verifiable, never a bare unauthenticated claim.*

**[REL-133]** The app peer MAY send `clock.hint {ts}` at any time on an established connection. A relay receiving a hint MUST treat it as adjusting its own runtime clock only (REL-132); it MUST reject a hint whose `ts` exceeds its own current certificate's `not_after` plus a bounded grace period, so that an app peer cannot use a hint alone to make the relay believe its own credential has not yet expired when, by any independently verified account, it has.

*draft-note: the bounded grace period REL-133 references is not fixed by any normative source yet; proposed default: the same skew-grace window applied to certificate validity checks at connect.*

**[REL-134]** When `clock_state` transitions from `untrusted` to `trusted`, the relay MUST re-evaluate every time-based trigger and condition immediately against the newly trusted time (`rules/1` RUL-371), rather than waiting for its next naturally scheduled tick; this is a `rules/1`-governed consequence of a floor/state transition this contract produces.

**[REL-135]** A relay's clock floor and `clock_state` MUST be evaluated independently of certificate validity for the purposes of Expired-certificate re-enrollment (REL-023) — an untrusted or stale relay clock MUST NOT block that path, since eligibility there is decided entirely from the app peer's own trusted time and issuance record.

**[REL-136]** A relay whose own `clock_state` is `untrusted` at connection time — including at cold boot, before any clock floor has ever been persisted (REL-130) — MUST still be able to complete the mutually authenticated connection REL-003 requires: the relay's own validation of the app peer's server certificate MUST be skew-tolerant, either applying a bounded skew grace to that certificate's `notBefore`/`notAfter` window or deferring temporal validation of it entirely until the relay's own clock becomes trusted (Clock trust) — relying meanwhile on the app peer's certificate matching the trust anchor established at enrollment (REL-003) to establish the app peer's identity, independent of temporal validity. This MUST hold identically for a loopback connection and for a relay dialing outbound to its app peer (REL-003), and MUST let a clock-less relay complete the handshake, reach `hello`, report `clock_state: untrusted` (REL-038), and receive a `clock.hint` (REL-133) — the handshake MUST NOT fail solely because the relay's own clock cannot yet validate the peer certificate's temporal window.

*draft-note: REL-136 relies on "the app peer's certificate matching the trust anchor established at enrollment" (REL-003) to carry identity once temporal validation is skew-tolerant or deferred; this contract does not yet pin down whether that match is validated as a leaf-key pin (the relay compares the presented certificate's public key itself against the exact key learned at enrollment) or as CA-chain validation (the relay accepts any leaf that chains to the CA learned at enrollment). The choice materially changes what REL-136's deferred temporal check costs: CA-chain validation would let any leaf the CA has ever issued satisfy identity once temporal checks are deferred, while leaf-key pinning requires the same current key and only breaks on a legitimate app-peer certificate rotation. Proposed default: the enrollment trust anchor is the app peer's leaf public key (SPKI-pin) — a deferred temporal check under REL-136 still requires that same current key, so a rotated-off leaf that still chains to the enrollment-learned CA does not satisfy it. Operational cost of this default: a legitimate app-peer certificate-key rotation requires re-establishing the anchor (for example, via a re-enrollment or a dedicated re-anchoring step), rather than rotating silently under an unchanged CA. Confirm before this contract leaves draft.*

### Gateway posture

**[REL-140]** A relay/1 message MUST NOT carry asset bytes, and this contract defines no verb or field for a relay to fetch, cache, or serve content bytes on the app peer's behalf. Every content reference this contract carries (`screen_programs`, REL-061) is a signed pointer a screen resolves directly against its own content origin — the relay is never in that data path.

**[REL-141]** This contract defines no storage-capacity negotiation, no lease-pinned eviction, and no disk-fits-check message of any kind — a relay has no asset store for the app peer to negotiate space in.

**[REL-142]** The relay's own durable local storage under this contract is limited to: its enrollment identity and certificate material — including the private key of its most-recently-issued certificate, which the relay MUST retain even after that certificate expires (never discard it at expiry) so it remains able to prove possession of it at a later Expired-certificate re-enrollment (REL-027), until a fresh enrollment or a completed renewal supersedes it — its persisted last-applied `{generation, hash}` and `desired_state_verification_key`, and its bounded telemetry buffer (Telemetry upstream). This contract defines no other durable local state for a relay to hold.

**[REL-143]** A `box.vitals` entry's low-disk signal (an `events/1`-defined field this contract merely carries, REL-095) reports on the health of the relay's own small operational storage (REL-142), never on any content-store capacity — because none exists under this contract to report on.

### Multi-relay identity

**[REL-150]** An app peer MUST disambiguate a connecting relay by its enrolled cryptographic identity (`relay_id` and certificate, Enrollment) — never by source IP address or any other network-layer property of the connection.

**[REL-151]** Two relays presenting overlapping or identical private-address ranges in their own `subnet_metadata` (Hello / negotiate) MUST be treated by the app peer as fully independent — `subnet_metadata` describes a relay's own LAN and carries no uniqueness expectation across relays.

**[REL-152]** This contract's schema places no upper bound on how many relays a single app peer may hold concurrent enrollments with; a deployment MAY restrict itself to fewer by its own policy, but this contract does not encode any such restriction as a wire-level rule.

**[REL-153]** A device's own identity (`device_id`, REL-063) is scoped to `(site, driver, native_id)`, never to the relay that happens to currently report it — re-homing an already-adopted device to a different relay serving the same site MUST resolve to the same `device_id`; the app peer's own records reflect only which relay most recently reported it.

## Wire shapes

```json
// ErrorFrame — the shape for a typed refusal not carried in an existing ack (REL-007; aligned with ctx/1 CTX-003)
{
  "type": "error",
  "id": "01J8Z4K4N5P6Q7R8S9T0V1W3E0",
  "trace_id": "01J8Z4K4N5P6Q7R8S9T0V1W3E0",
  "code": "PROTOCOL_VIOLATION",
  "message": "a message other than challenge/hello/hello-ack was sent before the handshake completed"
}
```

```json
// Claim credential (delivered to a relay ahead of enrollment, out of band — REL-011; non-loopback form shown)
{
  "claim_token": "9f2c7b1e4a3d4f5b8c6e7a9b1c2d3e4f",
  "app_endpoint": "wss://relay.example.internal:8443/relay/v1",
  "trust_pin": "sha256:5f6e7a1b2c3d4e5f8091a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d"
}
```

```json
// EnrollmentRequest (bootstrap — the sole pre-enrollment exchange, REL-010–012)
{
  "type": "enroll",
  "id": "01J8Z4K4N5P6Q7R8S9T0V1W3A0",
  "body": {
    "claim_token": "9f2c7b1e4a3d4f5b8c6e7a9b1c2d3e4f",
    "csr": "-----BEGIN CERTIFICATE REQUEST-----\nMIIBazCB4wIBADAxMS8...\n-----END CERTIFICATE REQUEST-----"
  }
}
```

```json
// EnrollmentResponse
{
  "type": "enroll-ack",
  "id": "01J8Z4K4N5P6Q7R8S9T0V1W3A0",
  "body": {
    "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
    "cert": "-----BEGIN CERTIFICATE-----\nMIIB6jCCAW+gAwIBAgI...\n-----END CERTIFICATE-----",
    "not_before": 1752537600000,
    "not_after": 1784073600000,
    "desired_state_verification_key": "ed25519:8f14e45fceea4b3e8c1e1a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1"
  }
}
```

```json
// RenewRequest / RenewResponse (in-band, over the authenticated connection, REL-015 — no pop_signature required; the connection's own mTLS handshake already proves possession)
{ "type": "renew", "id": "01J8Z4K4N5P6Q7R8S9T0V1W3A2", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "csr": "-----BEGIN CERTIFICATE REQUEST-----\nMIIBaz...\n-----END CERTIFICATE REQUEST-----" } }
```

```json
{ "type": "renew-ack", "id": "01J8Z4K4N5P6Q7R8S9T0V1W3A2", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "cert": "-----BEGIN CERTIFICATE-----\nMIIB6j...\n-----END CERTIFICATE-----", "not_before": 1784073600000, "not_after": 1815609600000 } }
```

```json
// RenewRequest over the bootstrap exchange (Expired-certificate re-enrollment, REL-020, REL-027 — pop_signature is required here, computed with the presented expired certificate's own private key over {nonce, csr})
{
  "type": "renew",
  "id": "01J8Z4K4N5P6Q7R8S9T0V1W3C1",
  "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
  "body": {
    "csr": "-----BEGIN CERTIFICATE REQUEST-----\nMIIBaz...\n-----END CERTIFICATE REQUEST-----",
    "pop_signature": "ed25519-sig-over-nonce-c4a1f8e2b3d4c5f60718293a4b5c6d7e-and-csr-signed-with-presented-certs-private-key"
  }
}
```

```json
// RenewResponse — rejected (REL-029, pop_signature absent or invalid)
{ "type": "error", "id": "01J8Z4K4N5P6Q7R8S9T0V1W3C1", "trace_id": "01J8Z4K4N5P6Q7R8S9T0V1W3C1", "code": "RE_ENROLL_POP_INVALID", "message": "proof-of-possession signature missing or did not verify against the certificate on record for this serial" }
```

```json
// Challenge (app peer -> relay, first message on a fresh authenticated connection, REL-030; the app peer's bootstrap listener reuses this same shape for Expired-certificate re-enrollment's proof-of-possession nonce, REL-026)
{ "type": "challenge", "body": { "nonce": "b3f1c2a9d4e5f60718293a4b5c6d7e8f" } }
```

```json
// Hello (relay -> app peer, frame zero after Challenge)
{
  "type": "hello",
  "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
  "body": {
    "protocol_version": "1.0",
    "features": ["telemetry.latest_only_v1"],
    "site_binding": { "scope_node": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "tz": "America/Chicago", "lat": 41.8781, "long": -87.6298 },
    "subnet_metadata": { "advertised_address": "192.0.2.12" },
    "clock_state": { "state": "trusted", "source": "ntp" },
    "channel_binding_signature": "ed25519-sig:5f6e7a1b2c3d4e5f8091a2b3c4d5e6f7"
  }
}
```

```json
// HelloAck (app peer -> relay)
{
  "type": "hello-ack",
  "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
  "body": {
    "negotiated_version": "1.0",
    "features": ["telemetry.latest_only_v1"],
    "site_binding": { "scope_node": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "tz": "America/Chicago", "lat": 41.8781, "long": -87.6298 },
    "deprecated": {}
  }
}
```

```json
// StatePull (relay -> app peer)
{ "type": "state.pull", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "since_generation": 41 } }
```

```json
// StateSnapshot (app peer -> relay — every sections key MUST be present, REL-060)
{
  "type": "state.snapshot",
  "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
  "body": {
    "generation": 42,
    "hash": "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
    "signature": "ed25519-sig-over-generation-42-and-hash-sha256-2cf24dba-signed-with-app-peers-desired-state-signing-key",
    "signed_with_key": "ed25519:8f14e45fceea4b3e8c1e1a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1",
    "sections": {
      "screen_programs": [
        {
          "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X6",
          "program_revision": "rev-17",
          "priority": "scheduled",
          "display": "content",
          "content": [
            { "asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", "url": "https://app.example/cas/e3b0c4...", "expires_at": 1752541200000 }
          ]
        }
      ],
      "edge_rules": {
        "rules_minor_version": "1.0",
        "rules": [
          { "rule_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z1", "execution_class": "edge", "closed_over": { "variables": {}, "selectors": {}, "preset_batches": {} } }
        ]
      },
      "device_inventory": {
        "devices": [
          {
            "device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
            "driver": "roku",
            "native_id": "192.0.2.40",
            "poll_cadence_seconds": 10,
            "entities": [
              { "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y2", "device_class": "media-player", "enabled": true, "hidden": false, "display_name": "Lobby TV", "category": "primary" }
            ]
          }
        ],
        "pack_match_patterns": [ { "deviceClass": "media-player", "match": [{ "ssdp": "urn:roku-com:device:player:1" }] } ]
      },
      "schedule": { "playlists": [], "dayparts": [], "presets": [] },
      "revocation_and_site": { "revoked": [], "site_effective": { "tz": "America/Chicago", "lat": 41.8781, "long": -87.6298 } },
      "pairing_grants": [
        { "grant_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZB", "purpose": "pairing", "resulting_principal_kind": "screen", "ttl": 900, "redemption_mode": "one-time", "issued_at": 1752537000000 }
      ],
      "workflow_generation": null
    }
  }
}
```

```json
// StateUnchanged (app peer -> relay, when since_generation already names the current generation)
{ "type": "state.unchanged", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "generation": 42 } }
```

```json
// StateAck — success
{ "type": "state.ack", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "applied_generation": 42 } }
```

```json
// StateAck — rejected (REL-072, snapshot fails verification against the persisted key)
{ "type": "state.ack", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "applied_generation": 41, "error": { "code": "SNAPSHOT_SIGNATURE_INVALID", "message": "signature did not verify against the persisted desired-state verification key" } } }
```

```json
// TelemetryPush (relay -> app peer — ordinary entries plus one loss marker recovering from an earlier overflow)
{
  "type": "telemetry.push",
  "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
  "body": {
    "entries": [
      { "seq": 1001, "schema": "automation.run", "payload": { "rule_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z1", "rule_revision": 4, "mode_disposition": "ran" } },
      { "seq": 1002, "schema": "device.heartbeat", "payload": { "device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA", "power_state": "on", "app_state": "app", "now_playing_content_id": null } }
    ],
    "loss_markers": [
      { "from_seq": 980, "to_seq": 999, "dropped_counts_by_schema": { "content.played": 12, "entity.state_changed": 8 }, "reason": "buffer_exceeded" }
    ]
  }
}
```

```json
// TelemetryAck (app peer -> relay — acknowledges both the ordinary entries and the loss marker)
{ "type": "telemetry.ack", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "ack_through_seq": 1002, "loss_markers_acked": [ { "from_seq": 980, "to_seq": 999 } ] } }
```

```json
// DeviceCandidatesReport (relay -> app peer)
{
  "type": "device.candidates",
  "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
  "body": {
    "candidates": [
      { "match": { "ssdp": "urn:roku-com:device:player:1" }, "provenance": "discovered", "status": "pending", "ignored_until": null, "first_seen": 1752537000000, "last_seen": 1752537600000 },
      { "match": { "mdns": "_googlecast._tcp" }, "provenance": "discovered", "status": "ignored", "ignored_until": "forever", "first_seen": 1752530000000, "last_seen": 1752537600000 }
    ]
  }
}
```

```json
// DeviceCommand (app peer -> relay) / DeviceCommandResult (relay -> app peer)
{ "type": "device.command", "id": "01J8Z4K4N5P6Q7R8S9T0V1W3B1", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "trace_id": "01J8Z4K4N5P6Q7R8S9T0V1W3B0", "body": { "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y2", "command": "launch", "params": { "channel": "dev" } } }
```

```json
{ "type": "device.command_result", "id": "01J8Z4K4N5P6Q7R8S9T0V1W3B1", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "trace_id": "01J8Z4K4N5P6Q7R8S9T0V1W3B0", "body": { "ok": true } }
```

```json
// DeviceCommandResult — rejected (REL-113, unresolved command name)
{ "type": "device.command_result", "id": "01J8Z4K4N5P6Q7R8S9T0V1W3B2", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "ok": false, "error": { "code": "COMMAND_UNRESOLVED", "message": "\"blast\" is not a command media-player declares" } } }
```

```json
// ClockHint (app peer -> relay)
{ "type": "clock.hint", "relay_id": "01J8Z4K4N5P6Q7R8S9T0V1W3A1", "body": { "ts": 1752537600000 } }
```

## Negotiation

- **Version selection** — a relay declares its `major.minor` in `hello` (`protocol_version`, REL-031/033); the app peer negotiates down within a shared major (REL-033) or refuses on a major mismatch. There is no separate transport-level version header — `protocol_version` is the sole signal.
- **N−1 compatibility** — an app peer implementing minor `M` MUST also implement minor `M-1` (REL-034), so a relay running one minor behind never sees a spurious major-mismatch refusal.
- **Capability flags** — `features` (REL-035) is the sole additive-capability signal; an app peer silently drops a flag it does not recognize rather than refusing the connection, so a relay running a newer minor than its app peer degrades gracefully to the flags both sides share.
- **Channel-binding** — every connection re-proves relay identity at the application layer (REL-030–032, REL-040–041) independent of the transport's own mutual-TLS handshake; the binding nonce is derived from that connection's own TLS exporter keying material (REL-040), and the verification key is looked up by the connection's mTLS-authenticated identity, never the self-asserted `hello.relay_id` (REL-041); this is re-established on every fresh connection, never cached across a reconnect.
- **Desired-state trust** — a relay verifies every snapshot against the `desired_state_verification_key` learned at enrollment (REL-012, REL-071), never against the platform's software-artifact trust bundle; this key is re-anchored only by a fresh enrollment or re-enrollment event (REL-017, REL-074), never by an ordinary desired-state pull.
- **Offline continuity** — a relay's own persisted last-applied generation (REL-055), persisted verification key (REL-073), and clock floor (REL-130) are together what let it keep evaluating and enforcing correctly across a restart or an extended disconnection, without contacting its app peer.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `CLAIM_TOKEN_INVALID` | The presented claim credential is malformed, unknown, or already redeemed (REL-013). | no — obtain a fresh claim credential |
| `CERT_REVOKED` | The certificate presented at connect has been revoked (REL-016). | no — re-enroll |
| `CERT_EXPIRED_INELIGIBLE` | An expired certificate was presented but does not qualify for Expired-certificate re-enrollment (REL-021/022) — superseded, revoked, or not the most-recently-issued certificate on record. | no — a fresh claim credential is required |
| `RE_ENROLL_RATE_LIMITED` | Expired-certificate re-enrollment (REL-025) was attempted more often than this `relay_id`'s bound permits. | yes — after the bound's window elapses |
| `RE_ENROLL_POP_INVALID` | The `renew` request presented through Expired-certificate re-enrollment lacked a proof-of-possession signature that verifies for the presented certificate's private key (REL-027–029). | no — a fresh claim credential is required |
| `CHANNEL_BINDING_INVALID` | `hello`'s `channel_binding_signature` did not verify against the relay's enrollment-learned public key (REL-032). | no — reconnect and retry the handshake |
| `RELAY_IDENTITY_MISMATCH` | `hello.relay_id` does not name the same relay as the connection's own mTLS-authenticated client-certificate identity (REL-041). | no — reconnect and retry the handshake |
| `PROTOCOL_VERSION_UNSUPPORTED` | No minor of the relay's declared major is implemented by the app peer (REL-033). | no |
| `PROTOCOL_VIOLATION` | A message other than `challenge`/`hello`/`hello-ack` was sent before the handshake completed, or another message-ordering rule was broken (REL-039). | no |
| `SNAPSHOT_SIGNATURE_INVALID` | A `state.snapshot` did not verify against the relay's persisted `desired_state_verification_key` (REL-071/072). | yes — after re-pulling; a persistent failure indicates the relay needs re-enrollment |
| `RULES_MAJOR_UNSUPPORTED` | The `edge_rules` section's `rules_minor_version` names a `rules/1` major this relay does not implement (REL-062). | no |
| `COMMAND_UNRESOLVED` | A `device.command`'s `command` does not resolve against the target entity's device class (REL-113). | no |
| `COMMAND_TARGET_UNREACHABLE` | The relay could not reach the target device to attempt the command. | yes |
| `MALFORMED_MESSAGE` | A message failed to parse as JSON, or did not satisfy its type's minimum shape (REL-002/003). | no |
| `INTERNAL` | An unclassified server-side failure. | yes — with backoff |
| `UNAVAILABLE` | The app peer or a dependency it needs is temporarily unable to serve the request. | yes — with backoff |

## Conformance notes

- Traceability map: `conformance/traceability/relay-1.md` — maps every `REL-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/relay-1/` — one JSON case file per `case-id` referenced from the traceability map.
- `rules/1`'s compiled-generation shape (`edge_rules`, REL-062), `device-class-registry/1`'s command vocabulary (REL-113), and `manifest/1`'s discovery-match grammar (REL-064, REL-110) are consumed by reference; corpus cases exercising them treat the referenced shape as a given input rather than re-deriving that other contract's own rules.
- The platform's scheduling-core row shapes (`schedule`, REL-065) are not yet normatively defined by any contract; corpus cases exercising snapshot completeness (REL-060) treat this section as an opaque, structurally-present value.
- Gateway posture (REL-140–143) and Multi-relay identity (REL-150–153) are largely negative/absence assertions — "no such verb exists," "no upper bound is encoded" — which a static input/expected transcript cannot itself exercise the way a positive wire exchange can; their conformance strategy is a structural check over this contract's own message-type catalog, left for a future driver rather than a corpus case here.
- Timing-dependent behavior (certificate-renewal windows, re-enrollment rate limits, clock-hint grace, telemetry retry backoff) is exercised against an injectable clock in a driver harness, not wall-clock sleeps in a static corpus.
- The claim-token issuance mechanism itself (Enrollment, REL-011/013), and the pairing-grant and revocation records a site's desired state carries (REL-066/067), are given inputs to this corpus, minted and authored by platform mechanisms outside this contract's own scope.
