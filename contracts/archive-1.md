# Workspace Archive Format

**Contract:** archive/1
**Version:** 1.0
**Status:** review

## Scope

archive/1 defines the workspace archive: the one portable container file a workspace's entire state — its relational data and its content-addressed assets — exports to and restores from. Backup, box migration, onboarding a workspace onto new infrastructure, and exporting a workspace's data for the operator who owns it are all the same file operation under this contract; what differs between them is only what happens to the resulting file afterward, never its shape.

- In scope: the container's byte-level framing, compression, encryption, and signing; the self-describing manifest's shape — the platform schema epoch, each locked pack's own schema epoch, the pack lockfile, asset references, and the carriage of re-wrappable secret stubs; the streamable and incremental archive structures; the invariants a restore operation MUST uphold (restore is an install path, not a trust bypass); the required content of the printable emergency kit; the size envelope this format is designed and tested against.
- Out of scope: the key hierarchy's own wrap/unwrap algorithm and key-custody model — this contract carries wrapped, opaque key material and references the hierarchy that produces and consumes it, never redefines it; the channel-resolution, trust-state, and revocation mechanics a restore consults when re-verifying a locked pack — referenced, not redefined; a single pack's own update-failure data snapshot and rollback, a distinct, pack-scoped mechanism at a much smaller grain than a workspace archive; the CLI or API surface an operator uses to trigger a create or restore operation (`api/1`); the platform's disk-pressure and health-signaling model that surfaces a capacity problem — referenced, not redefined; whether and how a create or restore operation is itself audit-logged (`events/1`'s `audit.event` schema is the natural home for that record; this contract does not mandate the emission).

## Definitions

- **ULID** — as defined in `manifest/1`: a 26-character Crockford-base32, time-sortable identifier.
- **Timestamp** — as defined in `rules/1`: an integer number of milliseconds since the Unix epoch (UTC).
- **Workspace** — the complete owned state this format exports: a relational store plus a content-addressed asset store, together with the installed packs operating on them.
- **Container** — one archive/1 file: the byte layout this contract defines (Container framing).
- **Manifest** — the container's first logical entry: a JSON document describing everything else the container holds, self-sufficient to validate before any bulk content is read (Manifest — general).
- **Asset** — a content-addressed object, referenced the same way `ctx/1`'s `assets` family references one: a `sha256:` URI.
- **Export passphrase** — the passphrase supplied at export time that a container's encryption key is derived from (Encryption); known only to whoever performed or received that specific export.
- **Recovery passphrase** — the passphrase printed in a workspace's emergency kit at setup time, used to recover that workspace's own data key on its own hardware; a distinct secret from an export passphrase (Emergency kit).
- **Data key** — the key that wraps every secret stub in a workspace, as defined by the platform's key hierarchy; this contract treats it as an opaque value it carries, re-wrapped, never unwraps or inspects.
- **Secret stub** — one wrapped, opaque secret value, as defined by the platform's key hierarchy; this contract carries stubs unchanged, byte for byte, from source workspace to destination.
- **Full archive** — an archive whose manifest declares `mode: full`: self-sufficient, referencing no other archive.
- **Incremental archive** — an archive whose manifest declares `mode: incremental`: a delta against a named base archive (Incremental archives).
- **Trust state** — the platform's current view of which pack versions are signed, channeled, and not revoked; maintained and evaluated outside this contract. A restore consults it fresh (Restore is an install path); it is never read from the archive's own recollection of it.
- **Developer mode** — a destination's own consent-gated setting for accepting unsigned or dev-channel pack versions; maintained outside this contract.
- **Emergency kit** — the printable recovery artifact this contract requires the platform to generate for a workspace's data key (Emergency kit).

## Normative requirements

### Container framing

**[ARC-001]** A container file MUST be laid out as exactly three consecutive regions: a 4-byte big-endian unsigned integer `N`; then `N` bytes of UTF-8 JSON, the outer header (ARC-002), in cleartext; then the remainder of the file to EOF, the encrypted body (Encryption).

