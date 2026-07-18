package virtualplayer_test

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	feedersigning "github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/virtualplayer"
)

// quietErrorLog silences net/http.Server's default per-connection error
// logging (os.Stderr) for the in-process test servers below. TestPhotonRejectsMITM
// deliberately drives a client that aborts the TLS handshake on a
// commitment mismatch (PLY-057) — expected, assertion-checked behavior, not
// a real error — and http.Server otherwise logs that as a "TLS handshake
// error: ... bad certificate" line that reads confusingly next to a PASSing
// test.
var quietErrorLog = log.New(io.Discard, "", 0)

// testImage is the tiny fixture image the in-process feeder signs and
// serves — a stand-in for cmd/waiveo-feeder's own placeholderImage, kept
// small and inline so this test has no fixture-file dependency.
func testImage() []byte {
	return []byte("virtualplayer-first-photon-test-image-bytes")
}

// bootTestFeeder boots a real in-process feeder over its own loopback TCP
// listener (not httptest.NewTLSServer): the feeder's content-origin URL
// embedded in its signed snapshot must be a real, later-dialable
// https://host:port a running relay actually pulls from and a player later
// fetches an image from DIRECT (PLY-084) — an httptest fake transport
// wouldn't give that a concrete network address. Returns the feeder's own
// https base URL and the exact image bytes it served (for the test's own
// content-hash assertion, independent of virtualplayer's internal
// verification of the same property).
func bootTestFeeder(t *testing.T) (baseURL string, img []byte) {
	t.Helper()

	id, err := feedersigning.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("feedersigning.LoadOrCreate: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	baseURL = "https://" + lis.Addr().String()

	img = testImage()
	contentStore := origin.New()
	contentStore.Add(img)

	g := grant.Mint()
	snap, err := snapshot.Build(img, baseURL, id, []wire.PairingGrant{g})
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	enrollSrv, err := enroll.NewServer(id, snap)
	if err != nil {
		t.Fatalf("enroll.NewServer: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/content/", contentStore.Handler())
	enrollSrv.Register(mux)

	cert, err := tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM())
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}

	srv := &http.Server{
		Handler:   apihttp.WithTraceID(mux),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		ErrorLog:  quietErrorLog,
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	return baseURL, img
}

// bootTestRelay enrolls a fresh relay (internal/relay/enroll) against the
// feeder at feederBaseURL, pulls + verifies its signed desired state
// (internal/relay/desiredstate), and serves player/1's pairing + program
// surface (internal/relay/playerserver) over its own real loopback TCP+TLS
// listener — mirroring cmd/waiveo-relay/main.go's own boot sequence, minus
// its enrollment retry loop (this test controls ordering directly: the
// feeder is already listening before this is called).
//
// Returns the relay's dial host/port (a formed pairing code's own {host,
// port}, PLY-024), its TLS leaf certificate's raw DER (the SAME bytes
// FormPairingCode's commitment is computed over and this listener actually
// presents — the value a correctly-formed, non-MITM pairing code's local
// PLY-052 check must verify against), the verified Applied desired state
// (its own PairingGrants is what a pairing code's grant_selector resolves
// against), and an *int32 counter of how many requests hit
// /player/v1/pair — TestPhotonRejectsMITM's own assertion that a rejected
// bootstrap fetch never proceeds to redemption.
func bootTestRelay(t *testing.T, feederBaseURL string) (host string, port int, certDER []byte, applied desiredstate.Applied, pairCalls *int32) {
	t.Helper()

	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := relayenroll.Run(feederBaseURL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}

	applied, err = desiredstate.Pull(feederBaseURL, store)
	if err != nil {
		t.Fatalf("desiredstate.Pull: %v", err)
	}

	relayID, ok, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	if !ok {
		t.Fatalf("store.Identity: no identity persisted after enrollment")
	}

	cert, der, err := relayTLSCertificateForTest(relayID.CertPEM, relayID.PrivateKey)
	if err != nil {
		t.Fatalf("relayTLSCertificateForTest: %v", err)
	}
	certDER = der

	pairingSrv, err := playerserver.NewServer(relayID.CertPEM, applied.PairingGrants)
	if err != nil {
		t.Fatalf("playerserver.NewServer: %v", err)
	}
	pairingSrv.SetProgram(applied.ProgramRevision, applied.Priority, applied.Display, []wire.LeaseContent{{
		Type:      "image",
		AssetRef:  applied.Image.AssetRef,
		URL:       applied.Image.URL,
		ExpiresAt: applied.Image.ExpiresAt,
	}}, relayID.PrivateKey)

	mux := http.NewServeMux()
	pairingSrv.Register(mux)

	var calls int32
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/player/v1/pair" {
			atomic.AddInt32(&calls, 1)
		}
		mux.ServeHTTP(w, r)
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &http.Server{
		Handler:   apihttp.WithTraceID(spy),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		ErrorLog:  quietErrorLog,
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	h, p, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatalf("net.SplitHostPort: %v", err)
	}
	port, err = strconv.Atoi(p)
	if err != nil {
		t.Fatalf("strconv.Atoi(%q): %v", p, err)
	}

	return h, port, certDER, applied, &calls
}

