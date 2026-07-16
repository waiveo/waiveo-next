# Player Protocol

**Contract:** player/1
**Version:** 1.0
**Status:** draft

## Scope

player/1 defines the protocol between a screen's player and its relay: capability negotiation and the content-type floor; the server-locating flow a player uses to find its relay; pairing, including the certificate bootstrap-fetch-verify-persist flow and the out-of-band authentication of the fetched trust material; channel-token issuance, scope, and expiry; compiled-program delivery through signed content references a player resolves directly against their own origin; the playback lease object, priority classes, and preemption; render acknowledgement and the playback/status telemetry a player emits; the player's own reconnect state machine; and a relay's screen-liveness recovery behavior together with its interaction with a compiled program's display state. This contract is player-agnostic: it presumes no specific client platform, runtime, or rendering technology, and defines no shape that only one such platform could implement.

- In scope: the capability handshake and the content-type floor, including the `composed` content type's restriction to `image`/`video` layers; the server-locating flow (a same-network discovery response, a pairing code that itself encodes a relay's dial address, and manual address entry as an equally first-class path); pairing redemption; the certificate bootstrap-fetch-verify-persist flow, including the out-of-band authentication of the fetched trust material and its two named mechanisms, and the residual risk that mechanism does and does not close; channel-token issuance, scope, and expiry (the credential `relay/1` reserves this contract to define, `relay/1` REL-120); compiled-program delivery (signed content references, direct fetch, reconcile-by-hash); the playback lease object, priority classes, and preemption; render acknowledgement and the playback/status telemetry a player emits, and their relationship to the registered event schemas that carry them onward; the player's reconnect state machine (retry-forever, never-wipe, re-run the locating flow); a relay's screen-liveness recovery behavior and its interaction with a compiled program's display state and a display-power schedule.
- Out of scope: the relay-to-app protocol a relay itself uses to enroll, obtain desired state, or deliver telemetry upstream (`relay/1`) — this contract only consumes the inputs `relay/1` says it supplies (screen program assignments, pairing grants, revocation state); the registered event schemas' own field definitions (`events/1`) — this contract only names which two schemas its own telemetry sources; the device-class registry's state/attribute/command vocabulary for automation-plane device control (`device-class-registry/1`) — this contract only reuses its `media-player` class vocabulary for status reporting and recovery gating, never redefines it; issuing a device-plane command that changes a screen's own physical power state (`relay/1` REL-112, `device-class-registry` REG-066's `power` command) — this contract defines only the on-but-blank display state a compiled program can assign, never a power-off/on verb; the scheduling core's own row shapes (playlist, daypart, validity-window, fallback, preset-batch) — that data's own content, not redefined here; a `composed` content item's own layer positioning/timing schema — reserved to whatever contract eventually authors slide composition, not this transport contract; a pack manifest's own fields (`manifest/1`); `api/1`'s Problem-shape error envelope and trace-ID propagation rules — this contract reuses them without redefinition.

## Definitions

- **ULID** — as defined in `manifest/1`: a 26-character Crockford-base32, time-sortable identifier.
- **Timestamp** — as defined in `rules/1`: an integer number of milliseconds since the Unix epoch (UTC).
- **Screen** — as defined in `relay/1`: a scope-node-attached display that `relay/1`'s `screen_programs` section assigns a compiled program to. This contract defines that screen's own runtime behavior, pairing, and content-fetch mechanics, which `relay/1` explicitly leaves to it.
- **Relay** — as defined in `relay/1`: the LAN-gateway peer a screen's player exchanges every message this contract defines with. A screen's player is always the connecting party; a relay never dials a player.
- **Player** — the client software, running on or for a Screen, that implements this contract's screen-facing role. No requirement in this contract presumes a specific client platform, runtime, or rendering technology (Scope).
- **Pairing grant** — as defined in `relay/1` (REL-121): a short-lived, redemption-scoped authorization a relay redeems on a screen's behalf. This contract defines the exchange a player uses to redeem one and the credentials that redemption produces.
- **Pairing code** — a short, human-enterable value a player accepts as manual pairing input (Server-locating, Pairing redemption). Distinct from a machine-delivered pairing payload, which carries no code a human types.
- **Trust anchor** — the certificate-authority material a player pins its post-bootstrap relay and content-origin connections to, once TLS bootstrap fetch and Out-of-band cert authentication both complete.
- **Bootstrap fetch** — the single, verification-disabled request a player is permitted to make in order to obtain unauthenticated trust-anchor material ahead of Out-of-band cert authentication (TLS bootstrap fetch).
- **Out-of-band authenticator** — a value or keyed computation, obtained through a channel independent of the bootstrap fetch itself, that a player uses to authenticate fetched trust-anchor material before installing it (Out-of-band cert authentication).
- **Player certificate** — the TLS server certificate a relay issues, from its own trust anchor, for its player-facing listener; reissued over a relay's lifetime (for instance on rotation), never a per-screen credential. The per-screen credential is the channel token.
- **Channel token** — a per-screen, revocable bearer credential a player presents on every request after pairing redemption completes (Channel tokens).
- **Content reference** — a content-addressed `sha256:` URI in the same form `ctx/1`'s `assets` family and `relay/1`'s `screen_programs` section use, paired with a signed, time-limited fetch URL a player resolves directly against its content origin.
- **Lease** — the signed, time-bounded assignment of program content to a screen, sourced from a relay's own program assignment for that screen (Leases).
- **Priority class** — the closed classification (Leases, Priority and preemption) that determines whether a newly assigned lease interrupts a screen's current rendering immediately or at its next natural boundary.

## Normative requirements

### Versioning & transport surface

**[PLY-001]** player/1 MUST be reachable at the stable path prefix `/player/v1` on a relay, as ordinary HTTPS request/response — never a persistent framed connection. A player is always the connecting party; a relay never opens a connection to a player.

**[PLY-002]** Every request or response body this contract defines MUST be UTF-8 JSON, with exactly two exceptions: the TLS bootstrap fetch's response body (raw trust-anchor material, TLS bootstrap fetch) and a content-origin asset fetch's response body (raw asset bytes, Program delivery) — both are content-typed as their own binary or PEM form, never wrapped in JSON.

**[PLY-003]** Within major version 1, evolution MUST be additive only: a new optional request or response field, or a new `content_types` member, MAY be introduced in a minor. An existing required field's meaning, or the path prefix (PLY-001), MUST NOT change within major version 1.

**[PLY-004]** Every request a player sends after pairing redemption completes (Pairing redemption) MUST carry its current channel token (Channel tokens). The pairing exchange itself (Pairing redemption, TLS bootstrap fetch) is the sole set of requests exempt from this rule, since no channel token yet exists to carry.

### Error responses

**[PLY-005]** Every error response this contract's own operations produce MUST use `api/1`'s Problem shape (`api/1` API-010, `application/problem+json`) as its envelope — this contract defines no error-frame shape of its own, only the `code` values in its own registry (Error taxonomy) that populate that shape's `code` extension member.

**[PLY-006]** Every player/1 response, success or error, MUST carry a `Trace-Id` response header under exactly the rules `api/1` API-060–062 define; a player MAY supply a request-side `Trace-Id` header under those same rules.

**[PLY-007]** A `code` value in this contract's own Error taxonomy is a distinct registry from `api/1`'s (API-011): a player/1 error response's `code` MUST be drawn from this contract's table, not `api/1`'s, except where this contract's table explicitly reuses an `api/1` code value by name (Error taxonomy) for a genuinely identical failure class.

### Capability handshake

