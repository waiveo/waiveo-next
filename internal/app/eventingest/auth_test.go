package eventingest

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/eventingest/ingesttest"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// auth_test.go drives the telemetry intake door: who may push a telemetry.push
// batch into the platform's event log, and what happens to everyone else.
//
// The route reaches the same event log /events/v1 streams from, so an
// unauthenticated writer is an unauthenticated way to put arbitrary records in
// front of every subscriber. It is authenticated as exactly what its one caller
// is — an enrolled relay presenting the mTLS client certificate this feeder
// issued it (relay/1 REL-003/041), checked against the enrollment registry's
// revocation record on every request (REL-016).
//
// Every refusal below is asserted to leave the LOG UNTOUCHED, not merely to
// return a status: a 403 that had already appended is not a refusal.

func problemOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("a refusal must be an api/1 Problem document (API-010): %v (body %s)", err, rec.Body.String())
	}
	return p
}

// TestIngest_PushWithoutAClientCertificateIsRefused: no certificate, no push.
// This is the regression guard on the route's previous posture — it accepted an
// anonymous POST from anything that could reach the port.
func TestIngest_PushWithoutAClientCertificateIsRefused(t *testing.T) {
	log := events.NewEventLog(0)
	h := newTestIngest(t, log)

	body, err := json.Marshal(pushBatch(autoEntry(1, validAutomationRunPayload())))
	if err != nil {
		t.Fatalf("marshaling push batch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/telemetry/v1/push", bytes.NewReader(body))
	// Deliberately NOT presented: req.TLS stays nil, exactly as it is for a
	// plaintext or certificate-free connection.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a push carrying no client certificate must be refused 401; got %d body=%s", rec.Code, rec.Body.String())
	}
	if code := problemOf(t, rec)["code"]; code != "AUTH_REQUIRED" {
		t.Fatalf("Problem code = %v, want AUTH_REQUIRED", code)
	}
	if got := log.After(""); len(got) != 0 {
		t.Fatalf("a refused push MUST NOT append anything to the event log; got %d record(s)", len(got))
	}
}

// TestIngest_UnverifiedCertificateIsRefused: a certificate the LISTENER did not
// verify is no certificate at all. The handler requires VerifiedChains, so a
// listener wired without the enrollment CA pool cannot silently downgrade this
// check to "any self-signed leaf will do".
func TestIngest_UnverifiedCertificateIsRefused(t *testing.T) {
	log := events.NewEventLog(0)
	h := newTestIngest(t, log)

	req := pushRequest(t, pushBatch(autoEntry(1, validAutomationRunPayload())))
	// Same leaf, but presented as an UNVERIFIED peer certificate.
	req.TLS = &tls.ConnectionState{PeerCertificates: req.TLS.PeerCertificates}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a presented-but-unverified certificate must be refused 401; got %d", rec.Code)
	}
	if got := log.After(""); len(got) != 0 {
		t.Fatalf("a refused push MUST NOT append anything; got %d record(s)", len(got))
	}
}

// TestIngest_UnenrolledOrRevokedRelayIsRefused: a verified certificate is not
// enough — the identity must be one this feeder enrolled (REL-041) and its
// serial must not be revoked (REL-016). Both are the authorizer's answer, and
// both must refuse before the body is read.
func TestIngest_UnenrolledOrRevokedRelayIsRefused(t *testing.T) {
	stranger, err := ingesttest.NewRelay("01J8Z3K4N5P6Q7R8S9T0V1W2ZS")
	if err != nil {
		t.Fatalf("ingesttest.NewRelay: %v", err)
	}

	for name, authorize := range map[string]RelayAuthorizer{
		// The fixture relay's own authorizer refuses every OTHER identity, which
		// is the "never enrolled" case.
		"unenrolled identity": testRelay().Authorizer(),
		// A registry that recognises the relay but reports its serial revoked.
		"revoked serial": func(relayID, serial string) bool { return false },
		// A handler wired with no authorizer at all fails closed.
		"no authorizer configured": nil,
	} {
		log := events.NewEventLog(0)
		h := New(log, siteScope, seqIDs(), testWallMs, authorize, nil)

		req := pushRequest(t, pushBatch(autoEntry(1, validAutomationRunPayload())))
		if name == "unenrolled identity" {
			stranger.Present(req)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: must be refused 403; got %d body=%s", name, rec.Code, rec.Body.String())
		}
		if code := problemOf(t, rec)["code"]; code != "FORBIDDEN" {
			t.Fatalf("%s: Problem code = %v, want FORBIDDEN", name, code)
		}
		if got := log.After(""); len(got) != 0 {
			t.Fatalf("%s: a refused push MUST NOT append anything; got %d record(s)", name, len(got))
		}
	}
}

