package tlsboot

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// certDER returns the raw DER of a fresh GenSelfSigned certificate,
// discarding the key PEM. Most tests below work directly with DER (the form
// SPKIFromCertDER / CommitmentForCertDER / VerifyCommitmentForCertDER take)
// since that is the wire form every TLS stack, Go included, actually hands
// callers.
func certDER(t *testing.T) []byte {
	t.Helper()
	certPEM, _ := GenSelfSigned()
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("certPEM did not decode to a CERTIFICATE block: %s", certPEM)
	}
	return block.Bytes
}

// spkiOf parses der and returns its SubjectPublicKeyInfo, failing the test
// on any error.
func spkiOf(t *testing.T, der []byte) []byte {
	t.Helper()
	spki, err := SPKIFromCertDER(der)
	if err != nil {
		t.Fatalf("SPKIFromCertDER() error: %v", err)
	}
	return spki
}

// TestCommitmentDeterministic confirms Commitment returns the exact same
// bytes every time it is computed over the same SPKI.
func TestCommitmentDeterministic(t *testing.T) {
	spki := spkiOf(t, certDER(t))

	c1 := Commitment(spki)
	c2 := Commitment(spki)

	if !bytes.Equal(c1, c2) {
		t.Fatalf("Commitment() not deterministic: %x != %x", c1, c2)
	}
}

// TestCommitmentLength confirms the commitment meets the banked PLY-052
// floor of >=80 bits (>=10 bytes) of truncated-SHA-256 material.
func TestCommitmentLength(t *testing.T) {
	spki := spkiOf(t, certDER(t))

	c := Commitment(spki)

	if len(c) < 10 {
		t.Fatalf("Commitment() length = %d bytes (%d bits), want >= 10 bytes (>=80 bits)", len(c), len(c)*8)
	}
}

// TestVerifyCommitmentAcceptsMatchingSPKI confirms VerifyCommitment returns
// true when recomputing the commitment over the same SPKI it was made from.
func TestVerifyCommitmentAcceptsMatchingSPKI(t *testing.T) {
	spki := spkiOf(t, certDER(t))

	commitment := Commitment(spki)

	if !VerifyCommitment(commitment, spki) {
		t.Fatal("VerifyCommitment() = false for the matching SPKI, want true")
	}
}

// TestVerifyCommitmentRejectsSubstitutedCert is the PLY-056 property made
// concrete: a pairing code carries a commitment computed over the real
// relay cert's SPKI. A MITM who substitutes their own self-signed cert
// during the verification-disabled fetch (a distinct key, hence a distinct
// SPKI) must NOT be able to pass local verification. The commitment is
// never recomputed from wire material supplied by the substituted party —
// only from the SPKI of the cert the player actually fetched — so a
// distinct cert (even another legitimately-generated self-signed cert) must
// fail VerifyCommitment against the original commitment.
func TestVerifyCommitmentRejectsSubstitutedCert(t *testing.T) {
	derA := certDER(t)
	derB := certDER(t)

	if bytes.Equal(derA, derB) {
		t.Fatal("GenSelfSigned() produced identical cert DER on two calls; test fixture invalid")
	}

	spkiA := spkiOf(t, derA)
	spkiB := spkiOf(t, derB)
	if bytes.Equal(spkiA, spkiB) {
		t.Fatal("two distinct GenSelfSigned() certs produced identical SPKI; test fixture invalid")
	}

	commitmentA := Commitment(spkiA)

	if VerifyCommitment(commitmentA, spkiB) {
		t.Fatal("VerifyCommitment(commitmentA, spkiB) = true, want false (MITM-substituted cert must be rejected)")
	}

	// Sanity: the original pairing still verifies correctly.
	if !VerifyCommitment(commitmentA, spkiA) {
		t.Fatal("VerifyCommitment(commitmentA, spkiA) = false, want true")
	}
}

