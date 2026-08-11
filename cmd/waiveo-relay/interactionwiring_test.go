package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// interactionwiring_test.go guards the RETURN PATH's only link to a real box:
// the recorder a shipped relay installs on its player/1 server, which turns an
// accepted viewer press into a durable events/1 `screen.interaction` (EVT-055).
//
// It exists because that wiring was UNGUARDED, in the precise shape this repo
// keeps shipping. Deleting the two lines from func main left `go test ./...` at
// exit 0 across every package: internal/relay/playerserver installs its own spy
// recorder in every interaction case, so the ROUTE was covered from every angle
// while "does the relay a customer runs install one?" was answered by nobody.
// Without it a real deployment answers every press 503 / UNAVAILABLE and fires
// no automation at all, with every gate green — the identical hole
// signingkeywiring_test.go was written for, after it had already cost us once.
//
// Two tests, deliberately, because the wiring can be broken two independent
// ways and neither test sees the other's failure:
//
//   - TestWireInteractionRecorderRecordsAPressIntoTheTelemetryBuffer drives a
//     real press at the live route and reads the real buffer. It catches the
//     recorder recording the wrong thing — a swapped argument, a dropped trace
//     id, a subject that is not the screen — which no source scan could.
//   - TestMainWiresTheInteractionRecorder asserts main CALLS it, with the
//     relay's own telemetry buffer. It catches the call being deleted, which no
//     behavioural test of a helper can.