// TestIngest_RevokingAServialStopsFurtherPushes: revocation takes effect on the
// NEXT request, not merely at the next enrollment — the authorizer is consulted
// per push (REL-016's "at every connection attempt", applied to a route where
// every request is the connection).
func TestIngest_RevokingASerialStopsFurtherPushes(t *testing.T) {
	relay := testRelay()
	revoked := false
	log := events.NewEventLog(0)
	h := New(log, siteScope, seqIDs(), testWallMs, func(relayID, serial string) bool {
		return !revoked && relayID == relay.RelayID && serial == relay.Serial
	}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pushRequest(t, pushBatch(autoEntry(1, validAutomationRunPayload()))))
	if rec.Code != http.StatusOK {
		t.Fatalf("the enrolled relay's first push must be accepted; got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := log.After(""); len(got) != 1 {
		t.Fatalf("the accepted push must append exactly one record; got %d", len(got))
	}

	revoked = true
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, pushRequest(t, pushBatch(autoEntry(2, validAutomationRunPayload()))))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a push presenting a revoked serial must be refused 403 (REL-016); got %d", rec.Code)
	}
	if got := log.After(""); len(got) != 1 {
		t.Fatalf("the refused push MUST NOT append; log grew to %d record(s)", len(got))
	}
}

// TestIngest_OverARealMutualTLSListener is the end-to-end proof that the
// LISTENER configuration a deployed feeder uses actually produces what this
// handler requires — verified chains for a relay's enrollment-issued leaf, and
// nothing at all for a client that presents none.
//
// Everything above stamps a *tls.ConnectionState onto a synthetic request, which
// proves the handler's logic but takes the listener's behavior on trust. This
// case takes nothing on trust: a real TLS server with VerifyClientCertIfGiven
// and the enrollment CA pool, and two real clients.
func TestIngest_OverARealMutualTLSListener(t *testing.T) {
	relay := testRelay()
	log := events.NewEventLog(0)

	srv := httptest.NewUnstartedServer(New(log, siteScope, seqIDs(), testWallMs, relay.Authorizer(), nil))
	srv.TLS = relay.ServerTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13})
	srv.StartTLS()
	defer srv.Close()

	serverCAs := x509.NewCertPool()
	serverCAs.AddCert(srv.Certificate())
	body, err := json.Marshal(pushBatch(autoEntry(1, validAutomationRunPayload())))
	if err != nil {
		t.Fatalf("marshaling push batch: %v", err)
	}

	// A client presenting the relay's enrollment-issued leaf: accepted, and the
	// record lands in the log.
	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: relay.ClientTLSConfig(serverCAs)}}
	resp, err := withCert.Post(srv.URL+"/telemetry/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("mutually authenticated push: %v", err)
	}
	var ack telemetry.Ack
	decodeErr := json.NewDecoder(resp.Body).Decode(&ack)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a push over a verified client certificate must be accepted; got %d", resp.StatusCode)
	}
	if decodeErr != nil {
		t.Fatalf("the ack must be JSON: %v", decodeErr)
	}
	if ack.AckThroughSeq != 1 {
		t.Fatalf("ack_through_seq = %d, want 1 (REL-092)", ack.AckThroughSeq)
	}
	if got := log.After(""); len(got) != 1 {
		t.Fatalf("the accepted push must append exactly one record; got %d", len(got))
	}

	// The same server, a client presenting NO certificate: refused, and the log
	// does not grow. VerifyClientCertIfGiven lets the handshake complete, so the
	// refusal is genuinely the handler's, at the HTTP layer, in the api/1
	// Problem shape.
	withoutCert := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: serverCAs, MinVersion: tls.VersionTLS13},
	}}
	resp, err = withoutCert.Post(srv.URL+"/telemetry/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("certificate-free push: %v", err)
	}
	var problem map[string]any
	decodeErr = json.NewDecoder(resp.Body).Decode(&problem)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a push over a certificate-free connection must be refused 401; got %d", resp.StatusCode)
	}
	if decodeErr != nil {
		t.Fatalf("the refusal must be an api/1 Problem document: %v", decodeErr)
	}
	if problem["code"] != "AUTH_REQUIRED" {
		t.Fatalf("Problem code = %v, want AUTH_REQUIRED", problem["code"])
	}
	if got := log.After(""); len(got) != 1 {
		t.Fatalf("the refused push MUST NOT append; log grew to %d record(s)", len(got))
	}
}
