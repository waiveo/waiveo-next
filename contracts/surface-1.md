# First-Party Surface Bridge

**Contract:** surface/1
**Version:** 1.0
**Status:** review

## Scope

surface/1 defines the one sanctioned escape hatch for a pack to ship real frontend code: how a signed first-party surface is mounted inside an origin-isolated iframe, the postMessage bridge between that iframe and its host page, the short-lived scoped tokens a mounted surface authenticates with, the isolation mechanism a deployment realizes between a surface's own origin and its host page's origin, and the trust boundary a mounted surface operates inside.

- In scope: the mount lifecycle (session issuance through an authenticated management-API operation, iframe construction, handshake, teardown); the postMessage bridge's message envelope and its closed verb allowlist; origin and window-identity verification on both the host page's and the iframe's own listener; surface-token issuance, shape, audience scoping, expiry, and renewal; the two named isolation mechanisms a deployment realizes an isolation origin through, and the precondition each depends on; the iframe's Content-Security-Policy posture and its `sandbox`/Permissions-Policy confinement; the trust boundary distinguishing a mounted surface from arbitrary pack code.
- Out of scope: the manifest field a pack uses to declare that it ships a surface (`manifest/1` MAN-063), and the trust-channel/signature check that qualifies a pack artifact as first-party (`manifest/1`; the software-artifact verification itself, `channel-index/1`); the platform's own TLS certificate provisioning for any hostname this contract mounts a surface under (deployment configuration); the session/credential model that authenticates the host page's own principal ahead of a mount request, and that principal's own role/scope-node permissions (a separate concern this contract consumes as a given input); the declarative UI grammar non-surface pack pages render against (`ui-schema/1`); the specific management-API operations a mounted surface calls once authenticated (`api/1` and each operation's own resource contract) — this contract governs only how a surface obtains and presents its credential, never the operations it spends that credential on; `api/1`'s Problem error shape and `Trace-Id` propagation (referenced, not redefined).

## Definitions

- **Surface** — a signed first-party pack's own bundled frontend code, mounted under this contract inside an origin-isolated iframe. The one exception to the platform's zero-frontend-code rule for packs (`manifest/1`).
- **Host page** — the top-level document, served from the platform's own primary origin, that embeds a surface's iframe.
- **Isolation origin** — the origin (scheme, hostname, and port) a surface's iframe is loaded from, realized by a deployment through exactly one of the two mechanisms this contract defines (Isolation mechanism) and always distinct from the host page's own origin.
- **Mount** — one instantiation of a surface's iframe inside a host page, identified by `mount_id`, from session issuance (Mount lifecycle) through teardown.
- **Bridge** — the postMessage channel this contract defines between a host page and one of its mounted surface's iframes.
- **Verb** — a named bridge operation. Only the enumerated verbs (Verb allowlist) may cross the bridge; this contract defines no mechanism for extending the set at runtime.
- **Surface token** — the short-lived, audience-scoped bearer credential a mount's surface presents to authenticate its own calls back to the platform; never the host page's own session cookie (Token issuance).
- **Audience** — the `{principal, pack_id, mount_id}` scope a surface token is bound to (Token scope, audience, and expiry); a request a server attributes to any other pack, mount, or principal MUST NOT be authorized by it.
- **Principal** — as defined in `api/1`: the authenticated caller a surface token is minted on behalf of. This contract narrows, and never widens, that principal's own permissions.

## Normative requirements

### Mount lifecycle

**[SUR-001]** A surface MUST be mounted only from a pack whose artifact signature has verified against the platform's software-artifact trust bundle (`channel-index/1`) under the `first-party` trust channel; a pack under any other trust channel MUST NOT be offered a mount under this contract, regardless of any manifest declaration it carries. A surface exists to be mounted only where the pack's manifest declares it in the surface-declaration slot (`manifest/1` MAN-063), which resolves it to exactly one bundled entry point a mount loads; a pack declaring no such slot ships no surface this contract can mount.