**[ARC-002]** The outer header MUST be an object with at least the fields `format` (the literal string `waiveo-archive`), `archive_format_version` (a `major.minor` string), `kdf` (Encryption), `digest` (the sha256 of the encrypted body region, hex-encoded), `signature`, and `signer_key_id` (Signing).

**[ARC-003]** A reader encountering a `format` value other than `waiveo-archive` MUST reject the file immediately with `FORMAT_UNRECOGNIZED` (Error taxonomy), without attempting to parse anything past the outer header.

**[ARC-004]** `archive_format_version`'s major component MUST match exactly between a container and the implementation reading it; a reader encountering a different major MUST refuse with `VERSION_UNSUPPORTED` before reading the encrypted body. A reader MAY accept a container whose minor component is newer than its own, tolerating any additive manifest field it does not recognize (Manifest — general).

### Encryption

**[ARC-010]** The export passphrase MUST be stretched by a memory-hard key-derivation function into an export root key; the `kdf` header field MUST record the algorithm and every parameter (including a per-archive random salt) needed to repeat that derivation, since a reader cannot derive the key without them.

*draft-note: the exact KDF (proposed: Argon2id) and its parameters are this contract's own proposal, not fixed by any normative source yet.*

**[ARC-011]** Two independent sub-keys MUST be derived from the export root key using distinct, fixed context labels — one for encrypting the container body (ARC-012), one for re-wrapping the workspace data key (Manifest — secret stubs and data key) — so the same derived key material is never used for two different cryptographic purposes.

**[ARC-012]** The container body MUST be produced by first assembling the tar stream (Manifest — general, Streaming structure), then compressing it with zstd, then encrypting the compressed result — never compressing already-encrypted bytes, which wastes the compressor's effort against high-entropy input.

**[ARC-013]** The encrypted body MUST be a sequence of independently authenticated frames, each a 4-byte big-endian length prefix followed by that many bytes of AEAD ciphertext (including its authentication tag), so a reader can decrypt and emit plaintext as frames arrive rather than buffering the whole body. Each frame's nonce MUST be derived from a per-archive base nonce (recorded in the outer header) combined with that frame's own sequence number, so no nonce repeats under the same key. Each frame's AEAD encryption MUST additionally bind a boolean final-flag as associated data, with exactly one frame — the last — marked final and every other frame marked non-final; ARC-016 defines what a reader does with this flag.

*draft-note: the exact AEAD cipher (proposed: XChaCha20-Poly1305, for its wide nonce making per-frame nonce derivation trivially collision-free) and frame size (proposed: 1 MiB) are this contract's own proposal, not fixed by any normative source yet.*

**[ARC-014]** A frame that fails AEAD authentication MUST abort the read immediately with `DECRYPT_FAILED` (Error taxonomy) — a reader MUST NOT emit any plaintext from a frame that failed to authenticate, and MUST NOT continue attempting to decrypt subsequent frames once one has failed. This authentication covers the frame's final-flag (ARC-013) exactly as it covers its ciphertext: a reader MUST treat a frame's final-flag as trustworthy once, and only once, that frame has itself authenticated.

**[ARC-015]** `DECRYPT_FAILED` (a frame failing authentication, most commonly from a wrong export passphrase) MUST be distinguishable, by its own code, from `ARCHIVE_SIGNATURE_INVALID` (Signing) — the two failure modes have different remedies (wrong passphrase versus a tampered or corrupted file) and MUST NOT be reported as one undifferentiated error.

**[ARC-016]** A restorer MUST refuse with `ARCHIVE_TRUNCATED` (Error taxonomy) — distinguishable from both `DECRYPT_FAILED` and `ARCHIVE_SIGNATURE_INVALID`, neither of which means content is missing — if it reaches EOF without having authenticated a frame whose final-flag (ARC-013) is set, or if any byte follows the frame whose final-flag is set. Both conditions signal a frame sequence shorter or longer than the one produced at export time; per-frame authentication (ARC-014) alone cannot catch either, since a dropped tail's remaining frames still authenticate individually and a frame appended after the final one authenticates on its own terms too.

