package playerserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// preemptProgramTestServer is programTestServer's preempt-priority twin: one
// redeemable grant and one configured program whose priority is "preempt"
// (PLY-100/108), returning the server and a freshly redeemed channel token.
func preemptProgramTestServer(t *testing.T) (srv *Server, token string) {
	t.Helper()

	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(testScreenIDA)

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetProgram(1, testScreenIDA, "rev-18", "preempt", "content", testImageContent(), priv)

	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	var pairResp PairingResponse
	remarshal(t, raw, &pairResp)
	if pairResp.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pairResp)
	}
	return srv, pairResp.ChannelToken
}

// TestProgramServesPreemptPriorityLease confirms the relay grants a
// preempt-priority Lease when its screen-program is preempt: priority is
// carried unmodified onto the issued Lease (PLY-100/108), the mechanism by
// which an interrupt-now takeover reaches a player.
func TestProgramServesPreemptPriorityLease(t *testing.T) {
	srv, token := preemptProgramTestServer(t)

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, raw)
	}
	var lease LeaseResponse
	remarshal(t, raw, &lease)

	if lease.Priority != "preempt" {
		t.Errorf("priority = %q, want %q (PLY-100/108, unmodified from the screen-program)", lease.Priority, "preempt")
	}
	if lease.Display != "content" {
		t.Errorf("display = %q, want %q", lease.Display, "content")
	}
}

// TestRenderStartRecorded confirms POST /player/v1/render/start (PLY-110)
// records the report, retrievable via Server.RenderStarts.
func TestRenderStartRecorded(t *testing.T) {
	srv, _ := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)

	start := RenderStartRequest{
		LeaseID:  "01J8Z3K4N5P6Q7R8S9T0V1W2ZG",
		AssetRef: "sha256:cccc",
		TS:       1752538000600,
	}
	body, _ := json.Marshal(start)
	resp, err := http.Post(ts.URL+"/player/v1/render/start", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /player/v1/render/start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("render/start status = %d, want 200", resp.StatusCode)
	}

	got := srv.RenderStarts()
	if len(got) != 1 {
		t.Fatalf("RenderStarts len = %d, want 1", len(got))
	}
	if got[0] != start {
		t.Errorf("RenderStarts[0] = %+v, want %+v", got[0], start)
	}
}

// TestRenderEndRecorded confirms POST /player/v1/render/end (PLY-111) records
// the report in its full events/1 EVT-050 shape, retrievable via
// Server.RenderEnds.
func TestRenderEndRecorded(t *testing.T) {
	srv, _ := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)

	end := RenderEndRequest{
		ScreenID:        "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
		AssetRef:        "sha256:aaaa",
		ProgramRevision: "rev-17",
		TStart:          1752537960000,
		TEnd:            1752538000500,
		Cause:           "scheduled",
		Completion:      "interrupted",
	}
	body, _ := json.Marshal(end)
	resp, err := http.Post(ts.URL+"/player/v1/render/end", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /player/v1/render/end: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("render/end status = %d, want 200", resp.StatusCode)
	}

	got := srv.RenderEnds()
	if len(got) != 1 {
		t.Fatalf("RenderEnds len = %d, want 1", len(got))
	}
	if got[0] != end {
		t.Errorf("RenderEnds[0] = %+v, want %+v", got[0], end)
	}
}

// TestRenderEndRejectsMissingBody confirms a malformed render/end body is
// refused with a typed VALIDATION_FAILED, never silently recorded.
func TestRenderEndRejectsMissingBody(t *testing.T) {
	srv, _ := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)

	resp, err := http.Post(ts.URL+"/player/v1/render/end", "application/json", bytes.NewReader([]byte("{")))
	if err != nil {
		t.Fatalf("POST /player/v1/render/end: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("render/end status = %d, want 400", resp.StatusCode)
	}
	if len(srv.RenderEnds()) != 0 {
		t.Errorf("a malformed render/end was recorded, want none")
	}
}
