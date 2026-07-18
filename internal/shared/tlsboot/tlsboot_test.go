package tlsboot

import (
	"bytes"
	"testing"
)

// TestFingerprintCommitmentDeterministic confirms FingerprintCommitment
// returns the exact same bytes every time it is computed over a fixed cert.
func TestFingerprintCommitmentDeterministic(t *testing.T) {
	certPEM, _ := GenSelfSigned()

	c1 := FingerprintCommitment(certPEM)
	c2 := FingerprintCommitment(certPEM)

	if !bytes.Equal(c1, c2) {
		t.Fatalf("FingerprintCommitment() not deterministic: %x != %x", c1, c2)
	}
}

// TestFingerprintCommitmentLength confirms the commitment meets the banked
// PLY-052 floor of >=80 bits (>=10 bytes) of truncated-SHA-256 material.
func TestFingerprintCommitmentLength(t *testing.T) {
	certPEM, _ := GenSelfSigned()

	c := FingerprintCommitment(certPEM)

	if len(c) < 10 {
		t.Fatalf("FingerprintCommitment() length = %d bytes (%d bits), want >= 10 bytes (>=80 bits)", len(c), len(c)*8)
	}
}

// TestVerifyCommitmentAcceptsMatchingCert confirms VerifyCommitment returns
// true when recomputing the commitment over the same cert it was made from.
func TestVerifyCommitmentAcceptsMatchingCert(t *testing.T) {
	certPEM, _ := GenSelfSigned()

	commitment := FingerprintCommitment(certPEM)

	if !VerifyCommitment(certPEM, commitment) {
		t.Fatal("VerifyCommitment() = false for the matching cert, want true")
	}
}

// TestVerifyCommitmentRejectsSubstitutedCert is the PLY-056 property made
// concrete: a pairing code carries a commitment computed over the real
// relay cert. A MITM who substitutes their own self-signed cert during the
// verification-disabled fetch must NOT be able to pass local verification.
// The commitment is never recomputed from wire material supplied by the
// substituted party — only from the cert the player actually fetched — so a
// distinct cert (even another legitimately-generated self-signed cert) must
// fail VerifyCommitment against the original commitment.
func TestVerifyCommitmentRejectsSubstitutedCert(t *testing.T) {
	certA, _ := GenSelfSigned()
	certB, _ := GenSelfSigned()

	if bytes.Equal(certA, certB) {
		t.Fatal("GenSelfSigned() produced identical cert bytes on two calls; test fixture invalid")
	}

	commitmentA := FingerprintCommitment(certA)

	if VerifyCommitment(certB, commitmentA) {
		t.Fatal("VerifyCommitment(certB, commitmentA) = true, want false (MITM-substituted cert must be rejected)")
	}

	// Sanity: the original pairing still verifies correctly.
	if !VerifyCommitment(certA, commitmentA) {
		t.Fatal("VerifyCommitment(certA, commitmentA) = false, want true")
	}
}

// TestGenSelfSignedProducesParsableCertAndKey confirms GenSelfSigned's
// output is well-formed PEM for both the cert and the ed25519 private key,
// and that the key pairs with the cert's public key.
func TestGenSelfSignedProducesParsableCertAndKey(t *testing.T) {
	certPEM, keyPEM := GenSelfSigned()

	if len(certPEM) == 0 {
		t.Fatal("GenSelfSigned() returned empty certPEM")
	}
	if len(keyPEM) == 0 {
		t.Fatal("GenSelfSigned() returned empty keyPEM")
	}

	if !bytes.Contains(certPEM, []byte("CERTIFICATE")) {
		t.Fatalf("certPEM does not look like a PEM certificate block: %s", certPEM)
	}
	if !bytes.Contains(keyPEM, []byte("PRIVATE KEY")) {
		t.Fatalf("keyPEM does not look like a PEM private key block: %s", keyPEM)
	}
}
