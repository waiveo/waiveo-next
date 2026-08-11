package archive

import (
	"archive/tar"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// Entry is one decoded tar entry of a container's body: its name and its bytes.
type Entry struct {
	Name string
	Body []byte
}

// Open reads a container back and verifies it, in the order the contract's
// Container framing, Encryption, Signing, and Manifest — general sections fix
// between them (see the package doc's "Order of checks on read"): `format`,
// then the major version, then the header signature, then per-frame
// authentication, then frame-sequence completeness, then the recomputed body
// digest, then the manifest's shape, then every embedded asset's own hash
// against its asset_ref.
//
// It returns the manifest and every tar entry the body carried, manifest.json
// included, in stream order.
//
// # What Open is and is not
//
// Open is the VERIFICATION half of this package: it proves a container is real
// by reading it back through the same checks a restore performs on the bytes
// themselves. It materializes each entry in memory, so it suits a container that
// fits there — the conformance suite's, a CLI's `verify`, a caller restoring a
// modest workspace.
//
// A restorer facing ARC-120's 20 GiB asset store wants ARC-081's shape instead:
// each embedded asset written straight into content-addressed storage as its
// bytes stream past, never a second full copy. That restorer is built from the
// SAME parts this function is assembled from — frameReader is an io.Reader whose
// memory ceiling is one frame, and the tar entries arrive in an order (ARC-030:
// manifest first) chosen precisely so validation completes before the first
// asset byte is consumed (ARC-082). Only the "collect it into a slice" step at
// the end is Open's own convenience.
//
// signerPub is the resolved public half of the key the header's `signer_key_id`
// names. Resolving an id to key material belongs to security-model/1's own
// custody model (SEC-046-048), explicitly outside archive/1's scope, so it
// arrives here already resolved.
func Open(r io.Reader, passphrase string, signerPub ed25519.PublicKey) (Manifest, []Entry, error) {
	headerJSON, err := readHeaderRegion(r)
	if err != nil {
		return Manifest{}, nil, err
	}

	// Only three fields are consulted before the signature has verified — see
	// headerPreamble on why the full header is not even unmarshaled yet.
	var pre headerPreamble
	if err := json.Unmarshal(headerJSON, &pre); err != nil {
		return Manifest{}, nil, wrapf(CodeFormatUnrecognized, err, "outer header did not parse as JSON")
	}

	// ARC-003: reject a foreign format immediately, without attempting to parse
	// anything past the outer header.
	if pre.Format != FormatMagic {
		return Manifest{}, nil, codedf(CodeFormatUnrecognized,
			"outer header declares format %q, not %q", pre.Format, FormatMagic)
	}
	// ARC-004: the major must match exactly, refused before the encrypted body
	// is read. A newer MINOR is tolerated — additive fields this reader does not
	// recognize are ignored, here and in the manifest (ARC-032).
	major, _, err := majorMinor(pre.ArchiveFormatVersion)
	if err != nil {
		return Manifest{}, nil, wrapf(CodeVersionUnsupported, err, "unreadable archive_format_version")
	}
	if major != formatMajor {
		return Manifest{}, nil, codedf(CodeVersionUnsupported,
			"container is archive_format_version %s; this reader implements major %d", pre.ArchiveFormatVersion, formatMajor)
	}

	// ARC-021/023: verify the signature over the whole cleartext header before
	// decrypting or reading any part of the body — and, critically, before the
	// KDF is invoked with any `kdf`-supplied parameter. A tampered `memory_kib`
	// or `iterations` therefore fails HERE, rather than driving a memory or CPU
	// allocation shaped by unauthenticated input.
	if err := verifyHeaderSignature(headerJSON, pre.Signature, signerPub); err != nil {
		return Manifest{}, nil, err
	}

	// Everything below this line is reading an AUTHENTICATED header.
	var header Header
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return Manifest{}, nil, fmt.Errorf("archive: Open: signed header did not decode: %w", err)
	}
	salt, baseNonce, err := headerCrypto(header)
	if err != nil {
		return Manifest{}, nil, err
	}

	keys, err := DeriveKeys(passphrase, KDFParams{
		MemoryKiB:   header.KDF.MemoryKiB,
		Iterations:  header.KDF.Iterations,
		Parallelism: header.KDF.Parallelism,
	}, salt)
	if err != nil {
		return Manifest{}, nil, err
	}
	aead, err := chacha20poly1305.NewX(keys.Body)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("archive: Open: init AEAD: %w", err)
	}

	body := newFrameReader(r, aead, baseNonce, sha256.New(), header.Digest)

	zr, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("archive: Open: init zstd: %w", err)
	}
	defer zr.Close()

	manifest, entries, tarErr := readEntries(tar.NewReader(zr))

	// Force the body's own verdict regardless of how the tar read ended: a tar
	// reader stops at the end-of-archive marker and never touches what follows,
	// so ARC-016's completeness check and ARC-024's digest comparison would
	// otherwise never run on a well-formed container.
	bodyErr := body.Finish()

	// Container-level refusals outrank whatever the decompressor made of the
	// stream it was handed. A truncated body reaches zstd as an abruptly ended
	// stream, and reporting THAT would bury ARCHIVE_TRUNCATED under an
	// unactionable "unexpected EOF" — the exact conflation ARC-015/074 forbid.
	if bodyErr != nil {
		return Manifest{}, nil, bodyErr
	}
	if tarErr != nil {
		return Manifest{}, nil, tarErr
	}
	return manifest, entries, nil
}

