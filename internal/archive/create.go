package archive

import (
	"archive/tar"
	"bytes"
	"crypto/cipher"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// Source is everything one container carries, already collected by the caller.
// It is a description of where the bytes come from, not the bytes: the two
// function fields are opened one at a time and streamed straight through, which
// is what keeps Create's memory bounded independent of workspace or asset-store
// size (ARC-080).
type Source struct {
	// CreatedAt is the manifest's `created_at` (ARC-031): epoch milliseconds,
	// INJECTED by the caller. This package never reads a wall clock, so the same
	// Source and the same randomness produce the same container.
	CreatedAt int64
	// WorkspaceID is the source workspace's ULID.
	WorkspaceID string
	// PlatformSchemaEpoch is the source workspace's own platform schema epoch
	// (ARC-040) — carried honestly, never adjusted to what a destination might
	// prefer.
	PlatformSchemaEpoch int
	// Packs is the pack lockfile (ARC-050).
	Packs []PackLock
	// Assets is the complete asset table (ARC-060). ARC-064 requires EVERY asset
	// the restored workspace's relational data references to appear here under
	// one of the three storage values; assembling a complete table is the
	// caller's obligation, since only the caller can see what the workspace data
	// references.
	Assets []AssetEntry
	// SecretStubs are carried unchanged, byte for byte (ARC-070/072).
	SecretStubs []SecretStub
	// WrapDataKey produces `data_key_wrap.wrapped_value` (ARC-071): the source
	// workspace's own data key, re-wrapped under the data-key sub-key ARC-011
	// derives. Create hands that sub-key in and carries whatever comes back as
	// opaque bytes — it never performs the wrap itself, because "the key
	// hierarchy's own wrap/unwrap algorithm and key-custody model" is explicitly
	// out of archive/1's Scope. Required: ARC-031 makes `data_key_wrap` a
	// required manifest member and ARC-071 forbids carrying a raw data key, so a
	// Source with no way to wrap one cannot produce a conformant container.
	//
	// It is a callback rather than a pre-computed string because the wrapping key
	// does not exist until Create has generated this archive's own KDF salt and
	// stretched the passphrase — the caller could not have wrapped anything under
	// it beforehand.
	WrapDataKey func(wrapKey []byte) (string, error)

	// Snapshot opens the workspace relational snapshot (ARC-085), returning its
	// reader and its exact byte length. ARC-083 requires this to come from a
	// consistent-snapshot mechanism — an online backup or equivalent atomic
	// operation, never a raw copy of a store still open for live writes. That
	// obligation is the caller's, because only the caller holds the store;
	// ARC-084 is why it costs nothing here, the snapshot's bytes streaming
	// through the same single pass as everything else.
	Snapshot func() (io.ReadCloser, int64, error)
	// Asset opens one `storage: embedded` asset's bytes by its asset_ref,
	// returning its reader and exact byte length. It is called once per embedded
	// entry, in manifest order, and never for a by-reference or inherited entry
	// (ARC-061).
	Asset func(assetRef string) (io.ReadCloser, int64, error)
}

// Options are the cryptographic and tuning inputs to Create.
type Options struct {
	// Passphrase is the export passphrase (Definitions) the body's key is
	// stretched from. It is a DISTINCT secret from the workspace's recovery
	// passphrase (ARC-112): neither substitutes for the other, and only this one
	// takes part in a container's encryption.
	//
	// It MUST NOT be empty. A container encrypted under the stretch of "" is
	// readable by anyone who knows this format, which is not encryption; refusing
	// it here means the property holds for every caller rather than for the ones
	// that remembered to check. The LENGTH policy (api/1's twelve-character
	// floor) stays with the surface that has an operator to tell — this package
	// enforces only the floor below which the encryption stops existing at all.
	Passphrase string
	// Signer is the workspace signing key's private half (security-model/1
	// SEC-046) that produces the header signature (ARC-021).
	Signer ed25519.PrivateKey
	// SignerKeyID identifies Signer's public half for a reader to resolve.
	SignerKeyID string
	// KDF is the argon2id cost profile. The zero value resolves to
	// DefaultKDFParams.
	KDF KDFParams
	// rand is the randomness source for the per-archive KDF salt and base nonce.
	// nil resolves to crypto/rand.Reader.
	//
	// It is UNEXPORTED, and that is the whole design. A fixed source makes two
	// archives share a KDF salt AND a base nonce, which is total keystream reuse
	// across them — the single worst thing a caller could do to this format, and
	// one line of code away if the seam is reachable from outside the package.
	// This package's own tests are in this package, so they can pin it; nothing
	// else can reach it, so no production call site can weaken a container's
	// randomness by mistake or by "just for this environment".
	rand io.Reader
	// FrameSize is the plaintext each body frame carries. 0 resolves to
	// DefaultFrameSize (1 MiB).
	FrameSize int
}

// placeholderDigest and placeholderSignature are the fixed-width stand-ins the
// header is FIRST written with (see Create). Their encoded widths are identical
// to the real values' — 64 hex characters for a sha256, 86 base64 characters for
// a 64-byte ed25519 signature — which is the entire trick: the header's byte
// length does not change when the real values replace them, so the rewrite lands
// exactly on top of the placeholder without moving the body that follows.
var (
	placeholderDigest    = strings.Repeat("0", hex.EncodedLen(sha256.Size))
	placeholderSignature = b64.EncodeToString(make([]byte, ed25519.SignatureSize))
)

// Create writes one complete archive/1 container to dst.
//
// # Single forward pass (ARC-080)
//
// Everything streams: the tar entries are opened one at a time, compressed as
// they go, sealed into frames as the compressor emits them, and written straight
// to dst. Memory use is a frame buffer plus a zstd window — bounded independent
// of workspace or asset-store size, which is what makes the 22 GiB envelope
// (ARC-121) a matter of time rather than of RAM. Nothing is spooled to local
// disk, and no intermediate stream is materialized in full.
//
// # Why dst is an io.WriteSeeker
//
// The container is header-first (ARC-001), but two header fields cannot be known
// until the body has been written: `digest` is the sha256 OF that body
// (ARC-020), and `signature` covers the header INCLUDING that digest (ARC-021).
// The naive resolutions are all worse: buffering the body to compute the digest
// first breaks ARC-080 outright, and moving the digest to a trailer breaks
// ARC-022's "verifiable from the outer header alone."
//
// So Create writes the header twice. The first write uses fixed-width
// placeholders for both fields; the body then streams past a sha256 on its way
// to dst; the final header — same fields, real values, provably identical
// LENGTH — is written back over the placeholder in place. The length equality is
// asserted, not assumed: if it ever failed, the rewrite would corrupt the first
// bytes of the body, so this fails loudly instead.
//
// # Mode
//
// Create emits `mode: full` only. Incremental archives (ARC-090-094) are not
// implemented; see the package doc.
func Create(dst io.WriteSeeker, src Source, opt Options) error {
	if dst == nil {
		return fmt.Errorf("archive: Create: dst is nil")
	}
	if err := validateSigner(opt.Signer); err != nil {
		return err
	}
	if opt.SignerKeyID == "" {
		return fmt.Errorf("archive: Create: signer_key_id is empty (ARC-002)")
	}
	if opt.Passphrase == "" {
		return fmt.Errorf("archive: Create: passphrase is empty — a container stretched from no passphrase is not encrypted against anyone who knows this format (ARC-010)")
	}
	if src.Snapshot == nil {
		return fmt.Errorf("archive: Create: Source.Snapshot is nil — every archive embeds the workspace snapshot in full (ARC-085/091)")
	}

	kdfParams := opt.KDF.orDefault()
	if err := kdfParams.validate(); err != nil {
		return err
	}
	randSrc := opt.rand
	if randSrc == nil {
		randSrc = cryptorand.Reader
	}
	frameSize := opt.FrameSize
	if frameSize <= 0 {
		frameSize = DefaultFrameSize
	}

	// ARC-010: a per-archive random salt. ARC-017: a base nonce generated fresh
	// and uniformly at random for every archive — never derived from the
	// passphrase, the workspace id, or anything else that could repeat.
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(randSrc, salt); err != nil {
		return fmt.Errorf("archive: Create: read kdf salt: %w", err)
	}
	baseNonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := io.ReadFull(randSrc, baseNonce); err != nil {
		return fmt.Errorf("archive: Create: read base nonce: %w", err)
	}

	keys, err := DeriveKeys(opt.Passphrase, kdfParams, salt)
	if err != nil {
		return err
	}
	aead, err := chacha20poly1305.NewX(keys.Body)
	if err != nil {
		return fmt.Errorf("archive: Create: init AEAD: %w", err)
	}

	// The data key is re-wrapped under ARC-011's SECOND sub-key — never the body
	// key — by the caller's own hierarchy, and the result is carried opaquely
	// (ARC-070/072). This has to happen after the derivation above, which is why
	// the manifest is assembled here rather than before it.
	if src.WrapDataKey == nil {
		return fmt.Errorf("archive: Create: Source.WrapDataKey is nil — every manifest carries data_key_wrap (ARC-031/071)")
	}
	wrapped, err := src.WrapDataKey(keys.DataKeyWrap)
	if err != nil {
		return fmt.Errorf("archive: Create: wrap data key: %w", err)
	}

	// The manifest is built and validated BEFORE anything is written, so a
	// shape Open would refuse (ARC-031/033) is caught here rather than shipped
	// as a container nobody can restore.
	manifest, err := buildManifest(src, wrapped)
	if err != nil {
		return err
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("archive: Create: marshal manifest: %w", err)
	}

	header := Header{
		Format:               FormatMagic,
		ArchiveFormatVersion: FormatVersion,
		KDF: KDFHeader{
			Algorithm:   KDFAlgorithm,
			Salt:        b64.EncodeToString(salt),
			MemoryKiB:   kdfParams.MemoryKiB,
			Iterations:  kdfParams.Iterations,
			Parallelism: kdfParams.Parallelism,
		},
		BaseNonce:   b64.EncodeToString(baseNonce),
		Digest:      placeholderDigest,
		Signature:   placeholderSignature,
		SignerKeyID: opt.SignerKeyID,
	}
	placeholderJSON, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("archive: Create: marshal header: %w", err)
	}

	// Remember where the container begins rather than assuming offset 0, so a
	// container may be written into an already-positioned file.
	start, err := dst.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("archive: Create: locate container start: %w", err)
	}

	var lenBE [4]byte
	binary.BigEndian.PutUint32(lenBE[:], uint32(len(placeholderJSON)))
	if _, err := dst.Write(lenBE[:]); err != nil {
		return fmt.Errorf("archive: Create: write header length: %w", err)
	}
	if _, err := dst.Write(placeholderJSON); err != nil {
		return fmt.Errorf("archive: Create: write header: %w", err)
	}

	// ARC-020: the digest is computed over the encrypted body exactly as it
	// appears in the container — length prefixes included — so it is taken from
	// the same bytes that go to dst, on their way there.
	hasher := sha256.New()
	if err := writeBody(io.MultiWriter(dst, hasher), aead, baseNonce, frameSize, manifestJSON, manifest, src); err != nil {
		return err
	}

	// The body is behind us; both unknown header fields are now knowable.
	header.Digest = hex.EncodeToString(hasher.Sum(nil))
	signable, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("archive: Create: marshal header for signing: %w", err)
	}
	signedBytes, err := canonicalSignedHeader(signable)
	if err != nil {
		return fmt.Errorf("archive: Create: %w", err)
	}
	sig := ed25519.Sign(opt.Signer, signedBytes)
	// The signature is verified against the signer's OWN public half before it is
	// written. This is not paranoia about ed25519; it is the last gate on the one
	// failure mode that produces a container which looks completely successful and
	// is worthless: key material that still signs but no longer corresponds to
	// anything a reader can resolve. A workspace signing key destroyed in place
	// (SEC-121) is exactly that — its bytes are still 64 long and ed25519.Sign
	// still returns 64 bytes for them — and an operator whose export "succeeded"
	// against such a key holds a backup no restorer will ever open. Better a
	// failed export than a false one.
	pub, ok := opt.Signer.Public().(ed25519.PublicKey)
	if !ok || !signhash.Verify(pub, signedBytes, sig) {
		return fmt.Errorf("archive: Create: the header signature does not verify under the signer's own public half — the signing key is corrupt or destroyed, and this container would be unreadable by any conformant restorer (ARC-021)")
	}
	header.Signature = b64.EncodeToString(sig)

	finalJSON, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("archive: Create: marshal final header: %w", err)
	}
	// The load-bearing assertion. Both rewritten fields are fixed-width by
	// construction, and base64's alphabet contains nothing encoding/json escapes
	// — but if that ever stopped being true, the rewrite below would overwrite
	// the first bytes of the body and produce a container that fails to decrypt
	// for reasons nobody would trace back to here.
	if len(finalJSON) != len(placeholderJSON) {
		return fmt.Errorf("archive: Create: signed header is %d bytes but its placeholder was %d — rewriting in place would corrupt the body (ARC-080)",
			len(finalJSON), len(placeholderJSON))
	}

	end, err := dst.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("archive: Create: locate container end: %w", err)
	}
	if _, err := dst.Seek(start+int64(len(lenBE)), io.SeekStart); err != nil {
		return fmt.Errorf("archive: Create: seek back to header: %w", err)
	}
	if _, err := dst.Write(finalJSON); err != nil {
		return fmt.Errorf("archive: Create: rewrite header: %w", err)
	}
	// Leave dst positioned at the container's end, so a caller writing more than
	// one thing into the same file is not surprised by a rewound cursor.
	if _, err := dst.Seek(end, io.SeekStart); err != nil {
		return fmt.Errorf("archive: Create: restore write position: %w", err)
	}
	return nil
}

