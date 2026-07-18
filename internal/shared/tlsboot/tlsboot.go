// Package tlsboot generates the relay's self-signed TLS bootstrap
// certificate and implements the out-of-band pairing-pin commitment scheme
// that closes the pairing-MITM window (player/1 PLY-052/053/054/056).
//
// The pairing flow: a screen fetches the relay's TLS cert over a
// verification-disabled connection (there is no CA to trust yet — this is
// the relay's own bootstrap cert). A network attacker positioned during
// pairing could substitute their own cert for the real one. The defense is
// an out-of-band channel (the pairing code, shown on the relay and typed or
// scanned on the screen) that carries FingerprintCommitment(realCertPEM).
// The player recomputes the commitment locally from the cert it actually
// fetched and compares it against the OOB value with VerifyCommitment. The
// commitment is NEVER retransmitted or re-derived from anything the wire
// connection supplies — a self-attesting authenticator delivered in-band
// would let a MITM simply forward its own commitment and defeat the check
// (the PLY-056 ban). See tlsboot_test.go's
// TestVerifyCommitmentRejectsSubstitutedCert for the concrete MITM case:
// committing to cert A and then verifying against a distinct cert B must
// fail.
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
// authenticity is instead established out-of-band via FingerprintCommitment
// carried in the pairing code.
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

// FingerprintCommitment returns a truncated-SHA-256 commitment over certPEM
// (the cert's own encoded bytes): the first commitmentBytes (>=10, the
// PLY-052 floor of >=80 bits) bytes of sha256(certPEM). It is deterministic
// over a fixed cert.
//
// This is the value a pairing code carries. It MUST be computed by the
// relay over its own real cert and delivered to the player only via the
// out-of-band pairing code — never over the same (untrusted) connection the
// cert itself was fetched on.
func FingerprintCommitment(certPEM []byte) []byte {
	sum := sha256.Sum256(certPEM)
	return sum[:commitmentBytes]
}

// VerifyCommitment reports whether commitment matches the
// FingerprintCommitment recomputed locally from certPEM, using a
// constant-time comparison so the check leaks no timing signal about how
// much of the commitment matched.
//
// Critically, this recomputes the commitment from certPEM (the cert the
// player actually fetched over the network) and compares it only against
// the OOB-delivered commitment argument — it never derives an expected
// value from certPEM itself and trusts it, and it never accepts a
// caller-supplied "commitment" that originated from the same connection as
// certPEM. Callers MUST source commitment from the out-of-band pairing
// code, never from the paired connection, or this check is a no-op against
// a MITM (PLY-056).
func VerifyCommitment(certPEM, commitment []byte) bool {
	want := FingerprintCommitment(certPEM)
	return subtle.ConstantTimeCompare(want, commitment) == 1
}