// readHeaderRegion consumes the 4-byte length prefix and the header bytes it
// counts (ARC-001), leaving r positioned at the first byte of the encrypted
// body.
//
// Every failure here is FORMAT_UNRECOGNIZED. This is the point in the file where
// nothing has been authenticated and nothing has even been identified as ours: a
// file too short to hold a length prefix, or one declaring a header larger than
// any this format produces, has not shown itself to be an archive/1 container at
// all, and "this is not our format" is the honest thing to say about it. Reading
// as ARCHIVE_TRUNCATED would instead tell an operator to go find the rest of a
// file that was never one of ours.
func readHeaderRegion(r io.Reader) ([]byte, error) {
	var lenBE [4]byte
	if _, err := io.ReadFull(r, lenBE[:]); err != nil {
		return nil, wrapf(CodeFormatUnrecognized, err, "file is too short to hold an outer header length prefix")
	}
	n := binary.BigEndian.Uint32(lenBE[:])
	if n == 0 || n > maxHeaderBytes {
		return nil, codedf(CodeFormatUnrecognized,
			"outer header length prefix declares %d bytes, outside the 1..%d range this format produces", n, maxHeaderBytes)
	}
	headerJSON := make([]byte, n)
	if _, err := io.ReadFull(r, headerJSON); err != nil {
		return nil, wrapf(CodeFormatUnrecognized, err, "file ends inside its declared %d-byte outer header", n)
	}
	return headerJSON, nil
}

// verifyHeaderSignature checks ARC-021's signature over the canonicalization of
// every header field except `signature` itself, refusing with
// ARCHIVE_SIGNATURE_INVALID (ARC-023) on any failure.
//
// It fails CLOSED on a malformed signature or a wrong-length public key rather
// than propagating a different error class: from a caller's standpoint "the
// signature did not verify" is exactly what happened, and crypto/ed25519.Verify
// panics on a wrong-length key, which signhash.Verify already turns into a
// false.
func verifyHeaderSignature(headerJSON []byte, signatureB64 string, signerPub ed25519.PublicKey) error {
	sig, err := b64.DecodeString(signatureB64)
	if err != nil {
		return wrapf(CodeArchiveSignatureInvalid, err, "header signature is not valid base64")
	}
	signed, err := canonicalSignedHeader(headerJSON)
	if err != nil {
		return wrapf(CodeArchiveSignatureInvalid, err, "header could not be canonicalized for verification")
	}
	if !signhash.Verify(signerPub, signed, sig) {
		return codedf(CodeArchiveSignatureInvalid,
			"header signature did not verify against the supplied signing key — the archive was tampered with, corrupted, or signed by a different workspace")
	}
	return nil
}

// headerCrypto decodes and range-checks the authenticated header's crypto
// material. Failures here are NOT taxonomy refusals: the signature has already
// verified, so a malformed field means the exporter that signed it produced a
// broken container — an implementation bug, not one of the operator-facing
// conditions the taxonomy names.
func headerCrypto(h Header) (salt, baseNonce []byte, err error) {
	if h.KDF.Algorithm != KDFAlgorithm {
		return nil, nil, fmt.Errorf("archive: Open: header declares kdf algorithm %q; this implementation implements %q (ARC-010)", h.KDF.Algorithm, KDFAlgorithm)
	}
	salt, err = b64.DecodeString(h.KDF.Salt)
	if err != nil {
		return nil, nil, fmt.Errorf("archive: Open: kdf salt is not valid base64: %w", err)
	}
	if len(salt) == 0 {
		return nil, nil, errors.New("archive: Open: kdf salt is empty (ARC-010)")
	}
	baseNonce, err = b64.DecodeString(h.BaseNonce)
	if err != nil {
		return nil, nil, fmt.Errorf("archive: Open: base_nonce is not valid base64: %w", err)
	}
	if len(baseNonce) != chacha20poly1305.NonceSizeX {
		return nil, nil, fmt.Errorf("archive: Open: base_nonce is %d bytes, want %d (ARC-013)", len(baseNonce), chacha20poly1305.NonceSizeX)
	}
	if len(h.Digest) != hex.EncodedLen(sha256.Size) {
		return nil, nil, fmt.Errorf("archive: Open: digest is %d characters, want %d hex characters (ARC-002)", len(h.Digest), hex.EncodedLen(sha256.Size))
	}
	if _, err := hex.DecodeString(h.Digest); err != nil {
		return nil, nil, fmt.Errorf("archive: Open: digest is not hex: %w", err)
	}
	if h.SignerKeyID == "" {
		return nil, nil, errors.New("archive: Open: signer_key_id is empty (ARC-002)")
	}
	return salt, baseNonce, nil
}