**[SUR-002]** A mount begins with the host page's own principal — already holding a valid platform session — requesting a surface session through an authenticated management-API operation. The operation's response MUST carry, at minimum, `{surface_token, audience, isolation_origin, mount_id, issued_at, expires_at}` (Wire shapes, `SurfaceSession`).

**[SUR-003]** The host page MUST construct the mount's iframe with its `src` naming a fixed entry path under the response's own `isolation_origin` (SUR-002) and carrying no query parameter, fragment, or other URL component derived from `surface_token` — a token MUST reach the iframe only via the bridge handshake (SUR-005), never via a URL a server access log, browser history entry, or `Referer` header could later disclose.

**[SUR-004]** Once the iframe's own document has loaded, the surface MUST send a `bridge.ready` message (Verb allowlist) carrying its own declared bridge protocol version. A host page receiving `bridge.ready` for a `mount_id` it did not itself just create (SUR-002) MUST discard it without response.

**[SUR-005]** Upon a valid `bridge.ready` (SUR-004), the host page MUST respond with exactly one `bridge.token` message (Verb allowlist) carrying that mount's `surface_token` and `expires_at`, targeted per SUR-033. The surface MUST treat any `bridge.token` arriving before its own `bridge.ready` was sent, or naming a `mount_id` other than its own, as a protocol violation and MUST discard it.

**[SUR-006]** A mount's `mount_id` MUST be a ULID, unique for the lifetime of the issuing host page's own document, and MUST accompany every bridge message and every management-API call authenticated by that mount's `surface_token` — this contract's own mount-scoped correlator, threading one mount's bridge traffic and the API calls it authorizes back to that single mount for its whole lifetime. `mount_id` is distinct from, and rides independently of, the per-message `trace_id` every bridge message separately carries (Bridge message envelope, SUR-010) and from `api/1`'s own per-operation `Trace-Id` propagation (`api/1` API-060–063): those correlate one message or one call, `mount_id` correlates everything belonging to one mount. This contract neither requires nor defines a dedicated `Mount-Id` transport header — `mount_id` reaches a management-API call however that call's own request shape already carries it, never via a header this contract does not itself define.

