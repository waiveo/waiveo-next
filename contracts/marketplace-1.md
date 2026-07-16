# Marketplace

**Contract:** marketplace/1
**Version:** 1.0
**Status:** draft

## Scope

marketplace/1 defines the pack-registry and publisher-identity layer a `manifest/1` pack or a content-package resolves through before a client ever reaches `channel-index/1`'s own signed-index verification: the publisher namespace a `pack_id` (`manifest/1` MAN-001) belongs to and how that namespace is registered, protected against collision and resurrection, and rebound across a key rotation; the trust-channel tier (`first-party`, `verified`, `community`, `dev`) a published pack or content-package version carries and the automated bar a version must clear to carry `verified`; the marketplace-specific fields a registry's signed index carries for `pack`/`content-package` kind entries on top of `channel-index/1`'s already-specified `platform-release`/`relay-release` kinds; the ordered, per-source-keyed registry source list a client resolves against; the content-package artifact kind and its `rule-template` subtype, including the input form a rule template's own parameters are collected through; and the default update policy an install auto-tracks its trust channel under. This contract resolves `channel-index/1`'s own deferred "a marketplace pack or content-package's own index entry — a distinct concern that may reuse this same signed-index envelope in a separate contract" (`channel-index/1` Scope).

- In scope: publisher registration, namespace reservation (fixed reserved prefixes/names and dynamic confusable collision), resurrection-attack defense, and registry-mediated key rebind; the trust-channel enum and its consent/provenance posture, including the `dev` channel's containment guarantees and developer mode's pack-acceptance semantics (the toggle mechanism itself is `security-model`'s, SEC-011); the verified bar's enumerated automated checks and its own non-endorsement UI-copy requirement; verified-status revocation as a graduated response distinct from a yank; the `pack`/`content-package` extensions to `channel-index/1`'s artifact-entry schema (the widened `kind` and `status` enums, `superseded_by`, and `hold_hours`'s extended applicability and its tightened security-flag authentication bar); the trust-channel-to-version pointer a registry index additionally carries; the ordered registry source list, per-source signing-key-id binding, and the source restriction on reserved-namespace resolution; the `content-package` artifact kind and its `rule-template` subtype, including the rule-template input form's reuse of `ui-schema/1`'s binding grammar and `api/1`'s selector grammar; the cross-pack-reference qualification rule; the default channel-auto-tracking update policy, its consent-diff classification, and required-pack revert-to-known-good; the install-record pin shape.
- Out of scope: the signed-index envelope itself — its four role layers, the client verification order, monotonic-version/max-age freshness, and the revocation feed's transport/signing/freshness discipline (`channel-index/1`, referenced and extended per above, never redefined); the wrapped-secret key hierarchy and the workspace signing key (`security-model` Key hierarchy) — a publisher's own signing-key identity belongs to `channel-index/1`'s software-artifact trust bundle, a distinct, unrelated trust root (`security-model` SEC-044); a pack manifest's own field validation beyond the `pack_id` namespace and cross-reference rules this contract adds (`manifest/1`); the declarative UI grammar's own syntax and the label-selector grammar's own syntax (`ui-schema/1`, `api/1` — reused by reference for the rule-template input form only); the pack ↔ host runtime protocol, process supervision, sandboxing, and restart policy (`ctx/1`; per `ctx/1`'s own Scope, sandboxing is presently "a host concern, not a wire contract" owned by no contract in this corpus yet — flagged, not invented here); `rules/1`'s own rule-template parameter-substitution semantics and per-instance execution-class classification ("Rule templates (blueprint substitution)") — this contract governs a rule template's distribution, signing, revocability, and input form only, never what a bound instance's trigger/condition/action fields mean once substituted; the concrete identity-proofing procedure behind a key-rebind's non-routine fallback path (deployment/operational, flagged via draft-note); the per-scope pin/hold override's own UX (noted to exist, not specified here); the pack update/rollback execution mechanism itself — the mechanics of actually applying, or rolling back, a resolved version on a running deployment (a host/runtime concern distinct from the resolution-and-policy layer this contract defines, exactly as `channel-index/1` itself stops at "verified and not revoked," never "install it"); `api/1`'s Problem error shape and `Trace-Id` propagation, for wherever this contract's own registration/resolution errors (Error taxonomy) surface through the management API (referenced, not redefined, exactly as `channel-index/1` and `security-model` reuse it).

## Definitions

- **Publisher** — as introduced in `manifest/1` (Definitions: "a registered signing identity that owns one or more pack namespaces"); this contract is the registration, protection, and rebind authority that definition depends on. A publisher is a **registered keypair identity**: a namespace (below) plus the currently-bound signing key-id(s) authorized to sign a `pack_id` under it.
- **Namespace** — the `publisher` segment of a `pack_id` (`manifest/1` MAN-001's `<publisher>/<name>` form): the unit this contract's registration, reservation, and confusable-collision rules operate on.
- **`pack_id`** — as defined in `manifest/1` MAN-001: `<publisher>/<name>`, each segment matching `^[a-z][a-z0-9-]{1,38}$`. This contract treats `pack_id` as the permanent identity for both a `manifest/1` pack and a content-package (Content-package subtypes and rule templates) alike — one publisher-namespace registry serves both.
- **Trust channel** — one of `first-party`, `verified`, `community`, `dev` (Trust channels and production install): the provenance/review tier a published pack or content-package version carries. Not to be confused with `channel-index/1`'s own **Channel** (its Definitions): a distribution-stream identifier (e.g. `platform-train/stable`) scoping one entire signed index. A registry source's signed index rides inside a `channel-index/1` Channel; trust channel is an orthogonal, marketplace-specific axis this contract adds within it (Marketplace index entry and pack kinds).
- **Content-package** — a marketplace artifact `kind` (alongside `pack`, `platform-release`, `relay-release`) whose primary contribution is authored content rather than executable pack logic, distributed and revoked through the same signed-index and trust-channel machinery as a pack, and identified by the same `pack_id`-shaped namespace (Definitions: `pack_id`). This version defines exactly one content-package subtype, `rule-template` (Content-package subtypes and rule templates).
- **Rule template** — a content-package subtype: a signed, channeled, revocable parameterized automation. Its own parameter-substitution semantics belong to `rules/1` ("Rule templates (blueprint substitution)"); this contract defines only its distribution and its input form (Content-package subtypes and rule templates).
- **Verified bar** — the enumerated set of automated checks and the conformance pass a pack or content-package version must clear to carry the `verified` trust channel (The verified bar).
- **Registry source** — one entry in a client's own ordered, configured list of index-hosting locations (Registry sources), each publishing the identical `channel-index/1` index format.
- **Channel pointer** — the marketplace-specific record mapping a trust channel to the version a registry source currently resolves that (`pack_id`, trust channel) pair to (Marketplace index entry and pack kinds).
- **Verified attestation** — a synthetic, marketplace-minted revocation-feed subject (Verified-status revocation) distinct from a pack's own release artifact, whose revocation demotes a specific pack version out of the `verified` trust channel without yanking the version itself.
- **Install record** — the pinned `(pack, trust channel, resolved version, source)` tuple a client persists for an installed pack or content-package (Update default and install records).

## Normative requirements

### Publisher identity and namespace

**[MKT-001]** A `pack_id`'s `publisher` segment (`manifest/1` MAN-001) MUST resolve to a registered publisher (Definitions) before any entry under it is accepted from any registry source. `waiveo` MUST be the sole reserved exact publisher name for first-party status: a `pack_id` whose publisher segment is exactly `waiveo` MUST carry the `first-party` trust channel (Trust channels and production install), and no other publisher segment MAY.

**[MKT-002]** A publisher's own signing key-id(s) MUST be bound to its namespace by a root-signed **publisher-namespace delegation** (`channel-index/1` CHI-012) — a targets-style delegation (`channel-index/1` CHI-003) scoped to that one publisher's namespace, distinct from and finer-grained than the channel-grained delegation `channel-index/1` CHI-009 provides — never a self-asserted binding, never inferred from which registry source served an entry, and never inferred from the order entries happen to appear in an index. This publisher-namespace-to-key-id binding lives in `channel-index/1`'s own root-signed trust bundle (`channel-index/1` Definitions: Trust bundle; CHI-002, CHI-012) — a distinct, unrelated trust root from `security-model`'s own wrapped-secret key hierarchy (`security-model` SEC-044); a publisher key is never protected by, derived from, or verified against the box key, data key, or workspace signing key that hierarchy defines. The concrete resolution-time verification this binding enables — that an entry claiming a namespace actually chains to that namespace's delegated key — is MKT-009; source order and index-signature validity never substitute for it.

**[MKT-003]** Publisher registration MUST reject a requested namespace matching, by exact value, either of the reserved publisher names `waiveo` (MKT-001) or `dev` (Trust channels and production install's `dev` publisher), and MUST reject a requested namespace matching, by leftmost-prefix, any of the reserved prefixes `waiveo-`, `official-`, `system-`, `local-` — refused as `NAMESPACE_RESERVED` (Error taxonomy). This reserved set is closed at this version; growth is a marketplace/1 minor.

**[MKT-004]** Publisher registration MUST additionally reject a requested namespace that is **confusable** with any name in the confusable baseline: a Unicode-homoglyph match after skeleton normalization, or a Levenshtein edit distance of 1 in **either** direction — an insertion, deletion, or substitution relative to the compared name, evaluated regardless of which of the two names is longer, so a one-character deletion of a longer existing name (as much as an addition to a shorter one) is caught — refused as `CONFUSABLE_COLLISION` (Error taxonomy), distinct from MKT-003's fixed reserved-set check. The confusable baseline MUST comprise all three of: (a) every currently-registered publisher namespace; (b) every **deleted** publisher namespace (MKT-005) — so a confusable of a retired-but-formerly-trusted name is refused just as MKT-005 refuses its exact former value, deletion being as permanent for the confusable check as for the exact-value check; and (c) the fixed reserved names and prefixes of MKT-003 (`waiveo`, `dev`, and the `waiveo-`/`official-`/`system-`/`local-` prefixes) — so a homoglyph of a reserved name (for instance a Cyrillic-`а` lookalike of `waiveo` that is neither an exact nor a leftmost-prefix match) is refused here rather than slipping past MKT-003's exact/prefix path. A successful registration itself becomes a confusable-check input for every later registration attempt — reservation is cumulative, not limited to a fixed list.

*draft-note: the exact skeleton-normalization algorithm (e.g. a Unicode confusables mapping) and the edit-distance threshold above are this contract's own proposal, not derived from a decided spec; confirm before this contract leaves draft.*

**[MKT-005]** A publisher's namespace MUST NOT be re-registrable, by any party, once that publisher has been deleted — an exact-value check independent of, and in addition to, MKT-004's confusable check, with no expiry: deleted is permanent. A registration attempt against a deleted publisher's exact former namespace MUST be refused as `RESURRECTION_ATTEMPT` (Error taxonomy), never silently treated as a fresh registration.

**[MKT-006]** A publisher's signing key MUST be rotatable through a **registry-mediated rebind**: a request that changes which key-id(s) a publisher's namespace delegation (MKT-002) currently authorizes, without changing the namespace itself and without requiring the publisher to abandon it. A rebind request MUST be authenticated by exactly one of: (a) a signature over the rebind request from the publisher's own currently-bound (outgoing) key — the **routine path**; or (b) an identity-proofed fallback path, used only where the outgoing key is unavailable. A rebind authenticated by neither MUST be refused as `REBIND_UNAUTHENTICATED` (Error taxonomy) — an incoming key alone, however validly generated, MUST NOT be sufficient to claim an existing namespace, the same principle `channel-index/1` CHI-002 applies to its own root-key rotation ("a rotation is never authorized by the incoming keys alone"), applied here at publisher-namespace grain rather than at the root role.

**[MKT-007]** A successful rebind (either path, MKT-006) MUST cause the outgoing key-id to be entered into the revocation feed's key/trust-class revocation (`channel-index/1` CHI-070 `revoked_keys`, enforced per CHI-071 unmodified) — a rebind's whole purpose is to recover a namespace from a suspected-compromised key, so the outgoing key MUST NOT remain trusted for that namespace afterward. This revocation MUST fire only once MKT-010's notify-plus-veto window has elapsed without a veto, never instantaneously on the rebind request — otherwise the revocation itself becomes the lock-out step of a hijack (MKT-010). This contract defines no second revocation mechanism for this; it is the ordinary, unmodified CHI-071 key-revocation path, triggered by a rebind once MKT-010's window closes.

*draft-note: the identity-proofed fallback path's own concrete evidentiary procedure (what "identity-proofed" requires in practice — e.g. domain-control proof, an out-of-band human review step) is deployment/operational and not fixed by this contract, the same custody-mechanics carve-out `channel-index/1` CHI-006 gives its own role-key ceremonies; this contract fixes only that the fallback path MUST exist, MUST be distinguishable in the audit trail from the routine old-key-signed path, and MUST NOT be satisfiable by the bare say-so of whoever files the request.*

**[MKT-008]** Every reference to another pack or content-package — in a manifest field, a content-package's own structure, or a marketplace index entry's `superseded_by` (Marketplace index entry and pack kinds) — MUST be a fully publisher-qualified `pack_id` (`manifest/1` MAN-001's `<publisher>/<name>` form); a bare, unqualified name MUST be refused as `CROSS_PACK_REFERENCE_UNQUALIFIED` (Error taxonomy) wherever this contract's own fields are validated. `manifest/1` MAN-001 fixes the grammar a `pack_id` itself matches; this contract fixes the rule that every cross-reference to one MUST use that full form.

**[MKT-009]** A marketplace entry claiming a publisher namespace — a `pack` or `content-package` entry (Marketplace index entry and pack kinds), or a channel pointer (MKT-046) resolving to one — MUST carry, and a verifier MUST check at resolution time, a **per-publisher signature**: the entry's own `publisher_signature` (Wire shapes), a detached signature over at least the entry's `artifact_id`, `version`, and `digest`, produced by a key-id the root-signed publisher-namespace delegation (`channel-index/1` CHI-012) authorizes for that `pack_id`'s exact namespace (MKT-002). This is a distinct check from, and in addition to, the channel/source-grained index signature `channel-index/1` CHI-050 already verifies: CHI-050 establishes only that a source's index is validly signed by that source's *own* delegated channel key, never that the source is entitled to the namespace an entry inside it claims. An entry is trusted for install, `verified`-bar promotion (MKT-030), or channel-tracking (MKT-090) **only if** its `publisher_signature` so chains; one that does not — including one carrying no `publisher_signature` at all — MUST be refused as `PUBLISHER_SIGNATURE_UNVERIFIED` (Error taxonomy). This check MUST be performed at resolution time, before any trust, install, or channel-tracking decision, and MUST be independent of which registry source served the entry and of that source's position in the ordered source list (MKT-061): a source whose index-signing key a client has configured (MKT-061) MUST NOT thereby be able to serve a trusted entry under a namespace CHI-012 never delegated to it. Source order and a valid index signature establish that a source may speak; they never establish *for which namespace* — only the CHI-012 chain does.

**[MKT-010]** A key rebind (MKT-006) is a security-sensitive operation and MUST carry its own safeguard against being weaponized for takeover. Because the routine path (MKT-006(a)) trusts a signature from the currently-bound key and a successful rebind revokes the outgoing key (MKT-007), a party holding only the *current* key could otherwise rebind a namespace to an attacker-controlled key and, in the same step, strip the legitimate owner's key of authority — a silent, immediate hijack. To prevent this, every rebind MUST: (1) emit a mandatory, non-suppressible notification and `audit.event` to every `owner`/publisher principal, modeled on `security-model` SEC-063's unconditional recovery-notify (not configurable away by any role); and (2) impose a bounded effective-delay / challenge window before the rebind takes effect **and** before MKT-007's outgoing-key revocation fires — during which the outgoing key retains its authority and the legitimate owner MAY veto the rebind. Only after the window elapses with no veto does the rebind take effect and the outgoing key enter revocation. The routine and identity-proofed-fallback paths (MKT-006) MUST remain distinguishable in the audit trail throughout (MKT-007). The window's exact duration is deployment/operational, the same custody carve-out MKT-006's own draft-note describes; this contract fixes only that a non-zero notify-plus-veto window MUST exist and MUST gate MKT-007.

### Trust channels and production install

**[MKT-020]** A published pack or content-package version MUST carry exactly one trust channel: `first-party`, `verified`, `community`, or `dev` — a value outside this closed set MUST be refused as `TRUST_CHANNEL_UNKNOWN` (Error taxonomy). Growth of this enum is exclusively a marketplace/1 minor, the same discipline `ui-schema/1` UIS-002 and `rules/1` RUL-001 apply to their own closed vocabularies.

**[MKT-021]** `first-party` MUST be used if and only if the version's `pack_id` publisher segment is `waiveo` (MKT-001); an entry of any other publisher segment claiming `first-party` — whether as its own trust channel or as a key of its channel pointer (Marketplace index entry and pack kinds) — MUST be refused as `FIRST_PARTY_CHANNEL_NAMESPACE_MISMATCH` (Error taxonomy), independent of, and in addition to, whichever registry source served it.

**[MKT-022]** **Production install** MUST enforce every other trust channel's own signature/provenance bar (MKT-023's `verified` bar; an ordinary publisher-namespace signature check for `community`, MKT-009) at all times on the cloud tier — where developer mode has no expression to relax it: developer mode is a self-hosted, owner-gated box setting, and the cloud tier structurally lacks the local escape hatch it rides on (`security-model` SEC-060's cloud "structurally absent, not merely access-denied" posture; the cloud/self-hosted tier split itself is `security-model` SEC-042). On the self-hosted tier, production install MUST enforce it identically unless **developer mode** is enabled; developer mode itself is a persistent, owner-role-gated, consent-gated, per-pack acceptance setting whose toggle and whose every dev-pack acceptance emit their own audit event — this contract adds no second toggle mechanism, reusing `security-model` SEC-011 exactly as declared there.

**[MKT-023]** The `dev` trust channel is unsigned: a `dev`-channel pack or content-package MUST NOT require, and MUST NOT be checked against, a publisher-namespace signature (Publisher identity and namespace) — it is installed through a local, out-of-band path this contract does not define the transport of (Scope). Every `dev`-channel install MUST be rendered with a persistent, non-dismissable UI badge for as long as it remains installed.

**[MKT-024]** The `dev` trust channel weakens provenance only. Every other containment property this platform applies to a pack or content-package — network egress (`manifest/1` MAN-030), resource ceilings (`manifest/1` MAN-040/MAN-041), and process/render isolation — MUST apply to a `dev`-channel install identically to an install of any other trust channel.

*draft-note: process/render isolation's own normative owner is not yet fixed in this corpus — `ctx/1`'s own Scope explicitly disclaims "process supervision, sandboxing, and restart policy" as "a host concern, not a wire contract," naming no other owner. MKT-024's requirement stands regardless of which contract eventually specifies the mechanism.*

**[MKT-025]** A `dev`-channel pack or content-package MUST NOT claim a reserved or already-registered namespace (Publisher identity and namespace) and MUST NOT occupy a **required-pack** slot (`security-model` SEC-131/SEC-132's tier-granted "Required" status) — refused as `DEV_CHANNEL_RESERVED_NAMESPACE` (Error taxonomy). A `dev`-channel identity's own `pack_id` publisher segment MUST be the reserved `dev` publisher (MKT-003) or a name matching the reserved `local-` prefix (MKT-003) — never a registered third-party or first-party namespace. The definitive required-pack roster is itself a deployment/packaging concern, not fixed by this contract (nor by `security-model`, whose SEC-132 draft-note says the same); this requirement fixes only that whatever pack holds that status cannot be a `dev`-channel identity.

### The verified bar

**[MKT-030]** The `verified` trust channel MUST be granted to a pack or content-package version only once every one of the following automated checks, and the conformance pass (MKT-031), all pass; a single failing check MUST refuse `verified`-channel promotion in full — refused as `VERIFIED_BAR_NOT_MET` (Error taxonomy), naming every failing check:

1. **Namespace proof** — the version's own `publisher_signature` verifies against the key-id the root-signed publisher-namespace delegation (`channel-index/1` CHI-012) authorizes for its `pack_id`'s namespace: concretely, the resolution-time per-publisher check MKT-009 defines, here additionally gating `verified`-channel promotion (a failure at this check refuses as `PUBLISHER_SIGNATURE_UNVERIFIED` per MKT-009, before `VERIFIED_BAR_NOT_MET` is even reached).
2. **Manifest–bundle consistency, including egress-host parity** — every network host the sealed artifact's bundled code actually contacts MUST be covered by the manifest's own declared `egress` allowlist (`manifest/1` MAN-030); the artifact's declared capabilities/resources/connections MUST match what the bundle actually uses.
3. **No native binaries** — the sealed artifact MUST contain no host-architecture-specific compiled executable or shared-object code.
4. **Install-time script freedom** — the sealed artifact MUST carry no life-cycle or install-time script capable of executing outside `ctx/1`'s own sandboxed runtime protocol, consistent with `manifest/1`'s own "entirely before any pack code runs" posture (`manifest/1` Scope).
5. **SBOM slot populated** — the artifact carries a non-empty software-bill-of-materials reference.
6. **Size/entropy sanity** — the artifact passes an automated heuristic size/entropy check against a host-configured bound, the same "the floor value is host configuration, not part of this contract" carve-out `manifest/1` MAN-042 gives its own resource floor.

*draft-note: the SBOM reference's own document format, and the size/entropy check's concrete thresholds, are not fixed by this contract — flagged as open, not invented confidently.*

**[MKT-031]** The **conformance pass** MUST include the version's manifest validating against `manifest/1`, and, for any pack declaring UI pages, its bundled page documents validating against `ui-schema/1` (`ui-schema/1` Validation, UIS-200).

**[MKT-032]** Every UI surface displaying a `verified` badge MUST render accompanying copy (a `msg:` reference, `manifest/1` MAN-003/MAN-111) stating plainly that `verified` status attests only to MKT-030's automated checks and MKT-031's conformance pass — never editorial endorsement, a safety certification, or a human security audit.

**[MKT-033]** A `content-package` version whose subtype is `rule-template` (Content-package subtypes and rule templates) additionally requires, as part of the verified bar (MKT-030): the template schema-validates, and every one of its parameter binding sites is **classifiable** — resolves to a specific field position in the template's own trigger/condition/action structure whose accepted value type is statically known (an EntityRef, `rules/1` RUL-010, per the reused entity-picker shape, `ui-schema/1` UIS-073, or a literal/typed scalar field). A binding site that does not resolve this way MUST refuse `verified`-channel promotion as `RULE_TEMPLATE_BINDING_UNCLASSIFIABLE` (Error taxonomy), distinct from a plain `VERIFIED_BAR_NOT_MET`.

### Marketplace index entry and pack kinds

**[MKT-040]** A `pack` or `content-package` kind marketplace entry MUST reuse `channel-index/1`'s own per-version artifact-entry schema (`channel-index/1` Index schema, CHI-020) for every field that schema already defines — `artifact_id`, `version`, `digest`, `size`, `download_url`, and `status`'s `active`/`deprecated`/`yanked` members — unmodified: the same digest/size/split/compression verification (CHI-021–028), the same `yanked`-at-resolution-time check (CHI-072), and the same client verification order (`channel-index/1` Client verification order, CHI-050) apply to a `pack`/`content-package` entry exactly as to a `platform-release`/`relay-release` entry. `artifact_id` MUST be that version's `pack_id` (Publisher identity and namespace). Onto that CHI-050 order this contract adds — never redefines — one marketplace-owned step: the per-publisher `publisher_signature` verification of MKT-009, which CHI-050 (channel/source-grained) does not itself perform.

*draft-note: `channel-index/1` CHI-020's own text closes `kind` to `platform-release`/`relay-release` and `status` to `active`/`deprecated`/`yanked`. That closure is now reconciled: `channel-index/1` CHI-031 defines an explicit extension point letting a companion contract add `kind`/`status` members it owns (with unknown-value = skip-entry forward-compat), CHI-032 carries `hold_hours` applicability to those added kinds, and CHI-012 provides the publisher-grained delegation this contract's per-publisher check (MKT-009) verifies against — the three companion-minor clauses a fully rigorous reconciliation required, landed in `channel-index/1` (which took its 1.1 minor for them) rather than assumed away here. MKT-041's two `kind` values and MKT-044's one `status` value are marketplace/1's own content riding that sanctioned extension point.*

**[MKT-041]** `kind` MUST additionally admit exactly two marketplace/1-defined values, `pack` and `content-package`, alongside — never in place of — `channel-index/1`'s own `platform-release`/`relay-release` (CHI-020, CHI-041), added under `channel-index/1` CHI-031's companion extension point. A `kind` value outside the resulting four-member set MUST be refused, by a client that implements this contract, as `PACK_KIND_UNKNOWN` (Error taxonomy); a client that does *not* implement this contract instead skips a `pack`/`content-package` entry per CHI-031's unknown-`kind` skip rule, never refusing the whole index over it.

**[MKT-042]** `channel-index/1` CHI-029/CHI-030's `hold_hours` staged-rollout mechanism — including its resolution-time eligibility check and its `security_flagged` early-selection escape hatch — MUST apply to a `pack`/`content-package` kind entry identically to a `platform-release`/`relay-release` entry, per `channel-index/1` CHI-032 (which carries `hold_hours`/`published_at` applicability to a companion contract's added kinds), without any other change to CHI-029/030's mechanics. MKT-043's additional authentication bar on the `security_flagged` zero-out is the "additional bar for its own kinds" CHI-032 permits — a tightening, never a weakening, of CHI-030.

**[MKT-043]** For a `pack`/`content-package` kind entry specifically, CHI-030's `security_flagged: true` early-selection marking MUST NOT by itself be sufficient to zero or bypass `hold_hours` when authenticated by nothing more than the routine publish-path targets-role signature already covering the rest of the entry (a "bare publisher-signed flag") — refused (the hold remains in force) as `HOLD_HOURS_ZERO_UNAUTHENTICATED` (Error taxonomy) if no further authentication is present. Zeroing `hold_hours` for a `pack`/`content-package` entry additionally requires one of the following, each bound to a **key or role distinct from — and not derivable from — the routine publish/targets key** (a separation of duties, so a single stolen publish key can never satisfy it):

  (a) a **matching artifact-class revocation** in the same registry source's revocation feed (`channel-index/1` `revoked_artifacts`, CHI-070) naming the exact prior `(pack_id, version)` this release supersedes — the version its `superseded_by` (MKT-045) names, or the immediately-preceding published version of the same `pack_id` — where "matching" is that specific superseded tuple, never any unrelated yank in the feed. Because the revocation feed is signed by the **delegated revocation role** (`channel-index/1` CHI-011), whose key root delegates separately from the targets role, a stolen publish/targets key alone cannot forge this entry; or

  (b) a **manual-approval countersignature** produced by one of exactly two distinct authorities: an `owner`-role acknowledgment (`security-model` SEC-011's owner-exclusive authority) or a countersignature by the delegated revocation role (`channel-index/1` CHI-011). A countersignature produced by the same key-id that signed the routine publish entry (or any key derivable from it) MUST NOT satisfy (b): "collected outside the routine publish path" means a distinct key/role, not merely a distinct tool, process, or moment — a second signature still made under the publish key does not meet this bar.

`hold_hours`'s own skip is otherwise obtainable only by explicit owner action (MKT-091); neither this requirement nor MKT-091 ever affects consent parking (MKT-092) — a widened-capability diff still parks for consent regardless of `hold_hours`'s own state.

**[MKT-044]** `status` MUST additionally admit exactly one marketplace/1-defined value beyond `channel-index/1`'s own three (CHI-020): `archived` — a publisher-initiated withdrawal from ordinary marketplace discovery/browse, distinct from `yanked` (which is checked at resolution time and blocks new resolution outright, `channel-index/1` CHI-072, reused unmodified for `pack`/`content-package`) in that an `archived` version remains resolvable for an existing install's own re-resolution or reinstall of that exact version. `deprecated` carries no behavioral consequence beyond CHI-020's own informational one, reused unmodified.

**[MKT-045]** An entry MAY carry `superseded_by`: a publisher-qualified `pack_id` reference (MKT-008) naming the **successor pack** that replaces it, resolved through that successor's own channel pointer at its then-current version — a bare `pack_id` per MKT-008/`manifest/1` MAN-001 (`<publisher>/<name>`), never a version-pinned `pack_id@version` string (which MKT-008's grammar forbids: `@` and `.` are outside a `pack_id` segment's charset). `superseded_by` therefore names *which pack* supersedes this one, not a frozen version string; it is conventionally populated once `status` is `deprecated`, `archived`, or `yanked`.

**[MKT-046]** A registry source's signed index MAY additionally carry a **channel pointer** record per `pack_id` it publishes: `{pack_id, channels}`, `channels` an object mapping each trust channel (Trust channels and production install) currently carrying a published version of that pack to the version string a client resolves that (`pack_id`, trust channel) pair to. This record MUST be carried within the same signed document (`channel-index/1`'s own `ChannelIndex`, riding its `format_version`, CHI-090) as the entry array MKT-040 describes — it inherits that document's own signature chain (CHI-001–011) and monotonic-version protection (CHI-060) automatically; this contract defines no second signing or transport mechanism for it.

**[MKT-047]** A channel pointer's own `channels[<trust channel>]` value MUST be treated as a lookup key only, never as a trust decision by itself: resolving it to a verifiable artifact MUST always proceed by locating the matching `(artifact_id: pack_id, version)` entry among the same source's ordinary artifact entries (MKT-040), running `channel-index/1`'s complete verification order (CHI-050) against that entry, and additionally verifying that entry's own per-publisher signature (MKT-009) and its resolved-version anti-rollback (MKT-050) — a channel pointer carries no digest and authorizes nothing on its own, and neither the pointer nor the enclosing index's own signature ever stands in for the entry's namespace proof.

**[MKT-048]** `yanked`'s resolution-time check (`channel-index/1` CHI-072) applies to a channel pointer's own resolution exactly as to a fresh install: a channel pointer naming a version whose matching entry is `yanked` MUST NOT be resolved for a new install or for an existing install's channel-tracking advance (Update default and install records) — the freshest fetched index and revocation feed govern, never a cached pointer value.

**[MKT-049]** A `pack` kind entry's artifact MUST be a `manifest/1`-conformant sealed pack artifact (`manifest/1` Definitions: Pack). A `content-package` kind entry's artifact is a sealed content-package artifact whose own subtype (Content-package subtypes and rule templates) determines its internal structure; this contract does not require a content-package to itself carry a `manifest/1` `PackManifest` document.

**[MKT-050]** A verifier MUST persist, per `(pack_id, trust channel)`, the highest **pack version** it has ever successfully resolved for that pair — a resolved-version high-water mark, surviving a process or device restart exactly as `channel-index/1` CHI-062 requires of its own rollback state — and MUST refuse to resolve a channel pointer (MKT-046), for a new install or for a channel-tracking advance (MKT-090), to a pack version **lower** than that high-water mark: refused as `POINTER_ROLLBACK_REJECTED` (Error taxonomy), never silently followed. `channel-index/1` CHI-060's monotonic-version protection guards only the *index document's* own metadata `version` counter, not the *pack* version a pointer names — a freshly-signed index carrying a higher metadata `version` may still name a lower pack version, and CHI-060 passes it — so this requirement is the marketplace-grained anti-rollback CHI-060 does not provide, mirroring CHI-060 at pointer grain so a validly-signed newer index cannot walk a channel back onto an older, non-yanked, since-superseded (for instance, since-vuln-fixed) version. The **sole** exception is a revert-to-known-good (MKT-093): where the install's current resolved version has itself become non-resolvable — `yanked` (CHI-072), key-revoked (CHI-071), or verified-attestation-revoked (MKT-072) for a `verified`-dependent install — a pointer MAY move the channel backward to the most recent still-resolvable, non-revoked version, and this is the only circumstance under which a channel's resolved version may decrease. That backward move is itself subject to the full resolution-time verification order (MKT-047, MKT-009): a revert never relaxes the per-publisher or signature checks, it only permits a lower target when the higher one can no longer be trusted.

### Registry sources

**[MKT-060]** A client's registry configuration MUST be an ordered list of **registry sources** (Definitions), each independently publishing the identical `channel-index/1` index format (MKT-040, MKT-046). `file://` (a local filesystem path) MUST be a legal registry-source scheme, included in the same ordered list.

**[MKT-061]** Each registry source's expected index-signer key-id(s) MUST be bound in the client's own configuration for that source, never learned or inferred from the source itself and never inferred from the source's position in the ordered list. The list's own order MUST NOT be treated as, or substitute for, a trust decision — every source's entries are independently verified per `channel-index/1` CHI-050 against that source's own client-configured key-id(s) regardless of list position; order MAY be used only as a plain resolution preference among sources that each independently pass verification.

**[MKT-062]** An entry under a reserved namespace (Publisher identity and namespace: `waiveo`, `waiveo-*`, `official-*`, `system-*`) MUST resolve only from a registry source whose index-signing key chains to that namespace's own publisher delegation (`channel-index/1` CHI-012, per MKT-002) — for the `waiveo` namespace specifically, the first-party publisher delegation established per MKT-001/MKT-021. A registry source whose signer does not so chain MUST NOT be permitted to resolve, or be offered as, an entry under a reserved namespace regardless of its position in the client's ordered source list — refused as `REGISTRY_SOURCE_NOT_DELEGATED` (Error taxonomy). This is a stricter, source-level guard specific to the reserved set; the per-entry per-publisher signature check MKT-009 now provides the namespace-proof guarantee for **every** registered namespace, reserved or not (closing the gap where a non-reserved third-party namespace would otherwise rest on source trust alone) — MKT-062 and MKT-009 are defense-in-depth, not alternatives.

**[MKT-063]** A `file://` registry source's index MUST be exempt from `channel-index/1`'s timestamp-role max-age freshness check (the CHI-061 step of CHI-050) — a local, self-published index has no meaningful staleness the way a network-fetched one does — but is exempt from nothing else CHI-050 requires. The exemption is scoped to that one timestamp-max-age step and MUST NOT weaken revocation: signature verification, monotonic-version rollback protection, and the revocation-feed signature/freshness/evaluation step (CHI-050 step 9) all apply to a `file://` resolution unchanged — a `file://` index can be replayed only up to the client's own monotonic-version and revocation high-water marks, never used to suppress a revocation. (This is a narrower, scheme-based relaxation than `channel-index/1` CHI-063's untrusted-*clock* fallback, a distinct condition MKT-063 does not invoke.) Every install resolved through a `file://` source MUST additionally be marked `stale_source` (Update default and install records) and MUST be re-checked against the freshest reachable revocation feed the next time connectivity to a non-`file://` source is available.

### Verified-status revocation

**[MKT-070]** `verified` trust-channel status MUST be removable, for a specific already-published pack or content-package version, as a graduated response short of yanking that version: the version remains fully installable and resolvable, only its `verified` status and the consent-baseline/UI treatment `verified` confers (The verified bar) are withdrawn.

**[MKT-071]** This is realized as revocation of a **verified attestation** (Definitions): a `channel-index/1` revocation-feed artifact-class entry (`channel-index/1` `revoked_artifacts`, CHI-070, reused unmodified at the field level) whose `artifact_id` is the synthetic value `verified-attestation:<pack_id>`, `channel` names `verified`, and `version` names the specific version whose attestation is revoked. This contract defines no new revocation-feed class, signing mechanism, or freshness rule — it is CHI-070's own existing artifact-class shape, applied to a marketplace-minted synthetic subject distinct from the pack's own release `artifact_id`. Two properties this synthetic use depends on are stated explicitly so an implementer keys on them rather than on coincidence: (i) the `verified-attestation:` subject prefix is **reserved** by this contract, and its collision-safety with a real release rests on the `:` delimiter being **outside** the `pack_id` grammar (`manifest/1` MAN-001's per-segment `^[a-z][a-z0-9-]{1,38}$`, separated by `/`, admits no `:`) — so a verifier MUST recognize a verified attestation by the exact colon-delimited `verified-attestation:` prefix, never a loose substring match, and a publisher literally named `verified-attestation` stays unambiguous (its real subjects are `verified-attestation/<name>` with a `/`, never `verified-attestation:` with a `:`); (ii) this entry deliberately carries a marketplace **trust channel** value (`verified`) in CHI-070's `channel` field, which for an ordinary yank names a `channel-index/1` **Channel** (a distribution stream) — a deliberate field reuse for the synthetic subject, safe because the synthetic `artifact_id` can never match a real release's, so no real resolution is affected by the reused `channel` value.

**[MKT-072]** A verifier or host encountering a `revoked_artifacts` entry matching a currently-resolved pack version's own `verified-attestation:<pack_id>`/`verified`/`version` tuple MUST cease presenting or treating that version as `verified` trust channel — refused for continued `verified` treatment as `VERIFIED_ATTESTATION_REVOKED` (Error taxonomy) — but MUST NOT treat this by itself as `ARTIFACT_YANKED` (`channel-index/1` Error taxonomy) against the pack's own release `artifact_id`: the version remains ordinarily installable under whatever other trust channel(s) its channel pointer (MKT-046) also names, unaffected.

**[MKT-073]** Revoking a verified attestation MUST be checked at the same resolution-time cadence `channel-index/1` CHI-072 requires for an ordinary yank — against the freshest fetched revocation feed, never cached past that feed's own freshness window (`channel-index/1` CHI-074).

### Content-package subtypes and rule templates

**[MKT-080]** A `content-package` kind entry (MKT-041) MUST declare a `subtype`; this version defines exactly one, `rule-template` (Definitions). Growth of the subtype set is additive and exclusively a marketplace/1 minor.

**[MKT-081]** A `rule-template` content-package's own parameter-substitution semantics — how its declared parameters bind into the resulting rule's trigger/condition/action fields, and the per-instance execution-class reclassification that follows — are `rules/1`'s own ("Rule templates (blueprint substitution)", RUL-250, RUL-251), reused unmodified; this contract governs only the template's distribution (Marketplace index entry and pack kinds, Trust channels and production install) and its input form (MKT-082).

**[MKT-082]** A rule template's own **input form** — the surface an operator fills the template's declared parameters in through before instantiation — MUST reuse `ui-schema/1`'s binding grammar (`ui-schema/1` Binding grammar: data paths, UIS-100) for its field bindings, and, for any parameter representing an entity, device, or scope selection, `ui-schema/1`'s `entity-picker` widget and its bound-value shape (`ui-schema/1` Widget catalog, UIS-070, UIS-073) — which in turn requires `api/1`'s label-selector grammar (`api/1` Label-selector grammar, API-040–046) for that widget's `selector` form. This contract defines no second binding or selector grammar of its own.

**[MKT-083]** Every parameter a `rule-template` content-package declares MUST correspond to a `manifest/1` `dataModel` row's `template_ref`/`params` fields (`manifest/1` MAN-051's universal entity envelope) once instantiated — that envelope's own carriage for "this row came from a template, with these params," reused unmodified as the instantiation record.

### Update default and install records

**[MKT-090]** An install's default update policy MUST be **channel auto-tracking**: the install's pinned trust channel (Install record, Definitions) is re-resolved, on each update check, to whatever version that registry source's channel pointer (MKT-046) currently names for that (`pack_id`, trust channel) pair — subject to the same resolution-time rules an initial install is subject to (`hold_hours` eligibility, `yanked` check, revocation-feed check, the per-publisher signature check MKT-009, and the resolved-version anti-rollback MKT-050), never a weakened or special-cased check for an update path. Because MKT-050 binds auto-tracking to a per-`(pack, channel)` high-water mark, a validly-signed newer index MUST NOT be able to auto-track an install *backward* onto a lower, non-yanked pack version (the sole exception being MKT-050's revert-to-known-good). A per-scope pin or hold overriding this default MAY exist; its own UX is out of scope for this contract (Scope).

**[MKT-091]** A diff between an install's currently-applied manifest and the auto-tracked candidate version's manifest, classified per `manifest/1`'s own semantic-diff rule (`manifest/1` MAN-023: `capabilities`/`egress`/`resources`/`connections` versus everything else), MUST apply automatically, with no owner action, once `hold_hours` (Marketplace index entry and pack kinds) has elapsed, when the diff touches none of those four fields (a **consent-neutral diff**) — the same field subset `manifest/1` MAN-022/023 already uses to decide whether re-consent is owed. `hold_hours` MAY be skipped for a specific update only by explicit owner action.

**[MKT-092]** A diff that widens `capabilities`, `egress`, `resources`, or `connections` beyond the install's currently-granted baseline (a **capability-widening diff**, `manifest/1` MAN-022) MUST park pending owner acknowledgment (`security-model` SEC-011's owner-exclusive acknowledgment authority) rather than auto-apply, regardless of whether `hold_hours` has elapsed — `hold_hours` governs timing only, never consent.

**[MKT-093]** A **required** pack (`security-model` SEC-131/SEC-132's tier-granted "Required" status, whose definitive roster is deployment-fixed, not set by this contract, per MKT-025) whose auto-tracked update fails to apply, or whose currently-applied version is subsequently revoked (`channel-index/1` `KEY_REVOKED`/`ARTIFACT_YANKED`, or this contract's own `VERIFIED_ATTESTATION_REVOKED` where the install depended on `verified`-tier trust), MUST revert to its own last-known-good version: the most recent version of that same `pack_id` that was itself successfully applied and has not itself since been revoked. This contract fixes the decision and its target only; the mechanics of actually performing the revert on a running deployment are out of scope (Scope).

**[MKT-094]** An install record MUST pin `{pack_id, trust_channel, resolved_version, source, stale_source}` (Wire shapes) — `source` identifying the registry source (Registry sources) the resolution was served from, `stale_source` set per MKT-063 for a `file://`-resolved install and cleared once that install has been re-checked against a non-`file://` source's revocation feed.

## Wire shapes

```json
// Publisher
{
  "publisher_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Q1",
  "name": "acme",
  "status": "active",
  "current_key_id": "ed25519:acme-2026-07",
  "created_at": "2026-06-01T00:00:00Z"
}
```

```json
// PublisherRebindRequest (MKT-006)
{
  "publisher": "acme",
  "from_key_id": "ed25519:acme-2026-07",
  "to_key_id": "ed25519:acme-2026-08",
  "mode": "old-key-signed",
  "evidence": null,
  "requested_at": "2026-08-01T00:00:00Z"
}
```

```json
// MarketplaceArtifactEntry -- kind: pack (extends channel-index/1 CHI-020's entry, MKT-040/041; publisher_signature per MKT-009)
{
  "artifact_id": "acme/weather-widget",
  "kind": "pack",
  "version": "1.2.0",
  "status": "active",
  "digest": "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
  "size": 1048576,
  "download_url": "https://index.example/acme/weather-widget-1.2.0.tar.zst",
  "compression": "zstd",
  "published_at": 1752620000000,
  "hold_hours": 24,
  "publisher_signature": "ed25519:acme-2026-07/MEUCIQDx...weather-widget-1.2.0-sig",
  "superseded_by": null
}
```

```json
// MarketplaceArtifactEntry -- kind: content-package, subtype: rule-template (MKT-041, MKT-044, MKT-080)
{
  "artifact_id": "acme/night-light-routine",
  "kind": "content-package",
  "subtype": "rule-template",
  "version": "2.0.0",
  "status": "deprecated",
  "digest": "sha256:aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666000077778888",
  "size": 4096,
  "download_url": "https://index.example/acme/night-light-routine-2.0.0.json.zst",
  "published_at": 1752610000000,
  "publisher_signature": "ed25519:acme-2026-07/MEQCIG9k...night-light-routine-2.0.0-sig",
  "superseded_by": "acme/night-light-pro"
}
```

```json
// ChannelPointerRecord (MKT-046, carried inside the same signed ChannelIndex document as the entry array)
{
  "pack_id": "acme/weather-widget",
  "channels": {
    "verified": "1.2.0",
    "community": "1.3.0-rc1"
  }
}
```

```json
// RevocationFeed excerpt -- a verified-attestation revocation (MKT-071), riding channel-index/1's own revoked_artifacts shape unmodified
{
  "revoked_artifacts": [
    { "artifact_id": "verified-attestation:acme/weather-widget", "channel": "verified", "version": "1.2.0", "revoked_at": 1752630000000, "reason": "egress-host parity check regressed post-publish" }
  ]
}
```

```json
// RuleTemplateInputForm excerpt -- a settings-form page reusing ui-schema/1's entity-picker for a rule-template parameter (MKT-082)
{
  "pageType": "settings-form",
  "source": "$ui.draft",
  "sections": [
    {
      "titleMsg": "msg:night-light-routine.params.title",
      "fields": [
        { "type": "entity-picker", "bind": "target_light", "props": { "labelMsg": "msg:night-light-routine.params.targetLight", "modes": ["entity", "selector"] } },
        { "type": "time-of-day", "bind": "activate_at", "props": { "labelMsg": "msg:night-light-routine.params.activateAt" } }
      ]
    }
  ],
  "actions": [
    { "type": "button", "props": { "labelMsg": "msg:night-light-routine.params.instantiate" }, "on": { "press": { "verb": "call-action", "action": "acme/night-light-routine.instantiate" } } }
  ]
}
```

```json
// InstallRecord (MKT-094)
{
  "pack_id": "acme/weather-widget",
  "trust_channel": "verified",
  "resolved_version": "1.2.0",
  "source": "https://index.example/",
  "stale_source": false
}
```

## Negotiation

marketplace/1 rides entirely inside `channel-index/1`'s own signed-index envelope and its `format_version` negotiation (`channel-index/1` Index format versioning, CHI-090) — this contract defines no separate handshake or version field of its own. A client implementing this contract's `pack`/`content-package` kind, `status`, and channel-pointer extensions recognizes them once it reads a `channel-index/1`-conformant index; a client that does not implement this contract simply skips `pack`/`content-package` entries (and does not resolve channel pointers) from that index rather than refusing it — a forward-compatibility guarantee now made concrete by `channel-index/1` CHI-031's unknown-`kind`/`status` = skip-entry rule (the extension point that formally sanctions this contract's `kind`/`status` widening, with CHI-032 for `hold_hours` applicability and CHI-012 for the publisher-grained delegation MKT-009 verifies against).