**[PLY-010]** Every request this section's capability fields apply to MUST carry `protocol_version` (a `major.minor` string). A relay implementing no minor of the player's declared major MUST refuse with `PROTOCOL_VERSION_UNSUPPORTED` (Error taxonomy) rather than partially interpret the request.

**[PLY-011]** A relay implementing player/1 minor `M` MUST also implement minor `M-1` of the same major, so a player running one minor behind a relay's current always finds a satisfying negotiated minor, never a spurious major-mismatch refusal (mirroring `relay/1`'s own N−1 rule, REL-034).

**[PLY-012]** A player's Pairing redemption request and every subsequent Program delivery request MUST carry `capabilities: {content_types, player_version}`. `content_types` MUST be a non-empty array whose members are drawn from `image`, `video`, `composed`; `player_version` is an implementation-defined diagnostic string, never itself a MUST-level gate on anything this contract defines.

**[PLY-013]** A relay MUST NOT include, in any Lease (Leases) it grants to a player, a content item whose `type` is not present in that player's most-recently-declared `content_types`. Because `content_types` rides every Program delivery request (PLY-012), a relay's view of a player's capabilities is never stale beyond one poll interval.

**[PLY-014]** `image` and `video` are this contract's content-type floor: every conformant player MUST declare both. `composed` MAY be declared additionally; growth of the content-type vocabulary beyond this floor is an additive minor (PLY-003).

**[PLY-015]** A `composed` content item's own `layers` array MUST contain only items whose `type` is `image` or `video` (Wire shapes, `ComposedContent`) — a nested `composed` layer, or a layer of any other type, MUST be rejected by the relay at program-compile time, never delivered to a player. A player declaring `composed` in `content_types` thereby declares support for exactly this restricted form, nothing broader.

**[PLY-016]** A relay MUST treat an unrecognized `content_types` member as forward-compatible data: it MUST NOT refuse the request carrying it, and MUST NOT treat the unrecognized member as though it had been declared supported.

### Server-locating

**[PLY-020]** A player MUST support locating its relay through at least the following three address-acquisition paths, and MUST document manual entry as an ordinary, supported path rather than a fallback exercised only when the other two have failed: (1) a same-network discovery response (PLY-021–023); (2) a pairing code that itself encodes the relay's dial address (PLY-024, Pairing redemption); (3) direct manual entry of the relay's address by an operator (PLY-025).

**[PLY-021]** A relay MUST answer a same-network discovery request for the search target `urn:waiveo:service:player:1` with a response carrying at least a resolvable base URL for its own `/player/v1` path (PLY-001). This contract does not fix the discovery transport's own framing beyond this response's minimum content — an implementation MAY use any same-network discovery mechanism capable of carrying a search target and a unicast response.