**[SUR-007]** Teardown (the host page removing a mount's iframe from its document, or navigating away) MUST cause the host page to stop dispatching or accepting any further bridge message carrying that `mount_id`; teardown MUST NOT itself be assumed to instantaneously invalidate the mount's `surface_token` server-side — Token scope, audience, and expiry states the token's own revocation and expiry guarantees independent of iframe teardown.

### Bridge message envelope

**[SUR-010]** Every bridge message MUST be a single object carrying at least `{type, mount_id, verb, body, trace_id}`, sent via `postMessage` with an explicit `targetOrigin` (never `"*"`, SUR-033) and no other transport. `type` MUST be one of `request`, `response`, or `event`; `verb` MUST be one of the enumerated names in Verb allowlist; `trace_id` MUST be a ULID, freshly generated by the message's sender when it is not itself already correlated to an inbound message being answered.

**[SUR-011]** A `request`-type message expecting a reply MUST be answered with exactly one `response`-type message carrying the same `trace_id`; a sender receiving no `response` within that verb's stated timeout (Verb allowlist) MUST treat the request as failed rather than wait indefinitely.

**[SUR-012]** A message whose top-level shape does not satisfy SUR-010 — a missing required field, a `verb` value outside the allowlist, or a `type` value outside the enumerated set — MUST be discarded by its receiver without dispatching to any verb-specific handler, and MUST NOT raise a JavaScript exception that could propagate beyond the receiver's own message listener.

### Verb allowlist

**[SUR-020]** The complete set of verbs permitted to cross a bridge is exactly: `bridge.ready`, `bridge.token`, `bridge.token_renew`, `bridge.resize`, `bridge.navigate_intent`, `bridge.close`, `bridge.error`. A conformant host page or surface implementation MUST NOT dispatch, and MUST discard on receipt, any message whose `verb` is not one of these — there is no mechanism by which a pack extends, aliases, or proxies an arbitrary operation through the bridge.

**[SUR-021]** `bridge.ready` (surface → host page, `event`) and `bridge.close` (surface → host page, `event`) carry no reply.

**[SUR-022]** `bridge.token` (host page → surface, `event`) delivers a mount's current `surface_token` (SUR-005) or a renewed one (SUR-024); it carries no reply.

**[SUR-023]** `bridge.resize` (surface → host page, `event`) and `bridge.navigate_intent` (surface → host page, `event`) carry the surface's own requested host-chrome adjustment or navigation; the host page MAY disregard either without that disregard being a protocol violation — both are advisory to the host page's own layout and routing, never a command it must obey. `bridge.navigate_intent`'s advisory status is enforceable because the surface holds no other means of navigating the host page at all: the iframe's own `sandbox` attribute structurally excludes top-level navigation (Iframe sandbox, SUR-065).

**[SUR-024]** `bridge.token_renew` (surface → host page, `request`) asks the host page to mint a fresh `surface_token` for the same `mount_id` ahead of the current one's `expires_at`; the host page's `response` MUST either carry a fresh `SurfaceSession`-shaped body (SUR-002) or an `error` body (Error taxonomy) — `SURFACE_SESSION_EXPIRED` when the host page's own principal session no longer permits re-issuance (SUR-053).

**[SUR-025]** `bridge.error` (surface → host page, `event`) reports a surface-side fault for host-page-level display or telemetry; it carries `{code, message}` and MUST NOT carry any field from Token issuance's own credential material.

### Origin verification

**[SUR-030]** The host page's bridge message listener MUST verify, before reading any field of an inbound event's `data`, that the event's `origin` is exactly equal (scheme, hostname, and port) to the mount's own `isolation_origin` (SUR-002) — never a prefix, suffix, or registrable-domain match. An event failing this check MUST be discarded before its `data` is parsed as a bridge message at all.

**[SUR-031]** The surface's own bridge message listener MUST symmetrically verify, before reading any field of an inbound event's `data`, that the event's `origin` is exactly equal to the host page's own origin. An event failing this check MUST be discarded before its `data` is parsed.

**[SUR-032]** Neither SUR-030 nor SUR-031's origin check alone is sufficient: both listeners MUST additionally verify the event's `source` equals the specific window reference established at mount time — the iframe's own `contentWindow`, from the host page's side (captured at iframe construction, SUR-003); `window.parent`, from the surface's side. A message whose `origin` matches but whose `source` does not MUST be discarded exactly as an origin mismatch is — an origin check alone does not distinguish between two same-origin windows (for instance two concurrent mounts of the same pack), and this contract treats that distinction as load-bearing, not incidental.

**[SUR-033]** Every `postMessage` call either side of a mount makes under this contract MUST supply an explicit `targetOrigin` argument equal to the intended recipient's own origin (the isolation origin from the host page's side, the host page's own origin from the surface's side) — a call supplying `"*"` is nonconformant regardless of whether the payload itself is later origin-checked by the receiver, because a mistargeted send already discloses the payload to whatever origin happens to be listening.

**[SUR-034]** A host page MUST reject (discard without dispatch) any bridge message naming a `mount_id` for which it holds no live mount record (SUR-002–006) — a message surviving SUR-030–033's origin and source checks but naming an unrecognized or already-torn-down `mount_id` MUST NOT be dispatched to a verb handler.

### Token issuance

**[SUR-040]** A `SurfaceSession` (SUR-002) MUST be issued only by a management-API operation that itself requires the host page's own pre-existing, valid platform session — the operation minting a surface token is itself an ordinary authenticated api/1 call, never one reachable without one (`api/1` API-090–092's security-override convention does not apply to it).

**[SUR-041]** `surface_token` MUST be an opaque bearer string carrying no independently-parseable authority — a server validating one MUST do so by server-side lookup or verification against its own issuance record, never by trusting claims decoded from the token's own contents by a relying party that only holds the token itself.

**[SUR-042]** `expires_at` (SUR-002) MUST bound `surface_token`'s validity to no more than 10 minutes from `issued_at` — short enough that a stolen token's usable window stays small, and comfortably longer than a `bridge.token_renew` (SUR-024) round trip so a live mount renews across the boundary without a user-visible interruption.

