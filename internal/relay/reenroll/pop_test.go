package reenroll

import (
	"crypto/ed25519"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// The nonce shape SignPoP/VerifyPoP bind is the bootstrap challenge nonce
// REL-026 issues (internal/relay/hello.NewChallenge) — a hex string. The
// corpus case REL-020 carries nonce "c4a1f8e2b3d4c5f60718293a4b5c6d7e".
const testNonce = "c4a1f8e2b3d4c5f60718293a4b5c6d7e"

// csrA / csrB stand in for two distinct CSR DER blobs — the substituted-CSR
// replay REL-027 forbids turns on these being different.
var (
	csrA = []byte("-----BEGIN CERTIFICATE REQUEST-----\nCSR-ALPHA\n-----END CERTIFICATE REQUEST-----")
	csrB = []byte("-----BEGIN CERTIFICATE REQUEST-----\nCSR-BRAVO\n-----END CERTIFICATE REQUEST-----")
)

// TestSignVerifyPoPRoundTrip is the happy path (REL-020/027/028): a
// pop_signature computed with the presented (expired) certificate's own
// private key over {nonce, csr} verifies against the public key on record for
// that serial.
func TestSignVerifyPoPRoundTrip(t *testing.T) {
	pub, priv := signhash.GenerateKey()

	sig := SignPoP(priv, testNonce, csrA)
	if sig == "" {
		t.Fatal("SignPoP() = empty string, want a signature")
	}

	if !VerifyPoP(pub, testNonce, csrA, sig) {
		t.Fatal("VerifyPoP() = false for an untampered nonce+csr round-trip, want true")
	}
}

// TestVerifyPoPWrongKeyFails covers REL-028's "against the public key on
// record for the presented serial": a pop_signature computed with an unrelated
// key MUST NOT verify against the recorded key — the second refusal in corpus
// case REL-027 ("computed with an unrelated key, not the presented
// certificate's own").
func TestVerifyPoPWrongKeyFails(t *testing.T) {
	_, unrelatedPriv := signhash.GenerateKey()
	recordedPub, _ := signhash.GenerateKey()

	sig := SignPoP(unrelatedPriv, testNonce, csrA)

	if VerifyPoP(recordedPub, testNonce, csrA, sig) {
		t.Fatal("VerifyPoP() = true for a signature made with an unrelated key, want false (REL-028)")
	}
}

// TestVerifyPoPSubstitutedCSRFails is REL-027's core replay-resistance
// property: a signature captured over one CSR MUST NOT verify against a
// substituted CSR, even with the correct key and nonce.
func TestVerifyPoPSubstitutedCSRFails(t *testing.T) {
	pub, priv := signhash.GenerateKey()

	sig := SignPoP(priv, testNonce, csrA)

	if VerifyPoP(pub, testNonce, csrB, sig) {
		t.Fatal("VerifyPoP() = true for a signature replayed against a substituted CSR, want false (REL-027)")
	}
}

// TestVerifyPoPWrongNonceFails: the binding is over the nonce too, so a
// signature over one challenge nonce MUST NOT verify against another (REL-026
// single-use nonce, REL-027 binding).
func TestVerifyPoPWrongNonceFails(t *testing.T) {
	pub, priv := signhash.GenerateKey()

	sig := SignPoP(priv, testNonce, csrA)

	if VerifyPoP(pub, "00000000000000000000000000000000", csrA, sig) {
		t.Fatal("VerifyPoP() = true for a signature over a different nonce, want false")
	}
}

// TestVerifyPoPAbsentOrMalformedFails covers REL-029's "absent, or malformed":
// an empty pop_signature (the first refusal in corpus case REL-027, which
// omits pop_signature entirely) and non-hex / wrong-length garbage MUST NOT
// verify.
func TestVerifyPoPAbsentOrMalformedFails(t *testing.T) {
	pub, _ := signhash.GenerateKey()

	cases := map[string]string{
		"absent":         "",
		"not hex":        "zzzz not hex zzzz",
		"odd length hex": "abc",
		"too short":      "00",
		"too long":       "00" + hex128(ed25519.SignatureSize+8),
	}

	for name, sig := range cases {
		t.Run(name, func(t *testing.T) {
			if VerifyPoP(pub, testNonce, csrA, sig) {
				t.Fatalf("VerifyPoP() = true for a %s signature, want false (REL-029)", name)
			}
		})
	}
}

// TestVerifyPoPMalformedRecordedKeyFailsClosed: the recorded public key is
// wire-decoded from the issuance record; a wrong-length key MUST fail closed
// (return false), never crash the verifier (crypto/ed25519.Verify panics on a
// bad-length key).
func TestVerifyPoPMalformedRecordedKeyFailsClosed(t *testing.T) {
	_, priv := signhash.GenerateKey()
	sig := SignPoP(priv, testNonce, csrA)

	keys := map[string]ed25519.PublicKey{
		"empty":     {},
		"too short": make(ed25519.PublicKey, ed25519.PublicKeySize-1),
		"too long":  make(ed25519.PublicKey, ed25519.PublicKeySize+1),
	}

	for name, pub := range keys {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("VerifyPoP() panicked on a %s recorded key: %v", name, r)
				}
			}()
			if VerifyPoP(pub, testNonce, csrA, sig) {
				t.Fatalf("VerifyPoP() = true for a %s recorded key, want false", name)
			}
		})
	}
}

// hex128 returns a hex string encoding n zero bytes.
func hex128(n int) string {
	const hexDigits = "00"
	out := ""
	for i := 0; i < n; i++ {
		out += hexDigits
	}
	return out
}
