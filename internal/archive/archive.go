// Package archive implements archive/1 (contracts/archive-1.md): the workspace
// archive container, both halves of it — Create writes one, Open reads one back
// and verifies it.
//
// A container is the single portable file a workspace's entire state exports to
// and restores from. Backup, box migration, onboarding onto new hardware, and a
// data-subject export are all the SAME file operation under this contract; only
// what happens to the file afterward differs. That is why the format is one
// shape with no per-use-case variants.
//
// # Layout
//
// Three consecutive regions, exactly (ARC-001):
//
//	[4 bytes] big-endian uint32 N
//	[N bytes] the outer header — UTF-8 JSON, CLEARTEXT
//	[to EOF]  the encrypted body, a sequence of AEAD frames
//
// The header is cleartext on purpose (ARC-022): its signature must be verifiable
// — and therefore the container's provenance and its body's digest must be
// checkable — WITHOUT the export passphrase and without decrypting a byte. A
// header inside the encryption would make signature verification require the
// very secret the signature is supposed to let you check without.
//
// # Pipeline
//
// Write: tar -> zstd -> AEAD frames (ARC-012, in that order — compressing
// ciphertext would spend the compressor's whole budget on high-entropy input
// for nothing). Read: the exact inverse.
//
// # Order of checks on read
//
// Open's ordering is itself normative, not an implementation preference:
//
//  1. `format` (ARC-003) — reject a foreign file before parsing anything past
//     the header.
//  2. `archive_format_version` major (ARC-004) — reject before reading the body.
//     A newer MINOR is tolerated.
//  3. The header signature (ARC-021/023) — BEFORE the KDF is invoked with any
//     `kdf`-supplied parameter, so a tampered `memory_kib` fails verification
//     rather than driving a multi-gigabyte allocation shaped by unauthenticated
//     input. This is why the full header (kdf, base_nonce, digest) is not even
//     unmarshaled until the signature over its raw bytes has verified.
//  4. Per-frame AEAD authentication (ARC-014), aborting on the first failure.
//  5. Frame-sequence completeness (ARC-016) — exactly one final-marked frame,
//     nothing after it.
//  6. The digest recomputed over the body's ACTUAL streamed bytes (ARC-024).
//     Step 3 proves the header's recorded digest was validly signed; only this
//     step proves the bytes delivered are the bytes that value describes.
//  7. Manifest shape (ARC-031/033), then each embedded asset's own sha256
//     against its asset_ref (ARC-062).
//
// # Scope
//
// Full-mode containers only. Incremental archives (ARC-090-094) are NOT
// implemented: Create emits `mode: full` and nothing else, and Open validates an
// incremental manifest's shape but never resolves a base-archive chain — that
// resolution (ARC-092/094) needs a store of prior archives this package has no
// business owning. The manifest type carries `base_archive` so an incremental
// manifest round-trips unmutilated rather than silently losing the field.
//
// Likewise out of scope, by the contract's own Scope section: the key
// hierarchy's wrap/unwrap algorithm (this package carries `data_key_wrap` and
// every secret stub as opaque bytes and performs no operation on their
// contents, ARC-070/072), the trust-state re-checks a restore performs
// (ARC-101-103), and the emergency kit (ARC-110-114).
//
// # No wall clock
//
// Nothing in this package reads the current time. `created_at` is supplied by
// the caller (Source.CreatedAt) and is also what dates every tar entry, so the
// same Source plus the same randomness produces the same container bytes.
// Randomness likewise comes from Options.Rand.
package archive

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	// FormatMagic is the only legal value of the outer header's `format` field
	// (ARC-002). Anything else is FORMAT_UNRECOGNIZED (ARC-003).
	FormatMagic = "waiveo-archive"

	// FormatVersion is the `archive_format_version` this package writes
	// (ARC-002). Readers match the MAJOR component exactly and tolerate a newer
	// MINOR (ARC-004).
	FormatVersion = "1.0"

	// formatMajor is the major component this reader accepts. A container
	// declaring any other major is VERSION_UNSUPPORTED (ARC-004).
	formatMajor = 1
)

// maxHeaderBytes caps the outer header's declared length. The 4-byte length
// prefix is the very first thing read, before a single field has been
// authenticated, so it is the one number in the format an attacker can set with
// no signature standing behind it. A real header is a few hundred bytes; 1 MiB
// is orders of magnitude of headroom and still refuses to let a hostile prefix
// dictate a large allocation. Over the cap is FORMAT_UNRECOGNIZED — a file
// claiming a megabyte-plus header is not one of ours.
const maxHeaderBytes = 1 << 20

// Header is the cleartext outer header (ARC-002). Field order here is the
// marshal order, and it matches the contract's own "Wire shapes" block so a
// container's first line reads the way the contract prints it.
//
// It is exported because a caller that needs the KDF parameters and salt — to
// re-wrap `data_key_wrap` under the data-key sub-key (ARC-011/071), which is a
// restorer's job, not this package's — needs to see them.
type Header struct {
	Format               string    `json:"format"`
	ArchiveFormatVersion string    `json:"archive_format_version"`
	KDF                  KDFHeader `json:"kdf"`
	// BaseNonce is the per-archive random 24-byte XChaCha20-Poly1305 base nonce
	// every frame nonce derives from (ARC-013/017), base64 (unpadded standard).
	BaseNonce string `json:"base_nonce"`
	// Digest is the hex sha256 of the encrypted body region exactly as it
	// appears in the container (ARC-020) — over ciphertext, so computing and
	// checking it needs no passphrase.
	Digest string `json:"digest"`
	// Signature is ed25519 over canonicalSignedHeader's rendering of every
	// OTHER field of this header (ARC-021), base64 (unpadded standard).
	Signature string `json:"signature"`
	// SignerKeyID identifies the workspace signing key (security-model/1
	// SEC-046) whose public half verifies Signature. Resolving an id to key
	// material is that contract's custody problem, not this one's: Open takes
	// the resolved public key from its caller.
	SignerKeyID string `json:"signer_key_id"`
}