// TestCommitmentSameKeyDifferentCertificatesMatch is the key-pinning
// property this fix exists to establish: PLY-052 commits to the
// SubjectPublicKeyInfo a certificate certifies, NOT the surrounding
// certificate's other fields (serial number, validity window, ...). Two
// certificates minted over the SAME public key — different serial numbers
// and different validity windows, e.g. across a cert renewal that keeps the
// same key — MUST produce the SAME commitment, because their SPKI is
// identical.
//
// A commitment computed over the full certificate DER (the pre-fix
// behavior) would FAIL this test: the differing serial/validity bytes would
// hash to two different digests. An SPKI-based commitment passes, because
// SPKI depends only on the public key, not on any other certificate field.
// This is the regression guard proving the fix is correct, not merely that
// it compiles.
func TestCommitmentSameKeyDifferentCertificatesMatch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error: %v", err)
	}

	mkCert := func(serial int64, notAfterYears int) []byte {
		t.Helper()
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject: pkix.Name{
				CommonName: "waiveo-relay",
			},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().AddDate(notAfterYears, 0, 0),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
		if err != nil {
			t.Fatalf("x509.CreateCertificate() error: %v", err)
		}
		return der
	}

	derOld := mkCert(1, 1)  // serial 1, expires in 1 year
	derNew := mkCert(2, 10) // serial 2 (renewal), expires in 10 years — same key

	if bytes.Equal(derOld, derNew) {
		t.Fatal("the two certificates have identical DER; test fixture invalid (expected differing serial/validity)")
	}

	spkiOld := spkiOf(t, derOld)
	spkiNew := spkiOf(t, derNew)
	if !bytes.Equal(spkiOld, spkiNew) {
		t.Fatalf("SPKI differs across a same-key renewal: %x != %x (SPKI should depend only on the public key)", spkiOld, spkiNew)
	}

	commitmentOld := Commitment(spkiOld)
	commitmentNew := Commitment(spkiNew)

	if !bytes.Equal(commitmentOld, commitmentNew) {
		t.Fatalf("Commitment() differs across a same-key cert renewal: %x != %x — commitment is pinning the certificate, not the key (this is the exact defect this fix corrects)", commitmentOld, commitmentNew)
	}

	// And CommitmentForCertDER (the whole-certificate convenience path)
	// must agree with the manual SPKI-extraction path above.
	viaDERold, err := CommitmentForCertDER(derOld)
	if err != nil {
		t.Fatalf("CommitmentForCertDER(derOld) error: %v", err)
	}
	viaDERnew, err := CommitmentForCertDER(derNew)
	if err != nil {
		t.Fatalf("CommitmentForCertDER(derNew) error: %v", err)
	}
	if !bytes.Equal(viaDERold, viaDERnew) {
		t.Fatalf("CommitmentForCertDER() differs across a same-key cert renewal: %x != %x", viaDERold, viaDERnew)
	}

	// And the renewed cert must still verify against the commitment minted
	// from the original.
	ok, err := VerifyCommitmentForCertDER(derNew, commitmentOld)
	if err != nil {
		t.Fatalf("VerifyCommitmentForCertDER(derNew, commitmentOld) error: %v", err)
	}
	if !ok {
		t.Fatal("VerifyCommitmentForCertDER(derNew, commitmentOld) = false, want true (same key across renewal must still verify)")
	}
}

// TestCommitmentOrderMatters confirms Commitment's ordered-concatenation
// property (PLY-052: "in trust_anchors array order"): committing to the
// same two SPKIs in a different order MUST produce a different commitment.
func TestCommitmentOrderMatters(t *testing.T) {
	spkiA := spkiOf(t, certDER(t))
	spkiB := spkiOf(t, certDER(t))

	forward := Commitment(spkiA, spkiB)
	reverse := Commitment(spkiB, spkiA)

	if bytes.Equal(forward, reverse) {
		t.Fatal("Commitment(spkiA, spkiB) == Commitment(spkiB, spkiA), want distinct (array order must matter per PLY-052)")
	}
}