Growth of this contract's own closed sets — the trust-channel enum (MKT-020), the verified-bar checklist (MKT-030), the content-package subtype set (MKT-080), and the registered-namespace reservation set (MKT-003) — is additive and exclusively a marketplace/1 minor; narrowing any of them, removing a required verified-bar check, or weakening the source-order-is-never-trust rule (MKT-061) is a marketplace/1 major.

## Error taxonomy

Every condition below, wherever it surfaces through the management API, is returned as an `api/1` Problem document (`api/1` Error shape, API-010) carrying the listed value in the Problem's `code` member, with `Trace-Id` propagation and the echoed `trace_id` extension member (`api/1` API-060–063) — referenced, not redefined. This contract mints no second error envelope, the same reuse of `api/1`'s Problem shape that `channel-index/1` and `security-model` (SEC-074) make of it.

| code | meaning | retryable |
|---|---|---|
| `NAMESPACE_RESERVED` | Publisher registration requested an exact- or prefix-reserved namespace (MKT-003). | no |
| `CONFUSABLE_COLLISION` | Publisher registration requested a namespace confusable with an existing registered namespace (MKT-004). | no — choose a clearly distinct namespace |
| `RESURRECTION_ATTEMPT` | Publisher registration targeted a deleted publisher's former exact namespace (MKT-005). | no |
| `REBIND_UNAUTHENTICATED` | A publisher key-rebind request carried neither an outgoing-key signature nor a completed identity-proofed fallback (MKT-006). | yes — resubmit with valid authentication |
| `CROSS_PACK_REFERENCE_UNQUALIFIED` | A cross-pack reference used a bare, unqualified name instead of a full `pack_id` (MKT-008). | no — fix the reference |
| `PUBLISHER_SIGNATURE_UNVERIFIED` | An entry's per-publisher `publisher_signature` did not chain, through the root-signed publisher-namespace delegation (`channel-index/1` CHI-012), to the key authorized for its `pack_id`'s namespace — or was absent (MKT-009, MKT-030 check 1). Not curable by source order. | no — the entry is not authenticated for the namespace it claims |
| `TRUST_CHANNEL_UNKNOWN` | A `trust_channel` value was outside the closed four-member enum (MKT-020). | no |
| `FIRST_PARTY_CHANNEL_NAMESPACE_MISMATCH` | An entry outside the `waiveo` publisher namespace claimed the `first-party` trust channel (MKT-021). | no |
| `DEV_CHANNEL_RESERVED_NAMESPACE` | A `dev`-channel entry claimed a reserved/registered namespace or a required-pack slot (MKT-025). | no |
| `VERIFIED_BAR_NOT_MET` | One or more of the verified bar's automated checks, or its conformance pass, failed (MKT-030, MKT-031). | yes — after the failing check(s) are fixed and resubmitted |
| `RULE_TEMPLATE_BINDING_UNCLASSIFIABLE` | A rule template's parameter binding site did not resolve to a statically classifiable field position (MKT-033). | yes — fix the binding and resubmit |
| `PACK_KIND_UNKNOWN` | An entry's `kind` value was outside the four-member set this contract and `channel-index/1` together define, refused by a client that implements this contract (MKT-041); a client that does not implement it skips the entry per `channel-index/1` CHI-031 instead. | no |
| `POINTER_ROLLBACK_REJECTED` | A channel pointer named a pack version lower than the persisted resolved-version high-water mark for that `(pack_id, trust channel)` pair, outside the revert-to-known-good exception (MKT-050, MKT-090). | no — a validly-signed newer index may not walk a channel backward |
| `HOLD_HOURS_ZERO_UNAUTHENTICATED` | A `pack`/`content-package` entry's `security_flagged` marking was authenticated by nothing beyond the routine publish-path signature (MKT-043). | yes — resubmit with the required revocation-feed entry or countersignature |
| `REGISTRY_SOURCE_NOT_DELEGATED` | A reserved-namespace entry was resolved from, or offered by, a source whose signer does not chain to that namespace's own delegation (MKT-062). | no |
| `VERIFIED_ATTESTATION_REVOKED` | A resolved pack version's verified attestation is present in the revocation feed's `revoked_artifacts` (MKT-071, MKT-072). | no — the version remains installable under a non-`verified` trust channel it may also carry |
| `STALE_SOURCE_INSTALL` | Informational condition marking an install resolved through a `file://` source, pending revocation-feed re-check on next connectivity (MKT-063). | n/a |

