package eventsse

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/eventingest"
	"github.com/maaxton/waiveo-next/internal/app/eventingest/ingesttest"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// pushRelay is the one minted relay identity the two end-to-end tests in this
// package push telemetry as. The ingest route identifies its caller by the
// enrollment-issued mTLS client certificate (relay/1 REL-003/041) and checks the
// enrollment registry's revocation record (REL-016), so a harness needs a real
// relay identity rather than a way to skip the check — which is exactly what
// keeps these end-to-ends proving the shipped path.
var pushRelay = sync.OnceValue(func() *ingesttest.Relay {
	r, err := ingesttest.NewRelay("01J8Z3K4N5P6Q7R8S9T0V1RELY")
	if err != nil {
		panic("eventsse: mint relay identity: " + err.Error())
	}
	return r
})

// newIngestServer mounts the telemetry ingest over hub on a REAL mutual-TLS
// listener — VerifyClientCertIfGiven against the enrollment CA, exactly as the
// feeder's own listener is configured — and returns it plus a client presenting
// the relay's leaf. Nothing here stamps a synthetic connection state: the
// verified chain the handler requires is produced by the TLS stack.
func newIngestServer(t *testing.T, hub *Hub) (*httptest.Server, *http.Client) {
	t.Helper()
	relay := pushRelay()
	srv := httptest.NewUnstartedServer(eventingest.New(hub, siteScope, ulidSeq(), relay.Authorizer()))
	srv.TLS = relay.ServerTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13})
	srv.StartTLS()

	serverCAs := x509.NewCertPool()
	serverCAs.AddCert(srv.Certificate())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: relay.ClientTLSConfig(serverCAs)}}
	return srv, client
}

// TestWire_TelemetryPushDrivesLiveSSE is the finding-5 regression: the live-push
// half of EVT-100 must be driven by the REAL writer, not a manually manufactured
// wake. The telemetry ingest (internal/app/eventingest) and the SSE server are
// wired to the SAME Hub — the ingest holds it as its EventSink — so a POST
// /telemetry/v1/push that appends an event MUST push it live to a connected SSE
// subscriber with no separate notify plumbing. Before the fix, eventingest had no
// wake channel at all and the subscriber would block on the live select forever.
func TestWire_TelemetryPushDrivesLiveSSE(t *testing.T) {
	log := events.NewEventLog(0)
	hub := NewHub(log)

	// Reader and writer share one Hub — the Hub IS the wiring seam.
	sseSrv := httptest.NewServer(newTestServer(hub))
	defer sseSrv.Close()
	// The ingest is mutually authenticated (relay/1 REL-003/041): it is mounted
	// on a real TLS listener that verifies a client certificate against the
	// enrollment CA, and pushed to by a client presenting the relay's leaf —
	// the same shape the deployed feeder and relay use.
	ingestSrv, ingestClient := newIngestServer(t, hub)
	defer ingestSrv.Close()

	br, closeConn := dialSSE(t, sseSrv, "", nil)
	defer closeConn()

	// Drive a real telemetry push at the ingest — no test-manufactured wake.
	batch := telemetry.PushBatch{
		Entries:     []telemetry.Entry{{Seq: 1, Schema: events.SchemaAutomationRun, Payload: corpusAutomationRunPayload()}},
		LossMarkers: []telemetry.LossMarker{},
	}
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshaling push batch: %v", err)
	}
	resp, err := ingestClient.Post(ingestSrv.URL+"/telemetry/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("posting telemetry push: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("telemetry push must respond 200; got %d", resp.StatusCode)
	}

	// The ingest's Append must have woken the SSE subscriber and pushed the
	// reconstructed envelope live (EVT-100/042).
	f := readFrameWithin(t, br, 2*time.Second)
	if f.event != "event" {
		t.Fatalf("the ingested telemetry event must push live as an SSE event frame (EVT-100); got event=%q", f.event)
	}
	var env events.Envelope
	if err := json.Unmarshal([]byte(f.data), &env); err != nil {
		t.Fatalf("frame data must be the reconstructed envelope JSON (EVT-103); got %v data=%s", err, f.data)
	}
	if env.Origin != "relay" || env.Schema != events.SchemaAutomationRun {
		t.Fatalf("live frame must carry the ingested automation.run envelope (origin relay); got origin=%q schema=%q", env.Origin, env.Schema)
	}
	if env.ScopeNode != siteScope {
		t.Fatalf("live frame envelope must carry the site scope node the ingest stamped; got %q", env.ScopeNode)
	}
}
