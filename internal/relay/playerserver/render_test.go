package playerserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	srv.SetProgram(1, testScreenIDA, "rev-18", "preempt", "content", testImageContent())

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

// postPlayerJSON posts body to a player/1 route with the channel token in the
// Authorization header, as PLY-076 requires on every operation a channel token
// authorizes. A test that means to post WITHOUT a credential passes an empty
// token — the absence then reads as deliberate rather than as an omission, which
// is how three of these routes came to require none at all.
func postPlayerJSON(t *testing.T, ts *httptest.Server, token, path string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// leaseFor pulls a program and returns the issued Lease, so a render or ack case
// references a lease_id this relay actually issued to this token's screen
// (PLY-114) instead of an invented one.
func leaseFor(t *testing.T, srv *Server, token string) LeaseResponse {
	t.Helper()
	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("program pull for a lease: status = %d, body %v", resp.StatusCode, raw)
	}
	var lease LeaseResponse
	remarshal(t, raw, &lease)
	return lease
}

// TestRenderStartRecorded confirms POST /player/v1/render/start (PLY-110)
// records the report, retrievable via Server.RenderStarts.
func TestRenderStartRecorded(t *testing.T) {
	srv, token := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)
	lease := leaseFor(t, srv, token)

	start := RenderStartRequest{
		LeaseID:  lease.LeaseID,
		AssetRef: "sha256:cccc",
		TS:       1752538000600,
	}
	body, _ := json.Marshal(start)
	resp := postPlayerJSON(t, ts, token, "/player/v1/render/start", body)
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
	srv, token := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)

	end := RenderEndRequest{
		// The token's OWN screen: PLY-111's body carries screen_id as the subject
		// of the record, and PLY-070 binds a channel token to exactly one.
		ScreenID:        testScreenIDA,
		AssetRef:        "sha256:aaaa",
		ProgramRevision: "rev-17",
		TStart:          1752537960000,
		TEnd:            1752538000500,
		Cause:           "scheduled",
		Completion:      "interrupted",
	}
	body, _ := json.Marshal(end)
	resp := postPlayerJSON(t, ts, token, "/player/v1/render/end", body)
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
	srv, token := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)

	resp := postPlayerJSON(t, ts, token, "/player/v1/render/end", []byte("{"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("render/end status = %d, want 400", resp.StatusCode)
	}
	if len(srv.RenderEnds()) != 0 {
		t.Errorf("a malformed render/end was recorded, want none")
	}
}