## Conformance notes

- Traceability map: `conformance/traceability/marketplace-1.md` — maps every `MKT-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/marketplace-1/` — one JSON case file per `case-id` referenced from the traceability map.
- `channel-index/1`'s own signature verification, digest/size checks, split/compression handling, monotonic-version rollback protection, and revocation-feed transport/signing/freshness (CHI-001–083) are exercised by `channel-index/1`'s own corpus; this corpus treats a well-formed, validly-signed `channel-index/1` envelope as a given input wherever a case needs one, exercising only the marketplace-specific fields, checks, and decisions this contract adds — the same "signing key's own legitimacy is a given, already-established input" posture `channel-index/1`'s own Conformance notes state for its corpus.
- `hold_hours` eligibility (MKT-042) and verified-attestation-revocation freshness (MKT-073) are time-dependent properties, exercised against an injectable (`published_at`/`revoked_at`, evaluation_time) pair in a driver harness rather than wall-clock sleeps in this static corpus, consistent with `channel-index/1`'s own timing-property posture (`channel-index/1` Conformance notes).
- Publisher registration's confusable/homoglyph check (MKT-004) is a fuzzy-matching algorithm; this corpus exercises only the accept/refuse decision for a small set of worked examples, not an exhaustive confusables table (MKT-004's own draft-note).
- The content-package subtype set beyond `rule-template`, the SBOM reference's own document format, the verified bar's size/entropy thresholds, and the identity-proofed rebind fallback's own evidentiary procedure are draft-noted open points (MKT-004, MKT-006, MKT-030, MKT-080) and are not exercised by this corpus.
- The pack update/rollback execution mechanism a required-pack's revert-to-known-good (MKT-093) ultimately runs through, and the per-scope pin/hold override's own UX (MKT-090), are out of this contract's scope (Scope) and are not exercised here; this corpus exercises only the resolution/policy decisions this contract itself fixes.
