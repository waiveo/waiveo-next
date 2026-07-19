package reenroll

import (
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// popDomain domain-separates the re-enrollment proof-of-possession signature
// from every other ed25519 signature in the system (desired-state snapshots,
// channel bindings) — the signed bytes always begin with this tag, so a
// signature made for one purpose can never be presented as another. The
// trailing NUL closes the tag off from the length-prefixed body.
const popDomain = "waiveo-next/relay-1/reenroll-pop\x00"

// popMessage builds the exact bytes the proof-of-possession signature covers:
// the domain tag, then the challenge nonce and the CSR DER, each 8-byte
// big-endian length-prefixed. The length prefixes bind nonce AND csr
// unambiguously (REL-027) — no run of bytes can be reinterpreted as belonging
// to the other field, so a signature captured over one {nonce, csr} pair
// cannot be replayed against a substituted CSR (or a different nonce).
func popMessage(nonceHex string, csrDER []byte) []byte {
	nonce := []byte(nonceHex)
	buf := make([]byte, 0, len(popDomain)+8+len(nonce)+8+len(csrDER))
	buf = append(buf, popDomain...)

	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(nonce)))
	buf = append(buf, l[:]...)
	buf = append(buf, nonce...)

	binary.BigEndian.PutUint64(l[:], uint64(len(csrDER)))
	buf = append(buf, l[:]...)
	buf = append(buf, csrDER...)
	return buf
}

// SignPoP produces the `pop_signature` a relay presents through the
// Expired-certificate re-enrollment path (REL-027): the relay proves it holds
// the private key of the certificate it is presenting for re-enrollment by
// signing, with that expired certificate's OWN private key
// (expiredCertPriv — never the fresh keypair its new csr was generated from),
// a value that binds together the bootstrap challenge nonce (REL-026,
// nonceHex) and that same renew request's own CSR DER (csrDER). The signature
// is returned hex-encoded for the JSON `pop_signature` field.
func SignPoP(expiredCertPriv ed25519.PrivateKey, nonceHex string, csrDER []byte) string {
	return hex.EncodeToString(signhash.Sign(expiredCertPriv, popMessage(nonceHex, csrDER)))
}

// VerifyPoP is the app peer's check of a presented `pop_signature` (REL-028):
// it verifies sig against recordedPub — the public key on record for the
// presented certificate's serial in the app peer's own issuance record — over
// the same nonce+csr binding SignPoP covers. It reports true only when sig is
// well-formed hex, matches recordedPub, and covers exactly this nonceHex and
// csrDER.
//
// It fails closed (returns false, never panics) for every rejection REL-029
// enumerates: an absent (empty) or malformed (non-hex, wrong-length) signature
// decodes/verifies to false, a signature made with any other key or over any
// other nonce or CSR does not verify, and a wrong-length recordedPub —
// wire-decoded from the issuance record — is rejected rather than crashing the
// verifier (signhash.Verify length-checks the key; crypto/ed25519.Verify
// length-checks the signature).
func VerifyPoP(recordedPub ed25519.PublicKey, nonceHex string, csrDER []byte, sig string) bool {
	raw, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	return signhash.Verify(recordedPub, popMessage(nonceHex, csrDER), raw)
}
