package clocktrust

import (
	"crypto/subtle"
	"crypto/x509"

	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
)

// ValidatePeerCert is the DER-facing REL-136/137 app-peer server-certificate
// check a relay applies while opening the mutually authenticated connection
// REL-003 requires. It is the pure, corpus-facing form
// (REL-136-valid-coldboot-skew-tolerant-connect.json /
// REL-133-valid-clock-hint-bounded.json drive it): raw certificate DER +
// enrollment-anchored SubjectPublicKeyInfo pin + the relay's own clock in Unix
// milliseconds, returning nil when the connection MAY proceed and a typed error
// that MUST fail the handshake otherwise. connect.go's VerifyAppPeerCert /
// AppPeerTLSConfig wrap the same decision for the live crypto/tls handshake and
// report the CertValidation outcome; ValidatePeerCert shares this file's error
// sentinels with them.
//
//   - REL-137 KEY PIN, FIRST and CONSTANT-TIME: the peer leaf's
//     SubjectPublicKeyInfo — extracted with tlsboot.SPKIFromCertDER, the single
//     shared SPKI extractor (never re-implemented here) — is compared key-for-key
//     against enrolledPeerSPKI (the REL-011 trust_pin) with
//     crypto/subtle.ConstantTimeCompare. This is never chain validation: a leaf
//     that has rotated off the pinned key, even one still chaining to the same
//     authority, does not satisfy the pin (REL-137). The pin is checked BEFORE
//     any temporal reasoning, so a wrong key is rejected regardless of the
//     relay's clock state — the deferred (untrusted-clock) path never lets a
//     foreign key through.
//   - REL-136 SKEW-TOLERANT / DEFERRED TEMPORAL VALIDATION: when clockTrusted is
//     false — including at cold boot before any floor is persisted (REL-130) —
//     temporal validation of the certificate's notBefore/notAfter window is
//     DEFERRED entirely; identity rests on the pin alone and the connection MAY
//     proceed. When clockTrusted is true, the window IS checked against
//     relayClockMs with a bounded graceMs skew applied to each edge.
//
// The handshake MUST NOT fail solely because an untrusted relay clock cannot
// validate the peer certificate's temporal window (REL-136).
func ValidatePeerCert(peerCertDER []byte, enrolledPeerSPKI []byte, relayClockMs int64, graceMs int64, clockTrusted bool) error {
	if len(peerCertDER) == 0 {
		return ErrNoPeerCertificate
	}

	// REL-137: reuse tlsboot's SPKI extractor — the one place SPKI is pulled
	// from a certificate's DER — rather than re-deriving it here.
	spki, err := tlsboot.SPKIFromCertDER(peerCertDER)
	if err != nil {
		return err
	}
	// REL-137: constant-time, key-for-key pin, BEFORE any temporal reasoning.
	// ConstantTimeCompare returns 0 on a length mismatch, so a truncated or
	// padded SPKI never matches.
	if subtle.ConstantTimeCompare(spki, enrolledPeerSPKI) != 1 {
		return ErrAppPeerKeyMismatch
	}

	if !clockTrusted {
		// REL-136: untrusted clock -> defer temporal validation entirely; the
		// pin above is the whole of the identity decision.
		return nil
	}

	// REL-136: trusted clock -> bounded-skew temporal validation. Parse the DER
	// once more only on this path, for the notBefore/notAfter window.
	cert, err := x509.ParseCertificate(peerCertDER)
	if err != nil {
		return err
	}
	if relayClockMs < cert.NotBefore.UnixMilli()-graceMs || relayClockMs > cert.NotAfter.UnixMilli()+graceMs {
		return ErrAppPeerCertOutsideWindow
	}
	return nil
}