**[ARC-017]** The per-archive base nonce (ARC-013) MUST be generated fresh and uniformly at random for every archive — never derived deterministically from the export passphrase, the workspace ID, or any other value that could repeat across archives — as defense-in-depth against nonce collision, independent of and in addition to ARC-013's own per-frame sequence-number derivation.

### Signing

**[ARC-020]** `digest` (ARC-002) MUST be computed over the encrypted body exactly as it appears in the container, before any decryption. Because `digest` is itself one field of the header ARC-021's signature covers, computing it this way — over ciphertext, needing no export passphrase — is what lets that signature attest to the encrypted body's identity without decrypting it.

**[ARC-021]** `signature` MUST be a signature over a canonicalization of the entire cleartext outer header (ARC-002) except the `signature` field itself — never over `digest` alone — verifiable against the exporting workspace's own operational signing identity. Covering the whole header this way authenticates `kdf`'s parameters and `base_nonce` exactly as it authenticates `digest`: a reader MUST verify this signature — and, transitively, every other header field — before invoking the KDF (ARC-010) with any `kdf`-supplied parameter, so a tampered `kdf.memory_kib` or `iterations` value fails signature verification rather than driving a memory or CPU allocation shaped by unauthenticated input. This is never the platform's software-artifact trust bundle: that bundle authenticates published pack and platform release artifacts and is an unrelated trust root from a workspace's own operational identity.

*draft-note: the exact canonicalization scheme the signature covers (proposed: a fixed-key-ordering JSON canonicalization, e.g. RFC 8785) is not fixed here and remains open. The key material `signer_key_id` resolves against is now defined: `security-model/1`'s workspace signing key (`security-model/1` SEC-046) is the signing identity whose private half produces this signature and whose public half (or a certificate binding to it) `signer_key_id` resolves against — that contract's own key hierarchy owns its custody (SEC-047–048), not this one. This contract fixes the header shape, the set of fields the signature covers (ARC-021), and the verification obligation (ARC-022–023); only the canonicalization scheme itself remains an open proposal.*

**[ARC-022]** Signature verification (ARC-021) MUST be possible from the outer header alone, without decrypting any part of the encrypted body — this is the reason the outer header is cleartext rather than itself encrypted.

**[ARC-023]** A restore operation MUST verify the signature (ARC-021) before decrypting or reading any part of the encrypted body, and MUST refuse with `ARCHIVE_SIGNATURE_INVALID` (Error taxonomy) on failure — a tampered or corrupted archive is rejected before any of its content is trusted enough to even attempt decrypting.

**[ARC-024]** In addition to ARC-023's header-signature check, a restorer MUST recompute the sha256 digest over the encrypted body's actual bytes as they stream past — once ARC-016's frame-sequence-completeness check is satisfied — and MUST refuse with `ARCHIVE_SIGNATURE_INVALID` (Error taxonomy) if the recomputed value does not match the header's `digest` field (ARC-002). ARC-023 alone proves only that the header's own recorded `digest` value was validly signed; this recompute is what proves the bytes actually delivered are the bytes that value describes — a restore that passes both checks has verified the body, not merely the header's claim about it. A mismatch discovered this way after manifest validation (Manifest — general) has already passed falls under Restore is an install path's rollback guarantee (ARC-107) exactly as any other post-manifest failure does.

### Manifest — general

**[ARC-030]** The manifest MUST be the first entry of the tar stream carried inside the encrypted body — a reader consumes and validates it before any other entry is available to read, which is what makes the pre-flight checks in Restore is an install path possible without first streaming the whole archive.

**[ARC-031]** The manifest MUST be a JSON object with at least the fields `created_at` (Timestamp), `mode` (`full` or `incremental`), `workspace_id` (ULID), `platform_schema_epoch` (Manifest — platform schema epoch), `packs` (Manifest — pack lockfile), `assets` (Manifest — asset references), `secret_stubs` and `data_key_wrap` (Manifest — secret stubs and data key); an incremental-mode manifest additionally requires `base_archive` (Incremental archives).

**[ARC-032]** A reader MUST tolerate an unrecognized top-level manifest field or an unrecognized optional field within one of ARC-031's objects, treating it as forward-compatible minor-version growth (ARC-004) rather than a validation failure.

