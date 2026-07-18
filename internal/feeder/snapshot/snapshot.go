// Package snapshot builds and signs the feeder's relay/1 desired-state
// generations (`state.snapshot` bodies, REL-051). Wave 1's first photon
// covers the minimal case: one generation carrying exactly one
// screen-program that shows one image, over the feeder's own signing
// identity (internal/feeder/signing).
//
// Canonicalization (there is no separate spec to consult beyond this
// package's own behavior — a later relay-side verifier reproduces both
// by marshaling the same Go struct shapes this package does):
//
//   - `hash` (REL-053) is sha256 over encoding/json's marshaling of the
//     wire.Sections value. encoding/json marshals struct fields in their
//     Go declaration order, so byte-identical Sections content always
//     marshals to byte-identical bytes, and therefore the same hash;
//     struct-marshal order IS the canonical form for this wire version.
//   - `signature` (REL-075) is an ed25519 signature over
//     encoding/json's marshaling of {generation, hash} (in that
//     declaration order) — never hash alone — so relabeling a validly
//     signed snapshot under a different generation number changes the
//     signed bytes and invalidates the old signature. The signature is
//     base64-standard-encoded for the wire (relay/1 gives no explicit
//     signature-field grammar beyond "a signature"; base64-std is this
//     package's own choice, applied consistently by both Build and this
//     package's own verification helpers).
package snapshot

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// SignedSnapshot is the feeder's fully-built, signed relay/1
// `state.snapshot` body (`{generation, hash, signature, sections}`,
// REL-051) — an alias over the shared wire type so callers (and a later
// relay-side verifier) work against the exact contract field names.
type SignedSnapshot = wire.StateSnapshotBody

// Wave 1 first-photon placeholders: this task builds exactly one
// screen-program for a single hard-coded screen, ahead of any real
// screen-registration/pairing task that would supply these IDs and a real
// content-URL TTL policy.
const (
	firstPhotonScreenID        = "screen-first-photon"
	firstPhotonProgramRevision = "rev-1"
	firstPhotonExpiresAt       = 0 // no TTL policy defined yet this wave
)

// Build builds and signs generation 1 of a relay/1 desired-state
// snapshot carrying exactly one screen-program that shows img: one
// `content` item whose `asset_ref` is img's sha256 content ID
// (signhash.ContentID) and whose `url` resolves to the content origin's
// `/content/<hex>` route under contentBaseURL. It signs with id's
// signing private key.
func Build(img []byte, contentBaseURL string, id *signing.Identity) (SignedSnapshot, error) {
	if id == nil {
		return SignedSnapshot{}, fmt.Errorf("snapshot: Build: id must not be nil")
	}

	assetRef := signhash.ContentID(img)
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	sections := wire.Sections{
		ScreenPrograms: []wire.ScreenProgram{
			{
				ScreenID:        firstPhotonScreenID,
				ProgramRevision: firstPhotonProgramRevision,
				Priority:        "scheduled",
				Display:         "content",
				Content: []wire.ContentRef{
					{
						AssetRef:  assetRef,
						URL:       contentBaseURL + "/content/" + hexDigest,
						ExpiresAt: firstPhotonExpiresAt,
					},
				},
			},
		},
		EdgeRules: wire.EdgeRules{
			RulesMinorVersion: "",
			Rules:             []json.RawMessage{},
		},
		DeviceInventory: wire.DeviceInventory{
			Devices:           []json.RawMessage{},
			PackMatchPatterns: []json.RawMessage{},
		},
		RevocationAndSite: wire.RevocationAndSite{
			Revoked:       []string{},
			SiteEffective: wire.SiteEffective{},
		},
		PairingGrants:      []wire.PairingGrant{}, // populated by a later Player-credential-authority task
		WorkflowGeneration: nil,                   // RESERVED, REL-068
	}

	hash, err := hashSections(sections)
	if err != nil {
		return SignedSnapshot{}, err
	}

	const generation = 1

	signature, err := signGenerationHash(generation, hash, id)
	if err != nil {
		return SignedSnapshot{}, err
	}

	return SignedSnapshot{
		Generation: generation,
		Hash:       hash,
		Signature:  signature,
		Sections:   sections,
	}, nil
}

// hashSections computes REL-053's `hash`: sha256 over the canonicalized
// (struct-marshaled) bytes of sections, expressed `sha256:<hex>` in the
// same grammar signhash.ContentID uses.
func hashSections(sections wire.Sections) (string, error) {
	b, err := json.Marshal(sections)
	if err != nil {
		return "", fmt.Errorf("snapshot: marshal sections: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// generationHashCanon is the small struct {generation, hash} REL-075
// requires the signature's signed scope to cover — declaration order
// (generation, then hash) is the canonicalization.
type generationHashCanon struct {
	Generation int64  `json:"generation"`
	Hash       string `json:"hash"`
}

// generationHashCanonBytes marshals the REL-075 signed scope for a given
// generation and hash — the exact bytes Build signs (and a verifier must
// reproduce to check a signature).
func generationHashCanonBytes(generation int64, hash string) ([]byte, error) {
	b, err := json.Marshal(generationHashCanon{Generation: generation, Hash: hash})
	if err != nil {
		return nil, fmt.Errorf("snapshot: marshal {generation,hash}: %w", err)
	}
	return b, nil
}

// signGenerationHash computes REL-075's `signature`: an ed25519 signature
// over generationHashCanonBytes(generation, hash), base64-std-encoded for
// the wire.
func signGenerationHash(generation int64, hash string, id *signing.Identity) (string, error) {
	canon, err := generationHashCanonBytes(generation, hash)
	if err != nil {
		return "", err
	}
	sig := signhash.Sign(id.SigningPriv(), canon)
	return base64.StdEncoding.EncodeToString(sig), nil
}

// decodeSignature reverses signGenerationHash's base64-std encoding,
// yielding the raw ed25519 signature bytes signhash.Verify expects.
func decodeSignature(signature string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return nil, fmt.Errorf("snapshot: decode signature: %w", err)
	}
	return b, nil
}
