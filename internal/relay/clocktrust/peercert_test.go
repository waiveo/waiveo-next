package clocktrust

import (
	"errors"
	"testing"
	"time"
)

// Corpus REL-136-valid-coldboot-skew-tolerant-connect.json exact input values:
// the app peer's server cert window and the relay's wrong cold-boot clock.
const (
	rel136CertNotBeforeMs = 1719792000000 // app_peer_server_cert.not_before (2024-07)
	rel136CertNotAfterMs  = 1782950400000 // app_peer_server_cert.not_after  (2026-07)
	rel136ColdBootMs      = 5000          // relay_local_clock_reading (a few seconds after the epoch)
)

// REL-136: a cold-boot relay with no persisted floor and a wrong local clock
// (a few seconds after the epoch) MUST still complete the connection — temporal
// validation of the app peer's cert is DEFERRED while the relay's clock is
// untrusted, identity resting entirely on the enrollment-anchored SPKI pin.
// This wires the REL-136 corpus's exact cert window + cold-boot reading; the
// corpus expects peer_cert_validation=skew_tolerant_deferred, handshake
// completed (here: a nil error, the connection is allowed to proceed).
func TestValidatePeerCert_REL136_ColdBootDefersOnPin(t *testing.T) {
	nb := time.UnixMilli(rel136CertNotBeforeMs)
	na := time.UnixMilli(rel136CertNotAfterMs)
	leaf, spki, _ := makeLeaf(t, nb, na)

	// clockTrusted=false, relayClockMs=cold-boot reading decades before notBefore:
	// a naive temporal check would fail outright, but temporal is deferred.
	if err := ValidatePeerCert(leaf.Raw, spki, rel136ColdBootMs, DefaultBoundedGraceMs, false); err != nil {
		t.Fatalf("cold-boot untrusted-clock connect with matching pin rejected: %v (REL-136: a clock-less relay MUST still complete the handshake, temporal deferred)", err)
	}
}

// REL-137: a leaf whose SubjectPublicKeyInfo is not the enrollment-anchored pin
// MUST be rejected key-for-key, regardless of the relay's clock state and even
// when the cert's own temporal window is perfectly valid — never chain
// validation. The pin is checked BEFORE any temporal reasoning, so a deferred
// (untrusted-clock) path never lets a wrong key through.
func TestValidatePeerCert_REL137_WrongSPKIRejectedRegardlessOfClock(t *testing.T) {
	now := time.Now()
	leaf, _, _ := makeLeaf(t, now.Add(-time.Hour), now.Add(time.Hour))
	_, wrongSPKI, _ := makeLeaf(t, now.Add(-time.Hour), now.Add(time.Hour)) // a different key's pin

	for _, clockTrusted := range []bool{false, true} {
		err := ValidatePeerCert(leaf.Raw, wrongSPKI, now.UnixMilli(), DefaultBoundedGraceMs, clockTrusted)
		if !errors.Is(err, ErrAppPeerKeyMismatch) {
			t.Errorf("clockTrusted=%v: wrong-key leaf accepted (err=%v), want ErrAppPeerKeyMismatch — a rotated/foreign key MUST be rejected key-for-key regardless of clock (REL-137)", clockTrusted, err)
		}
	}
}

// REL-136: on a TRUSTED clock the temporal window IS checked, with a bounded
// skew grace. A cert past not_after within the grace is accepted; once beyond
// not_after+grace it is rejected.
func TestValidatePeerCert_REL136_TrustedClockSkewBounded(t *testing.T) {
	base := time.Now()
	notAfter := base.Add(-2 * time.Minute) // expired 2 minutes ago
	leaf, spki, _ := makeLeaf(t, base.Add(-time.Hour), notAfter)

	graceWide := int64(5 * 60 * 1000) // 5m — covers the 2m overshoot
	graceNarrow := int64(30 * 1000)   // 30s — does not

	if err := ValidatePeerCert(leaf.Raw, spki, base.UnixMilli(), graceWide, true); err != nil {
		t.Fatalf("cert 2m past not_after within 5m skew grace rejected on a trusted clock: %v (REL-136 bounded skew)", err)
	}
	if err := ValidatePeerCert(leaf.Raw, spki, base.UnixMilli(), graceNarrow, true); !errors.Is(err, ErrAppPeerCertOutsideWindow) {
		t.Errorf("cert 2m past not_after with only 30s skew grace accepted on a trusted clock (err=%v), want ErrAppPeerCertOutsideWindow (REL-136)", err)
	}
}

// REL-136 composition: the SAME not-yet-valid cert is refused on time when the
// clock is trusted, but connects (deferred) when the clock is untrusted.
func TestValidatePeerCert_REL136_NotYetValidTrustedVsUntrusted(t *testing.T) {
	base := time.Now()
	leaf, spki, _ := makeLeaf(t, base.Add(time.Hour), base.Add(2*time.Hour)) // valid an hour from now

	if err := ValidatePeerCert(leaf.Raw, spki, base.UnixMilli(), int64(60*1000), true); !errors.Is(err, ErrAppPeerCertOutsideWindow) {
		t.Errorf("not-yet-valid cert accepted on a trusted clock (err=%v), want ErrAppPeerCertOutsideWindow (REL-136)", err)
	}
	if err := ValidatePeerCert(leaf.Raw, spki, base.UnixMilli(), int64(60*1000), false); err != nil {
		t.Errorf("not-yet-valid cert on an untrusted clock rejected (err=%v), want nil — temporal deferred (REL-136)", err)
	}
}

// An empty / absent peer certificate has nothing to pin or temporally validate.
func TestValidatePeerCert_NoCert(t *testing.T) {
	if err := ValidatePeerCert(nil, []byte("x"), 0, DefaultBoundedGraceMs, false); !errors.Is(err, ErrNoPeerCertificate) {
		t.Errorf("nil DER: err=%v, want ErrNoPeerCertificate", err)
	}
}

// A corrupted / non-certificate DER blob is a parse error, never a silent pass.
func TestValidatePeerCert_MalformedDER(t *testing.T) {
	if err := ValidatePeerCert([]byte{0x30, 0x01, 0xff}, []byte("x"), 0, DefaultBoundedGraceMs, false); err == nil {
		t.Error("malformed DER validated with a nil error, want a parse error")
	}
}
