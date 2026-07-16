# Channel Index

**Contract:** channel-index/1
**Version:** 1.1
**Status:** draft

## Scope

channel-index defines the signed, host-agnostic release-artifact index a client resolves an update or install candidate through: the signing-role structure that makes the index (and the trust root every other contract's own reference to "the platform's software-artifact trust bundle" resolves to, Signed index roles) fully offline-verifiable; the index schema an artifact entry satisfies, including split/compressed large artifacts; the two channel-namespace families (a platform-train channel and the relay's own independent channel); the exact order a client MUST perform verification in; monotonic-version rollback protection and bounded metadata freshness; the revocation feed and its two revocation classes; and the hosting properties (a stable per-channel URL, unauthenticated browser-download artifact URLs, a per-object size cap) that keep the format itself independent of any one hosting backend.

- In scope: the signed-index envelope and its four role layers (root, targets/delegated-targets, snapshot, timestamp); the trust bundle a verifier holds and how it is updated; namespace/channel delegation; the index schema (artifact identity, digest, size, split parts, compression, download URL, status); the platform-train and relay channel namespace families and how a deployed unit's update source binds to one; the client's required verification order; monotonic-version rollback protection at every role layer; timestamp freshness and its bounded-staleness/untrusted-clock fallback; the revocation feed's two classes (key/trust and artifact) and their distinct enforcement posture; host-agnostic stable-URL hosting, unauthenticated browser-download artifact URLs, and the per-object size cap driving the split/compression requirement.
- Out of scope: the operational custody of any role's own signing key (offline root-key ceremony, an online signer's own key-management mechanics) — this contract defines the role structure those keys back, never how a key is generated, stored, or ceremonially rotated; a marketplace pack or content-package's own index entry — a distinct concern that may reuse this same signed-index envelope in a separate contract, never redefined here; how a deployed unit selects which channel identifier its own update source binds to (deployment configuration); the install/update/rollback state machine that consumes a resolved, verified artifact (a separate concern; this contract ends at "this artifact is verified and its entry is not revoked," not at "install it"); `api/1`'s Problem error shape and `Trace-Id` propagation, for wherever an install/update operation surfaces this contract's own errors through the management API (referenced, not redefined).

## Definitions

- **Index** — the signed document, scoped to one channel, enumerating that channel's currently available artifacts (Index schema). The targets role (or a role it delegates to) is the signer of this document.
- **Channel** — a named release stream resolving to one update source (Channel namespaces): a platform-train channel or the relay's own independent channel.
- **Artifact** — one downloadable release object an index entry describes: one architecture's build of one component, at one version.
- **Root role** — the offline-keyed role whose own metadata is the trust bundle's foundation; the sole role authorized to reassign which keys back every other role (Signed index roles).
- **Targets role** — the role (or a namespace delegated from it) authorized to sign an artifact's own digest into an index.
- **Snapshot role** — the role that signs a consistency manifest binding the current version and digest of every targets-role file, so a verifier can detect a stale or substituted targets file before fetching it.
- **Timestamp role** — the role that signs a short-lived freshness statement naming the snapshot role's own current version and digest.
- **Trust bundle** ("the platform's software-artifact trust bundle") — the root role's current metadata plus every key-id it has delegated authority to; sufficient, held alone, to verify every other role's signature with no network fetch. This is the trust root every other contract's own reference to "the platform's software-artifact trust bundle" resolves to (`relay/1`'s desired-state verification and `archive/1`'s export-container signing both explicitly name it only to state that they are a *different*, unrelated trust root from this one — this contract is where that name is actually defined).
- **Delegation** — the root role's assignment of a channel identifier to the specific targets key-id(s) authorized to sign that channel's own index entries. Root MAY also carry a finer-grained **publisher-namespace delegation** (CHI-012): the same targets-style assignment made at the grain of a companion contract's publisher namespace rather than a channel, so a namespace's own signing authority is root-anchored and offline-verifiable independent of any one source's configuration.
- **Monotonic version** — a role's own strictly-increasing integer version counter (Freshness and rollback protection), independent per role and per channel.
- **Revocation feed** — the separately-fetched, separately-signed document — signed by the delegated revocation role (Signed index roles, CHI-011) — enumerating revoked signing keys and revoked artifact versions (Revocation classes).

## Normative requirements

### Signed index roles