**[PLY-022]** A relay MUST re-announce its discovery response after its own advertised address changes (mirroring `relay/1` REL-037's own re-`hello` rule for exactly this event), so a player relying on discovery is never left holding a stale address without any signal that it changed.

**[PLY-023]** A same-network discovery response's own reach is not a normative property this contract can guarantee: this contract states only what a relay MUST answer with (PLY-021) and MUST NOT presume every player on a given network reaches every relay — this is exactly why paths (2) and (3) exist as first-class, not merely as a fallback for path (1)'s occasional failure.

**[PLY-024]** A pairing code (Pairing redemption) MUST deterministically decode, by the player accepting it, to a relay dial address `{host, port}` — a player MUST be able to reach a relay from a pairing code alone, with no dependency on same-network discovery having succeeded.

*draft-note: the pairing code's own character grammar and length are not fixed by any normative source yet, beyond PLY-024's functional requirement (decodes to a dial address) and PLY-052's (also serves as an out-of-band authentication key). Proposed: a fixed-length alphanumeric string, grouped for manual entry (e.g. hyphen-separated blocks), short enough for a bounded-input remote-control-style entry method — subject to revision once a concrete encoding is chosen.*

**[PLY-025]** A relay MUST accept a player's manual entry of its dial address (`{host, port}`) as functionally equivalent, from that point forward, to an address obtained via discovery (PLY-021) or a pairing code (PLY-024): the same Pairing redemption exchange proceeds next, over whichever address-acquisition path was used. Manual entry supplies an address only — it MUST NOT be treated as pairing credential material by itself (Pairing redemption).

**[PLY-026]** A player MUST persist whichever relay address it last successfully used, independent of which of the three paths (PLY-020) supplied it, and MUST treat that persisted address exactly as the Reconnect state machine requires (never-wipe) once pairing has completed against it.

### Pairing redemption

**[PLY-030]** A player pairs by sending a `PairingRequest` (Wire shapes) to a relay's `/player/v1/pair` path, over the verification-disabled bootstrap connection this section and TLS bootstrap fetch together define. `PairingRequest` MUST carry `hardware_id`, `capabilities` (PLY-012), and — on the manual, human-entered path only — `pairing_code`.

**[PLY-031]** `hardware_id` MUST be a value stable across a player's own reinstall or upgrade. A value that changes when a player's own software is reinstalled or upgraded (an application-instance-generated identifier, for instance) MUST NOT be used to populate `hardware_id`, and MUST NOT be relied on elsewhere in this contract as a screen's continuity key across a re-pair.

**[PLY-032]** A relay's `PairingResponse` (Wire shapes) to a `PairingRequest` MUST carry `trust_anchors` (TLS bootstrap fetch), `authenticator_path` (Out-of-band cert authentication), and `pairing_status`, one of `pending` or `redeemed`.

**[PLY-033]** `pairing_status: pending` MUST additionally carry `poll_token`, an opaque value a player presents on `PairingStatus` polls (PLY-034) while awaiting redemption; `pairing_status: redeemed` MUST additionally carry the terminal pairing result directly, `{channel_token, screen_id, issued_at, expires_at}` (Channel tokens), with no further poll required.

**[PLY-034]** While `pairing_status` is `pending`, a player MUST poll `GET /player/v1/pair/status`, presenting `poll_token`, at a bounded interval, over the now-pinned connection (Out-of-band cert authentication, TLS bootstrap fetch — pinning MUST already have succeeded before this poll begins; PLY-030's bootstrap connection carries only the single `PairingRequest`/`PairingResponse` exchange). Each poll response MUST carry the same two-value `pairing_status` shape (PLY-032–033) until redemption completes.

*draft-note: the polling interval and the pairing grant's own maximum lifetime bound how long a player waits in `pending` before giving up and returning to Server-locating; the grant's `ttl` (`relay/1` REL-121) is the authoritative expiry, not a value this contract invents. Proposed poll interval: 3 seconds — subject to revision.*

**[PLY-035]** A `screen_id` first comes into existence, from a player's perspective, only in a `redeemed` pairing result (PLY-033) — a player MUST NOT assume or fabricate a `screen_id` before that point, and MUST use `hardware_id` as its sole self-identifying field until then.

**[PLY-036]** A relay MUST reject a `PairingRequest` whose `pairing_code` is absent, malformed, expired, or already redeemed under a `redemption_mode: one-time` grant (`relay/1` REL-121) with a typed error (`PAIRING_CODE_INVALID` or `PAIRING_EXPIRED`, Error taxonomy) rather than a `pending` status that can never resolve.

**[PLY-037]** A `redemption_mode: multi` grant (`relay/1` REL-121) MAY be redeemed by more than one player's `PairingRequest`, each producing its own independent `screen_id` and `channel_token` — this contract does not itself decide when a deployment uses `one-time` versus `multi`, only that both grant behaviors (`relay/1` REL-121) are honored identically by the exchange this section defines.

**[PLY-038]** A `PairingRequest`/`PairingResponse` exchange and any following `PairingStatus` polls (PLY-034) together constitute one pairing attempt. A player MUST treat a pairing attempt as atomic with respect to trust: Out-of-band cert authentication (Out-of-band cert authentication) MUST complete successfully before a player proceeds past `PairingResponse` to any `PairingStatus` poll or uses any credential this attempt produces.

### TLS bootstrap fetch

**[PLY-040]** A player MUST obtain trust-anchor material through exactly one mechanism: a single request, made with certificate verification explicitly disabled, whose response is the `PairingResponse` (PLY-032) carrying `trust_anchors`. This is a verification-disabled fetch by construction, not "default trust" — a player MUST NOT represent, log, or otherwise treat this one request as though ordinary certificate validation occurred on it.

**[PLY-041]** The verification-disabled bootstrap window (PLY-040) MUST be open only for the duration of a single, explicitly initiated pairing attempt (Pairing redemption) — entered only via an explicit pairing trigger a player's own operator-facing setup surface provides, out of this contract's scope — and MUST close immediately once that attempt's `PairingResponse` has been processed, whether Out-of-band cert authentication (Out-of-band cert authentication) succeeds or fails. No other code path this contract defines — in particular, no path in the Reconnect state machine — MAY reopen it.

**[PLY-042]** `trust_anchors` MUST be a non-empty array of `{covers, pem}`, where `covers` is a non-empty array whose members are drawn from `player` and `content` — `player` naming the relay's own player/1 endpoint, `content` naming a content origin a Lease's assets may be fetched from (Program delivery). A single entry MAY declare both, when one certificate authority issues leaves for both purposes.

**[PLY-043]** A content origin whose certificate independently chains to a publicly-trusted root MUST NOT require a `trust_anchors` entry covering `content` — Out-of-band cert authentication and Steady-state pinning apply only to trust-anchor material this contract's own bootstrap fetch delivers, never to a player's ordinary system trust store.

**[PLY-044]** A relay MUST issue trust-anchor material as CA-level material, never a leaf certificate or a leaf-level fingerprint: a `trust_anchors` entry's `pem` MUST be the issuing certificate authority a relay's (or content origin's) leaf chains to, so that leaf rotation under an unchanged authority (Steady-state pinning) requires no change to persisted trust-anchor material.

**[PLY-045]** A player MUST persist authenticated trust-anchor material (Out-of-band cert authentication) to storage documented by its own platform as surviving an application restart. A player MUST NOT rely on any storage class not so documented for trust-anchor material that must survive a relaunch.

**[PLY-046]** Before establishing any post-bootstrap connection (Steady-state pinning), a player MUST resolve its persisted trust-anchor material into whatever form its own platform's certificate-pinning mechanism requires — a persistence layer and a pinning-configuration layer are not guaranteed to share one representation, and this resolution step MUST be treated as a required part of a player's boot sequence, never an incidental implementation detail.

**[PLY-047]** A `PairingRequest`/`PairingResponse` exchange (PLY-030) that fails for any reason before `pairing_status` is reached — a network failure, a malformed response, a refusal (PLY-036) — MUST leave a player with no persisted trust-anchor material change: a failed bootstrap attempt MUST NOT partially persist `trust_anchors`.

**[PLY-048]** A certificate-validation failure encountered while establishing an ordinary, already-pinned steady-state connection (Steady-state pinning) — as distinct from an Out-of-band cert authentication failure at bootstrap (PLY-05x) — MUST be treated as an ordinary retryable connectivity failure (Reconnect state machine), never as fatal or as a trigger to re-enter Pairing redemption. This absorbs a plausible first-boot clock-skew rejection: a player's own clock being wrong at boot can make a genuinely valid certificate appear not-yet-valid or expired, and that condition MUST resolve itself on retry as the player's clock corrects, without bricking setup.

### Out-of-band cert authentication

**[PLY-050]** A player MUST authenticate `trust_anchors` (TLS bootstrap fetch) against an out-of-band authenticator before persisting or pinning any of it. A player that persists or pins trust-anchor material without completing this authentication is nonconformant with this contract — the bootstrap fetch (TLS bootstrap fetch) supplies bytes; this section is what makes trusting them conformant.

**[PLY-051]** `PairingResponse`'s `authenticator_path` (PLY-032) MUST be exactly one of `fingerprint` or `pairing_code_hmac`, and MUST match the path implied by the `PairingRequest` that produced it: `fingerprint` when no `pairing_code` was sent (a same-network or manually-addressed exchange proceeding without a human-entered code), `pairing_code_hmac` when one was. A player MUST treat a mismatch between the `authenticator_path` a relay declares and the path implied by its own request as an authentication failure (PLY-057) — never proceed under the relay's claimed path if it disagrees with the player's own record of what it sent.

**[PLY-052]** Under `authenticator_path: fingerprint`, `PairingResponse` MUST additionally carry `trust_anchor_fingerprint`: a SHA-256 digest, computed over the canonical byte concatenation of every `trust_anchors` entry's `pem` in array order, expressed as a lowercase hex string. A player MUST compute the same digest over the `trust_anchors` bytes it received and compare it byte-exact against `trust_anchor_fingerprint`; any mismatch is an authentication failure (PLY-057).

**[PLY-053]** Under `authenticator_path: pairing_code_hmac`, a player MUST compute an HMAC-SHA256 over the canonical byte concatenation of every `trust_anchors` entry's `pem` in array order, keyed by the UTF-8 bytes of the `pairing_code` it sent in `PairingRequest`, and compare the result byte-exact against `expected_authenticator`, a field `PairingResponse` MUST carry under this path. Any mismatch is an authentication failure (PLY-057).

**[PLY-054]** `authenticator_path: fingerprint` MUST be used whenever `trust_anchors` was delivered to a player over a connection it did not reach by a human keying in the authenticating material itself — a same-network discovery response (Server-locating) followed by a `PairingResponse` a relay already recognizes this player for, or an address obtained by manual entry (PLY-025) followed by a relay-initiated grant delivered the same way — because such a channel can carry a full digest without a human transcription step.

**[PLY-055]** `authenticator_path: pairing_code_hmac` MUST be used whenever the pairing material a player is authenticating against was, at any point in this pairing attempt, obtained by a human typing it in — most concretely, the pairing code itself, which by PLY-024 must be short enough for manual entry and therefore cannot itself carry a fingerprint-length value. The pairing code doubles as this path's authentication key precisely because it is already the one value a human transcribed.

**[PLY-056]** A relay MUST NOT offer a player any authenticator path beyond the two PLY-051 names. A future minor extending this set (PLY-003) MUST assign the new path its own distinct `authenticator_path` value rather than overload either existing one.

**[PLY-057]** On an authentication failure under this section — a computed value mismatch (PLY-052–053), an `authenticator_path` mismatch (PLY-051), or a `PairingResponse` missing a field this section requires for its declared path — a player MUST discard the fetched `trust_anchors` immediately, MUST NOT persist or pin any part of it, MUST NOT proceed to any `PairingStatus` poll or use any credential from this pairing attempt, and MUST return to Server-locating for a fresh attempt. A player is not required to report this failure upstream over a connection it has just determined it cannot trust; it MAY surface a local, operator-facing indication through means outside this contract's scope.

**[PLY-058]** An authentication failure (PLY-057) MUST NOT itself invalidate the pairing grant or pairing code that produced it, beyond whatever redemption-count or expiry rule already governs that grant (`relay/1` REL-121) — a transient failure (for instance, a mistyped pairing code later corrected) MUST remain retryable within the grant's own `ttl` and `redemption_mode`.

**[PLY-059]** A relay MUST NOT complete pairing redemption (Pairing redemption) for any player whose Out-of-band cert authentication it cannot itself verify was possible to perform — concretely, a relay MUST always deliver a `trust_anchor_fingerprint` (PLY-052) or `expected_authenticator` (PLY-053) matching the `authenticator_path` it declares; a relay that cannot compute one for a given pairing attempt MUST refuse the attempt outright rather than omit the field.

#### Residual risk: MITM at first pairing

Out-of-band cert authentication (PLY-050–059) closes the specific gap the underlying bootstrap-fetch-verify-persist mechanism leaves open — it stops an on-path party from substituting its own trust-anchor material during the one verification-disabled request TLS bootstrap fetch requires, because that substituted material would then fail the digest or HMAC comparison this section requires before anything is trusted. It does not, and cannot, eliminate every risk at first pairing: the safety of a given pairing attempt still depends entirely on the secrecy and integrity of whatever out-of-band value authenticates it — a fingerprint carried in a machine-delivered pairing payload, or a pairing code a human transcribes. A party that can both observe the bootstrap connection and also obtain or intercept that same out-of-band value ahead of the legitimate player — by reading a discovery-linked grant it was never meant to see, or by learning a pairing code before the intended installer enters it — can still complete a first pairing this contract cannot distinguish from a legitimate one. The channel that delivers a pairing code or a machine-delivered grant to its intended player is therefore part of this contract's own security boundary, not a detail external to it, even though this contract does not itself define that delivery channel's own protection (Pairing redemption references it only as a given input). This residual is structural to any trust-on-first-contact design and is not expected to close through a future minor of this section alone.

*draft-note: PLY-052's digest computation and PLY-053's keyed HMAC computation are, as specified, ordinary SHA-256 and HMAC-SHA256 over well-defined byte inputs — standard, widely implemented primitives — but neither this contract nor any source it draws on has yet confirmed both primitives are available, standard-conformant, and performant enough on every client platform this contract must run on. Confirm on real client hardware, against a published test vector, before this section leaves draft.*

### Steady-state pinning

**[PLY-060]** Every connection a player makes after Out-of-band cert authentication succeeds (Out-of-band cert authentication) — to its relay's player/1 endpoint or to a content origin (Program delivery) — MUST use full, platform-default certificate verification, configured to trust exactly the persisted `trust_anchors` whose `covers` includes that connection's own purpose (PLY-042), and MUST NOT disable any verification step. Verification is disabled only for the single bootstrap request TLS bootstrap fetch defines, never afterward.

**[PLY-061]** A player MUST reject a connection whose presented certificate does not chain to a persisted, appropriately-scoped trust anchor (PLY-060) — a wrong-authority or expired leaf is refused by ordinary certificate validation, and this contract requires no additional player-side validation logic beyond correctly configuring that trust scope.

**[PLY-062]** A player SHOULD distinguish, in whatever failure signal its own platform's TLS stack provides, a wrong-authority rejection from an expired-certificate rejection from another connection failure, and MUST surface that distinction to the Reconnect state machine's own failure classification (PLY-13x) rather than collapsing every TLS failure into one generic disconnection.

**[PLY-063]** Loss of a player's persisted trust-anchor material — through storage corruption, a factory reset, or any other event that leaves a player unable to resolve `trust_anchors` (PLY-046) — MUST be treated with exactly the same single-credential-clearing discipline this contract's Reconnect state machine applies elsewhere (PLY-136): the player clears its trust-anchor material and its channel token, keeps its persisted relay address (Server-locating, PLY-026) unless that too is independently lost, and returns to Pairing redemption for a fresh attempt.

**[PLY-064]** A player MUST NOT, on trust-anchor loss (PLY-063), silently reopen a verification-disabled bootstrap window (TLS bootstrap fetch) using any previously stored pairing code, grant reference, or other credential from a prior attempt without a fresh, explicit pairing trigger (PLY-041) — loss recovery re-enters Pairing redemption through the same explicit entry point an entirely new pairing attempt would, never through an implicit, standing fallback.

**[PLY-065]** A relay's own leaf-certificate rotation under an unchanged trust anchor (PLY-044) MUST require no player-side action: a player's persisted `trust_anchors` remains valid, and Steady-state pinning (PLY-060) continues to accept the new leaf without any bootstrap fetch, redemption, or Out-of-band cert authentication repeating.

### Channel tokens

**[PLY-070]** A channel token MUST authorize exactly the operations this contract defines — Program delivery, Leases, render acknowledgement and telemetry (Render acknowledgement, Status telemetry), and its own renewal (PLY-074) — and exactly the one `screen_id` it was issued to (`relay/1` REL-120). A relay MUST reject a channel token presented for any other `screen_id`, for any `relay/1` operation, or for any other contract's operation.

**[PLY-071]** A channel token MUST carry a bounded expiry (`expires_at`, a Timestamp) at issuance (PLY-033) and at every renewal (PLY-074).

*draft-note: the specific expiry duration is not fixed by any normative source yet. Proposed: 24 hours from issuance or renewal — long enough that a player polling at its ordinary cadence (Program delivery) renews well ahead of expiry without a dedicated renewal round trip on the common path, short enough that a revoked-but-still-cached token has a bounded natural lifetime. Subject to revision.*

**[PLY-072]** A relay MUST reject a request whose channel token has passed its `expires_at` with `CHANNEL_TOKEN_EXPIRED` (Error taxonomy), and MUST reject a request whose channel token names a `screen_id` present in `revocation_and_site.revoked` (`relay/1` REL-066) with `CHANNEL_TOKEN_REVOKED`, checked against the relay's own last-synced copy even while disconnected from its app peer, exactly as `relay/1` REL-123 requires of the relay's own enforcement.

**[PLY-073]** A `CHANNEL_TOKEN_EXPIRED` response MUST be treated by a player as retryable via renewal (PLY-074), never as a trigger to re-enter Pairing redemption. A `CHANNEL_TOKEN_REVOKED` response MUST be treated as terminal for that token: a player MUST clear it and re-enter Pairing redemption, without discarding its persisted relay address or trust-anchor material (PLY-063's pattern, applied here to token revocation rather than trust-anchor loss).

**[PLY-074]** A player MAY renew its channel token via `POST /player/v1/token/renew`, carrying `{screen_id}`, over an already-pinned Steady-state pinning connection; a relay MUST also accept a token renewal expressed as a refreshed token returned inline in an ordinary Program delivery response issued while the current token remains within a bounded window of its own expiry, so that a player exercising only its ordinary poll cadence still renews without a dedicated request on the common path.

**[PLY-075]** Token renewal (PLY-074) MUST NOT itself require a pairing code, a pairing grant, or any Out-of-band cert authentication material — only a still-valid (or recently expired, PLY-073) prior token's `screen_id`, over a connection already authenticated by Steady-state pinning. A relay presented a `screen_id` it no longer recognizes at all (for instance, the screen record was deleted) MUST refuse renewal with `CHANNEL_TOKEN_REVOKED` (PLY-073's terminal path applies identically), never issue a token for an unrecognized `screen_id`.

**[PLY-076]** A channel token MUST be presentable as an `Authorization: Bearer` request header on every operation it authorizes (PLY-070); this contract defines no alternate credential placement.

### Program delivery

**[PLY-080]** A player pulls its compiled program by `GET /player/v1/program`, presenting its channel token (Channel tokens) and current `capabilities` (PLY-012), and MAY supply `generation` (the `program_revision` of the Lease it currently holds) so a relay can answer efficiently when nothing has changed.

**[PLY-081]** A relay's response to a Program delivery pull MUST be exactly one of: `program.unchanged {program_revision}`, when the supplied `generation` already names the player's current assignment; or a fresh Lease (Leases), otherwise.

**[PLY-082]** A player MAY request the pull be held open (a long-poll) rather than answered immediately when nothing has changed, via a `long_poll` request parameter, up to a bounded server-side hold duration; a relay honoring this MUST still answer `program.unchanged` once that bound elapses with nothing new, rather than holding indefinitely.

*draft-note: the long-poll hold-duration bound and the ordinary (non-long-poll) pull cadence are not fixed by any normative source yet. Proposed: an ordinary pull roughly every 10 seconds; a long-poll hold bounded at roughly 25–30 seconds. Subject to revision.*

**[PLY-083]** Every content item a Lease carries MUST be a Content reference: `{type, asset_ref, url, expires_at}` for a plain `image`/`video` item, or `{type: "composed", layers: [...]}` for a `composed` item whose `layers` satisfies PLY-015. `asset_ref` and `url`/`expires_at` MUST use the same shape `relay/1` REL-061 defines for its own `screen_programs.content` entries.

**[PLY-084]** A player MUST fetch asset bytes directly from `url` — never through its relay, which carries no verb or field for fetching, caching, or serving content bytes on a player's behalf (mirroring `relay/1`'s own Gateway posture, REL-140).

**[PLY-085]** A player MUST reconcile its currently held content against a newly delivered Lease by content reference (`asset_ref`), fetching only assets it does not already hold, never re-fetching a byte-identical `asset_ref` it has already resolved.

**[PLY-086]** A `url` past its own `expires_at` MUST NOT be fetched; a player needing an asset whose reference has expired MUST obtain a fresh Lease (PLY-080) rather than retry the stale `url`.

**[PLY-087]** A player unable to reach a content origin for an asset a current Lease requires MUST continue rendering whatever content it already holds locally that remains valid under that Lease, rather than treat the fetch failure as a reason to stop rendering entirely — content availability during a content-origin outage is bounded by whatever a player has already fetched, never guaranteed beyond it.

**[PLY-088]** A Lease's own delivery and acknowledgement (Leases) are independent of whether its content is yet fetchable: a player MUST be able to accept and acknowledge a Lease (PLY-091) whose assets it cannot yet fetch, so that lease-acceptance telemetry and an operator's own confirmation that a takeover was delivered do not depend on content-origin reachability.

### Leases

**[PLY-090]** A Lease MUST be shaped `{lease_id, screen_id, program_revision, priority, display, content, issued_at, valid_until, signature}` (Wire shapes). `signature` MUST be verifiable by the player against the same trust anchor its Steady-state pinning connection to this relay is itself pinned to.

**[PLY-091]** On receiving a new Lease, a player MUST acknowledge it with `POST /player/v1/lease/ack`, `{lease_id, accepted, reason?}`, independent of whether the Lease's own content is yet fetchable (PLY-088). `accepted: false` MUST carry `reason`; a relay MUST persist a Lease's delivery and acknowledgement state locally (`relay/1`'s own operational storage, mirroring the persistence `relay/1` REL-142 already requires of it) so an acknowledgement is not lost across a relay's own disconnection from its app peer.

**[PLY-092]** `valid_until` (a Timestamp) MUST bound a Lease's own validity independent of `expires_at` on any individual content reference (PLY-083) — a player MUST NOT render content under a Lease whose `valid_until` has passed, even if a referenced asset's own URL remains technically fetchable.

**[PLY-093]** `display` MUST be exactly one of `content` or `blank`. `display: "blank"` MUST be rendered by a player as a blank display surface, showing none of the Lease's own `content` array (which MAY be empty under this value) — this is the on-but-showing-nothing state a compiled program can assign; it is distinct from, and does not itself request, any device-level power-off (Scope, out of scope).

**[PLY-094]** A newly delivered Lease supersedes a player's previously active Lease in full: a player MUST NOT hold more than one active Lease at a time, and MUST discard whatever remained of a prior Lease's own validity once a new one is accepted (PLY-091).

**[PLY-095]** `program_revision` MUST identify the exact compiled program revision a Lease was issued under, remaining stable across a Lease's own reissuance (for instance, a signature or URL refresh with no content change) so that playback telemetry (Status telemetry, Render acknowledgement) attributing a playback occurrence to a `program_revision` remains accurate even after a later revision has since been assigned.

**[PLY-096]** A relay MUST NOT assign a Lease containing a content item whose `type` falls outside a player's most-recently-declared `content_types` (PLY-013); this rule applies identically to a `blank`-display Lease's own (possibly empty) `content` array.

**[PLY-097]** `lease_id` MUST be a ULID, unique per issuance — a reissued Lease carrying otherwise-identical `program_revision` and `content` still receives a fresh `lease_id`, so that Render acknowledgement's own records unambiguously attribute a specific playback occurrence to a specific issuance.

### Priority and preemption

**[PLY-100]** `priority` MUST be exactly one of `scheduled` or `preempt`. Growth of this vocabulary beyond these two members is an additive minor (PLY-003).

**[PLY-101]** A Lease whose `priority` is `preempt` MUST be adopted by a player immediately, interrupting whatever it is currently rendering — a player MUST NOT wait for its currently rendering asset's own natural end before switching to a `preempt`-priority Lease.

**[PLY-102]** A Lease whose `priority` is `scheduled` MAY be adopted at a player's own next natural content boundary (for instance, the end of a currently playing asset or slide) rather than interrupting immediately; a player MAY also adopt it immediately when no content is currently rendering.

**[PLY-103]** This contract defines no distinct "restore" verb: the natural end of a preemption, from a player's perspective, is simply the arrival of the next `scheduled`-priority Lease (PLY-094's supersession rule applies identically regardless of the priority transition direction). What causes a relay to grant a `preempt`-priority Lease, and when it later grants a `scheduled`-priority one restoring ordinary programming, is a decision this contract does not itself make (Scope, out of scope) — this section defines only how a player, having received either, behaves.

**[PLY-104]** A `preempt`-priority Lease assigned to a screen whose most recent Lease had `display: "blank"` (Leases) MUST NOT itself force the display out of the blank state — a player MUST accept and persist the new Lease (PLY-091) but MUST continue rendering `blank` until a subsequent Lease's own `display` value says otherwise. Preemption changes what a player would render next; it does not override an intentional blank assignment already in force.

*draft-note: whether a genuinely urgent takeover should be able to force a blanked screen back to visible content — an explicit override this contract does not currently define — is an open question. PLY-104's conservative default (preemption never overrides an intentional blank) is this contract's own proposal, not dictated by any normative source beyond the instruction that this interaction be stated explicitly. Confirm before this contract leaves draft.*

**[PLY-105]** A relay's own persistence of a `preempt`-priority Lease grant (`relay/1`'s operational storage, mirroring REL-142) MUST survive that relay's own disconnection from its app peer — a preemption already delivered to a player and acknowledged (PLY-091) MUST remain the player's active Lease across a relay-side reconnection, with no re-delivery required once acknowledged.

**[PLY-106]** A player MUST NOT itself originate a priority value or select which Lease to request — `priority` is exclusively a relay-assigned field on a Lease this contract's Program delivery pull returns; a player's own role is limited to PLY-101–104's adoption behavior.

**[PLY-107]** Render acknowledgement (Render acknowledgement) MUST report which priority class a rendered occurrence was assigned under via its own `cause` field (Render acknowledgement) — `cause: "preempted"` for an occurrence rendered under a `preempt`-priority Lease, `cause: "scheduled"` for one under a `scheduled`-priority Lease with no other cause applying.

### Render acknowledgement

**[PLY-110]** A player MUST report the start of rendering a content item via `POST /player/v1/render/start`, `{lease_id, asset_ref, ts}`, at the moment it begins presenting that item on screen.

**[PLY-111]** A player MUST report the end of rendering a content item via `POST /player/v1/render/end`, once its own `t_end` is known — this item's playback has completed or been definitively interrupted, never while still in progress. `render/end`'s body MUST be shaped `{screen_id, asset_ref, program_revision, t_start, t_end, cause, completion, power_evidence?}` — exactly `events/1`'s `content.played` payload shape (`events/1` EVT-050), field for field.

**[PLY-112]** `cause` MUST be one of `scheduled`, `preempted`, `fallback`, `manual`, `loop` and `completion` one of `completed`, `interrupted`, `skipped` — the same enumerations `events/1` EVT-050 defines; this contract assigns no member of either enumeration a meaning different from `events/1`'s own.

**[PLY-113]** A relay MUST forward a player's `render/end` report upstream as an `events/1` `content.played` durable event (`relay/1`'s telemetry-upstream channel, REL-090, REL-093's durable-class rule), populating the envelope's own generic fields (`events/1` EVT-010) from context this contract does not itself define, and carrying `render/end`'s body as that event's `payload` unmodified.

**[PLY-114]** A player MUST NOT emit `render/end` for a content item it never reported `render/start` for, and MUST NOT emit `render/start` for a `lease_id` it has not acknowledged accepting (PLY-091).

**[PLY-115]** `render/start` and `render/end` together are this contract's sole source of playback telemetry: a relay MUST NOT itself infer a `content.played` occurrence from any other signal (Program delivery pulls, for instance) — an occurrence a player never explicitly reports is never delivered as a `content.played` event.

**[PLY-116]** `power_evidence`, when a player's own underlying device class supplies device-power corroboration for a playback window, MUST be shaped exactly as `events/1` EVT-050 defines it; a player with no such corroboration available MUST omit the field rather than populate it with a placeholder.

### Status telemetry

**[PLY-120]** A player MUST report its own status via `POST /player/v1/status`, `{screen_id, ts, power_state, app_state, now_playing_content_id}`, at least once per a bounded interval while connected, and immediately on any `power_state` or `app_state` transition.

*draft-note: the bounded interval is not fixed by any normative source yet. Proposed: at least once every 30 seconds while connected — subject to revision once real fleet data exists. A relay treating three consecutive missed expected reports as marking a screen offline is a reasonable downstream consumer policy but is not itself a requirement this contract imposes; `relay/1`'s own connectivity-timeout behavior governs that determination.*

**[PLY-121]** `power_state` and `app_state` MUST each be a member of the `media-player` device class's own vocabulary (`device-class-registry` REG-061, REG-064's `app_type`) — the same canonical vocabulary a relay's own device-plane polling of an adopted device would report, so a status report and an adopted device's own reported state are directly comparable when both exist for the same physical screen.

