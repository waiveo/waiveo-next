package clocktrust

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

// makeLeaf mints a self-signed ed25519 leaf certificate with the given
// validity window and returns the parsed certificate, its raw
// SubjectPublicKeyInfo (the REL-137 pin material), and a tls.Certificate for
// serving it.
func makeLeaf(t *testing.T, notBefore, notAfter time.Time) (*x509.Certificate, []byte, tls.Certificate) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "app-peer"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     []string{"127.0.0.1"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	tlsCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}
	return leaf, leaf.RawSubjectPublicKeyInfo, tlsCert
}

// REL-136 security property #1: cold-boot connects on the pin only. An
// untrusted-clock relay whose local clock reads a few seconds after the epoch
// completes validation of an app peer cert whose real window is decades later —
// temporal validation is deferred, identity rests on the pin.
func TestVerifyAppPeerCert_ColdBoot_ConnectsOnPinOnly(t *testing.T) {
	// Corpus REL-136 shape: cert window years in the future relative to a
	// cold-boot clock a few seconds after the epoch.
	notBefore := time.UnixMilli(1719792000000) // 2024-07
	notAfter := time.UnixMilli(1782950400000)  // 2026-07
	leaf, spki, _ := makeLeaf(t, notBefore, notAfter)

	coldBoot := time.UnixMilli(5000) // REL-136 relay_local_clock_reading

	got, err := VerifyAppPeerCert(leaf, spki, false /* clock untrusted */, coldBoot, time.Duration(DefaultBoundedGraceMs)*time.Millisecond)
	if err != nil {
		t.Fatalf("cold-boot connect with matching pin rejected: %v (a clock-less relay MUST still complete the handshake, REL-136)", err)
	}
	if got != CertSkewTolerantDeferred {
		t.Errorf("peer_cert_validation = %q, want %q (untrusted clock defers temporal validation)", got, CertSkewTolerantDeferred)
	}
}

// REL-137 security property #2: wrong key rejected regardless of clock. A leaf
// whose SPKI is not the pin is refused whether the relay's clock is trusted or
// untrusted, and even when its temporal window is perfectly valid.
func TestVerifyAppPeerCert_WrongKey_RejectedRegardlessOfClock(t *testing.T) {
	now := time.Now()
	leaf, _, _ := makeLeaf(t, now.Add(-time.Hour), now.Add(time.Hour))
	_, wrongSPKI, _ := makeLeaf(t, now.Add(-time.Hour), now.Add(time.Hour)) // a different key's pin

	for _, clockTrusted := range []bool{false, true} {
		got, err := VerifyAppPeerCert(leaf, wrongSPKI, clockTrusted, now, time.Minute)
		if !errors.Is(err, ErrAppPeerKeyMismatch) {
			t.Errorf("clockTrusted=%v: wrong-key leaf accepted (got %q, err=%v), want ErrAppPeerKeyMismatch — a rotated/foreign key MUST be rejected regardless of clock (REL-137)", clockTrusted, got, err)
		}
	}
}

// REL-137: the pin is checked BEFORE any temporal reasoning, so a wrong key
// with an expired window still fails on the key, never silently passing
// because temporal validation was deferred.
func TestVerifyAppPeerCert_WrongKey_ExpiredWindow_UntrustedClock(t *testing.T) {
	epoch := time.UnixMilli(5000)
	leaf, _, _ := makeLeaf(t, time.Now(), time.Now().Add(time.Hour))
	_, wrongSPKI, _ := makeLeaf(t, time.Now(), time.Now().Add(time.Hour))

	if _, err := VerifyAppPeerCert(leaf, wrongSPKI, false, epoch, time.Minute); !errors.Is(err, ErrAppPeerKeyMismatch) {
		t.Errorf("wrong key with deferred temporal validation was not rejected on the pin: err=%v", err)
	}
}