// KDFHeader records everything needed to repeat the export root key's
// derivation (ARC-010) — a reader cannot derive the key without them, which is
// exactly why they are in the cleartext header, and exactly why ARC-021 makes
// the signature cover them.
type KDFHeader struct {
	Algorithm   string `json:"algorithm"`
	Salt        string `json:"salt"`
	MemoryKiB   uint32 `json:"memory_kib"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
}

// headerPreamble is the DELIBERATELY minimal view of the outer header: the only
// three fields Open is allowed to consult before the signature has verified
// (ARC-003, ARC-004, ARC-021). Parsing the full Header this early would mean
// letting unauthenticated `kdf` values through a type conversion before the
// signature has had its say; parsing only these three keeps that impossible by
// construction rather than by careful ordering of statements.
type headerPreamble struct {
	Format               string `json:"format"`
	ArchiveFormatVersion string `json:"archive_format_version"`
	Signature            string `json:"signature"`
}

// b64 is the base64 alphabet every binary header field uses: standard, unpadded,
// matching the contract's "Wire shapes" samples. Its alphabet (A-Za-z0-9+/)
// contains no character encoding/json escapes, which is load-bearing for
// Create's placeholder-rewrite trick (ARC-080): a base64 field's JSON-encoded
// length depends only on its byte length, never on its contents.
var b64 = base64.RawStdEncoding

// canonicalSignedHeader renders the bytes ARC-021's signature covers: the
// entire cleartext outer header EXCEPT the `signature` field itself.
//
// The scheme is a JCS-shaped canonicalization built out of encoding/json's own
// determinism rather than a hand-rolled serializer:
//
//   - The header is decoded into a generic map, so the canonicalization covers
//     EVERY field actually present — including a field added by a newer minor
//     version that this reader does not know about (ARC-004/032). Canonicalizing
//     a fixed struct instead would silently drop such a field OUT of the signed
//     scope, handing a forger a free unauthenticated slot. That trade — total
//     coverage over a tidy struct — is the whole reason this goes through a map.
//   - `signature` is deleted, never blanked: the signature cannot cover itself,
//     and a placeholder value would make the signed bytes depend on a value the
//     verifier has no way to reproduce.
//   - encoding/json marshals map keys in sorted order at every nesting level, so
//     key ordering is fixed without a sorting pass here.
//   - Numbers are decoded with UseNumber and re-emitted as their original
//     literal, so 262144 stays 262144 and never round-trips through a float.
//
// Both halves feed this the same input: Create canonicalizes the header bytes it
// is about to write, Open canonicalizes the header bytes it just read. One
// function, no chance of the two drifting apart on the signed scope.
func canonicalSignedHeader(headerJSON []byte) ([]byte, error) {
	if err := rejectDuplicateMembers(headerJSON); err != nil {
		return nil, fmt.Errorf("canonicalize header: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(headerJSON))
	dec.UseNumber()

	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("canonicalize header: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("canonicalize header: trailing content after the header object")
	}
	if obj == nil {
		return nil, fmt.Errorf("canonicalize header: header is JSON null, not an object")
	}

	delete(obj, "signature")

	b, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("canonicalize header: %w", err)
	}
	return b, nil
}

// rejectDuplicateMembers refuses a header in which any JSON object declares the
// same member name twice.
//
// The signature covers a CANONICALIZATION of the header rather than its raw
// bytes (see canonicalSignedHeader), and encoding/json resolves a duplicate
// member by keeping the LAST occurrence. Nothing in JSON makes that the only
// reading: an implementation built on a parser that keeps the first — and there
// are several — would canonicalize the same signed bytes into a different
// header, verify the same signature, and then act on a different `digest`,
// `signer_key_id` or `kdf` than this one does. That is a divergence between two
// conformant readers over a container both consider valid, which is worse than
// either one refusing.
//
// So a duplicate is refused outright, on the write side and the read side alike
// (one function serves both). No legitimate producer emits one, and the refusal
// costs a container nothing it should have had.
func rejectDuplicateMembers(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return scanForDuplicateMembers(dec)
}

// scanForDuplicateMembers consumes exactly one JSON value from dec, descending
// into every object and array it contains.
func scanForDuplicateMembers(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// A scalar: nothing beneath it to check.
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object member name is %T, not a string", keyTok)
			}
			if seen[key] {
				return fmt.Errorf("object declares the member %q more than once", key)
			}
			seen[key] = true
			if err := scanForDuplicateMembers(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := scanForDuplicateMembers(dec); err != nil {
				return err
			}
		}
	}
	// The matching closing delimiter.
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// majorMinor splits an `archive_format_version` into its two components
// (ARC-004). A version that is not exactly two non-negative decimal components
// is refused: an unparseable version cannot be shown to share this reader's
// major, and the failure mode for "cannot establish the major matches" is the
// same refusal as "the major does not match".
func majorMinor(version string) (major, minor int, err error) {
	parts := strings.Split(version, ".")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%q is not a major.minor version", version)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return 0, 0, fmt.Errorf("%q has a non-numeric major component", version)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return 0, 0, fmt.Errorf("%q has a non-numeric minor component", version)
	}
	return major, minor, nil
}