*draft-note: whether a given screen's `POST /player/v1/status` reports are additionally forwarded upstream as `events/1` `device.heartbeat` events (`events/1` EVT-060) for an associated `device_id` depends on that screen having a linked adopted device — a linkage this contract does not itself establish, define, or require to exist. Where such a linkage exists, a status report is the natural upstream source for that device's `device.heartbeat` telemetry, exactly as `render/end` sources `content.played` (PLY-113); where none exists, a status report still serves this contract's own liveness and recovery purposes (Screen liveness and recovery) without producing a `device.heartbeat` event at all. Confirm the linkage mechanism before this contract leaves draft.*

**[PLY-122]** `now_playing_content_id` MUST be the `asset_ref` of the content item currently rendering (matching an in-progress `render/start` with no corresponding `render/end` yet, Render acknowledgement), or `null` when nothing is.

**[PLY-123]** A status report MUST reflect a player's own directly observed state — never a value copied from the Lease it was most recently assigned, since a Lease's assignment and a player's own actual rendering state can diverge (for instance, while an asset fetch is still in progress, PLY-087).

### Reconnect state machine

**[PLY-130]** A player MUST retry a failed connection to its relay indefinitely, with capped backoff, and MUST NOT wipe its persisted relay address (Server-locating, PLY-026), trust-anchor material (TLS bootstrap fetch), or channel token (Channel tokens) as a consequence of connection failure alone, however many consecutive attempts fail.