**[CHI-001]** An index MUST be verifiable through exactly four signed role layers: a root role, a targets role (with any number of role-delegated sub-namespaces), a snapshot role, and a timestamp role — each layer's metadata signed independently of the others, so that a compromise of one role's key does not by itself forge another role's signature.

**[CHI-002]** The root role's own metadata MUST be signed by a threshold of independently-held root keys, and is the sole role authorized to reassign which key-id(s) back the targets, snapshot, timestamp, and revocation roles. A root role update (a rotation of any key it assigns, including its own) MUST itself carry threshold signatures spanning both the outgoing and the incoming root key sets — a rotation is never authorized by the incoming keys alone.

**[CHI-003]** The targets role — or a namespace explicitly delegated from it (Delegation) — MUST be the sole role authorized to sign an individual artifact entry's own `digest` (Index schema) into an index. A snapshot- or timestamp-role signature MUST NOT be treated as sufficient authority for an artifact's own digest, even where the document that signature covers otherwise appears well-formed.

**[CHI-004]** The snapshot role MUST sign a manifest binding the current `{version, digest}` of every targets-role file (every channel's own index, and any further role-delegated file) it is aware of — the reference a verifier checks a freshly-fetched targets file against before trusting it (Client verification order).

**[CHI-005]** The timestamp role MUST sign a short-lived statement naming the snapshot role's own current `{version, digest}` (Freshness and rollback protection); its own signing key MUST be distinct from, and rotated independently of, the snapshot role's key and the root/targets roles' keys.

**[CHI-006]** The timestamp role's and the snapshot role's own signing keys MAY be held online (routinely accessible for automated, frequent signing); the root role's key MUST NOT be. This contract defines only this role-key separation; the custody mechanics of any role's key (an offline root-key ceremony, an online signer's own storage) are deployment/operational concerns this contract does not define (Scope).

**[CHI-007]** A verifier MUST hold a trust bundle (Definitions) sufficient, by itself, to validate every role's signature with no network fetch — a verifier unable to reach any network at all MUST still be able to determine whether a locally-cached, previously-verified index remains validly signed.

**[CHI-008]** A verifier's trust bundle MUST be updatable only via a root-role update satisfying CHI-002; no other role's metadata — however validly signed under the verifier's CURRENT trust bundle — MAY alter which key-id(s) that trust bundle itself recognizes as authoritative for any role.

**[CHI-009]** Root metadata MUST carry namespace delegations: an explicit mapping from a channel identifier (Channel namespaces) to the targets key-id(s) authorized to sign entries for it. A verifier MUST reject an index entry whose channel is not covered by its own signer's delegated namespace — regardless of which URL or hosting backend served the index it arrived in (Hosting and size envelope).

**[CHI-010]** A channel's delegated signing key MUST NOT be treated as authorized for a different channel's own entries, even where the two channels are related (for instance a platform-train channel and a relay-release entry traveling inside that same train's index, Channel namespaces) — delegation is scoped per channel exactly as declared in root metadata, never inherited across channels by naming similarity or operational proximity.

**[CHI-011]** Root metadata MUST additionally delegate a **revocation role**, distinct from the targets/snapshot/timestamp roles (CHI-001), authorized to sign the revocation feed (Definitions) — delegated from root exactly as a channel's targets key-id is delegated (CHI-009), never assumed or inferred from any other role's own key. The revocation feed MUST be signed by this delegated key; a verifier MUST reject a revocation feed signed by any key-id root metadata has not delegated this role to, regardless of which URL or hosting backend served it (Hosting and size envelope).

**[CHI-012]** Root metadata MAY additionally carry **publisher-namespace delegations**, a second, finer-grained delegation family parallel to the channel delegations of CHI-009: an explicit, root-signed mapping from a publisher namespace — the identity unit a companion contract (Scope) resolves entries by — to the signing key-id(s) authorized to sign that one publisher's own entries. A publisher-namespace delegation is a targets-style delegation (CHI-003) scoped to its one namespace exactly as a channel delegation is scoped to its one channel (CHI-010): a namespace's delegated key MUST NOT be treated as authorized for a different namespace's entries, never inherited across namespaces by naming similarity, and never satisfied by the order entries happen to appear in a source's index. Where a companion contract resolves entries keyed by publisher namespace, a verifier MUST **verify before it trusts**: an entry claiming a delegated publisher namespace MUST be verified to carry a signature chaining to the key-id this delegation authorizes for that exact namespace before the entry is trusted, installed, or acted on — a check independent of, and in addition to, the channel/source-grained index signature CHI-050 already establishes (which authenticates only that a source's index is validly signed by that source's own delegated channel key, never that the source is entitled to a given publisher's namespace). An entry under a delegated publisher namespace whose signature does not so chain MUST NOT be trusted on the strength of the enclosing index's own signature, its position in an ordered source list, or which hosting backend served it; the companion contract owning that namespace defines the specific outcome (a skip per CHI-031, or a named refusal of its own). This delegation family, held in the root role's trust bundle (Definitions), is what lets a namespace's own signing authority be verified offline without any per-source configuration vouching for it.

### Index schema

**[CHI-020]** A channel's index document MUST be an array of artifact entries, each carrying at minimum `{artifact_id, kind, version, status, digest, size, download_url}` (Wire shapes, `ChannelIndex`). `kind` MUST be one of `platform-release` or `relay-release` (Channel namespaces). `status` MUST be one of `active`, `deprecated`, `yanked`.

**[CHI-021]** `digest` MUST be a `sha256:`-prefixed hex digest computed over the complete artifact's own bytes as published — after part reassembly, in `part_index` order, where `parts` (CHI-024) is present, and always over the decompressed form regardless of `compression` (CHI-026).

**[CHI-022]** `download_url` (and every split part's own `download_url`, CHI-024) MUST be a browser-download-class URL: one not subject to a separate, lower rate limit than an ordinary file or object fetch on its hosting service, and requiring no API credential or session to retrieve.

**[CHI-023]** `size` MUST be the artifact's complete byte size in its published form — after part reassembly, where split (CHI-024), and after decompression, where `compression` (CHI-026) applies; `size` and `digest` (CHI-021) always describe the same bytes. A verifier MUST refuse an artifact whose downloaded, reassembled, and decompressed byte count does not equal `size`, independent of and in addition to the `digest` check — the two checks catch different failure modes (a truncated or substituted transfer changes both; a fault isolated to one check without the other still indicates corruption, never a reason to trust the artifact on the strength of the other check alone).

**[CHI-024]** An artifact whose complete published size would exceed this contract's stated per-object cap (Hosting and size envelope) MUST instead publish `parts`: an ordered array of `{part_index, digest, size, download_url}`, `part_index` starting at 0 with no gap. The entry's own top-level `digest` (CHI-021) MUST be computed over the concatenation of all parts in `part_index` order — never over any single part alone, and never over any other ordering.

**[CHI-025]** Every part MUST independently satisfy CHI-021/023's digest/size check, against its own `digest`/`size` fields, before being concatenated with any other part. A verifier encountering one corrupt or truncated part MUST refuse the whole artifact rather than assemble from whichever parts did individually verify.

**[CHI-026]** An artifact MAY be published compressed for transport (`compression`, naming the scheme, e.g. `zstd`); `digest` and `size` (CHI-021, CHI-023) always describe the artifact's decompressed bytes. Compression is a transport-layer concern this contract's integrity checks see through — it MUST NOT be used as, or mistaken for, a substitute for CHI-021/023's own checks.

**[CHI-027]** An entry's `status: yanked` MUST NOT be removed from its channel's index — a yanked entry remains present, still digest/size-verifiable, so that a client already holding a reference to it (for instance an already-completed download awaiting a later step) can still distinguish "yanked" from "never existed." Resolution-time handling of a yanked entry is defined in Revocation classes.

**[CHI-028]** Where `compression` (CHI-026) is present, a verifier MUST decompress the downloaded bytes — an unsplit artifact's, or, where split, one part's at a time (CHI-024, CHI-025) — through a bounded, streaming decompressor that tracks decompressed output size as it is produced and ABORTS decompression the instant that running total would exceed the corresponding trusted `size` field (the entry's own `size`, CHI-023, or that specific part's own `size`, CHI-025) — a bound itself never more than 2 GiB per single `download_url` (CHI-082) — rather than buffering a complete decompressed payload into memory before ever comparing its length against that `size`. This guard MUST execute before, and independent of, CHI-023's (or CHI-025's) own post-decompression byte-count comparison: `download_url` (CHI-022) names untrusted transport fetched pre-authentication, ahead of CHI-050 step 8's digest/size check, so an unbounded decompressor is itself an attack surface a malicious host's compressed payload (a decompression bomb) exploits regardless of what a fully-decompressed byte count would eventually compare to. An abort under this guard MUST be treated as the same `SIZE_MISMATCH` (or, for a part, `PART_INVALID`) outcome (Error taxonomy) as a post-decompression size mismatch — the artifact is refused either way.

**[CHI-029]** An artifact entry (Index schema) MAY carry `hold_hours`: a non-negative integer of hours a staged rollout holds that entry from new-install eligibility (CHI-030), and, whenever `hold_hours` is present, MUST also carry `published_at` — the entry's own publication timestamp (Unix epoch milliseconds) `hold_hours` is measured from. An entry carrying no `hold_hours` is immediately install-eligible on its signed metadata alone, equivalent to `hold_hours: 0`. `hold_hours` and `published_at` apply to an entry whose `kind` (CHI-020) is `platform-release` or `relay-release`, and, through CHI-032, to any additional `kind` a companion contract defines under CHI-031's extension point — this contract's own two kinds are not the closed limit of the mechanism's applicability.

**[CHI-030]** A verifier MUST treat an artifact entry carrying a nonzero `hold_hours` (CHI-029) as ineligible for selection in a NEW install or update decision until at least `hold_hours` hours have elapsed since its `published_at` — checked at *resolution* time, the moment a verifier is about to select a version for a new decision, exactly as CHI-072 checks artifact-class revocation; it MUST NOT be checked only once, at original publish time. An entry the signing targets role additionally marks `security_flagged: true` MAY be selected before its own hold elapses — the sole exception this contract defines to CHI-029's hold, for a release superseding a since-disclosed vulnerability. A version already installed before its own hold elapsed is unaffected by this requirement, exactly as CHI-072 states for a later yank: this requirement governs only whether the index may resolve a held entry for a new decision, never an already-running artifact's own lifecycle (Scope).

**[CHI-031]** CHI-020's `kind` and `status` value sets are the closed sets **this** contract defines, but they are not closed against a companion contract: a companion contract (Scope) MAY extend either value set with additional members it defines and owns, for the entries it resolves under its own root-signed publisher-namespace delegation (CHI-012). An entry carrying a companion-defined `kind` or `status` remains gated by the same signing roles (CHI-001–012) and the same client verification order (CHI-050) as any entry this contract defines — the extension point widens the vocabulary, never the trust model. A verifier that does **not** implement the companion contract, encountering an entry whose `kind` is a value it does not recognize, MUST **skip that individual entry** — treat it as not-resolvable, exactly as if that entry were absent — and MUST NOT refuse the whole index over it, and MUST NOT treat an unrecognized `status` value as if it were `active`. This extends CHI-090's additive-tolerance rule (which, as stated there, covers an unrecognized *field*) to an unrecognized *value* of the `kind`/`status` fields: an unknown `kind`/`status` from a later minor or a companion contract is skip-at-entry-grain, never refuse-at-index-grain. A companion contract MAY define, for the kinds it owns, a stricter local outcome than a skip for a client that *does* implement it (for instance a named refusal); the skip rule here is the floor guaranteeing forward-compatibility for a client that does not.

**[CHI-032]** `hold_hours`/`published_at` (CHI-029) and their resolution-time eligibility check (CHI-030) apply to a companion-defined `kind` (CHI-031) exactly as to `platform-release`/`relay-release`, wherever the companion contract states its kinds carry them — the staged-rollout mechanism is not restricted to this contract's own two kinds. CHI-030's `security_flagged` early-selection exception applies unchanged; a companion contract MAY impose an **additional** authentication bar on that exception for its own kinds (it MUST NOT weaken CHI-030's mechanics), but this contract fixes only that the base mechanism carries over.

### Channel namespaces

**[CHI-040]** This contract defines exactly two channel-namespace families: a **platform-train channel**, naming one released, conformance-tested combination of the platform's own components at a shared version line, and a **relay channel**, naming the relay's own independently-released stream. A channel identifier MUST be unique across the whole trust bundle's delegations (CHI-009).

**[CHI-041]** A platform-train channel's index MAY enumerate both `platform-release` entries (that train's own components) and `relay-release` entries (the relay build versioned and conformance-tested alongside that train). A relay channel's index MUST enumerate `relay-release` entries only — never a `platform-release` entry.

**[CHI-042]** A deployed unit MUST bind its own update source to exactly one channel identifier at a time (Definitions); this contract defines the index format that bound channel resolves through, not how a unit selects which channel identifier to bind to, which is deployment configuration (Scope).

**[CHI-043]** A relay MUST resolve its own `relay-release` artifact from exactly one of: (a) a platform-train channel's index, when its update source is bound to that train (CHI-041's `relay-release` entries); or (b) the independent relay channel, when its update source is bound there instead. A single relay's update source MUST NOT be bound to both at once, and this contract defines no mechanism for a relay to consult both channels' indexes for the same update decision.

### Client verification order

**[CHI-050]** A verifier MUST perform the following steps, in this exact order, for any index resolution, and MUST NOT act on — cache as current, expose to a caller, or use to authorize a download of — any layer's content before every earlier step in this order has succeeded:

1. Verify the timestamp role's signature against the trust bundle (CHI-007).
2. Check the timestamp's own freshness and version (Freshness and rollback protection) — refuse to proceed if either check fails.
3. Using the timestamp's own reference to the current snapshot `{version, digest}` (CHI-005), fetch the snapshot role's metadata (if not already cached at that exact version) and verify its signature against the trust bundle.
4. Check the snapshot's own version against the verifier's last-persisted snapshot version (Freshness and rollback protection) — refuse to proceed if this is a regression.
5. Using the snapshot's own reference to the target channel's index `{version, digest}` (CHI-004), fetch that index (if not already cached at that exact version) and verify its signature against the key(s) the trust bundle's namespace delegation (CHI-009) authorizes for that channel.
6. Check the index's own `version` against the verifier's last-persisted version for that channel (Freshness and rollback protection) — refuse to proceed if this is a regression.
7. Only once steps 1–6 all succeed, locate the specific artifact entry within the now-trusted index and download its bytes (and every part's bytes, where split).
8. Verify the downloaded (and reassembled, if split) bytes against the now-trusted entry's own `digest` and `size` (Index schema) — refuse the artifact on either mismatch.
9. Verify the revocation feed's own signature against the trust bundle's delegated revocation-role key (CHI-011); check the feed's own freshness against its stated max-age (CHI-074). Only once both of those succeed, evaluate the now-trusted feed's `revoked_keys` and `revoked_artifacts` (Revocation classes) for the resolved artifact's key-id, `artifact_id`, and `version` as a final gate before the artifact is used. A revocation feed failing either its signature or its freshness check MUST be refused exactly as CHI-073's unreachable-feed case is refused — fail-closed, never treated as "nothing revoked."

**[CHI-051]** A failure at any step of CHI-050 MUST abort verification at that step; no later step's success MAY be used to compensate for, or excuse, an earlier step's failure — a correct artifact digest (step 8) MUST NOT cause a verifier to overlook a targets-role signature that failed to verify at step 5 against the metadata that named that digest in the first place.

**[CHI-052]** Every signature check in CHI-050 (steps 1, 3, 5, 9) MUST validate against only the trust bundle's own currently-recognized key-ids (CHI-007) — never against a key-id supplied by the fetched metadata itself as though the metadata could self-authorize its own signer.

**[CHI-053]** Where a verifier's already-cached copy of a role's metadata already matches the exact `{version, digest}` its parent role currently references, the verifier MAY skip re-fetching that role's file — but MUST NOT skip re-verifying that cached copy's own signature and version against the newly-fetched parent's reference. A cache hit shortens the fetch; it never shortens the verification.

### Freshness and rollback protection

**[CHI-060]** Every role's metadata (timestamp, snapshot, and each channel's own index) MUST carry a monotonically increasing `version` integer, scoped independently per role and, for the targets/index layer, per channel (CHI-064). A verifier MUST persist the highest `version` it has successfully verified for each such scope and MUST refuse — never merely warn on — any subsequently-fetched metadata for that same scope carrying a `version` lower than its own persisted high-water mark.

**[CHI-061]** The timestamp role's metadata MUST additionally carry its own signing time and a stated max-age. A verifier MUST treat timestamp metadata older than its max-age as stale and MUST refuse to proceed past step 2 of the verification order (CHI-050) on a stale timestamp — even where its signature and version both otherwise check out.

*draft-note: no normative source yet fixes the timestamp role's own max-age value; propose a short bound (on the order of hours, not days), consistent with the timestamp role's purpose of bounding how long a stale-but-validly-signed metadata set could otherwise be replayed — revisit once real numbers exist.*

**[CHI-062]** A verifier's persisted high-water-mark version state (CHI-060) MUST survive a process or device restart — an attacker MUST NOT be able to reset rollback protection merely by causing the verifying process or device to restart.

**[CHI-063]** Under a local clock a verifier cannot trust (an independently-verified clock-trust floor being unavailable), a verifier MUST NOT refuse metadata *solely* for appearing to violate CHI-061's max-age; it MUST instead rely on the monotonic version check (CHI-060) alone for that decision, and MUST refuse only the specific action that itself depends on freshness it cannot establish (an install or an update) while leaving an already-installed artifact running — never brick a working installation over an unverifiable clock.

**[CHI-064]** A verifier's persisted high-water-mark version (CHI-060) MUST be scoped per role, per channel — a version regression check on one channel's index MUST NOT be satisfied, and MUST NOT be defeated, by a version number observed on a different channel.

### Revocation classes

**[CHI-070]** The revocation feed (Definitions) MUST enumerate revoked entries as one of exactly two classes: a **key/trust-class revocation** (`revoked_keys`: a compromised or retired signing key-id, for any role) and an **artifact-class revocation** (`revoked_artifacts`: a specific `{artifact_id, channel, version}` marked yanked). The feed itself MUST be signed and MUST carry its own monotonic `version` (CHI-074).

**[CHI-071]** A key/trust-class revocation MUST be enforced as mandatory and immediate: once a verifier has fetched a revocation feed naming a key-id as revoked, it MUST refuse to accept ANY metadata or artifact signed by that key-id from that point forward — regardless of an otherwise-valid signature, an otherwise-current version, or an otherwise-matching digest on the content that key-id signed.

**[CHI-072]** An artifact-class (`yanked`, CHI-027) revocation MUST be checked at *resolution* time — the moment a verifier is about to select a version for a NEW install or update decision — against the freshest fetched revocation feed and index; it MUST NOT be checked only once, at original publish time. A version already installed before its revocation is unaffected by this contract: whether an already-running artifact is rolled back, held, or left running on a later yank is governed by whatever contract owns that installed artifact's own lifecycle, out of this contract's scope (Scope) — this contract governs only whether the index may resolve a yanked version for a new decision, which it MUST NOT.

**[CHI-073]** A verifier unable to fetch the revocation feed at all MUST NOT default to treating an artifact as un-revoked — fetch failure and an explicit "nothing revoked" response are distinct outcomes this contract requires a verifier to distinguish. The specific fallback policy (block the pending install/update outright, or proceed under a bounded grace window against the last-fetched feed) is deployment policy this contract does not fix, beyond requiring that a verifier never silently collapses "feed unreachable" into "feed says clean."

**[CHI-074]** The revocation feed's own freshness and rollback protection MUST follow the same monotonic-version and max-age discipline as Freshness and rollback protection, applied to the feed's own `version` counter — independent of, and checked in addition to, any one channel's own index version.

### Hosting and size envelope

**[CHI-080]** A channel's index MUST be resolvable from one stable URL that does not change across a migration of the hosting backend serving it. The index format itself MUST carry no field whose validity depends on being fetched from one specific hosting backend — `download_url` (and each part's own `download_url`) is the only backend-specific value this schema carries, and it MAY name any HTTP(S) host.

**[CHI-081]** Every `download_url` this contract's schema carries MUST satisfy CHI-022 (a browser-download-class URL, unauthenticated, no separate rate limit) regardless of which hosting backend currently serves it — moving an index or its artifacts to a different backend MUST change no verification outcome this contract defines, because every integrity property this contract provides (signature, digest, size) is independent of transport.

**[CHI-082]** An artifact whose complete decompressed size would exceed **2 GiB** (2,147,483,648 bytes) MUST be published split (CHI-024); an implementation MUST NOT publish a single `download_url` object, or a single split part, exceeding that cap.

**[CHI-083]** A verifier MUST treat every `download_url`'s hosting location as untrusted transport: CHI-050's verification order establishes trust in an artifact's `digest`/`size` entirely from signed metadata (steps 1–6) before that artifact's bytes are ever fetched (step 7) — a compromised or malicious host serving `download_url` can cause a fetch to fail CHI-021/023's checks, but cannot cause a verifier to accept different bytes than the signed metadata described.

### Index format versioning

**[CHI-090]** A `ChannelIndex`'s own `format_version` (Wire shapes) MUST be a `major.minor` string. A verifier encountering a `format_version` whose major it does not implement MUST refuse to resolve that channel's index (falling back to whatever version it last successfully verified, per Freshness and rollback protection) rather than parse a schema it does not understand. A minor it does not recognize MUST NOT itself cause refusal — an artifact-entry field this contract has not yet defined is additive and ignored.

## Wire shapes

```json
// TimestampRole — signed envelope (Signed index roles)
{
  "signed": {
    "role": "timestamp",
    "version": 482,
    "signing_time": 1752623000000,
    "expires_at": 1752623900000,
    "snapshot_ref": { "version": 217, "digest": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" }
  },
  "signatures": [
    { "key_id": "ed25519:timestamp-2026-07", "sig": "MEUCIQDx7f2a...timestamp-sig" }
  ]
}
```

```json
// SnapshotRole — signed envelope
{
  "signed": {
    "role": "snapshot",
    "version": 217,
    "signing_time": 1752623000000,
    "targets_refs": {
      "platform-train/stable": { "version": 96, "digest": "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" },
      "relay-channel/stable": { "version": 41, "digest": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85" }
    }
  },
  "signatures": [
    { "key_id": "ed25519:snapshot-2026-07", "sig": "MEUCIQCy91fa...snapshot-sig" }
  ]
}
```

```json
// ChannelIndex — the targets-delegated file for one channel (Index schema)
{
  "signed": {
    "role": "targets",
    "format_version": "1.0",
    "channel": "platform-train/stable",
    "version": 96,
    "signing_time": 1752620000000,
    "artifacts": [
      {
        "artifact_id": "waiveo-app",
        "kind": "platform-release",
        "version": "1.4.2",
        "arch": "linux/amd64",
        "status": "active",
        "digest": "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
        "size": 184320000,
        "download_url": "https://github.com/waiveo/waiveo-next/releases/download/v1.4.2/waiveo-app-linux-amd64.tar.zst",
        "compression": "zstd",
        "published_at": 1752619000000,
        "hold_hours": 24
      },
      {
        "artifact_id": "waiveo-relay",
        "kind": "relay-release",
        "version": "1.4.2",
        "arch": "linux/arm64",
        "status": "active",
        "digest": "sha256:aaaabbbbccccddddeeeeffff00001111222233334444555566667777888899",
        "size": 9437184,
        "download_url": "https://github.com/waiveo/waiveo-next/releases/download/v1.4.2/waiveo-relay-linux-arm64"
      }
    ]
  },
  "signatures": [
    { "key_id": "ed25519:platform-train-targets-2026", "sig": "MEQCIHc4df...targets-sig" }
  ]
}
```

```json
// A split artifact entry (Index schema — split parts, CHI-024)
{
  "artifact_id": "waiveo-derive",
  "kind": "platform-release",
  "version": "1.4.2",
  "arch": "linux/amd64",
  "status": "active",
  "digest": "sha256:1111222233334444555566667777888899990000aaaabbbbccccddddeeee",
  "size": 3221225472,
  "parts": [
    { "part_index": 0, "digest": "sha256:aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666000077778888", "size": 2147483648, "download_url": "https://cdn.example/waiveo-derive-1.4.2.part0.zst" },
    { "part_index": 1, "digest": "sha256:bbbb2222cccc3333dddd4444eeee5555ffff666600007777888899990000", "size": 1073741824, "download_url": "https://cdn.example/waiveo-derive-1.4.2.part1.zst" }
  ],
  "compression": "zstd"
}
```

```json
// RevocationFeed — signed envelope (Revocation classes)
{
  "signed": {
    "role": "revocation",
    "version": 14,
    "signing_time": 1752620500000,
    "revoked_keys": [
      { "key_id": "ed25519:platform-train-targets-2025-stale", "revoked_at": 1752600000000, "reason": "key rotation" }
    ],
    "revoked_artifacts": [
      { "artifact_id": "waiveo-app", "channel": "platform-train/stable", "version": "1.4.1", "revoked_at": 1752610000000, "reason": "security" }
    ]
  },
  "signatures": [
    { "key_id": "ed25519:revocation-2026-07", "sig": "MEYCIQD82ea...revocation-sig" }
  ]
}
```

## Negotiation

A `ChannelIndex`'s `format_version` field (Index format versioning, CHI-090) is this contract's schema-versioning point; role-layer trust (root/snapshot/timestamp) is negotiated implicitly through Client verification order and Freshness and rollback protection rather than through any separate handshake — an index file is a fetched artifact, not a connection:

- **Major mismatch** — refuse to resolve; retain the last-verified index (CHI-090).
- **Minor mismatch** — proceed; unrecognized fields are additive and ignored.
- Role-layer versioning (root, snapshot, timestamp) is governed entirely by Freshness and rollback protection's monotonic-version rule, not by `format_version` — `format_version` names only the artifact-entry schema this contract itself defines (Index schema).

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `CHANNEL_INDEX_SIGNATURE_INVALID` | A role's metadata (timestamp, snapshot, or a channel index) did not verify against the trust bundle's recognized key-id(s) for that role (CHI-050 steps 1/3/5). | no — the served content is untrusted; retry only after the underlying key/hosting issue is resolved |
| `NAMESPACE_NOT_DELEGATED` | An index entry's channel is not covered by its signer's namespace delegation (CHI-009). | no |
| `VERSION_ROLLBACK_REJECTED` | A role's fetched `version` was lower than the verifier's own persisted high-water mark for that role/channel scope (CHI-060, CHI-064). | no |
| `TIMESTAMP_STALE` | The timestamp role's metadata is older than its stated max-age (CHI-061). | yes — after a fresher timestamp is fetched |
| `DIGEST_MISMATCH` | Downloaded (and reassembled, if split) artifact bytes did not match the trusted entry's `digest` (CHI-021, CHI-025). | yes — re-download; a repeated mismatch indicates a compromised or broken host |
| `SIZE_MISMATCH` | Downloaded (and reassembled, if split) artifact bytes did not match the trusted entry's `size` (CHI-023). | yes — re-download |
| `PART_INVALID` | One split part failed its own digest/size check before concatenation (CHI-025). | yes — re-download that part |
| `KEY_REVOKED` | Metadata or an artifact was signed by a key-id present in the revocation feed's `revoked_keys` (CHI-071). | no |
| `ARTIFACT_YANKED` | The resolved `{artifact_id, channel, version}` is present in the revocation feed's `revoked_artifacts` (CHI-072). | no — resolve a different, non-yanked version |
| `REVOCATION_FEED_UNAVAILABLE` | The revocation feed could not be fetched at all for this resolution (CHI-073). | yes — per the deployment's own fallback policy |
| `REVOCATION_FEED_INVALID` | The revocation feed was fetched but failed its own signature verification against the trust bundle's delegated revocation-role key, or failed its own freshness/rollback check (CHI-050 step 9, CHI-074) — refused fail-closed, distinct from `REVOCATION_FEED_UNAVAILABLE`'s fetch-failure case. | no — the served feed is untrusted or stale; retry only once a validly-signed, current feed is reachable |
| `CHANNEL_KIND_MISMATCH` | A relay channel's index carried a `platform-release` entry, or a `kind` value otherwise violated Channel namespaces (CHI-041). | no |

## Conformance notes

- Traceability map: `conformance/traceability/channel-index.md` — maps every `CHI-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/channel-index/` — one JSON case file per `case-id` referenced from the traceability map.
- The operational custody of any role's signing key (an offline root-key ceremony, an online signer's own key-management mechanics, CHI-006) is out of this contract's scope (Scope) and is not exercised by this corpus; cases that need a signature treat the signing key's own legitimacy as a given, already-established input, and exercise only whether a verifier's stated logic reaches the correct accept/refuse outcome from it.
- A marketplace pack or content-package's own index entry is a distinct concern, reserved to a separate contract that may reuse this same signed-index envelope (Scope); this corpus exercises only `platform-release` and `relay-release` entries.
- CHI-030's `hold_hours` eligibility, CHI-061's timestamp max-age, and CHI-074's revocation-feed freshness are all time-dependent properties; corpus cases exercise the accept/refuse decisions these rules produce for a given (`published_at`/signing_time, evaluation_time) pair, not elapsed real time — timing behavior is exercised against an injectable clock in a driver harness, consistent with this platform's other contracts.
- The concrete on-the-wire field names this contract's Wire shapes use (`signed`/`signatures`, `snapshot_ref`, `targets_refs`, etc.) express the semantic role structure and verification order this contract requires; they are not asserted to be byte-identical to any specific underlying metadata-format library's own file shape, which this contract does not name (Scope, CHI-006's custody carve-out extends to implementation-library choice).
