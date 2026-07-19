package hello

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
)

// genKeypair is a small test helper generating a fresh ed25519 keypair with
// crypto/rand — mirroring internal/shared/signhash.GenerateKey without
// importing it, so these tests exercise SignChannelBinding/
// VerifyChannelBinding exactly as a caller holding raw ed25519 keys would.
func genKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

// TestChannelBindingRoundTrips asserts the baseline positive case: a
// signature computed with SignChannelBinding over a given nonce verifies
// with VerifyChannelBinding against the matching public key and the SAME
// nonce (REL-032).
func TestChannelBindingRoundTrips(t *testing.T) {
	pub, priv := genKeypair(t)
	const nonce = "b3f1c2a9d4e5f60718293a4b5c6d7e8f"

	sig, err := SignChannelBinding(priv, nonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}

	if err := VerifyChannelBinding(pub, nonce, sig); err != nil {
		t.Errorf("VerifyChannelBinding(valid sig) = %v, want nil", err)
	}
}

// TestChannelBindingRefusesWrongKey is the core REL-032 security property:
// a signature computed under one relay's enrollment private key MUST be
// refused when verified against a DIFFERENT relay's public key — proving an
// impostor cannot present a validly-shaped signature for a key it does not
// hold, even though the bytes are a genuine ed25519 signature over the
// right nonce.
func TestChannelBindingRefusesWrongKey(t *testing.T) {
	_, impostorPriv := genKeypair(t)
	genuinePub, _ := genKeypair(t)
	const nonce = "b3f1c2a9d4e5f60718293a4b5c6d7e8f"

	sig, err := SignChannelBinding(impostorPriv, nonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}

	err = VerifyChannelBinding(genuinePub, nonce, sig)
	if !errors.Is(err, ErrChannelBindingInvalid) {
		t.Errorf("VerifyChannelBinding(wrong-key sig) = %v, want ErrChannelBindingInvalid", err)
	}
}

// TestChannelBindingRefusesAbsentSignature asserts an empty
// channel_binding_signature — the field present but carrying no value, as a
// relay that skipped signing would send — is refused, not treated as
// vacuously valid.
func TestChannelBindingRefusesAbsentSignature(t *testing.T) {
	pub, _ := genKeypair(t)
	const nonce = "b3f1c2a9d4e5f60718293a4b5c6d7e8f"

	err := VerifyChannelBinding(pub, nonce, "")
	if !errors.Is(err, ErrChannelBindingInvalid) {
		t.Errorf("VerifyChannelBinding(absent sig) = %v, want ErrChannelBindingInvalid", err)
	}
}

// TestChannelBindingRefusesTamperedSignature asserts a genuinely-signed
// signature that has since been bit-flipped is refused (a corrupted or
// intercepted-and-modified signature must not verify).
func TestChannelBindingRefusesTamperedSignature(t *testing.T) {
	pub, priv := genKeypair(t)
	const nonce = "b3f1c2a9d4e5f60718293a4b5c6d7e8f"

	sig, err := SignChannelBinding(priv, nonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}

	// Flip the last hex character so the byte string decodes but no longer
	// carries the genuine signature.
	tampered := []byte(sig)
	last := tampered[len(tampered)-1]
	if last == '0' {
		tampered[len(tampered)-1] = '1'
	} else {
		tampered[len(tampered)-1] = '0'
	}

	err = VerifyChannelBinding(pub, nonce, string(tampered))
	if !errors.Is(err, ErrChannelBindingInvalid) {
		t.Errorf("VerifyChannelBinding(tampered sig) = %v, want ErrChannelBindingInvalid", err)
	}
}

// TestChannelBindingRefusesWrongNonce asserts a signature that is
// genuinely valid over one nonce is refused when checked against a
// DIFFERENT nonce — proving verification is bound to the specific
// challenge nonce for this connection attempt, not just to the signing
// key (REL-030/REL-032 together: the nonce is single-use per connection,
// and the signature must cover exactly that nonce).
func TestChannelBindingRefusesWrongNonce(t *testing.T) {
	pub, priv := genKeypair(t)
	const signedNonce = "b3f1c2a9d4e5f60718293a4b5c6d7e8f"
	const otherNonce = "c4a1f8e2b3d4c5f60718293a4b5c6d7e"

	sig, err := SignChannelBinding(priv, signedNonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}

	err = VerifyChannelBinding(pub, otherNonce, sig)
	if !errors.Is(err, ErrChannelBindingInvalid) {
		t.Errorf("VerifyChannelBinding(wrong-nonce sig) = %v, want ErrChannelBindingInvalid", err)
	}
}

// TestChannelBindingRefusesMalformedSignatureString asserts a
// channel_binding_signature that is not even well-formed
// ("ed25519-sig:"+hex) — no recognized prefix, or a prefix followed by
// non-hex garbage — is refused rather than panicking or being silently
// accepted.
func TestChannelBindingRefusesMalformedSignatureString(t *testing.T) {
	pub, _ := genKeypair(t)
	const nonce = "b3f1c2a9d4e5f60718293a4b5c6d7e8f"

	cases := []string{
		"not-even-the-right-shape",
		"ed25519-sig:not-valid-hex-zz",
		"ed25519-sig-over-nonce-b3f1c2a9d4e5f60718293a4b5c6d7e8f-signed-with-relay-enrollment-key", // the REL-030 corpus's own descriptive placeholder string, never real crypto
	}
	for _, sig := range cases {
		err := VerifyChannelBinding(pub, nonce, sig)
		if !errors.Is(err, ErrChannelBindingInvalid) {
			t.Errorf("VerifyChannelBinding(%q) = %v, want ErrChannelBindingInvalid", sig, err)
		}
	}
}

// TestChannelBindingRefusesMalformedPublicKey asserts VerifyChannelBinding
// fails closed (returns ErrChannelBindingInvalid) rather than panicking
// when handed a public key that is not exactly ed25519.PublicKeySize bytes
// — the shape an enrollment-learned key would take if it were ever
// corrupted or wire-malformed, which must never crash the app peer's
// hello-handling path.
func TestChannelBindingRefusesMalformedPublicKey(t *testing.T) {
	_, priv := genKeypair(t)
	const nonce = "b3f1c2a9d4e5f60718293a4b5c6d7e8f"

	sig, err := SignChannelBinding(priv, nonce)
	if err != nil {
		t.Fatalf("SignChannelBinding: %v", err)
	}

	badPub := ed25519.PublicKey([]byte{0x01, 0x02, 0x03})
	err = VerifyChannelBinding(badPub, nonce, sig)
	if !errors.Is(err, ErrChannelBindingInvalid) {
		t.Errorf("VerifyChannelBinding(malformed pub) = %v, want ErrChannelBindingInvalid", err)
	}
}

// TestSignChannelBindingRejectsNonHexNonce asserts SignChannelBinding
// itself refuses a nonce that is not valid hex, rather than signing
// garbage bytes silently.
func TestSignChannelBindingRejectsNonHexNonce(t *testing.T) {
	_, priv := genKeypair(t)
	if _, err := SignChannelBinding(priv, "not-hex-zz"); err == nil {
		t.Errorf("SignChannelBinding(non-hex nonce) = nil error, want an error")
	}
}
