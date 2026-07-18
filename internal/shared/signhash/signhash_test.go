package signhash

import (
	"crypto/ed25519"
	"testing"
)

// TestSignVerifyRoundTrip confirms a message signed with a generated key
// verifies successfully against the matching public key.
func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := GenerateKey()
	msg := []byte("desired-state snapshot v1")

	sig := Sign(priv, msg)

	if !Verify(pub, msg, sig) {
		t.Fatal("Verify() = false for an untampered sign/verify round-trip, want true")
	}
}

// TestVerifyTamperedMessageFails confirms a signature no longer validates
// once the signed message is altered after signing.
func TestVerifyTamperedMessageFails(t *testing.T) {
	pub, priv := GenerateKey()
	msg := []byte("desired-state snapshot v1")

	sig := Sign(priv, msg)

	tampered := []byte("desired-state snapshot v2")
	if Verify(pub, tampered, sig) {
		t.Fatal("Verify() = true for a tampered message, want false")
	}
}

// TestContentID checks ContentID against a fixture whose sha256 was computed
// independently via `printf 'hello waiveo' | shasum -a 256`:
//
//	196474ea2e67632e23744e07fb79db7d2cea8b2e22a45c9dffbc1c9e38838f8a
func TestContentID(t *testing.T) {
	const want = "sha256:196474ea2e67632e23744e07fb79db7d2cea8b2e22a45c9dffbc1c9e38838f8a"

	got := ContentID([]byte("hello waiveo"))

	if got != want {
		t.Fatalf("ContentID() = %q, want %q", got, want)
	}
}

// TestVerifyWrongLengthKeyReturnsFalse is a carry-forward guard from Task 2:
// crypto/ed25519.Verify panics on a public key of the wrong length, and the
// relay feeds Verify an untrusted, wire-decoded
// `desired_state_verification_key` (relay/1 REL-012, REL-071) — so a
// malformed or truncated key MUST fail closed (return false), never crash
// the process that's verifying a snapshot's signature.
func TestVerifyWrongLengthKeyReturnsFalse(t *testing.T) {
	msg := []byte("desired-state snapshot v1")
	sig := make([]byte, ed25519.SignatureSize)

	tests := map[string]ed25519.PublicKey{
		"empty":     {},
		"too short": make(ed25519.PublicKey, ed25519.PublicKeySize-1),
		"too long":  make(ed25519.PublicKey, ed25519.PublicKeySize+1),
	}

	for name, pub := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Verify() panicked on a %s key: %v", name, r)
				}
			}()
			if Verify(pub, msg, sig) {
				t.Fatalf("Verify() = true for a %s (length %d) public key, want false", name, len(pub))
			}
		})
	}
}
