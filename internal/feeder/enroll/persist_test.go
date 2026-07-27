package enroll

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// newPersistedTestServer builds an enroll.Server exactly like newTestServer,
// except it also enables durable persistence to dir — the shape
// cmd/waiveo-feeder wires in production (EnablePersistence). withConn also
// mounts the persistent-connection server (internal/feeder/relayconn) on the
// same mux — wired to this Server's own RelayEnrollmentKey/IsRevoked, over
// an mTLS listener validating against this Server's own ClientCAPool — so a
// test can run the real challenge → hello → hello-ack handshake against it.
func newPersistedTestServer(t *testing.T, dir string, withConn bool) (*Server, *httptest.Server) {
	t.Helper()
	id := testIdentity(t)

	srv, err := NewServer(id)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.EnablePersistence(dir); err != nil {
		t.Fatalf("EnablePersistence(%q): %v", dir, err)
	}

	mux := http.NewServeMux()
	srv.Register(mux)
	if !withConn {
		ts := httptest.NewTLSServer(apihttp.WithTraceID(mux))
		t.Cleanup(ts.Close)
		return srv, ts
	}

	connSrv := feederrelayconn.New(
		func() (wire.StateSnapshotBody, error) { return wire.StateSnapshotBody{Generation: 1}, nil },
		srv.RelayEnrollmentKey,
		srv.IsRevoked,
		hello.SiteBinding{},
		hello.AppPeerImplementedMinors(1, 1),
		nil,
	)
	mux.Handle("/relay/v1", connSrv.Handler())
	ts := httptest.NewUnstartedServer(apihttp.WithTraceID(mux))
	ts.TLS = &tls.Config{
		// ClientCAPool is read AFTER EnablePersistence, exactly as
		// cmd/waiveo-feeder reads it, so a restarted server keeps verifying
		// leaves the pre-restart CA issued.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  srv.ClientCAPool(),
		MinVersion: tls.VersionTLS13,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return srv, ts
}

// TestEnablePersistenceSurvivesRestart is the persistence-half regression
// test: without EnablePersistence's load-back behavior, a freshly built
// Server (exactly what a restarted feeder process constructs — NewServer
// always starts from an empty relayKeys map and a fresh in-memory CA) has no
// record of a relay this same directory's PRIOR Server instance enrolled.
// RelayEnrollmentKey is exactly what the connection handshake's app-peer
// server (internal/relay/hello) looks a relay's enrollment key up through
// (REL-032) — so an ok=false here is precisely what turns a relay's next
// hello into CHANNEL_BINDING_INVALID.
func TestEnablePersistenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	first, ts := newPersistedTestServer(t, dir, false)
	client := ts.Client()

	claimToken := fetchClaimToken(t, client, ts.URL)
	relayPub, _, csrPEM := generateCSR(t, "test-relay")
	resp, body := postEnroll(t, client, ts.URL, claimToken, csrPEM)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /enroll status = %d, want 200", resp.StatusCode)
	}
	relayID := body.RelayID
	if relayID == "" {
		t.Fatal("enroll response carried an empty relay_id")
	}

	// Sanity: the server that just enrolled it recognizes the key immediately.
	gotPub, ok := first.RelayEnrollmentKey(relayID)
	if !ok || !gotPub.Equal(relayPub) {
		t.Fatalf("first.RelayEnrollmentKey(%q) = (%x, %v), want (%x, true)", relayID, []byte(gotPub), ok, []byte(relayPub))
	}
	ts.Close()

	// Simulate a feeder restart: a brand-new Server, built exactly as
	// NewServer always builds one (empty registry, fresh in-memory CA),
	// pointed at the SAME persist directory. Without this fix it would have
	// no memory of relayID at all.
	second, _ := newPersistedTestServer(t, dir, false)

	gotPub, ok = second.RelayEnrollmentKey(relayID)
	if !ok {
		t.Fatalf("second.RelayEnrollmentKey(%q) ok = false after a simulated restart against the same persist dir %q, want true (the enrollment must survive)", relayID, dir)
	}
	if !gotPub.Equal(relayPub) {
		t.Errorf("second.RelayEnrollmentKey(%q) = %x, want the originally-enrolled key %x", relayID, []byte(gotPub), []byte(relayPub))
	}

	// The issuance record (REL-021/022, Expired-certificate re-enrollment
	// eligibility) must also have survived — not just the hello-verification key.
	serial, issuedPub, present := second.MostRecentSerial(relayID)
	if !present {
		t.Fatalf("second.MostRecentSerial(%q) present = false, want the issuance recorded before the restart", relayID)
	}
	if serial == "" || !issuedPub.Equal(relayPub) {
		t.Errorf("second.MostRecentSerial(%q) = (%q, %x), want a non-empty serial and the enrolled key %x", relayID, serial, []byte(issuedPub), []byte(relayPub))
	}
}

// TestHelloSucceedsAfterFeederRestartWithPersistedEnrollment reproduces the
// field defect end to end at the protocol layer, on the persistent
// transport: a relay enrolls, its connection handshake is accepted, the
// feeder process "restarts" (a fresh Server + fresh /relay/v1 server, same
// persist dir, new httptest listener — nothing about the relay's own
// identity changes), and the SAME relay's next dial MUST still be accepted
// rather than refused CHANNEL_BINDING_INVALID. Without EnablePersistence's
// restore, the second server's RelayKeyLookup has no record of the relay
// (and its fresh CA would not even validate the relay's leaf), so the
// handshake is refused exactly as it was on the box (REL-032).
func TestHelloSucceedsAfterFeederRestartWithPersistedEnrollment(t *testing.T) {
	dir := t.TempDir()

	// --- boot 1: enroll + connection handshake succeeds ---
	_, ts1 := newPersistedTestServer(t, dir, true)

	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := relayenroll.Run(ts1.URL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}

	decl := hello.Declaration{
		ProtocolVersion: "1.0",
		ClockState:      hello.ClockState{State: "untrusted", Source: "cold_boot"},
	}
	c1, err := relayclient.Dial(relayclient.Config{URL: ts1.URL, Store: store, Declaration: decl})
	if err != nil {
		t.Fatalf("first dial (pre-restart): %v", err)
	}
	_ = c1.Close()
	ts1.Close()

	// --- boot 2: simulated feeder restart, same persist dir ---
	//
	// NOTE: the httptest listener presents the SAME fixed leaf across both
	// boots, exactly like a feeder that persists its serving identity
	// (signing.LoadOrCreate), so the relay's enrollment-captured SPKI pin
	// (REL-137) still matches — what breaks without persistence is the
	// SERVER's memory of the relay, which is this test's subject.
	_, ts2 := newPersistedTestServer(t, dir, true)

	c2, err := relayclient.Dial(relayclient.Config{URL: ts2.URL, Store: store, Declaration: decl})
	if err != nil {
		var refused *relayclient.Refusal
		if errors.As(err, &refused) {
			t.Fatalf("handshake after simulated feeder restart was refused (%s): %s — the restarted app peer forgot this relay's enrollment", refused.Code, refused.Message)
		}
		t.Fatalf("dial after simulated feeder restart: %v", err)
	}
	defer c2.Close()
	if c2.HelloAck().NegotiatedVersion == "" {
		t.Error("hello-ack after restart carried no negotiated_version")
	}
}
