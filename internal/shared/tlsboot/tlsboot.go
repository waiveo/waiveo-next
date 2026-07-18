// Package tlsboot generates the relay's self-signed TLS bootstrap
// certificate and implements the out-of-band pairing-pin commitment scheme
// that closes the pairing-MITM window (player/1 PLY-052/053/054/056).
//
// The pairing flow: a screen fetches the relay's TLS cert over a
// verification-disabled connection (there is no CA to trust yet — this is
// the relay's own bootstrap cert). A network attacker positioned during
// pairing could substitute their own cert for the real one. The defense is
// an out-of-band channel (the pairing code, shown on the relay and typed or
// scanned on the screen) that carries a commitment over the real cert's
// public key. The player recomputes the commitment locally from the SPKI of
// the cert it actually fetched and compares it against the OOB value with
// VerifyCommitment (or VerifyCommitmentForCertDER). The commitment is NEVER
// retransmitted or re-derived from anything the wire connection supplies —
// a self-attesting authenticator delivered in-band would let a MITM simply
// forward its own commitment and defeat the check (the PLY-056 ban). See
// tlsboot_test.go's TestVerifyCommitmentRejectsSubstitutedCert for the
// concrete MITM case: committing to cert A's SPKI and then verifying against
// a distinct cert B's SPKI must fail.
//
// The commitment is computed over the certificate's SubjectPublicKeyInfo
// (SPKI) — the public key the cert certifies, not the surrounding
// certificate's other fields (serial number, validity window, subject,
// signature, ...) — per player/1 PLY-052 and relay/1 REL-126. For multiple
// trust anchors, the commitment is a single SHA-256 digest over the ordered
// concatenation of each anchor's SPKI, in `trust_anchors` array order
// (PLY-052's "canonical concatenation ... in trust_anchors array order").
// Committing to the SPKI rather than the full certificate is deliberate: it
// is a key pin, not a cert pin (the same property REL-137 banks for the
// enrolled leaf) — a certificate can be reissued (new serial, new validity
// window) over the SAME key without invalidating a commitment minted against
// an earlier certificate carrying that key, and it is the specific public
// key — not any other certificate field an issuer could vary — that a
// second-preimage attacker would need to forge.
//
// SPKI is extracted from the certificate's canonical DER bytes, never its
// PEM encoding. DER is the on-the-wire form every TLS stack already has in
// hand (Go's tls.ConnectionState.PeerCertificates[0].Raw, a BrightScript
// player's raw peer-cert bytes, etc.) — hashing PEM instead would force
// every non-Go verifier to reproduce Go's exact PEM wrapping (block type,
// header-free, 64-column base64) byte-for-byte, which is a pointless and
// fragile cross-implementation dependency.
//
// Stdlib-only (crypto/ed25519, crypto/rand, crypto/sha256, crypto/subtle,
// crypto/x509, encoding/pem, math/big) — no protocol framing or
// pairing-code transport lives here, only the primitives. Callers that need
// a tls.Certificate for serving assemble one from GenSelfSigned's PEM
// output via crypto/tls's X509KeyPair.
package tlsboot

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"
)

// commitmentBytes is the truncated-SHA-256 commitment length: 16 bytes
// (128 bits) — comfortably above the PLY-052 banked floor of >=80 bits
// (>=10 bytes), leaving margin without lengthening the pairing code much.
const commitmentBytes = 16

// GenSelfSigned generates a fresh ed25519 key pair and a self-signed TLS
// leaf certificate over it, returning both as PEM. This is the relay's
// bootstrap identity: it has no CA, so the cert is self-signed, and its
// authenticity is instead established out-of-band via the SPKI commitment
// (Commitment / CommitmentForCertDER) carried in the pairing code.
func GenSelfSigned() (certPEM, keyPEM []byte) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		// crypto/rand.Reader failing is a fatal environment problem
		// (entropy source unavailable); there is no meaningful error
		// to propagate to callers.
		panic("tlsboot: GenSelfSigned: generate key: " + err.Error())
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic("tlsboot: GenSelfSigned: generate serial: " + err.Error())
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "waiveo-relay",
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		panic("tlsboot: GenSelfSigned: create certificate: " + err.Error())
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic("tlsboot: GenSelfSigned: marshal key: " + err.Error())
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM
}