**[SUR-043]** A server MUST reject a request authenticated by a `surface_token` whose `expires_at` has passed with `SURFACE_TOKEN_EXPIRED` (Error taxonomy); the surface MUST treat this as a signal to renew (`bridge.token_renew`, SUR-024) rather than retry the same token.

**[SUR-044]** `surface_token` MUST be revocable ahead of its natural expiry — at minimum, on host-page-initiated teardown proactively revoking it (rather than only relying on SUR-042's bound to eventually close the window) and on revocation of the underlying principal's own platform session. A server MUST reject a request authenticated by a revoked `surface_token` with `SURFACE_TOKEN_REVOKED` (Error taxonomy), distinguishably from `SURFACE_TOKEN_EXPIRED`.

**[SUR-045]** **A platform session cookie MUST NOT be accepted, alone or in combination with any other ambient credential, as authentication for a request this contract attributes to a mount's surface.** A server receiving such a request MUST require `surface_token` presented explicitly (e.g. an `Authorization` header) and MUST NOT fall back to, or additionally honor, a cookie that happens to accompany the same request — this holds under both isolation mechanisms (Isolation mechanism) equally; neither mechanism is a substitute for this requirement, and this requirement is not a substitute for either mechanism.

### Token scope, audience, and expiry

**[SUR-050]** `audience` (SUR-002) MUST be the object `{principal, pack_id, mount_id}` — the host page's own authenticated principal (Definitions), the pack the mount was issued for, and the specific mount instance.

**[SUR-051]** A server validating a `surface_token`-authenticated request MUST refuse it with `SURFACE_TOKEN_AUDIENCE_MISMATCH` (Error taxonomy) if the request targets any pack other than the token's own `audience.pack_id` — a token minted for one pack's surface MUST NOT authorize a call attributable to a different pack's own actions or data collections, mirroring the per-pack data isolation `ctx/1` enforces host-side (`ctx/1` CTX-040's `COLLECTION_NOT_OWNED`) at this contract's own token-authenticated boundary instead.

**[SUR-052]** A `surface_token`'s authority MUST NOT exceed its `audience.principal`'s own current role/scope-node permissions at the moment of each request — this contract's token narrows a principal's reach to one pack and one mount; it never grants authority the principal does not independently hold, and a permission revoked from the principal after issuance MUST take effect on the very next request the token authenticates, not only at the token's own next renewal.

**[SUR-053]** `bridge.token_renew` (SUR-024) MUST cause the issuing server to re-evaluate the host page's own platform session as of the renewal request — a renewal is exactly as strong a check as original issuance (SUR-040), never a blind re-stamp of a new `expires_at` onto an otherwise-unexamined mount. A host page whose own platform session has itself expired or been revoked MUST fail renewal with `SURFACE_SESSION_EXPIRED` (SUR-024, Error taxonomy) rather than mint a fresh `surface_token`.

**[SUR-054]** Two concurrent mounts of the same pack under the same principal (e.g. two browser tabs) MUST receive independently-scoped `surface_token`s, each carrying its own `mount_id`; revoking or expiring one MUST NOT affect the other's continued validity.

### Isolation mechanism

**[SUR-060]** A deployment MUST realize every mount's `isolation_origin` through exactly one of the following two mechanisms, applied uniformly across every surface it mounts:

- **(a) Second hostname** — the isolation origin uses a hostname distinct from the host page's own hostname, with a server certificate chaining to the same TLS trust the host page's own origin uses.
- **(b) Same-host port isolation** — the isolation origin shares the host page's own hostname at a distinct port, relying on Token issuance's bearer-credential requirement (SUR-040–045) in place of any hostname-level separation.

**[SUR-061]** Mechanism (b) MUST NOT be treated as providing credential isolation by itself: a cookie's `SameSite` scoping does not distinguish port, so a platform session cookie set without a narrow, host-only scope would be presented by the browser to both the host page's origin and a same-host isolation origin alike. This is exactly the gap SUR-045's prohibition on cookie-based surface authentication closes; mechanism (b) is conformant only in combination with SUR-045, never on its own.