**[ARC-033]** A manifest failing to satisfy ARC-031's required-field shape, or any other requirement in this Manifest group, MUST cause the restore to refuse with `MANIFEST_INVALID` (Error taxonomy) before any asset or the workspace snapshot entry is written to the destination.

### Manifest — platform schema epoch

**[ARC-040]** `platform_schema_epoch` MUST be a positive integer, set at export time to the source workspace's own platform schema epoch.

**[ARC-041]** A restore operation MUST refuse to open — apply into a destination, or boot from — an archive whose `platform_schema_epoch` is newer than the destination understands, exactly as the destination would refuse to open a live workspace at that epoch; refusal MUST be a typed error, never an attempted downgrade-open.

**[ARC-042]** An archive whose `platform_schema_epoch` is older than the destination understands MAY be opened via the destination's normal migrate-on-open path; this contract requires only that the number be carried honestly, not that this contract itself perform the migration.

### Manifest — pack lockfile

**[ARC-050]** `packs` MUST be an array of objects, each with at least `pack_id` (`manifest/1` MAN-001's `<publisher>/<name>` form), `version` (MAN-002's dotted form), `channel` (the pack's trust channel at export time), `source` (the registry source it was resolved from), and `schema_epoch` (the locked pack's own `dataModel.version`, `manifest/1` MAN-050, at export time) — together, the pack lockfile.

**[ARC-051]** `pack_id` MUST be unique within one manifest's `packs` array — a workspace locks at most one version of any given pack.

**[ARC-052]** `channel` MUST distinguish a dev-channel lock from any other, since restore-time gating (Restore is an install path) treats a dev-channel lock differently from every other channel.

### Manifest — asset references

