package tlsboot

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
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
// output actually parses as a real X.509 certificate and a real PKCS8
// ed25519 private key, and that the key pairs with the cert's public key —
// not merely that the PEM bytes are non-empty and mention the right words.
func TestGenSelfSignedProducesParsableCertAndKey(t *testing.T) {
	certPEM, keyPEM := GenSelfSigned()

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Fatalf("certPEM did not decode to a CERTIFICATE block: %s", certPEM)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate(certBlock.Bytes) error: %v", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		t.Fatalf("keyPEM did not decode to a PRIVATE KEY block: %s", keyPEM)
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("x509.ParsePKCS8PrivateKey(keyBlock.Bytes) error: %v", err)
	}

	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("parsed private key is %T, want ed25519.PrivateKey", key)
	}

	certPub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("cert.PublicKey is %T, want ed25519.PublicKey", cert.PublicKey)
	}

	if !priv.Public().(ed25519.PublicKey).Equal(certPub) {
		t.Fatal("parsed private key's public half does not match the cert's public key")
	}
}

// TestFingerprintCommitmentMatchesDERCore confirms the PEM-holding
// convenience FingerprintCommitment agrees exactly with the DER core
// FingerprintCommitmentDER when both are fed the same certificate — the
// interop guarantee that lets a non-Go verifier (which only ever sees DER)
// reproduce the same commitment the relay computed from PEM.
func TestFingerprintCommitmentMatchesDERCore(t *testing.T) {
	certPEM, _ := GenSelfSigned()

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("certPEM did not decode to a CERTIFICATE block: %s", certPEM)
	}

	fromPEM := FingerprintCommitment(certPEM)
	fromDER := FingerprintCommitmentDER(block.Bytes)

	if !bytes.Equal(fromPEM, fromDER) {
		t.Fatalf("FingerprintCommitment(certPEM) = %x, FingerprintCommitmentDER(der) = %x, want equal", fromPEM, fromDER)
	}
}

// TestVerifyCommitmentDERRejectsSubstitutedCert is the DER-variant
// equivalent of TestVerifyCommitmentRejectsSubstitutedCert: the same
// PLY-056 MITM property, exercised via the raw-DER path a non-Go player
// (which only ever holds DER, never PEM) would actually call.
func TestVerifyCommitmentDERRejectsSubstitutedCert(t *testing.T) {
	certA, _ := GenSelfSigned()
	certB, _ := GenSelfSigned()

	blockA, _ := pem.Decode(certA)
	blockB, _ := pem.Decode(certB)
	if blockA == nil || blockB == nil {
		t.Fatal("failed to PEM-decode one of the generated certs; test fixture invalid")
	}

	derA, err := x509.ParseCertificate(blockA.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate(certA) error: %v", err)
	}
	derB, err := x509.ParseCertificate(blockB.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate(certB) error: %v", err)
	}

	if bytes.Equal(derA.Raw, derB.Raw) {
		t.Fatal("GenSelfSigned() produced identical cert DER on two calls; test fixture invalid")
	}

	commitmentA := FingerprintCommitmentDER(derA.Raw)

	if VerifyCommitmentDER(derB.Raw, commitmentA) {
		t.Fatal("VerifyCommitmentDER(derB.Raw, commitmentA) = true, want false (MITM-substituted cert must be rejected)")
	}

	// Sanity: the original pairing still verifies correctly.
	if !VerifyCommitmentDER(derA.Raw, commitmentA) {
		t.Fatal("VerifyCommitmentDER(derA.Raw, commitmentA) = false, want true")
	}
}

// TestVerifyCommitmentRejectsGarbagePEM confirms VerifyCommitment fails
// closed — returns false, never panics — when certPEM is not decodable PEM
// (or not a CERTIFICATE block) at all, e.g. a truncated or corrupted fetch.
func TestVerifyCommitmentRejectsGarbagePEM(t *testing.T) {
	garbage := []byte("this is not PEM at all")

	commitment := make([]byte, commitmentBytes) // arbitrary non-empty commitment

	if VerifyCommitment(garbage, commitment) {
		t.Fatal("VerifyCommitment(garbage, commitment) = true, want false")
	}

	// Also confirm FingerprintCommitment itself returns nil rather than
	// panicking on undecodable input.
	if got := FingerprintCommitment(garbage); got != nil {
		t.Fatalf("FingerprintCommitment(garbage) = %x, want nil", got)
	}
}