// validateSigner refuses signing key material that cannot produce a signature
// anyone will be able to verify — BEFORE a byte of the container is written, so
// a refusal costs nothing and leaves nothing behind.
//
// The all-zeros case is called out by name because it is not hypothetical. It is
// what a workspace signing key looks like after a destruction that zeroed it in
// place, and what an unfilled ed25519.PrivateKey looks like: 64 bytes, the right
// length, and cryptographically useless. A length check alone accepts it, which
// is precisely how a destroyed key kept producing "successful" exports.
//
// The comparison is constant-time out of habit rather than necessity — nothing
// here is attacker-timed — but the habit is cheap and the alternative invites a
// reader to wonder whether it should have been.
func validateSigner(priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("archive: Create: signer key is %d bytes, want %d (ARC-021)", len(priv), ed25519.PrivateKeySize)
	}
	var zero [ed25519.PrivateKeySize]byte
	if subtle.ConstantTimeCompare(priv, zero[:]) == 1 {
		return fmt.Errorf("archive: Create: signer key is all zeros — a destroyed or never-established workspace signing key cannot sign an export (SEC-046/SEC-121, ARC-021)")
	}
	return nil
}

// buildManifest assembles the full-mode manifest from src and validates it
// against the same rules Open enforces (ARC-031/033) — one validator, so a
// writer cannot emit a shape its own reader would refuse.
func buildManifest(src Source, dataKeyWrapped string) (Manifest, error) {
	m := Manifest{
		CreatedAt:           src.CreatedAt,
		Mode:                ModeFull,
		WorkspaceID:         src.WorkspaceID,
		PlatformSchemaEpoch: src.PlatformSchemaEpoch,
		Packs:               src.Packs,
		Assets:              src.Assets,
		SecretStubs:         src.SecretStubs,
		DataKeyWrap:         DataKeyWrap{WrappedValue: dataKeyWrapped},
	}
	// Never marshal a nil slice as JSON null: ARC-031 requires the fields, and
	// parseManifest rejects a null one. An empty workspace has empty arrays.
	if m.Packs == nil {
		m.Packs = []PackLock{}
	}
	if m.Assets == nil {
		m.Assets = []AssetEntry{}
	}
	if m.SecretStubs == nil {
		m.SecretStubs = []SecretStub{}
	}
	if err := validateManifest(m, map[string]json.RawMessage{}); err != nil {
		return Manifest{}, fmt.Errorf("archive: Create: %w", err)
	}
	for _, a := range m.Assets {
		if a.Storage == StorageEmbedded && src.Asset == nil {
			return Manifest{}, fmt.Errorf("archive: Create: Source.Asset is nil but %s is %q (ARC-061)", a.AssetRef, StorageEmbedded)
		}
	}
	return m, nil
}