// REL-136 security property #3: skew-tolerant temporal validation on a trusted
// clock. A cert just past not_after is accepted within the skew grace, and
// rejected once past the grace.
func TestVerifyAppPeerCert_TrustedClock_SkewTolerant(t *testing.T) {
	base := time.Now()
	notAfter := base.Add(-2 * time.Minute) // expired 2m ago
	leaf, spki, _ := makeLeaf(t, base.Add(-time.Hour), notAfter)

	// Within a 5m skew grace -> accepted despite the clock reading past not_after.
	got, err := VerifyAppPeerCert(leaf, spki, true, base, 5*time.Minute)
	if err != nil {
		t.Fatalf("cert 2m past not_after within 5m skew grace rejected: %v (REL-136 bounded skew)", err)
	}
	if got != CertValidatedTemporal {
		t.Errorf("peer_cert_validation = %q, want %q (trusted clock validates the window)", got, CertValidatedTemporal)
	}

	// Past the skew grace -> rejected.
	if _, err := VerifyAppPeerCert(leaf, spki, true, base, 30*time.Second); !errors.Is(err, ErrAppPeerCertOutsideWindow) {
		t.Errorf("cert 2m past not_after with only 30s skew grace accepted, want ErrAppPeerCertOutsideWindow (err=%v)", err)
	}
}

// REL-136 security property #4 is proved in hint_test.go (a hint adjusts the
// runtime clock, never the persisted floor). This test proves the two checks
// compose: a matching pin + not-yet-valid window on a TRUSTED clock is refused
// on time, while the SAME cert on an untrusted clock connects (deferred).
func TestVerifyAppPeerCert_NotYetValid_TrustedVsUntrusted(t *testing.T) {
	base := time.Now()
	leaf, spki, _ := makeLeaf(t, base.Add(time.Hour), base.Add(2*time.Hour)) // valid an hour from now

	if _, err := VerifyAppPeerCert(leaf, spki, true, base, time.Minute); !errors.Is(err, ErrAppPeerCertOutsideWindow) {
		t.Errorf("not-yet-valid cert accepted on a trusted clock, want ErrAppPeerCertOutsideWindow (err=%v)", err)
	}
	if got, err := VerifyAppPeerCert(leaf, spki, false, base, time.Minute); err != nil || got != CertSkewTolerantDeferred {
		t.Errorf("not-yet-valid cert on an untrusted clock: got (%q, %v), want (skew_tolerant_deferred, nil) — deferred, REL-136", got, err)
	}
}

func TestVerifyAppPeerCert_NoCert(t *testing.T) {
	if _, err := VerifyAppPeerCert(nil, []byte("x"), false, time.Now(), time.Minute); !errors.Is(err, ErrNoPeerCertificate) {
		t.Errorf("nil leaf: err=%v, want ErrNoPeerCertificate", err)
	}
}

// TestAppPeerTLSConfig_RealHandshake drives REL-136/137 over a REAL TLS
// handshake: an untrusted-clock relay dials a live app-peer listener and the
// pinned, skew-tolerant/deferred config either completes the handshake (pin
// matches) or fails it (pin does not) — Go's own chain validation never runs.
func TestAppPeerTLSConfig_RealHandshake(t *testing.T) {
	// App peer serves a cert whose window is entirely in the future, so a naive
	// temporal check against a cold-boot clock would fail outright.
	serverLeaf, spki, serverCert := makeLeaf(t, time.Now().Add(24*time.Hour), time.Now().Add(48*time.Hour))
	_ = serverLeaf

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// Force the handshake, then close.
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}(conn)
		}
	}()

	coldBoot := time.UnixMilli(5000)
	grace := time.Duration(DefaultBoundedGraceMs) * time.Millisecond

	// Matching pin + untrusted clock -> handshake completes (deferred temporal).
	okConn, err := tls.Dial("tcp", ln.Addr().String(), AppPeerTLSConfig(spki, false, coldBoot, grace))
	if err != nil {
		t.Fatalf("pinned untrusted-clock dial against a future-dated cert failed: %v (REL-136 deferred temporal validation must complete the handshake)", err)
	}
	// The pin held over a REAL handshake against the app peer's real leaf.
	if got := okConn.ConnectionState().PeerCertificates[0].RawSubjectPublicKeyInfo; subtleUnequal(got, spki) {
		t.Errorf("handshake peer SPKI differs from the pin it was validated against")
	}
	_ = okConn.Close()

	// Wrong pin -> handshake refused, regardless of clock.
	_, wrongSPKI, _ := makeLeaf(t, time.Now(), time.Now().Add(time.Hour))
	if badConn, err := tls.Dial("tcp", ln.Addr().String(), AppPeerTLSConfig(wrongSPKI, false, coldBoot, grace)); err == nil {
		_ = badConn.Close()
		t.Errorf("handshake completed against a wrong-pin app peer, want failure (REL-137 key pin)")
	}
}

func subtleUnequal(a, b []byte) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}
