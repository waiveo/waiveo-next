package enroll

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// newPersistedTestServer builds an enroll.Server exactly like newTestServer,
// except it also enables durable persistence to dir — the shape
// cmd/waiveo-feeder wires in production (EnablePersistence). withHello also
// mounts the connection handshake's app-peer server (internal/relay/hello)
// on the same mux, wired to this Server's own RelayEnrollmentKey, so a test
// can perform a real hello against it.
func newPersistedTestServer(t *testing.T, dir string, withHello bool) (*Server, *httptest.Server) {
	t.Helper()
	id := testIdentity(t)
	img := loadTestImage(t)
	g := grant.Mint()

	snap, err := snapshot.Build(img, "https://origin.example", id, []wire.PairingGrant{g})
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	srv, err := NewServer(id, snap)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.EnablePersistence(dir); err != nil {
		t.Fatalf("EnablePersistence(%q): %v", dir, err)
	}

	mux := http.NewServeMux()
	srv.Register(mux)
	if withHello {
		helloSrv := hello.NewAppPeerServer(srv.RelayEnrollmentKey, hello.SiteBinding{}, hello.AppPeerImplementedMinors(1, 1), nil, nil)
		helloSrv.Register(mux)
	}
	ts := httptest.NewTLSServer(apihttp.WithTraceID(mux))
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
// field defect end to end at the protocol layer: a relay enrolls, its hello
// is accepted, the feeder process "restarts" (a fresh Server + fresh
// hello.AppPeerServer, same persist dir, new httptest listener — nothing
// about the relay's own identity changes), and the SAME relay's next hello
// MUST still be accepted rather than failing CHANNEL_BINDING_INVALID.
// Without EnablePersistence's restore, the second server's RelayKeyLookup
// has no record of the relay and this hello is refused exactly as it was on
// the box (REL-032, HTTP 403).
func TestHelloSucceedsAfterFeederRestartWithPersistedEnrollment(t *testing.T) {
	dir := t.TempDir()

	// --- boot 1: enroll + hello succeeds ---
	_, ts1 := newPersistedTestServer(t, dir, true)

	client := ts1.Client()
	claimToken := fetchClaimToken(t, client, ts1.URL)
	_, relayPriv, csrPEM := generateCSR(t, "test-relay")
	resp, body := postEnroll(t, client, ts1.URL, claimToken, csrPEM)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /enroll status = %d, want 200", resp.StatusCode)
	}
	relayID := body.RelayID

	decl := hello.Declaration{
		ProtocolVersion: "1.0",
		ClockState:      hello.ClockState{State: "untrusted", Source: "cold_boot"},
	}
	if _, err := hello.PerformHello(ts1.URL, relayPriv, relayID, decl); err != nil {
		t.Fatalf("first hello (pre-restart): %v", err)
	}
	ts1.Close()

	// --- boot 2: simulated feeder restart, same persist dir ---
	_, ts2 := newPersistedTestServer(t, dir, true)

	ack, err := hello.PerformHello(ts2.URL, relayPriv, relayID, decl)
	if err != nil {
		var refused *hello.RefusedError
		if errors.As(err, &refused) {
			t.Fatalf("hello after simulated feeder restart was refused (%s): %s — the restarted app peer forgot this relay's enrollment", refused.Code, refused.Title)
		}
		t.Fatalf("hello after simulated feeder restart: %v", err)
	}
	if ack.Body.NegotiatedVersion == "" {
		t.Error("hello-ack after restart carried no negotiated_version")
	}
}