**[PLY-131]** Backoff MUST be capped: retries continue indefinitely but the interval between attempts MUST NOT grow without bound. This contract does not fix the exact growth curve or cap.

*draft-note: the backoff curve and cap are not fixed by any normative source yet. Proposed: an exponential curve capped at 60 seconds between attempts — subject to revision.*

**[PLY-132]** A player MUST periodically re-run Server-locating (specifically, a fresh same-network discovery attempt, PLY-021) at a bounded multiple of its own retry attempts, rather than retry only its persisted address forever — this is what lets a same-network relay that has moved to a new address be rediscovered without operator intervention, while a player on a different network than its relay (where discovery cannot succeed) simply continues retrying its persisted address with no worse outcome than before.

*draft-note: the specific multiple (how many ordinary retries between each re-discovery attempt) is not fixed by any normative source yet. Proposed: every fourth attempt — subject to revision.*

**[PLY-133]** A player MUST classify each connection failure into at least `unreachable` (no response at the network level) or `unavailable` (a response was received, but indicated the relay's player/1 surface itself is not currently serving) — this distinction MUST be available to whatever operator-facing failure indication a player's own platform provides, so that indication is accurate rather than a single undifferentiated "disconnected" state.

**[PLY-134]** Below a bounded count of consecutive failures, a player MAY retry silently, with no operator-facing indication; at or above that count, a player MUST surface a persistent, accurate failure indication (using PLY-133's classification) until connectivity recovers — this contract does not fix the exact threshold.

**[PLY-135]** A connection failure classified `unreachable` or `unavailable` (PLY-133) MUST NOT itself trigger any credential-clearing behavior — Reconnect state machine failures are transient by default; only the narrower conditions PLY-136 and PLY-063/PLY-073 name clear anything.

**[PLY-136]** The sole reconnect-path condition that clears a credential MUST be: a relay response that is neither a network-level failure (`unreachable`) nor ordinary success, but an explicit indication that a presented channel token no longer validates for a screen the relay does recognize by other means, or that a screen record itself no longer exists — in either case, a player MUST clear only its channel token, MUST NOT clear its persisted relay address or trust-anchor material, and MUST re-enter Pairing redemption (mirroring PLY-063's and PLY-073's identical pattern for the other two credential classes this contract defines).

**[PLY-137]** A player implementation MUST NOT provide any automatic path — reachable without an explicit, operator-initiated action — that clears a player's persisted relay address, trust-anchor material, and channel token all at once. A full reset remains available only as a deliberate, explicit operator action outside this contract's own scope, never as a consequence any Reconnect state machine failure path this contract defines can reach on its own.

**[PLY-138]** A `unreachable`/`unavailable` classification and a certificate-validation failure (PLY-048) are members of the same retry-forever failure space this section governs — a certificate-validation failure at steady state MUST be folded into this section's own backoff and re-locate behavior (PLY-131–132), never handled by a separate mechanism.

**[PLY-139]** Loss of a player's persisted relay address itself (as distinct from trust-anchor loss, PLY-063) — through storage corruption or a factory reset, for instance — MUST cause a player to fall back to Server-locating from the beginning (PLY-020), attempting all three address-acquisition paths, rather than entering any degraded or blocked state.

**[PLY-140]** Every retry this section governs MUST use the credentials and trust-anchor material a player currently holds (Channel tokens, TLS bootstrap fetch) without prompting a fresh pairing attempt, for as long as those remain valid under Channel tokens' and Steady-state pinning's own rules — the Reconnect state machine and Pairing redemption are triggered by disjoint conditions (transient connectivity loss versus an explicit credential-clearing event, PLY-136/063/073), never by the same failure.

### Screen liveness and recovery

**[PLY-150]** A relay MUST be able to attempt recovering a screen whose player has stopped presenting its assigned program in the foreground — a power interruption or a crash, for instance — by issuing whatever foreground-recovery command its device plane supports for that screen's own device class (`relay/1` REL-112, `device-class-registry` REG-066's `launch` command).

**[PLY-151]** A relay MUST NOT attempt recovery (PLY-150) unless the target device's own most recently reported canonical `state` is a member of its device class's `on` semantic group (`device-class-registry` REG-063) — a device reporting `standby`, `off`, or `unavailable` MUST be left alone, since a recovery attempt against a device that is not genuinely powered on would itself force an unintended wake.

**[PLY-152]** A relay MUST NOT attempt recovery (PLY-150) unless the target device's own most recently reported `app_type` attribute (`device-class-registry` REG-064) is `home`, `menu`, or absent/unknown — a device reporting `app_type: "app"` (a real foreground application) or `app_type: "screensaver"` MUST be left alone; recovery targets only a screen that has fallen back to an idle baseline or menu surface, never one a viewer or another application is legitimately using.

**[PLY-153]** PLY-151 and PLY-152 MUST both hold before any recovery attempt — a relay MUST NOT substitute one check for the other, since each independently rules out a distinct false-recovery case (a device that is not genuinely powered on but still answers device-plane queries; a genuinely-in-use foreground application).

**[PLY-154]** When a relay observes a target device's own canonical `state` newly enter its `on` semantic group (`device-class-registry` REG-063) — for instance, a transition out of `standby` or `off` — it MUST wait a bounded settle delay before issuing any recovery command against that device, so the command does not race the device's own boot or foreground sequence and pend without effect.

*draft-note: the settle delay is not fixed by any normative source yet. Proposed: on the order of one to two seconds — subject to revision once measured against real device boot behavior.*

**[PLY-155]** A relay MUST suppress recovery (PLY-150) entirely, regardless of PLY-151–152's own outcome, whenever the target screen's own currently active Lease has `display: "blank"` (Leases) — an intentionally blanked screen is not a recovery target.

**[PLY-156]** A relay MUST suppress recovery (PLY-150) entirely whenever the platform's own display-power schedule most recently commanded the target device `off` (Scope, out of scope — the schedule's own row shape is not this contract's concern, only that a relay honors its most recent power-off directive here) — an intentionally powered-off screen is not a recovery target, exactly as PLY-155 holds for an intentionally blanked one.

**[PLY-157]** PLY-151–156 together are this contract's complete statement of when foreground-recovery MAY be attempted: genuinely powered-on (PLY-151), not in genuine foreground use (PLY-152), settled past its own power-on transition (PLY-154), and not currently subject to an intentional blank or scheduled-off state (PLY-155–156).

**[PLY-158]** A player itself MUST defeat its own platform's idle/inactivity-triggered surfaces (for instance, a screensaver a player's runtime might otherwise engage) whenever it is actively assigned non-blank content (Leases) — a player showing static content generates no interaction a platform's own idle detection would otherwise observe, and this contract requires a player not to let that idle detection cover its assigned content. This requirement is independent of Screen liveness's relay-side recovery mechanism (PLY-150–157) and of server connectivity entirely — it is a player-local obligation.