// TestWireInteractionRecorderRecordsAPressIntoTheTelemetryBuffer is the
// behavioural half: the wiring under test, the live HTTP route, and the relay's
// real durable telemetry buffer — nothing stubbed between them.
func TestWireInteractionRecorderRecordsAPressIntoTheTelemetryBuffer(t *testing.T) {
	srv, grantID := newTestPlayerServer(t)
	buf := telemetry.NewBuffer(64)

	wireInteractionRecorder(srv, buf)

	ts, token := serveAndPair(t, srv, grantID)
	lease := pullLease(t, ts, token)

	press, err := json.Marshal(map[string]string{"lease_id": lease.LeaseID, "interaction": "call_service"})
	if err != nil {
		t.Fatalf("marshal press: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/player/v1/interaction", bytes.NewReader(press))
	if err != nil {
		t.Fatalf("build interaction request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /player/v1/interaction: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("press status = %d, want 200 — a relay with no recorder installed answers 503 and the press is lost", resp.StatusCode)
	}

	pending := buf.Pending()
	if len(pending) != 1 {
		t.Fatalf("the telemetry buffer holds %d entry/entries after one press, want exactly 1; "+
			"a press that reaches no buffer reaches no app peer and fires no automation", len(pending))
	}
	got := pending[0]
	if got.Schema != "screen.interaction" {
		t.Errorf("buffered schema = %q, want screen.interaction (EVT-055)", got.Schema)
	}
	// The subject is the screen, not the interaction name: it is the buffer's
	// supersession key, and a wrong one would let two presses on one screen
	// coalesce the day this schema's class changed (EVT-056 forbids that).
	if got.Subject != testPlayerServerScreenID {
		t.Errorf("buffered subject = %q, want the screen id %q", got.Subject, testPlayerServerScreenID)
	}
	// The trace id is the recorder's fifth argument. Swap it with `subject` —
	// both strings, both accepted by the compiler — and correlation across the
	// press, the ingest and the rule run breaks with nothing else failing.
	if got.TraceID == "" {
		t.Error("the buffered entry carries no trace_id; the press cannot be correlated to the run it causes (API-063)")
	}
	if got.TraceID == got.Subject {
		t.Errorf("the buffered trace_id and subject are both %q — the recorder's arguments are swapped", got.TraceID)
	}
	var payload struct {
		ScreenID    string `json:"screen_id"`
		Interaction string `json:"interaction"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("decode buffered payload: %v", err)
	}
	if payload.Interaction != "call_service" {
		t.Errorf("buffered payload interaction = %q, want call_service carried verbatim (EVT-057)", payload.Interaction)
	}
	if payload.ScreenID != testPlayerServerScreenID {
		t.Errorf("buffered payload screen_id = %q, want %q", payload.ScreenID, testPlayerServerScreenID)
	}
}

// TestWireInteractionRecorderIsWhatMakesAPressAcceptedAtAll is the control for
// the case above: the SAME server, the SAME press, with the wiring not applied.
// Without it the behavioural test could pass on a route that accepts a press and
// records nothing, or on a buffer that filled itself.
func TestWireInteractionRecorderIsWhatMakesAPressAcceptedAtAll(t *testing.T) {
	srv, grantID := newTestPlayerServer(t)
	ts, token := serveAndPair(t, srv, grantID)
	lease := pullLease(t, ts, token)

	press, err := json.Marshal(map[string]string{"lease_id": lease.LeaseID, "interaction": "call_service"})
	if err != nil {
		t.Fatalf("marshal press: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/player/v1/interaction", bytes.NewReader(press))
	if err != nil {
		t.Fatalf("build interaction request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /player/v1/interaction: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("with no recorder installed the press answered %d, want 503 — this is the state a relay whose "+
			"wiring was deleted serves in, and it must be visibly broken rather than silently accepted", resp.StatusCode)
	}
}

// TestMainWiresTheInteractionRecorder is the source half: a shipped relay must
// call wireInteractionRecorder, once, with its player server and its OWN durable
// telemetry buffer.
//
// It is a check on main's SOURCE, with the same limits clockwiring_test.go and
// signingkeywiring_test.go state for theirs: it catches the call being REMOVED
// or its arguments being changed, not the call being made unreachable. The
// behavioural half is the test above.
func TestMainWiresTheInteractionRecorder(t *testing.T) {
	mainFn := parseRelayMainFunc(t)

	const (
		wantCallee = "wireInteractionRecorder"
		wantServer = "pairingSrv"
		// The buffer MUST be the automation host's own — the one the telemetry
		// Channel constructed further down in main pushes to the app peer.
		// Handing it a fresh telemetry.NewBuffer would record every press into a
		// buffer nothing drains: 200 to the player, and the automation never
		// runs, which is the failure mode hardest to notice from outside.
		wantBuffer = "host.TelemetryBuffer(…)"
	)

	found := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || renderExpr(call.Fun) != wantCallee {
			return true
		}
		found++
		if len(call.Args) != 2 {
			t.Errorf("%s is called with %d argument(s), want exactly 2", wantCallee, len(call.Args))
			return true
		}
		if got := renderExpr(call.Args[0]); got != wantServer {
			t.Errorf("%s receives %s as its player server, want %s — the recorder must be installed on the server that MOUNTS /player/v1/interaction",
				wantCallee, got, wantServer)
		}
		if got := renderExpr(call.Args[1]); got != wantBuffer {
			t.Errorf("%s receives %s as its telemetry buffer, want %s — a press recorded into any other buffer is never pushed to the app peer, so the automation never runs while every layer reports success",
				wantCallee, got, wantBuffer)
		}
		return true
	})

	if found == 0 {
		t.Errorf("func main never calls %s. Without it this relay installs no interaction recorder: POST /player/v1/interaction answers 503 / UNAVAILABLE for every viewer press on every screen at the site, no events/1 screen.interaction is ever recorded, and no `event`-triggered automation ever fires — with the entire test suite green, because every other test installs its own recorder.",
			wantCallee)
	}
	if found > 1 {
		t.Errorf("func main calls %s %d times; the recorder is installed once, before any route is mounted", wantCallee, found)
	}
}

// serveAndPair mounts srv on a test listener and redeems grantID, returning the
// listener and the paired screen's channel token.
func serveAndPair(t *testing.T, srv *playerserver.Server, grantID string) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(apihttp.WithTraceID(mux))
	t.Cleanup(ts.Close)

	body, err := json.Marshal(playerserver.PairingRequest{
		HardwareID:    "hw-interaction-0001",
		GrantSelector: grantID,
		Capabilities:  playerserver.Capabilities{ContentTypes: []string{"image", "video", "slide"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	resp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	defer resp.Body.Close()
	var pr playerserver.PairingResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode pairing response: %v", err)
	}
	if pr.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pr)
	}
	return ts, pr.ChannelToken
}

// pullLease pulls one program so a Lease exists for a press to be bound to
// (PLY-114: an interaction names a lease this relay issued to this screen).
func pullLease(t *testing.T, ts *httptest.Server, token string) playerserver.LeaseResponse {
	t.Helper()
	body, err := json.Marshal(playerserver.ProgramPullRequest{
		Capabilities: playerserver.Capabilities{ContentTypes: []string{"image", "video", "slide"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal program pull: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/player/v1/program", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build program request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /player/v1/program: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("program pull status = %d, want 200", resp.StatusCode)
	}
	var lease playerserver.LeaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	if lease.LeaseID == "" {
		t.Fatal("the program pull yielded no lease id; a press has nothing to bind to")
	}
	return lease
}