**[ARC-060]** `assets` MUST be an array of objects, each with `asset_ref` (a `sha256:` URI, in the same form `ctx/1`'s `assets` family uses), `size` (bytes), an optional `content_type`, and `storage`, one of `embedded`, `by-reference`, or `inherited` (Incremental archives).

**[ARC-061]** An `embedded` entry's bytes MUST appear as a tar entry at `assets/<hex>`, where `<hex>` is `asset_ref`'s hash portion with its `sha256:` prefix stripped. A `by-reference` or `inherited` entry MUST NOT have a corresponding tar entry — its bytes are not carried in this container.

**[ARC-062]** For every `embedded` entry, a restorer MUST recompute the sha256 of the entry's bytes as they stream past and MUST refuse the restore with `MANIFEST_INVALID` if the computed hash does not match `asset_ref` — an asset is trusted only by matching its own name, never by the manifest's unverified say-so.

**[ARC-063]** A `by-reference` entry declares that its bytes are not carried in this container because the destination is expected to already hold them, or to be able to obtain them independently of this container's own byte stream. A restore encountering a `by-reference` entry it cannot resolve by either means MUST fail closed with `ASSET_UNAVAILABLE` (Error taxonomy) rather than silently proceed with a broken reference.

**[ARC-064]** Every asset the restored workspace's own relational data references MUST appear in `assets` under one of the three `storage` values — an asset referenced by workspace data but absent from the manifest entirely MUST cause `MANIFEST_INVALID` at manifest-validation time (ARC-033), never discovered partway through a restore already in progress.

**[ARC-065]** `content_type`, where present, is informative only — no requirement in this contract conditions behavior on its value.

### Manifest — secret stubs and data key

**[ARC-070]** `secret_stubs` MUST be an array of objects, each with `stub_id` (ULID) and `wrapped_value` (an opaque value, produced and interpreted solely by the platform's key hierarchy). This contract defines no further shape for `wrapped_value` and performs no operation on its contents beyond carrying it unchanged.

**[ARC-071]** `data_key_wrap` MUST carry the source workspace's own data key, re-wrapped under the sub-key ARC-011 derives for that purpose — never the raw, unwrapped data key, and never a secret stub re-wrapped individually. This is what makes the entire `secret_stubs` array portable as one unit without the export operation touching a single secret's own value.

**[ARC-072]** No requirement in this contract, and no step of a conformant export or restore, MUST cause an individual secret's plaintext value to be computed, logged, or written anywhere outside the destination's own key hierarchy after a successful restore — every intermediate step (export, transport, restore up to the final re-wrap) handles `secret_stubs` entries as opaque bytes.

**[ARC-073]** Restore MUST, as its first step touching secret material, re-wrap `data_key_wrap`'s data key under the destination's own key hierarchy, and MUST NOT retain the export-passphrase-derived wrapping (ARC-011) as part of the destination's ongoing operational state once that re-wrap succeeds.

**[ARC-074]** A restore presented with an export passphrase that fails to decrypt the container (`DECRYPT_FAILED`, ARC-014) MUST be reported distinguishably from a restore that decrypts successfully but then fails a later check (`MANIFEST_INVALID` or any other manifest-validation-class code in Error taxonomy) — an operator MUST be able to tell "wrong passphrase" from "broken or tampered file" from the error alone.

### Streaming structure

**[ARC-080]** A conformant implementation MUST be able to create a container as a single forward pass over its own source data, with memory use bounded independent of workspace or asset-store size — never requiring the full tar stream, the full compressed stream, or the full container to be materialized on local disk before the last byte is written.

**[ARC-081]** A conformant implementation MUST be able to restore a container as a single forward pass, writing each `embedded` asset entry (Manifest — asset references) directly into content-addressed storage as its bytes stream past, without requiring free space for a second full copy of the archive's asset content.

**[ARC-082]** Manifest-first ordering (ARC-030) is what makes ARC-081 possible: a restorer MUST fully validate the manifest — format version, platform schema epoch, signature, and the trust-state re-checks in Restore is an install path — before consuming any asset entry, and MUST abort before writing any asset if manifest validation fails.

**[ARC-083]** The workspace's relational store MUST enter the archive only via a consistent-snapshot mechanism (an online backup or an equivalent atomic-snapshot operation) — never a raw filesystem copy of a store still open for live writes, which risks capturing a torn, inconsistent image.

**[ARC-084]** ARC-083 constrains how the workspace snapshot entry is produced, not whether it can stream: the resulting snapshot's bytes MAY still be written into and read from the same single forward-streaming pass ARC-080–081 describe.

**[ARC-085]** The workspace snapshot tar entry (`workspace.sqlite`) carries no manifest-recorded hash of its own, unlike an `embedded` asset's `asset_ref` (ARC-062) — it does not need one: the workspace snapshot is always embedded in full, inside the same encrypted body ARC-024's digest recompute already covers (ARC-091), so a restorer that has passed ARC-024's check has verified `workspace.sqlite`'s actual bytes exactly as thoroughly as ARC-062 verifies an embedded asset's.

### Incremental archives

**[ARC-090]** An incremental archive's manifest MUST carry `base_archive`: `{digest, created_at}`, identifying the prior archive (by its own outer-header `digest`, ARC-002) this one deltas against.

**[ARC-091]** An incremental archive's `assets` array MUST enumerate every asset the resulting workspace references, exactly as a full archive's does (ARC-064) — never only the entries new to this archive. An entry whose bytes are already present in the base archive (embedded or by-reference there) MUST be marked `storage: inherited` here rather than re-embedded, so unchanged assets cost nothing beyond one manifest row. The workspace snapshot entry itself MUST always be embedded in full, regardless of mode — it is never incrementally diffed.

**[ARC-092]** Restoring an incremental archive MUST have access to the complete base-archive chain back to the nearest full archive, resolving every `inherited` entry along the way. A restorer unable to resolve that chain MUST refuse with `BASE_ARCHIVE_UNAVAILABLE` (Error taxonomy) rather than restore a workspace with missing asset content.

**[ARC-093]** An assets-by-reference export (`storage: by-reference`, Manifest — asset references) and an incremental export (`storage: inherited`) are independent mechanisms and MAY be combined freely in the same manifest — one entry MAY be `by-reference` against a shared asset store while a sibling entry is `inherited` from a base archive and a third is freshly `embedded`.

**[ARC-094]** Every archive touched while resolving a base-archive chain (ARC-092) — not merely the terminal, most-recently-requested archive — MUST independently satisfy every requirement in Container framing, Encryption, and Signing on its own bytes: its own header parses and matches `archive_format_version` (ARC-001–004), its own encrypted-body frames authenticate and terminate on exactly one final-marked frame with nothing trailing (ARC-013–016), its own header signature verifies (ARC-021), and its own digest is recomputed against its own actual streamed bytes and matches (ARC-024). A base archive's actual bytes MUST additionally match the `base_archive.digest` its child manifest records (ARC-090) — a base archive earns trust only by satisfying these checks itself, never by inheriting a child archive's already-established trust.

### Restore is an install path

**[ARC-100]** A restore MUST leave the destination in the same state a fresh installation followed by fresh enrollment would produce, re-establishing every identity and credential natively rather than cloning the source workspace's own live identity — restore is an install path, never a trust bypass.

**[ARC-101]** Every locked pack (`packs`, Manifest — pack lockfile) MUST be re-verified against the destination's own current trust state at restore time — never against any trust assertion the archive itself might carry or imply. An archive recorded before a pack's trust status changed carries no special standing; the destination's present-day trust state is authoritative.

**[ARC-102]** A locked pack version the destination's current trust state marks revoked or yanked MUST either restore with a substituted version chosen by the destination's normal channel-resolution rule, or have that one pack's restore blocked — either path MUST raise an operator-facing signal, and silently restoring the yanked version is not a legal outcome of either path.

**[ARC-103]** A locked pack on a dev channel (ARC-052) MUST be refused at restore time on a destination without developer mode enabled — refusing that one pack's restore (with a typed signal) rather than failing the entire restore, unless that pack is one the destination cannot boot without.

**[ARC-104]** The platform-schema-epoch refusal rule (ARC-041) applies identically whether an archive is being restored onto fresh infrastructure or applied over an already-running destination — there is no restore-time exception to the newer-epoch refusal a normal boot would also apply.

**[ARC-105]** Restore MUST NOT carry forward any credential, session, or cryptographic identity the source workspace held live at export time — an enrollment, an issued session, or a relay's own connection identity re-establishes fresh after restore, never resumes from an archived value, consistent with ARC-100.

**[ARC-106]** The destination's emergency kit (Emergency kit) after a restore MUST be freshly generated for that destination, never copied from the source workspace's own kit — it protects a data key that ARC-073 has already re-wrapped under the destination's own hierarchy, making the source kit's recovery passphrase meaningless there.

**[ARC-107]** A restore that fails after manifest validation has passed (ARC-082) MUST leave the destination in its pre-restore state — a partially applied restore MUST NOT be left reachable as though it were a valid, opened workspace; the destination either reaches a fully restored state or reverts to what it was before the attempt began.

### Emergency kit

**[ARC-110]** The platform MUST generate a printable emergency kit at the point a workspace's data key is first established, and MUST be able to regenerate one on demand thereafter.

**[ARC-111]** A kit MUST contain, at minimum: the recovery passphrase, an identifier for the workspace or hardware it recovers, and instructions sufficient to complete recovery using only the kit itself and the platform's ordinary recovery tooling — no other document.

**[ARC-112]** A recovery passphrase (Emergency kit) and an export passphrase (Encryption) are distinct secrets protecting distinct recoveries: a kit's recovery passphrase MUST NOT by itself decrypt any archive/1 container, and an export passphrase MUST NOT by itself recover a workspace's data key on its original hardware. Neither is a substitute for the other, and this contract's own restore path (Encryption, Restore is an install path) depends only on the export passphrase, never the recovery passphrase.

**[ARC-113]** The platform MUST NOT transmit a kit's content electronically (email, chat, a copyable on-screen value left rendered indefinitely) as part of generating it — a kit is a printed or otherwise physically-delivered artifact by design, so that recovering it later requires the same physical access its own recovery scenario (lost credentials, dead hardware) presupposes.

**[ARC-114]** Regenerating a kit MUST invalidate the previously issued recovery passphrase, replacing it with a new one — a lost or exposed kit is neutralized by regenerating it, without requiring a full factory reset of the workspace it protects.

*draft-note: ARC-114's rotate-on-regenerate behavior is this contract's own proposal, not fixed by any normative source yet.*

### Size envelope

**[ARC-120]** A conformant implementation MUST support, within the streaming and incremental mechanics above, a workspace relational store up to 1 GiB and a content-addressed asset store up to 20 GiB, without a failure attributable to size alone.

**[ARC-121]** A conformant implementation MUST support a full-mode export of up to 22 GiB end to end — creation, transport, and restore — as a single container.

**[ARC-122]** ARC-120–121 are the design and test targets this format is built against, not protocol-level maximums enforced by any requirement in this contract; a workspace exceeding them is not itself a format violation, and this contract defines no error code for "too large." The authoritative numeric capacity catalog these bounds are drawn from is maintained as a separate document; a future revision of that catalog's numbers does not by itself require a change to this contract unless the container format itself must change to accommodate them.

**[ARC-123]** A size condition beyond ARC-120–121 is a capacity or operational concern, surfaced through the platform's own health and disk-pressure signaling — this contract neither defines that signaling nor gates its own behavior on it.

## Wire shapes

```json
// Outer header (cleartext; the first N bytes after the 4-byte length prefix, ARC-001–002)
{
  "format": "waiveo-archive",
  "archive_format_version": "1.0",
  "kdf": { "algorithm": "argon2id", "salt": "c2FtcGxlLXNhbHQ", "memory_kib": 262144, "iterations": 3, "parallelism": 4 },
  "base_nonce": "MDEyMzQ1Njc4OWFi",
  "digest": "b5d4045c3f466fa91fe2cc6abe79232a1a57cdf104f7a26e716e0a1e2789df78",
  "signature": "MEUCIQDx...",
  "signer_key_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZB"
}
```

```json
// manifest.json (first tar entry inside the decrypted, decompressed body) — full mode
{
  "created_at": 1752537600000,
  "mode": "full",
  "workspace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZC",
  "platform_schema_epoch": 4,
  "packs": [
    { "pack_id": "waiveo/slidecast", "version": "2.2.0", "channel": "first-party", "source": "https://index.example/waiveo", "schema_epoch": 3 },
    { "pack_id": "acme/weather-widget", "version": "1.2.0", "channel": "verified", "source": "https://index.example/community", "schema_epoch": 1 }
  ],
  "assets": [
    { "asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", "size": 20481, "content_type": "image/png", "storage": "embedded" },
    { "asset_ref": "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", "size": 883220, "content_type": "video/mp4", "storage": "by-reference" }
  ],
  "secret_stubs": [
    { "stub_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZD", "wrapped_value": "AQIDBAUGBwgJCgsMDQ4PEA" }
  ],
  "data_key_wrap": { "wrapped_value": "EBAPDg0MCwoJCAcGBQQDAgE" }
}
```

```json
// manifest.json — incremental mode (deltas against the full archive above)
{
  "created_at": 1752624000000,
  "mode": "incremental",
  "base_archive": { "digest": "b5d4045c3f466fa91fe2cc6abe79232a1a57cdf104f7a26e716e0a1e2789df78", "created_at": 1752537600000 },
  "workspace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZC",
  "platform_schema_epoch": 4,
  "packs": [
    { "pack_id": "waiveo/slidecast", "version": "2.2.0", "channel": "first-party", "source": "https://index.example/waiveo", "schema_epoch": 3 },
    { "pack_id": "acme/weather-widget", "version": "1.3.0", "channel": "verified", "source": "https://index.example/community", "schema_epoch": 1 }
  ],
  "assets": [
    { "asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", "size": 20481, "content_type": "image/png", "storage": "inherited" },
    { "asset_ref": "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", "size": 883220, "content_type": "video/mp4", "storage": "by-reference" },
    { "asset_ref": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "size": 4096, "content_type": "image/png", "storage": "embedded" }
  ],
  "secret_stubs": [
    { "stub_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZD", "wrapped_value": "AQIDBAUGBwgJCgsMDQ4PEA" }
  ],
  "data_key_wrap": { "wrapped_value": "EBAPDg0MCwoJCAcGBQQDAgE" }
}
```

```
# Decrypted, decompressed tar stream entry order (Manifest — general, Streaming structure)
manifest.json
workspace.sqlite
assets/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

## Negotiation

- **Container-format version** — `archive_format_version` (ARC-002) is negotiated structurally, the same way `api/1` is: a reader checks it once, before trusting anything else in the file. A major mismatch refuses outright (`VERSION_UNSUPPORTED`, ARC-004); a newer minor is tolerated by ignoring additive fields (ARC-032).
- **Platform schema epoch is a separate axis** — `archive_format_version` answers "can this reader even parse the container's framing and manifest shape"; `platform_schema_epoch` (Manifest — platform schema epoch) answers an unrelated question, "does this reader understand the workspace schema the container holds." A reader can satisfy the first and still have to refuse on the second (ARC-041), and the two MUST NOT be conflated into one version check.
- **No connection-time handshake** — unlike the platform's live protocols, archive/1 has no peer to negotiate with at read time; every check in this section is a reader validating a static file against its own capabilities, in the order Container framing and Manifest — general establish.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `FORMAT_UNRECOGNIZED` | The outer header's `format` field was not `waiveo-archive`. | no |
| `VERSION_UNSUPPORTED` | `archive_format_version`'s major component does not match the reader's. | no |
| `ARCHIVE_SIGNATURE_INVALID` | The outer header's signature failed verification against the header it covers (ARC-021) — including a tampered `kdf` parameter or `base_nonce` — or the digest recomputed over the actual streamed encrypted body did not match the header's `digest` field (ARC-024). | no |
| `DECRYPT_FAILED` | An encrypted-body frame failed AEAD authentication (commonly a wrong export passphrase). | yes — retry with the correct export passphrase |
| `ARCHIVE_TRUNCATED` | The encrypted body reached EOF without an authenticated final-marked frame, or bytes followed the final-marked frame (ARC-016). | yes — once a complete, untruncated copy of the archive is available |
| `MANIFEST_INVALID` | The manifest failed a required-shape, uniqueness, or asset-completeness check. | no |
| `EPOCH_TOO_NEW` | `platform_schema_epoch` is newer than the destination understands. | no — until the destination is upgraded |
| `BASE_ARCHIVE_UNAVAILABLE` | An incremental archive's base-archive chain could not be fully resolved. | yes — once the missing base archive(s) are available |
| `ASSET_UNAVAILABLE` | A `by-reference` asset entry could not be resolved at the destination. | yes — once the referenced asset is available |
| `PACK_YANKED_BLOCKED` | A locked pack's restore was blocked because its version is revoked or yanked and no substitution was applied. | no — until the lockfile entry is updated or a substitution policy applies |
| `DEV_CHANNEL_REFUSED` | A locked dev-channel pack was refused because the destination does not have developer mode enabled. | no — until developer mode is enabled at the destination |

## Conformance notes

- Traceability map: `conformance/traceability/archive-1.md` — maps every `ARC-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/archive-1/` — one JSON case file per `case-id` referenced from the traceability map. A corpus case represents a container's outer header and manifest as JSON directly (Wire shapes) rather than as raw container bytes — the encryption and tar framing themselves are exercised by a driver harness against real byte streams, not by the static corpus.
- The exact cryptographic primitives (ARC-010, ARC-013), the signature's own canonicalization scheme (ARC-021 draft-note), and the kit-rotation behavior (ARC-114 draft-note) remain draft-note proposals pending confirmation. The signing key's own custody — previously an open dependency here — is now defined: `security-model/1`'s workspace signing key (SEC-046–048) is the signing identity ARC-021's `signature` and `signer_key_id` resolve against. Corpus cases exercise the shapes and orderings these rules produce, not a specific cipher implementation or key-custody mechanism.
- The size envelope (Size envelope) is exercised by a soak/capacity lane against real byte counts on representative hardware, not by the static corpus, which asserts shape and ordering at small scale.
- The key hierarchy's own wrap/unwrap algorithm, and the trust-state/revocation mechanics Restore is an install path consults, are out of this contract's scope (Scope) and are not exercised by this corpus; cases that need either treat them as a given, opaque input or a given, opaque outcome.