## Wire shapes

```
// Discovery response (Server-locating, PLY-021) — a same-network discovery mechanism's
// unicast reply to a search for "urn:waiveo:service:player:1"; this contract fixes only
// the search target and the minimum resolvable content below, not a specific transport framing.
ST: urn:waiveo:service:player:1
LOCATION: https://198.51.100.12:5173/player/v1
USN: relay:01J8Z4K4N5P6Q7R8S9T0V1W3A1
```

```json
// PairingRequest (player -> relay, verification-disabled bootstrap connection, PLY-030)
// — human-entered cross-network path (pairing_code present)
{
  "hardware_id": "opaque-stable-device-id-9f2c7b1e",
  "pairing_code": "7K3M9-QX2F8",
  "capabilities": { "content_types": ["image", "video"], "player_version": "3.0.0" }
}
```

```json
// PairingResponse — pairing_code_hmac path, pending redemption
{
  "trust_anchors": [
    { "covers": ["player", "content"], "pem": "-----BEGIN CERTIFICATE-----\nMIIB6jCCAW+gAwIBAgI...\n-----END CERTIFICATE-----" }
  ],
  "authenticator_path": "pairing_code_hmac",
  "expected_authenticator": "b3f1c2a9d4e5f60718293a4b5c6d7e8f091a2b3c4d5e6f708192a3b4c5d6e7f",
  "pairing_status": "pending",
  "poll_token": "9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f"
}
```

