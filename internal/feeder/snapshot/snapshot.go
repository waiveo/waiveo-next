// Package snapshot builds and signs the feeder's relay/1 desired-state
// generations (`state.snapshot` bodies, REL-051). Wave 1's first photon
// covers the minimal case: one generation carrying exactly one
// screen-program that shows one image, over the feeder's own signing
// identity (internal/feeder/signing).
//
// Canonicalization (no separate spec to consult beyond this package's own
// behavior — a later relay-side verifier, internal/relay/desiredstate,
// reproduces both by calling the exact same shared helpers this package
// does, so the two sides cannot drift apart):
//
//   - `hash` (REL-053) is sha256 over encoding/json's marshaling of the
//     wire.Sections value, computed via wire.HashSections. encoding/json
//     marshals struct fields in their Go declaration order, so
//     byte-identical Sections content always marshals to byte-identical
//     bytes, and therefore the same hash; struct-marshal order IS the
//     canonical form for this wire version.
//   - `signature` (REL-075) is an ed25519 signature over
//     wire.SignedScopeBytes(generation, hash) — encoding/json's marshaling
//     of {generation, hash} in that declaration order — never hash alone —
//     so relabeling a validly signed snapshot under a different generation
//     number changes the signed bytes and invalidates the old signature.
//     The signature is encoded for the wire via wire.EncodeSignature
//     (base64-standard; relay/1 gives no explicit signature-field grammar
//     beyond "a signature" — base64-std is this codec's own choice).
//
// wire.HashSections, wire.SignedScopeBytes, and wire.EncodeSignature all
// live in internal/shared/wire, not here, so this package's signing side
// and internal/relay/desiredstate's verifying side call the exact same
// functions and cannot drift apart on any of the three.
package snapshot

import (
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
//
// grants populates `sections.pairing_grants` (REL-067) — typically the
// single grant.Mint() record a later Task 6 rides to the relay. A nil
// grants is normalized to a non-nil empty slice, so the section always
// marshals as `[]`, never `null` (REL-060). grants is included in
// `sections` ahead of hashing/signing, so it is covered by `hash`
// (REL-053) and transitively by `signature` (REL-075) exactly like every
// other section.
func Build(img []byte, contentBaseURL string, id *signing.Identity, grants []wire.PairingGrant) (SignedSnapshot, error) {
	if id == nil {
		return SignedSnapshot{}, fmt.Errorf("snapshot: Build: id must not be nil")
	}

	if grants == nil {
		grants = []wire.PairingGrant{}
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
		PairingGrants:      grants,
		WorkflowGeneration: nil, // RESERVED, REL-068
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

// hashSections computes REL-053's `hash` by delegating to
// wire.HashSections — THE single shared canonicalization a later
// relay-side verifier (internal/relay/desiredstate) also calls, so signing
// and verifying cannot drift apart on it.
func hashSections(sections wire.Sections) (string, error) {
	return wire.HashSections(sections)
}

// generationHashCanonBytes marshals the REL-075 signed scope for a given
// generation and hash by delegating to wire.SignedScopeBytes — the exact
// bytes Build signs (and a verifier must reproduce to check a signature),
// from the same shared helper a later relay-side verifier calls.
func generationHashCanonBytes(generation int64, hash string) ([]byte, error) {
	return wire.SignedScopeBytes(generation, hash)
}

// signGenerationHash computes REL-075's `signature`: an ed25519 signature
// over generationHashCanonBytes(generation, hash), encoded for the wire via
// wire.EncodeSignature — the shared codec both this (signing) side and a
// later relay-side verifier must use, so they cannot drift.
func signGenerationHash(generation int64, hash string, id *signing.Identity) (string, error) {
	canon, err := generationHashCanonBytes(generation, hash)
	if err != nil {
		return "", err
	}
	sig := signhash.Sign(id.SigningPriv(), canon)
	return wire.EncodeSignature(sig), nil
}