// relayTLSCertificateForTest mirrors cmd/waiveo-relay/main.go's own
// unexported relayTLSCertificate: builds a crypto/tls.Certificate (to serve
// player/1 over HTTPS) from the relay's enrollment identity (certPEM, priv),
// returning its raw DER alongside it — the same DER a pairing code's
// commitment (FormPairingCode) is computed over, so the certificate this
// listener actually presents is always the one a formed pairing code
// commits to.
func relayTLSCertificateForTest(certPEM []byte, priv ed25519.PrivateKey) (tls.Certificate, []byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return tls.Certificate{}, nil, fmt.Errorf("relayTLSCertificateForTest: identity cert did not PEM-decode to a CERTIFICATE block")
	}

	return cert, block.Bytes, nil
}

// TestPhoton is the software photon (Wave-1 first-photon Task 12's own
// gate): a real in-process feeder + relay, one minted grant, one formed
// pairing code, and virtualplayer.Photon(code) driven against them end to
// end with zero hardware. The returned bytes' content address
// (signhash.ContentID) must equal the feeder's own served image's content
// address — the full player/1 client thread, proven in software.
func TestPhoton(t *testing.T) {
	feederBaseURL, img := bootTestFeeder(t)
	host, port, certDER, applied, pairCalls := bootTestRelay(t, feederBaseURL)

	if len(applied.PairingGrants) == 0 {
		t.Fatalf("applied desired state carried no pairing_grants")
	}
	grantRecord := applied.PairingGrants[0]

	code, err := playerserver.FormPairingCode(host, port, grantRecord, certDER)
	if err != nil {
		t.Fatalf("playerserver.FormPairingCode: %v", err)
	}

	gotBytes, err := virtualplayer.Photon(code)
	if err != nil {
		t.Fatalf("virtualplayer.Photon(%q): unexpected error: %v", code, err)
	}

	if got, want := signhash.ContentID(gotBytes), signhash.ContentID(img); got != want {
		t.Errorf("Photon returned bytes with content id %q, want the feeder's own served image content id %q", got, want)
	}
	if atomic.LoadInt32(pairCalls) != 1 {
		t.Errorf("/player/v1/pair was called %d time(s), want exactly 1 (one redemption)", atomic.LoadInt32(pairCalls))
	}
}

// TestPhotonRejectsMITM is player/1's PLY-056/057 property made concrete: a
// pairing code whose fingerprint_commitment does NOT match the relay it
// actually dials (simulating a MITM having substituted a different
// certificate, or an attacker having tampered with the pairing code's own
// commitment field) must be rejected at the LOCAL commitment check —
// Photon must return an error and must NEVER reach the relay's
// /player/v1/pair endpoint at all, confirming the fingerprint_commitment
// mismatch is caught before any redemption attempt (and, structurally,
// that fingerprint_commitment itself is never a field this client could put
// on the wire — see virtualplayer.go's own doc).
func TestPhotonRejectsMITM(t *testing.T) {
	feederBaseURL, _ := bootTestFeeder(t)
	host, port, _, applied, pairCalls := bootTestRelay(t, feederBaseURL)

	if len(applied.PairingGrants) == 0 {
		t.Fatalf("applied desired state carried no pairing_grants")
	}
	grantRecord := applied.PairingGrants[0]

	// A DIFFERENT certificate's DER — never presented by the relay this
	// pairing code actually dials — stands in for a MITM's own substituted
	// certificate (or, equivalently, a tampered pairing code): the
	// commitment this code carries was formed over a cert the relay's real
	// listener does not present.
	mitmCertPEM, _ := tlsboot.GenSelfSigned()
	block, _ := pem.Decode(mitmCertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("tlsboot.GenSelfSigned() cert did not PEM-decode to a CERTIFICATE block")
	}
	mitmCertDER := block.Bytes

	code, err := playerserver.FormPairingCode(host, port, grantRecord, mitmCertDER)
	if err != nil {
		t.Fatalf("playerserver.FormPairingCode: %v", err)
	}

	gotBytes, err := virtualplayer.Photon(code)
	if err == nil {
		t.Fatalf("virtualplayer.Photon(%q) succeeded, want a commitment-mismatch error (PLY-057)", code)
	}
	if gotBytes != nil {
		t.Errorf("virtualplayer.Photon returned %d bytes alongside an error, want nil", len(gotBytes))
	}
	if atomic.LoadInt32(pairCalls) != 0 {
		t.Errorf("/player/v1/pair was called %d time(s), want 0 (Photon must never proceed to redemption on a commitment mismatch, PLY-056/057)", atomic.LoadInt32(pairCalls))
	}
}
