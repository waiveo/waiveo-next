package clocktrust

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"
)

// DefaultBoundedGraceMs is the bounded skew-grace window, in milliseconds,
// this package applies both to a clock.hint's not_after bound (REL-133) and to
// the connect-time certificate temporal window (REL-136). REL-133's draft-note
// proposes the two be the same window; keeping a single constant makes that
// concrete. 5 minutes matches the REL-133 corpus fixture's bounded_grace_ms.
const DefaultBoundedGraceMs int64 = 5 * 60 * 1000

// CertValidation names the outcome of REL-136 temporal validation of the app
// peer's server certificate. It is the observable `peer_cert_validation` datum
// the relay/1 corpus's REL-136 case declares.
type CertValidation string

const (
	// CertSkewTolerantDeferred means the relay's own clock was untrusted, so
	// temporal validation of the app peer's certificate window was deferred
	// (REL-136): identity rested entirely on the enrollment-anchored key pin
	// (REL-137), never the certificate's notBefore/notAfter.
	CertSkewTolerantDeferred CertValidation = "skew_tolerant_deferred"
	// CertValidatedTemporal means the relay's clock was trusted, so the app
	// peer certificate's temporal window WAS checked — with a bounded skew
	// grace applied to its notBefore/notAfter (REL-136).
	CertValidatedTemporal CertValidation = "validated_temporal"
)

var (
	// ErrAppPeerKeyMismatch is REL-137's key-pin failure: the app peer's
	// presented leaf SubjectPublicKeyInfo did not match the enrollment-anchored
	// pin key-for-key. It is returned regardless of the relay's clock state, so
	// a wrong key is rejected whether or not the relay can validate time.
	ErrAppPeerKeyMismatch = errors.New("clocktrust: app peer certificate SubjectPublicKeyInfo does not match the enrollment-anchored pin (REL-137)")
	// ErrAppPeerCertOutsideWindow is REL-136's temporal failure on a TRUSTED
	// clock: the app peer certificate is outside its notBefore/notAfter window
	// even after the bounded skew grace. It never fires while the clock is
	// untrusted (that path defers temporal validation entirely).
	ErrAppPeerCertOutsideWindow = errors.New("clocktrust: app peer certificate outside its validity window plus bounded skew grace (REL-136)")
	// ErrNoPeerCertificate is returned when the TLS peer presented no
	// certificate at all — there is nothing to pin or temporally validate.
	ErrNoPeerCertificate = errors.New("clocktrust: TLS peer presented no certificate")
)

// VerifyAppPeerCert performs REL-136/137 validation of the app peer's server
// certificate for a relay opening the mutually authenticated connection
// (REL-003):
//
//   - REL-137 KEY PIN, FIRST and CONSTANT-TIME: the app peer's leaf
//     SubjectPublicKeyInfo is compared, key-for-key, against anchoredSPKI —
//     the enrollment-anchored `trust_pin` (REL-011) — via
//     crypto/subtle.ConstantTimeCompare. This is never chain validation: a
//     leaf that has rotated off the pinned key, even one still chaining to the
//     same authority, does not satisfy the pin. The pin is checked BEFORE any
//     temporal reasoning, so a wrong key is rejected regardless of the relay's
//     clock state.
//   - REL-136 SKEW-TOLERANT / DEFERRED TEMPORAL VALIDATION: when the relay's
//     own clock is untrusted (clockTrusted=false) — including at cold boot
//     before any floor is persisted — temporal validation of the certificate
//     window is DEFERRED entirely; identity rests on the pin alone and the
//     handshake completes. When the clock is trusted, the notBefore/notAfter
//     window IS checked, with a bounded skewGrace applied to each edge.
//
// It returns the temporal outcome for the caller to report, or a typed error
// that MUST fail the handshake.
func VerifyAppPeerCert(peerLeaf *x509.Certificate, anchoredSPKI []byte, clockTrusted bool, now time.Time, skewGrace time.Duration) (CertValidation, error) {
	if peerLeaf == nil {
		return "", ErrNoPeerCertificate
	}
	// REL-137: constant-time, key-for-key SPKI pin. ConstantTimeCompare returns
	// 0 when the lengths differ, so a truncated/padded SPKI never matches.
	if subtle.ConstantTimeCompare(peerLeaf.RawSubjectPublicKeyInfo, anchoredSPKI) != 1 {
		return "", ErrAppPeerKeyMismatch
	}

	if !clockTrusted {
		// REL-136: untrusted clock -> defer temporal validation; the pin above
		// is the whole of the identity decision.
		return CertSkewTolerantDeferred, nil
	}

	// REL-136: trusted clock -> bounded-skew temporal validation.
	if now.Before(peerLeaf.NotBefore.Add(-skewGrace)) || now.After(peerLeaf.NotAfter.Add(skewGrace)) {
		return "", ErrAppPeerCertOutsideWindow
	}
	return CertValidatedTemporal, nil
}

// VerifyPeerCertificate builds a crypto/tls VerifyPeerCertificate callback that
// applies VerifyAppPeerCert (REL-136/137) to the leaf the TLS peer presents.
// Used with tls.Config.InsecureSkipVerify=true (see AppPeerTLSConfig), it is
// the ONLY certificate check on the connection: Go's default chain+temporal
// validation is deliberately disabled, because REL-137 pins the leaf key
// itself, never a chain.
func VerifyPeerCertificate(anchoredSPKI []byte, clockTrusted bool, now time.Time, skewGrace time.Duration) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrNoPeerCertificate
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		_, err = VerifyAppPeerCert(leaf, anchoredSPKI, clockTrusted, now, skewGrace)
		return err
	}
}

// AppPeerTLSConfig returns the tls.Config a relay dials its app peer with under
// REL-136/137: chain validation is turned off (InsecureSkipVerify) and replaced
// by the enrollment-anchored leaf-SPKI pin + skew-tolerant/deferred temporal
// check in VerifyPeerCertificate. This is what lets an untrusted-clock relay —
// cold boot included — complete the handshake against a certificate whose
// temporal window its own clock cannot yet validate, while still refusing a
// leaf whose key is not the pinned one.
func AppPeerTLSConfig(anchoredSPKI []byte, clockTrusted bool, now time.Time, skewGrace time.Duration) *tls.Config {
	return &tls.Config{
		//nolint:gosec // G402: REL-137 pins the app peer's leaf SubjectPublicKeyInfo key-for-key in VerifyPeerCertificate; Go's chain+temporal validation is intentionally NOT used (never chain validation).
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: VerifyPeerCertificate(anchoredSPKI, clockTrusted, now, skewGrace),
		MinVersion:            tls.VersionTLS12,
	}
}