// readEntries walks the decrypted, decompressed tar stream, enforcing the
// Manifest group's rules in the order the stream itself makes possible.
//
// The manifest is validated in full the moment it arrives, BEFORE any other
// entry has been touched (ARC-082) — which is only possible because it is the
// first entry (ARC-030), and is why a restorer can refuse a container without
// having written an asset anywhere.
func readEntries(tr *tar.Reader) (Manifest, []Entry, error) {
	var (
		manifest    Manifest
		entries     []Entry
		seenAssets  = map[string]bool{}
		seenSnaphot bool
		declared    = map[string]AssetEntry{}
	)

	for i := 0; ; i++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("archive: Open: read tar stream: %w", err)
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("archive: Open: read tar entry %s: %w", hdr.Name, err)
		}

		if i == 0 {
			// ARC-030: the manifest MUST be the first entry.
			if hdr.Name != ManifestEntryName {
				return Manifest{}, nil, codedf(CodeManifestInvalid,
					"first tar entry is %q, not %q (ARC-030)", hdr.Name, ManifestEntryName)
			}
			manifest, err = parseManifest(body)
			if err != nil {
				return Manifest{}, nil, err
			}
			for _, a := range manifest.Assets {
				declared[a.AssetRef] = a
			}
			entries = append(entries, Entry{Name: hdr.Name, Body: body})
			continue
		}

		switch {
		case hdr.Name == ManifestEntryName:
			return Manifest{}, nil, codedf(CodeManifestInvalid, "tar stream carries a second %q entry", ManifestEntryName)
		case hdr.Name == SnapshotEntryName:
			if seenSnaphot {
				return Manifest{}, nil, codedf(CodeManifestInvalid, "tar stream carries a second %q entry", SnapshotEntryName)
			}
			seenSnaphot = true
		default:
			ref, ok := AssetRefFromEntryName(hdr.Name)
			if !ok {
				return Manifest{}, nil, codedf(CodeManifestInvalid,
					"tar entry %q is neither %q nor a well-formed %s<hex> asset entry (ARC-061)", hdr.Name, SnapshotEntryName, assetEntryPrefix)
			}
			a, declaredHere := declared[ref]
			if !declaredHere {
				// ARC-064's mirror image: content the manifest never declared is
				// as invalid as a declaration whose content is missing.
				return Manifest{}, nil, codedf(CodeManifestInvalid, "tar entry %q carries an asset the manifest does not declare", hdr.Name)
			}
			// ARC-061: a by-reference or inherited entry's bytes are NOT carried
			// in this container — finding them here contradicts the manifest.
			if a.Storage != StorageEmbedded {
				return Manifest{}, nil, codedf(CodeManifestInvalid,
					"asset %s is declared %q but its bytes are carried in this container (ARC-061)", ref, a.Storage)
			}
			// ARC-062: an asset is trusted only by matching its own name, never
			// by the manifest's unverified say-so.
			if got := "sha256:" + hex.EncodeToString(sha256Sum(body)); got != ref {
				return Manifest{}, nil, codedf(CodeManifestInvalid,
					"asset entry %q hashes to %s, which does not match its asset_ref (ARC-062)", hdr.Name, got)
			}
			if int64(len(body)) != a.Size {
				return Manifest{}, nil, codedf(CodeManifestInvalid,
					"asset %s is %d bytes but the manifest declares %d (ARC-060)", ref, len(body), a.Size)
			}
			seenAssets[ref] = true
		}
		entries = append(entries, Entry{Name: hdr.Name, Body: body})
	}

	if len(entries) == 0 {
		return Manifest{}, nil, codedf(CodeManifestInvalid, "tar stream is empty; %q must be its first entry (ARC-030)", ManifestEntryName)
	}
	// ARC-085/091: the workspace snapshot is always embedded in full, in every
	// archive, regardless of mode.
	if !seenSnaphot {
		return Manifest{}, nil, codedf(CodeManifestInvalid, "tar stream carries no %q entry (ARC-085)", SnapshotEntryName)
	}
	// ARC-061 again, from the other side: every `embedded` declaration must have
	// produced bytes. A missing one would otherwise be discovered partway
	// through a restore already in progress, which ARC-064 exists to prevent.
	for _, a := range manifest.Assets {
		if a.Storage == StorageEmbedded && !seenAssets[a.AssetRef] {
			return Manifest{}, nil, codedf(CodeManifestInvalid,
				"asset %s is declared %q but no %q entry carries its bytes (ARC-061)", a.AssetRef, StorageEmbedded, assetEntryName(a.AssetRef))
		}
	}
	return manifest, entries, nil
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