```json
// PairingRequest — same-network path (no pairing_code)
{
  "hardware_id": "opaque-stable-device-id-9f2c7b1e",
  "capabilities": { "content_types": ["image", "video", "composed"], "player_version": "3.0.0" }
}
```

```json
// PairingResponse — fingerprint path, redeemed immediately
{
  "trust_anchors": [
    { "covers": ["player"], "pem": "-----BEGIN CERTIFICATE-----\nMIIB6jCCAW+gAwIBAgI...\n-----END CERTIFICATE-----" },
    { "covers": ["content"], "pem": "-----BEGIN CERTIFICATE-----\nMIIB7kCCAW+gAwIBAgJ...\n-----END CERTIFICATE-----" }
  ],
  "authenticator_path": "fingerprint",
  "trust_anchor_fingerprint": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85",
  "pairing_status": "redeemed",
  "channel_token": "ct_5f6e7a1b2c3d4e5f8091a2b3c4d5e6f7",
  "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
  "issued_at": 1752537000000,
  "expires_at": 1752623400000
}
```

```json
// PairingStatus poll response (player -> relay, pinned connection, PLY-034) — still pending
{ "pairing_status": "pending" }
```

```json
// PairingStatus poll response — redeemed
{
  "pairing_status": "redeemed",
  "channel_token": "ct_5f6e7a1b2c3d4e5f8091a2b3c4d5e6f7",
  "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
  "issued_at": 1752537000000,
  "expires_at": 1752623400000
}
```

```json
// PairingResponse — malformed for its declared path (missing expected_authenticator; PLY-059 forbids a relay from sending this)
{
  "trust_anchors": [ { "covers": ["player"], "pem": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----" } ],
  "authenticator_path": "pairing_code_hmac",
  "pairing_status": "pending",
  "poll_token": "9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f"
}
```

```json
// ProgramPull request (player -> relay, GET /player/v1/program?generation=rev-16&long_poll=true)
{
  "capabilities": { "content_types": ["image", "video"], "player_version": "3.0.0" },
  "generation": "rev-16"
}
```

```json
// ProgramPull response — unchanged
{ "type": "program.unchanged", "program_revision": "rev-16" }
```

```json
// Lease — scheduled priority, plain content
{
  "lease_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
  "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
  "program_revision": "rev-17",
  "priority": "scheduled",
  "display": "content",
  "content": [
    { "type": "image", "asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", "url": "https://198.51.100.20/cas/e3b0c4...", "expires_at": 1752541200000 }
  ],
  "issued_at": 1752537600000,
  "valid_until": 1752538500000,
  "signature": "ed25519-sig-computed-with-the-relays-player-facing-signing-key"
}
```