**[SUR-062]** Regardless of mechanism, the platform's own session cookie MUST be issued host-only (no `Domain` attribute broadening its scope beyond the exact host that set it) — a precondition this contract's isolation guarantee depends on even under mechanism (a), where a broadened `Domain` could otherwise still leak the host page's cookie to a sibling isolation hostname sharing a parent domain. Cookie issuance itself is defined elsewhere; this contract states the precondition because its own security argument depends on it.

**[SUR-063]** This contract fixes no granularity requirement for how many distinct isolation origins a deployment allocates (one shared origin for every mounted surface, one per pack, or one per mount) — any granularity is conformant provided Token scope, audience, and expiry's audience scoping (SUR-050–052) is what a server actually relies on to keep one mount's authority from reaching another's, never an assumption that distinct browsing contexts alone provide that isolation. A shared isolation origin carries no client-storage-confidentiality guarantee between co-located surfaces regardless of this audience scoping (SUR-064).

*draft-note: no normative source yet states a preferred granularity; a deployment allocating one isolation origin per pack (rather than one shared origin) additionally gains browser-level separation between two different packs' surfaces as defense in depth, but this contract does not require it.*

**[SUR-064]** Same-origin client-side storage (`localStorage`, `IndexedDB`, `BroadcastChannel`, and any other storage or messaging primitive the browser scopes to origin rather than to frame) is NOT isolated by `surface_token`'s audience scoping (Token scope, audience, and expiry) — a surface token bounds which server-side calls a mount may authorize, never which same-origin browsing contexts may read or write which same-origin client-side storage. Under a shared isolation origin (SUR-063), a co-located sibling surface — a different mount, of a different pack, sharing that same isolation origin — can read, write, or otherwise reach another mount's own client-side storage ambiently, entirely outside the token-authenticated request path this contract otherwise governs. The shared-isolation-origin mode (SUR-063) therefore carries NO client-storage-confidentiality guarantee between co-located surfaces. A surface whose own functionality requires confidential client-side state — state it cannot safely expose to another co-located surface sharing its isolation origin — MUST be allocated its own dedicated isolation origin, not sharing one with any other pack's surface.

### Iframe sandbox

**[SUR-065]** The host page MUST construct every mount's iframe (SUR-003) with a `sandbox` attribute whose token set excludes `allow-top-navigation`: a surface's own code MUST NOT hold the capability to navigate the top-level host page directly — `bridge.navigate_intent` (SUR-023) MUST be a surface's only path to requesting a host-page-level navigation, never a capability its own browsing context holds by default.

**[SUR-066]** `allow-top-navigation-by-user-activation` MAY be included only where a specific surface's own functionality requires a user-activated top-level navigation and that inclusion is explicitly justified for that mount; it MUST NOT be included by default alongside every mount the way SUR-067's baseline tokens are.