// TestCommitmentMultiAnchorRoundTrip confirms VerifyCommitment accepts a
// multi-anchor commitment when given the same ordered SPKIs, and rejects it
// under any reordering or subset.
func TestCommitmentMultiAnchorRoundTrip(t *testing.T) {
	spkiA := spkiOf(t, certDER(t))
	spkiB := spkiOf(t, certDER(t))

	commitment := Commitment(spkiA, spkiB)

	if !VerifyCommitment(commitment, spkiA, spkiB) {
		t.Fatal("VerifyCommitment(commitment, spkiA, spkiB) = false, want true")
	}
	if VerifyCommitment(commitment, spkiB, spkiA) {
		t.Fatal("VerifyCommitment(commitment, spkiB, spkiA) = true, want false (reordered anchors must not verify)")
	}
	if VerifyCommitment(commitment, spkiA) {
		t.Fatal("VerifyCommitment(commitment, spkiA) = true, want false (subset of anchors must not verify)")
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

// TestCommitmentForCertDERMatchesManualSPKI confirms the single-cert
// convenience CommitmentForCertDER agrees exactly with the manual
// SPKIFromCertDER + Commitment path — the interop guarantee that lets a
// non-Go verifier (which only ever sees DER) reproduce the same commitment
// the relay computed.
func TestCommitmentForCertDERMatchesManualSPKI(t *testing.T) {
	der := certDER(t)

	viaConvenience, err := CommitmentForCertDER(der)
	if err != nil {
		t.Fatalf("CommitmentForCertDER() error: %v", err)
	}
	viaManual := Commitment(spkiOf(t, der))

	if !bytes.Equal(viaConvenience, viaManual) {
		t.Fatalf("CommitmentForCertDER(der) = %x, Commitment(SPKIFromCertDER(der)) = %x, want equal", viaConvenience, viaManual)
	}
}

// TestVerifyCommitmentForCertDERRejectsSubstitutedCert is the
// CommitmentForCertDER/VerifyCommitmentForCertDER-variant equivalent of
// TestVerifyCommitmentRejectsSubstitutedCert: the same PLY-056 MITM
// property, exercised via the single-cert convenience path the feeder,
// relay, and virtual player actually call.
func TestVerifyCommitmentForCertDERRejectsSubstitutedCert(t *testing.T) {
	derA := certDER(t)
	derB := certDER(t)

	if bytes.Equal(derA, derB) {
		t.Fatal("GenSelfSigned() produced identical cert DER on two calls; test fixture invalid")
	}

	commitmentA, err := CommitmentForCertDER(derA)
	if err != nil {
		t.Fatalf("CommitmentForCertDER(derA) error: %v", err)
	}

	ok, err := VerifyCommitmentForCertDER(derB, commitmentA)
	if err != nil {
		t.Fatalf("VerifyCommitmentForCertDER(derB, commitmentA) error: %v", err)
	}
	if ok {
		t.Fatal("VerifyCommitmentForCertDER(derB, commitmentA) = true, want false (MITM-substituted cert must be rejected)")
	}

	// Sanity: the original pairing still verifies correctly.
	ok, err = VerifyCommitmentForCertDER(derA, commitmentA)
	if err != nil {
		t.Fatalf("VerifyCommitmentForCertDER(derA, commitmentA) error: %v", err)
	}
	if !ok {
		t.Fatal("VerifyCommitmentForCertDER(derA, commitmentA) = false, want true")
	}
}

// TestSPKIFromCertDERRejectsGarbage confirms SPKIFromCertDER fails closed —
// returns an error, never panics — when der is not a parsable certificate at
// all, e.g. a truncated or corrupted fetch.
func TestSPKIFromCertDERRejectsGarbage(t *testing.T) {
	garbage := []byte("this is not a certificate at all")

	spki, err := SPKIFromCertDER(garbage)
	if err == nil {
		t.Fatal("SPKIFromCertDER(garbage) error = nil, want non-nil")
	}
	if spki != nil {
		t.Fatalf("SPKIFromCertDER(garbage) spki = %x, want nil", spki)
	}
}

// TestCommitmentForCertDERRejectsGarbage confirms CommitmentForCertDER and
// VerifyCommitmentForCertDER fail closed on undecodable DER — an error, not
// a panic, and VerifyCommitmentForCertDER never reports a match.
func TestCommitmentForCertDERRejectsGarbage(t *testing.T) {
	garbage := []byte("this is not a certificate at all")
	commitment := make([]byte, commitmentBytes) // arbitrary non-empty commitment

	if _, err := CommitmentForCertDER(garbage); err == nil {
		t.Fatal("CommitmentForCertDER(garbage) error = nil, want non-nil")
	}

	ok, err := VerifyCommitmentForCertDER(garbage, commitment)
	if err == nil {
		t.Fatal("VerifyCommitmentForCertDER(garbage, commitment) error = nil, want non-nil")
	}
	if ok {
		t.Fatal("VerifyCommitmentForCertDER(garbage, commitment) ok = true, want false")
	}
}

// TestVerifyCommitmentRejectsEmptyInput confirms VerifyCommitment fails
// closed — returns false, never panics — on empty or malformed input: no
// SPKIs at all, or an SPKI that is itself empty.
func TestVerifyCommitmentRejectsEmptyInput(t *testing.T) {
	commitment := make([]byte, commitmentBytes)

	if VerifyCommitment(commitment) {
		t.Fatal("VerifyCommitment(commitment) with no SPKIs = true, want false")
	}
	if VerifyCommitment(commitment, []byte{}) {
		t.Fatal("VerifyCommitment(commitment, <empty SPKI>) = true, want false")
	}
	spki := spkiOf(t, certDER(t))
	if VerifyCommitment(commitment, spki, []byte{}) {
		t.Fatal("VerifyCommitment(commitment, spki, <empty SPKI>) = true, want false")
	}
}