```json
// Lease — composed content item (image + video layers)
{ "type": "composed", "layers": [
  { "type": "image", "asset_ref": "sha256:aaaa...", "url": "https://198.51.100.20/cas/aaaa...", "expires_at": 1752541200000 },
  { "type": "video", "asset_ref": "sha256:bbbb...", "url": "https://198.51.100.20/cas/bbbb...", "expires_at": 1752541200000 }
] }
```

```json
// Lease — preempt priority, emergency-takeover asset
{
  "lease_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZG",
  "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
  "program_revision": "rev-18",
  "priority": "preempt",
  "display": "content",
  "content": [
    { "type": "image", "asset_ref": "sha256:cccc...", "url": "https://198.51.100.20/cas/cccc...", "expires_at": 1752541800000 }
  ],
  "issued_at": 1752538000000,
  "valid_until": 1752538300000,
  "signature": "ed25519-sig-computed-with-the-relays-player-facing-signing-key"
}
```

```json
// LeaseAck (player -> relay, POST /player/v1/lease/ack)
{ "lease_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZG", "accepted": true }
```

```json
// RenderStart (player -> relay, POST /player/v1/render/start)
{ "lease_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZG", "asset_ref": "sha256:cccc...", "ts": 1752538005000 }
```

```json
// RenderEnd / PlaybackReport (player -> relay, POST /player/v1/render/end — shape matches events/1 EVT-050 exactly)
{
  "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
  "asset_ref": "sha256:cccc...",
  "program_revision": "rev-18",
  "t_start": 1752538005000,
  "t_end": 1752538035000,
  "cause": "preempted",
  "completion": "completed"
}
```

```json
// StatusReport (player -> relay, POST /player/v1/status)
{
  "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
  "ts": 1752538035000,
  "power_state": "on",
  "app_state": "playing",
  "now_playing_content_id": null
}
```

```json
// TokenRenew request/response (player -> relay, POST /player/v1/token/renew)
{ "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE" }
```

```json
{ "channel_token": "ct_7a8b9c0d1e2f3a4b5c6d7e8f90a1b2c3", "screen_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZE", "issued_at": 1752623000000, "expires_at": 1752709400000 }
```

```json
// Problem — trust-anchor authentication failure at bootstrap (PLY-057, api/1 API-010 envelope)
{
  "type": "about:blank",
  "title": "Trust Anchor Authentication Failed",
  "status": 401,
  "code": "TRUST_ANCHOR_AUTH_FAILED",
  "trace_id": "01J8Z4K4N5P6Q7R8S9T0V1W3E1",
  "detail": "the computed pairing-code HMAC did not match expected_authenticator"
}
```

## Negotiation

- **Version selection** — a player declares its `major.minor` in `protocol_version` on every request this contract's capability fields apply to (PLY-010); a relay refuses on a major mismatch and otherwise proceeds under the shared major. There is no separate connection-level handshake distinct from the request itself, since this contract's transport is ordinary request/response, never a persistent connection.
- **N−1 compatibility** — a relay implementing minor `M` also implements minor `M-1` (PLY-011), so a player running one minor behind never sees a spurious major-mismatch refusal.
- **Capability declaration, not negotiation** — `content_types` (PLY-012–016) is a one-directional declaration a relay filters program assignment against; there is no bidirectional capability-flag exchange comparable to `relay/1`'s `features`, since a player's content-type support is a fixed property of its own build, not something a relay's own support varies by.
- **Out-of-band authenticator path selection** — `authenticator_path` (PLY-051) is determined by which of Server-locating's address-acquisition paths a pairing attempt used, never independently negotiated; a relay's declared path and a player's own record of what it sent must agree (PLY-051) or the attempt fails closed.
- **Credential renewal without re-pairing** — a channel token renews (PLY-074–075) without repeating Pairing redemption or Out-of-band cert authentication, for as long as a player's underlying trust-anchor material and screen identity remain valid; only trust-anchor loss (PLY-063), token revocation (PLY-073), or an unrecognized `screen_id` (PLY-075) forces a fresh pairing attempt.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `PROTOCOL_VERSION_UNSUPPORTED` | No minor of the player's declared major is implemented by the relay (PLY-010). | no |
| `PAIRING_CODE_INVALID` | The presented `pairing_code` is malformed or unknown (PLY-036). | no — obtain a fresh pairing code |
| `PAIRING_EXPIRED` | The pairing grant behind this code or attempt has passed its `ttl` (PLY-036, `relay/1` REL-121). | no — obtain a fresh pairing grant |
| `TRUST_ANCHOR_AUTH_FAILED` | Out-of-band cert authentication failed for this pairing attempt (PLY-057). | yes — a fresh pairing attempt with correct out-of-band material |
| `CHANNEL_TOKEN_INVALID` | The presented channel token is malformed or unknown. | no — renew or re-pair |
| `CHANNEL_TOKEN_EXPIRED` | The presented channel token's `expires_at` has passed (PLY-072). | yes — renew (PLY-074) |
| `CHANNEL_TOKEN_REVOKED` | The presented channel token names a `screen_id` present in `revocation_and_site.revoked`, or names a `screen_id` the relay no longer recognizes (PLY-072–073, PLY-075). | no — re-enter Pairing redemption |
| `CONTENT_TYPE_UNSUPPORTED` | A request implied a content type outside the caller's own declared `content_types` (PLY-012). | no — declare the type first, or the caller does not support it |
| `LEASE_UNKNOWN` | A `lease_id` referenced in a LeaseAck, RenderStart, or RenderEnd does not match the player's currently or most-recently active Lease (PLY-114). | no |
| `VALIDATION_FAILED` | The request body or a query parameter failed schema validation. | no — fix the request |
| `INTERNAL` | An unclassified server-side failure. | yes — with backoff |
| `UNAVAILABLE` | The relay or a dependency it needs is temporarily unable to serve the request. | yes — with backoff |

## Conformance notes

- Traceability map: `conformance/traceability/player-1.md` — maps every `PLY-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/player-1/` — one JSON case file per `case-id` referenced from the traceability map.
- **Residual risk, restated:** the Out-of-band cert authentication section (PLY-050–059) is a MUST-level requirement of this contract and is not optional in any conformant implementation; the MITM-at-first-pairing residual documented alongside it (Residual risk: MITM at first pairing) is a property of trust-on-first-contact pairing generally, not a gap this contract's own mechanism leaves unaddressed by omission — it is called out here so a reviewer does not mistake the mechanism's presence for the residual's absence.
- The pairing code's own character grammar (PLY-024's draft-note), the channel-token expiry duration (PLY-071), the ordinary and long-poll pull cadences (PLY-082), the status-report interval (PLY-120), the reconnect backoff curve and re-locate multiple (PLY-131–132), and the recovery settle delay (PLY-154) are all draft-note proposals pending real measurement; corpus cases exercise the shapes and orderings these rules produce, not elapsed real time.
- Whether a given screen's status reports are also forwarded as `events/1` `device.heartbeat` events depends on an open screen-to-device linkage question (PLY-121's draft-note); corpus cases exercise `content.played` sourcing (unconditional, PLY-113) and status-report shape and cadence (unconditional, PLY-120–123), not `device.heartbeat` forwarding specifically.
- The Out-of-band cert authentication primitives (a SHA-256 digest, an HMAC-SHA256) are confirmed only as ordinary, well-specified cryptographic constructions (Residual risk's draft-note); their availability and correctness on any specific client platform is not exercised by this static corpus and remains a hardware-verification item.
- A relay's own foreground-recovery command (Screen liveness and recovery, PLY-150) rides `relay/1`'s device plane (REL-112) and `device-class-registry`'s `launch` command (REG-066); this corpus exercises player/1's own gating requirements (PLY-151–157) against a given device-state input, not the device-plane command's own delivery, which those other contracts' corpora cover.