**[SUR-067]** The `sandbox` attribute MUST additionally include `allow-scripts` (the surface is executable frontend code, Definitions) and `allow-same-origin` (without it, the iframe would load into a unique opaque origin on every navigation, breaking the `isolation_origin` identity SUR-002's, SUR-030's, and SUR-031's origin checks depend on) — and MAY include `allow-forms` or other tokens only where a specific surface's own functionality requires them.

**[SUR-068]** The host page MUST NOT delegate any Permissions-Policy feature (the iframe's `allow` attribute — camera, microphone, geolocation, and any similarly gated capability) to a mount's isolation origin by default; every such feature starts denied to a mounted surface. A deployment MAY delegate a specific feature to a specific mount only where the mounting pack's own manifest declares a need for it, mirroring `connect-src`'s own manifest-declared-egress pattern (CSP posture, SUR-073).

### CSP posture

**[SUR-070]** The host page's own Content-Security-Policy MUST include a `frame-src` (or `child-src`, where `frame-src` is absent) directive naming exactly the isolation origin(s) it mounts — never a wildcard, and never a value broad enough to admit an origin this contract did not itself issue a `SurfaceSession` for.

**[SUR-071]** A surface's own response MUST send `frame-ancestors` naming exactly the host page's own origin — never `*`, and never omitted (an omitted `frame-ancestors` is not equivalent to a narrow one; the default is permissive).

**[SUR-072]** A surface's own `script-src` directive MUST NOT include `unsafe-inline` or `unsafe-eval` — a signed first-party surface ships as a static, precompiled bundle (`manifest/1`'s pack-sealing rule extends to surface bundles), and no requirement in this contract depends on runtime script evaluation from a string.

**[SUR-073]** A surface's own `connect-src` directive MUST be no broader than the platform's own management-API origin plus any host named in the mounting pack's own manifest `egress` allowlist — mounting as a surface MUST NOT itself widen a pack's network reach beyond what its manifest already declares for any other pack (`manifest/1`).

### Trust boundary

**[SUR-080]** A surface's own bundled code runs with its isolation origin's full browsing-context capability (real DOM, real `fetch`); this contract's isolation and token guarantees (Origin verification, Token issuance) bound WHICH origin that code executes at and WHAT credential authenticates its calls — they impose no restriction on what the code itself computes. Every restriction on what a surface may reach is therefore enforced at the boundary the code cannot itself bypass by construction: origin/window verification on the bridge (Origin verification), audience scoping on the token (Token scope, audience, and expiry), and CSP (CSP posture) — never by any property of the surface's own trustworthiness as code.

**[SUR-081]** A surface MUST NOT be able to reach `ctx/1` directly — `ctx/1`'s bindings connect a host-managed pack process to its host, never a browser context; a surface's only path to platform capability is the management API, authenticated per Token issuance and scoped per Token scope, audience, and expiry.

**[SUR-082]** SUR-001's trust-channel restriction is enforced independent of, and prior to, any per-pack manifest opt-in: a pack whose trust channel is `verified`, `community`, or `dev` MUST NOT be mounted under this contract even if its own manifest declares a surface entry point — the trust-channel check is a precondition to consulting that declaration at all, never a check performed alongside or after it.

**[SUR-083]** This contract's guarantees describe confinement of a legitimately-mounted, signed first-party surface's credential and browsing context; they are not a sandbox for adversarial code. A surface's own code is trusted at the level its signature and `first-party` trust channel (SUR-001) attest to — the same trust basis the platform extends to any other first-party-channel artifact (`channel-index/1`) — and this contract's isolation exists to bound the blast radius of a defect or compromise in that trusted code (a leaked token authenticates one pack, one mount, one principal's own permissions — never the host page's own session, never another pack), not to treat the surface as untrusted the way a community pack's declarative-only contribution is treated.

### Bridge protocol versioning

**[SUR-090]** `bridge.ready`'s `bridge_version` (Wire shapes) MUST be a `major.minor` string. A host page receiving a `bridge_version` whose major does not match the version this contract's implementation supports MUST refuse the mount — tearing the iframe down and never issuing `bridge.token` — rather than attempt to speak a bridge protocol it does not implement. A minor mismatch (an unrecognized but structurally-additive minor) MUST NOT itself cause refusal; unrecognized optional fields on an otherwise-valid message are ignored.

## Wire shapes

```json
// SurfaceSession — response of the management-API operation that mints a mount (SUR-002)
{
  "surface_token": "sft_8f14e45fceea4b3e8c1e1a1b2c3d4e5f6a7b8c9d",
  "audience": {
    "principal": "01J8Z3K4N5P6Q7R8S9T0V1W2X3",
    "pack_id": "waiveo/slidecast",
    "mount_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4"
  },
  "isolation_origin": "https://studio.box.local:8443",
  "mount_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "issued_at": 1752623000000,
  "expires_at": 1752623600000
}
```

```json
// bridge.ready — surface -> host page, event (SUR-004)
{
  "type": "event",
  "mount_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "verb": "bridge.ready",
  "body": { "bridge_version": "1.0" },
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y5"
}
```

```json
// bridge.token — host page -> surface, event (SUR-005)
{
  "type": "event",
  "mount_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "verb": "bridge.token",
  "body": {
    "surface_token": "sft_8f14e45fceea4b3e8c1e1a1b2c3d4e5f6a7b8c9d",
    "expires_at": 1752623600000
  },
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y5"
}
```

```json
// bridge.token_renew — surface -> host page, request (SUR-024)
{
  "type": "request",
  "mount_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "verb": "bridge.token_renew",
  "body": {},
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y6"
}
```

```json
// bridge.token_renew reply — host page -> surface, response (SUR-024, success)
{
  "type": "response",
  "mount_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "verb": "bridge.token_renew",
  "body": {
    "surface_token": "sft_1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d",
    "expires_at": 1752624200000
  },
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y6"
}
```

```json
// bridge.error — surface -> host page, event (SUR-025)
{
  "type": "event",
  "mount_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y4",
  "verb": "bridge.error",
  "body": { "code": "RENDER_FAILED", "message": "failed to load editor canvas" },
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y7"
}
```

## Negotiation

`bridge.ready`'s `bridge_version` field (Bridge protocol versioning, SUR-090) is this contract's only version-negotiation point — there is no separate handshake round-trip beyond the mount lifecycle already defines (Mount lifecycle):

- **Major mismatch** — refuse the mount outright (SUR-090).
- **Minor mismatch** — proceed; unrecognized fields are additive and ignored.
- There is no protocol-level deprecation timeline yet defined for a bridge verb (Verb allowlist); this contract's verb set is fixed for major version 1, and a future minor may only add a verb, never remove or repurpose one already listed (SUR-020).

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `SURFACE_TOKEN_EXPIRED` | A request's `surface_token` carries an `expires_at` in the past (SUR-043). | yes — renew (`bridge.token_renew`, SUR-024) and retry |
| `SURFACE_TOKEN_REVOKED` | A request's `surface_token` was revoked ahead of its natural expiry (SUR-044). | no — re-mount |
| `SURFACE_TOKEN_AUDIENCE_MISMATCH` | A request targeted a pack, mount, or principal other than the presented token's own `audience` (SUR-051). | no |
| `SURFACE_SESSION_EXPIRED` | A `bridge.token_renew` (or initial mint, SUR-040) could not proceed because the host page's own platform session no longer permits it (SUR-053). | no — the host page must re-authenticate its own session first |
| `VERB_NOT_ALLOWLISTED` | A bridge message named a `verb` outside Verb allowlist's enumerated set (SUR-020). | no |
| `BRIDGE_VERSION_INCOMPATIBLE` | `bridge.ready`'s `bridge_version` major does not match the host page's implemented major (SUR-090). | no |

## Conformance notes

- Traceability map: `conformance/traceability/surface-1.md` — maps every `SUR-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/surface-1/` — one JSON case file per `case-id` referenced from the traceability map.
- Origin and window-identity verification (Origin verification) are properties of a specific runtime's `postMessage` listener implementation; this corpus expresses them as input/expected-decision pairs (an event's `origin`/`source` versus the receiver's discard-or-dispatch decision) rather than driving a real browser — a future browser-level conformance driver is expected to replay these same cases against a real `iframe`/`window.postMessage` pair, not redefine them.
- SUR-042's proposed token-lifetime bound and SUR-090's version-negotiation behavior are exercised for shape and sequencing, not elapsed real time; timing-dependent behavior is exercised against an injectable clock in a driver harness, consistent with this platform's other contracts.
- The manifest field that declares a pack ships a surface (SUR-001's draft-note) is out of this contract's own scope and is not exercised by this corpus; cases that need a mount to begin treat a pack's surface-eligibility as a given, already-resolved input.
- The principal/role/session model Token scope, audience, and expiry presupposes (SUR-052–053) is out of this contract's scope (Scope) and is not exercised by this corpus; cases that need a principal or a platform session treat either as a given, opaque input or a given, opaque outcome.