// SPKIFromCertDER parses der as an X.509 certificate and returns its
// SubjectPublicKeyInfo (SPKI) — the DER-encoded public-key document the
// certificate certifies (PLY-052: "the SubjectPublicKeyInfo its pem
// certifies — not the surrounding certificate's other fields"), exposed by
// the stdlib as the parsed certificate's RawSubjectPublicKeyInfo field.
//
// Returns an error, never panics, if der does not parse as a certificate —
// e.g. a truncated or corrupted fetch.
func SPKIFromCertDER(der []byte) ([]byte, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return cert.RawSubjectPublicKeyInfo, nil
}

// Commitment computes the PLY-052/REL-126 fingerprint commitment: a
// truncated SHA-256 digest over the ordered concatenation of one or more
// SubjectPublicKeyInfo (SPKI) byte strings, in `trust_anchors` array order —
// the first commitmentBytes (16, >=10 the PLY-052 floor of >=80 bits) bytes
// of sha256(spkis[0] || spkis[1] || ...). It is deterministic over a fixed,
// ordered set of SPKIs; reordering the same SPKIs produces a different
// commitment (order is part of the committed value, per PLY-052's "array
// order").
//
// Commitment does not validate or bounds-check its arguments beyond hashing
// them; callers wanting fail-closed behavior on malformed or absent input
// should use VerifyCommitment, which does.
//
// This is the value a pairing code carries. It MUST be computed by the
// relay over its own real trust-anchor public key(s) and delivered to the
// player only via the out-of-band pairing code — never over the same
// (untrusted) connection the certificate itself was fetched on.
func Commitment(spkis ...[]byte) []byte {
	h := sha256.New()
	for _, spki := range spkis {
		h.Write(spki)
	}
	sum := h.Sum(nil)
	return sum[:commitmentBytes]
}

// VerifyCommitment reports whether commitment matches the Commitment
// recomputed locally from spkis (in the same order the original commitment
// was minted over), using a constant-time comparison so the check leaks no
// timing signal about how much of the commitment matched.
//
// Safe (returns false, never panics) when spkis is empty, when any element
// of spkis is empty, or on any length mismatch between the recomputed and
// supplied commitment — malformed or absent input never verifies.
//
// Critically, this recomputes the commitment from spkis (the SPKI(s) of the
// certificate(s) the player actually fetched over the network) and compares
// it only against the OOB-delivered commitment argument — it never derives
// an expected value from spkis and trusts it, and it never accepts a
// caller-supplied "commitment" that originated from the same connection as
// spkis. Callers MUST source commitment from the out-of-band pairing code,
// never from the paired connection, or this check is a no-op against a MITM
// (PLY-056): commitment here is always the caller-supplied, independently
// out-of-band-delivered value, never anything re-derived from wire material.
func VerifyCommitment(commitment []byte, spkis ...[]byte) bool {
	if len(spkis) == 0 {
		return false
	}
	for _, spki := range spkis {
		if len(spki) == 0 {
			return false
		}
	}
	want := Commitment(spkis...)
	return subtle.ConstantTimeCompare(want, commitment) == 1
}

// CommitmentForCertDER is the single-certificate convenience form of
// Commitment: it extracts der's SPKI (SPKIFromCertDER) and computes the
// commitment over just that one SPKI. This is the common case — the feeder,
// relay, and virtual player each mint or check a commitment for their own
// single bootstrap certificate — and the entry point most first-photon
// callers should use instead of extracting the SPKI themselves.
//
// Returns an error, never panics, if der does not parse as a certificate.
func CommitmentForCertDER(der []byte) ([]byte, error) {
	spki, err := SPKIFromCertDER(der)
	if err != nil {
		return nil, err
	}
	return Commitment(spki), nil
}

// VerifyCommitmentForCertDER is the single-certificate convenience form of
// VerifyCommitment: it extracts der's SPKI (SPKIFromCertDER) and verifies
// commitment against just that one SPKI.
//
// Returns (false, error), never panics, if der does not parse as a
// certificate. See VerifyCommitment's doc for the full MITM-defense
// contract; it applies identically here.
func VerifyCommitmentForCertDER(der, commitment []byte) (bool, error) {
	spki, err := SPKIFromCertDER(der)
	if err != nil {
		return false, err
	}
	return VerifyCommitment(commitment, spki), nil
}