// writeBody streams the whole body: tar -> zstd -> AEAD frames (ARC-012, in that
// order). Each stage is closed in reverse order of construction so the tar
// end-of-archive marker reaches the compressor, the compressor's tail reaches
// the framer, and the framer emits its final-marked frame (ARC-013) — a close
// skipped anywhere in that chain silently truncates the container.
func writeBody(dst io.Writer, aead cipher.AEAD, baseNonce []byte, frameSize int, manifestJSON []byte, manifest Manifest, src Source) error {
	fw := newFrameWriter(dst, aead, baseNonce, frameSize)

	zw, err := zstd.NewWriter(fw,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		// One encoder goroutine: this pipeline is already streaming and its
		// memory ceiling is the point (ARC-080), so extra concurrent windows buy
		// nothing worth their footprint.
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return fmt.Errorf("archive: Create: init zstd: %w", err)
	}
	tw := tar.NewWriter(zw)

	// Every entry is dated from the injected created_at, never a wall clock.
	modTime := time.UnixMilli(manifest.CreatedAt).UTC().Truncate(time.Second)

	// ARC-030: the manifest is the FIRST entry.
	if err := writeEntry(tw, ManifestEntryName, int64(len(manifestJSON)), bytes.NewReader(manifestJSON), modTime); err != nil {
		return err
	}

	// ARC-085/091: the workspace snapshot, always embedded in full.
	snap, snapSize, err := src.Snapshot()
	if err != nil {
		return fmt.Errorf("archive: Create: open workspace snapshot: %w", err)
	}
	err = writeEntry(tw, SnapshotEntryName, snapSize, snap, modTime)
	if cerr := snap.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("archive: Create: close workspace snapshot: %w", cerr)
	}
	if err != nil {
		return err
	}

	// ARC-061: one tar entry per `embedded` asset, at assets/<hex>; nothing at
	// all for a by-reference or inherited entry, whose bytes this container does
	// not carry.
	//
	// Create does NOT recompute an embedded asset's sha256 against its
	// asset_ref: the source asset store is content-addressed by construction, so
	// re-hashing here would only restate what the store already guarantees.
	// ARC-062 places that verification on the READ side, where the archive has
	// crossed a wire and is the untrusted party — and Open enforces it there.
	for _, a := range manifest.Assets {
		if a.Storage != StorageEmbedded {
			continue
		}
		rc, size, err := src.Asset(a.AssetRef)
		if err != nil {
			return fmt.Errorf("archive: Create: open asset %s: %w", a.AssetRef, err)
		}
		if size != a.Size {
			rc.Close()
			return fmt.Errorf("archive: Create: asset %s is %d bytes but the manifest declares %d (ARC-060)", a.AssetRef, size, a.Size)
		}
		err = writeEntry(tw, assetEntryName(a.AssetRef), size, rc, modTime)
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("archive: Create: close asset %s: %w", a.AssetRef, cerr)
		}
		if err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("archive: Create: close tar stream: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("archive: Create: close zstd stream: %w", err)
	}
	if err := fw.Close(); err != nil {
		return fmt.Errorf("archive: Create: close frame stream: %w", err)
	}
	return nil
}

// writeEntry emits one tar entry, streaming r's bytes through rather than
// buffering them. The tar format is left unset so archive/tar picks the minimal
// one that fits — USTAR for ordinary entries, PAX only when a size beyond 8 GiB
// demands it, which ARC-120's 20 GiB asset store makes a real possibility.
func writeEntry(tw *tar.Writer, name string, size int64, r io.Reader, modTime time.Time) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     size,
		Mode:     0o600,
		ModTime:  modTime,
	}); err != nil {
		return fmt.Errorf("archive: Create: write tar header %s: %w", name, err)
	}
	n, err := io.Copy(tw, r)
	if err != nil {
		return fmt.Errorf("archive: Create: write tar entry %s: %w", name, err)
	}
	if n != size {
		return fmt.Errorf("archive: Create: tar entry %s declared %d bytes but its source yielded %d", name, size, n)
	}
	return nil
}
