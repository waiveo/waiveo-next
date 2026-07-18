package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// HashSections computes relay/1's `state.snapshot` `hash` field (REL-053):
// sha256 over encoding/json's marshaling of sections, expressed
// `sha256:<hex>` in the same grammar signhash.ContentID uses.
// encoding/json marshals struct fields in their Go declaration order, so
// byte-identical Sections content always marshals to byte-identical bytes,
// and therefore the same hash — struct-marshal order IS the canonical form
// for this wire version.
//
// THE single shared canonicalization: both the feeder (internal/feeder/
// snapshot, which signs a snapshot) and the relay (internal/relay/
// desiredstate, which must recompute the same hash to verify one) call
// this function rather than each marshaling sections independently — so
// they cannot drift apart on it. Do not reimplement this elsewhere.
func HashSections(sections Sections) (string, error) {
	b, err := json.Marshal(sections)
	if err != nil {
		return "", fmt.Errorf("wire: HashSections: marshal sections: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// signedScopeCanon is the small struct {generation, hash} REL-075 requires
// a `state.snapshot` signature's signed scope to cover — declaration order
// (generation, then hash) is the canonicalization.
type signedScopeCanon struct {
	Generation int64  `json:"generation"`
	Hash       string `json:"hash"`
}

// SignedScopeBytes marshals REL-075's signed scope for a given generation
// and hash — the exact bytes a `state.snapshot`'s `signature` covers, both
// when signing (the feeder, internal/feeder/snapshot) and when verifying
// (the relay, internal/relay/desiredstate). Sharing this one function is
// what keeps the two sides from drifting apart on the signed scope's byte
// representation.
func SignedScopeBytes(generation int64, hash string) ([]byte, error) {
	b, err := json.Marshal(signedScopeCanon{Generation: generation, Hash: hash})
	if err != nil {
		return nil, fmt.Errorf("wire: SignedScopeBytes: marshal {generation,hash}: %w", err)
	}
	return b, nil
}
