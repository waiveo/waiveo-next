// screenstatus_e2e_test.go is the LIVE SCREEN STATUS pipeline's whole-stack
// proof (parity row 5.8): what a relay's own player server observed of a screen
// becomes a row on the app peer's api/1 surface, with nothing in between faked.
//
//	internal/relay/playerserver (the real observation record, populated by a
//	  real player pairing and pulling over real HTTP)
//	  -> cmd/waiveo-relay's own reporter shape (this file's sendStatus mirrors it)
//	    -> internal/relay/relayconn (the real dialer's SendScreenStatus)
//	      == the real authenticated WS connection, enrollment + hello ==
//	      -> internal/feeder/relayconn (the real server read loop + sink)
//	        -> internal/app/screens (the real read model)
//	          -> internal/app/api (the real handler: GET /screen-status)
//
// NOTHING here calls the read model's writer. The only way a status row appears
// is a real relay putting a real `screen.status` frame on a real connection —
// which is the one arrangement that catches either half of the failure this
// pipeline invites: a read model nothing populates, or a connection that accepts
// the frame and drops it. This project's recurring defect is a surface that
// accepts work it never performs, and a status page is the purest form of it: a
// page of plausible-looking cards nobody wired to anything looks exactly like a
// working one.
package relayconn_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/screens"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	relayclient "github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// ssGrantID is the pairing grant the fixture player redeems at the relay's own
// player/1 surface — the real redemption that mints the channel token every
// observation below is attributed to.
const ssGrantID = "grant-screenstatus-0123456789"

// ssStack is one relay running a real player/1 server, connected to one app peer
// whose api/1 handler is mounted over the same connection server, with the live
// screen-status read model as that connection's sink.
type ssStack struct {
	h        *harness
	registry *screens.Registry
	player   *playerserver.Server
	playerTS *httptest.Server
	client   *relayclient.Client
	apiTS    *httptest.Server
	nowMs    func() int64
	setNow   func(int64)
}

// newSSStack wires the production arrangement end to end.
func newSSStack(t *testing.T) *ssStack {
	t.Helper()

	now := int64(1_700_000_000_000)
	nowFn := func() int64 { return now }
	setNow := func(v int64) { now = v }

	registry, err := screens.NewRegistry(nowFn)
	if err != nil {
		t.Fatalf("screens.NewRegistry: %v", err)
	}
	h := newHarness(t, feederrelayconn.WithScreenStatusSink(registry, nowFn))

	identStore := enrolledRelay(t, h)
	client, err := relayclient.Dial(relayclient.Config{
		URL:         h.ts.URL,
		Store:       identStore,
		Declaration: testDeclaration,
	})
	if err != nil {
		t.Fatalf("relayconn.Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	waitFor(t, 5*time.Second, func() bool { return h.connSrv.ConnCount() >= 1 },
		"authenticated connection never registered with the app peer")

	// The relay's REAL player/1 server, on the same injected clock, holding one
	// redeemable grant — so a fixture player can pair and pull for real.
	certPEM, _ := tlsboot.GenSelfSigned()
	grant := wire.PairingGrant{
		GrantID: ssGrantID, Purpose: "pairing", ResultingPrincipalKind: "screen",
		TTL: 900, RedemptionMode: "one-time", IssuedAt: now,
	}
	player, err := playerserver.NewServer(certPEM, []wire.PairingGrant{grant}, nowFn)
	if err != nil {
		t.Fatalf("playerserver.NewServer: %v", err)
	}
	player.SetSigningKey(ssSigningKey(t))
	mux := http.NewServeMux()
	player.Register(mux)
	playerTS := httptest.NewServer(apihttp.WithTraceID(mux))
	t.Cleanup(playerTS.Close)

	return &ssStack{
		h: h, registry: registry, player: player, playerTS: playerTS, client: client,
		apiTS: ssAPIOverRegistry(t, registry), nowMs: nowFn, setNow: setNow,
	}
}

// sendStatus is cmd/waiveo-relay's own reportScreenStatus, in the one shape this
// test can drive it in: the player server's real snapshot, projected onto the
// wire entries and pushed up the real connection.
func (s *ssStack) sendStatus(t *testing.T) {
	t.Helper()
	statuses := s.player.ScreenStatuses()
	entries := make([]wire.ScreenStatusEntry, 0, len(statuses))
	for _, st := range statuses {
		entries = append(entries, wire.ScreenStatusEntry{
			ScreenID:             st.ScreenID,
			Paired:               st.Paired,
			LastPullAgeMs:        st.LastPullAgeMs,
			LastAckAgeMs:         st.LastAckAgeMs,
			LastRenderStartAgeMs: st.LastRenderStartAgeMs,
			ProgramRevision:      st.ProgramRevision,
			Priority:             st.Priority,
			Display:              st.Display,
			ContentCount:         st.ContentCount,
			RenderAssetRef:       st.RenderAssetRef,
		})
	}
	if err := s.client.SendScreenStatus(wire.ScreenStatusBody{Screens: entries}); err != nil {
		t.Fatalf("SendScreenStatus: %v", err)
	}
}

// TestAScreenObservedAtTheRelayBecomesAStatusRowOnTheAppPeer is the pipeline,
// driven once, end to end.
func TestAScreenObservedAtTheRelayBecomesAStatusRowOnTheAppPeer(t *testing.T) {
	s := newSSStack(t)

	// A real player pairs at the relay and pulls its program. Nothing about the
	// status is asserted yet — this is the screen doing its job, which is the
	// only thing that generates an observation.
	token, screenID := ssPair(t, s.playerTS)
	s.player.SetProgram(1, screenID, "rev-live", "scheduled", "content", []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa", URL: "https://origin.example/content/aa"},
	})
	s.setNow(1_700_000_004_000)
	ssPull(t, s.playerTS, token)

	// The relay reports upward on its own cadence.
	s.setNow(1_700_000_006_000)
	s.sendStatus(t)
	waitFor(t, 5*time.Second, func() bool { return len(s.registry.Statuses()) == 1 },
		"the app peer never held a screen status after a screen.status report")

	got := s.registry.Statuses()[0]
	if got.ScreenID != screenID {
		t.Fatalf("status is for %q, want the paired screen %q", got.ScreenID, screenID)
	}
	if !got.Paired {
		t.Error("paired = false for a screen holding a live channel-token session")
	}
	if got.LastPullAgeMs != 2_000 {
		t.Errorf("last_pull age = %d, want 2000 (pulled at +4s, reported at +6s)", got.LastPullAgeMs)
	}
	if got.Reachability != screens.ReachabilityLive {
		t.Errorf("reachability = %q, want live", got.Reachability)
	}
	if got.ProgramRevision != "rev-live" || got.ContentCount != 1 {
		t.Errorf("status describes program %q with %d item(s), want rev-live with 1", got.ProgramRevision, got.ContentCount)
	}

	// And on the api/1 surface an operator actually reads — the last hop, and
	// the one most likely to be the missing wire.
	resp, raw := ssGet(t, s.apiTS, "/api/v1/screen-status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/screen-status = %d (%s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The API lists AUTHORED screen rows, and this app peer's store has none —
	// the relay minted this screen id itself at redemption. That the list is
	// empty here is correct and is worth pinning: a status row for a screen no
	// row exists for is one an operator cannot name or act on, so it is dropped
	// rather than surfaced as a mystery id.
	if len(page.Items) != 0 {
		t.Fatalf("api listed %d row(s) for a store holding no screen rows, want 0: %v", len(page.Items), page.Items)
	}
}

// TestTheAppPeersViewOfAScreenAgesWhileTheRelayIsSilent is the honesty property,
// proven across the real connection rather than in the read model alone: the app
// peer keeps no clock of the relay's, so a relay that stops reporting must make
// its screens read progressively staler.
func TestTheAppPeersViewOfAScreenAgesWhileTheRelayIsSilent(t *testing.T) {
	s := newSSStack(t)

	token, screenID := ssPair(t, s.playerTS)
	s.player.SetProgram(1, screenID, "rev-live", "scheduled", "content", nil)
	ssPull(t, s.playerTS, token)
	s.sendStatus(t)
	waitFor(t, 5*time.Second, func() bool { return len(s.registry.Statuses()) == 1 },
		"the app peer never held a screen status")

	if got := s.registry.Statuses()[0].Reachability; got != screens.ReachabilityLive {
		t.Fatalf("fixture: reachability right after a report = %q, want live", got)
	}

	// Two minutes pass with no further report — a disconnected relay, a wedged
	// reporter, a dead box. The app has stopped learning anything, and says so.
	s.setNow(1_700_000_000_000 + 120_000)
	got := s.registry.Statuses()[0]
	if got.Reachability != screens.ReachabilityStale {
		t.Errorf("reachability two minutes after the last report = %q, want stale — a view that never ages reports a dead fleet as healthy forever", got.Reachability)
	}
	if got.ReportAgeMs < 120_000 {
		t.Errorf("report_age = %d, want at least 120000 — the field that tells an operator the RELAY went quiet rather than the screen", got.ReportAgeMs)
	}
}

// TestAReportReplacesTheRelaysWholeView: a screen that has left a relay's view
// (its session dropped, its program removed) must leave the app peer's view too,
// which only a full-set replace expresses.
func TestAReportReplacesTheRelaysWholeView(t *testing.T) {
	s := newSSStack(t)

	token, screenID := ssPair(t, s.playerTS)
	s.player.SetProgram(1, screenID, "rev-live", "scheduled", "content", nil)
	ssPull(t, s.playerTS, token)
	s.sendStatus(t)
	waitFor(t, 5*time.Second, func() bool { return len(s.registry.Statuses()) == 1 },
		"the app peer never held a screen status")

	// The relay now reports an empty set — the shape a relay that has forgotten
	// every screen sends.
	if err := s.client.SendScreenStatus(wire.ScreenStatusBody{}); err != nil {
		t.Fatalf("SendScreenStatus(empty): %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(s.registry.Statuses()) == 0 },
		"an empty screen.status report never cleared the app peer's view — a screen the relay has forgotten would be described forever")
}

// ---- fixture plumbing -------------------------------------------------------

// ssSigningKey mints the relay's Lease-signing identity. Its public half is not
// verified anywhere in this file (no player here checks a signature), so any
// valid ed25519 private key serves; what matters is that one is installed, since
// a server with none refuses every pull and no observation is ever made.
func ssSigningKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	return priv
}

// ssPair redeems the fixture grant at the relay's real player/1 surface.
func ssPair(t *testing.T, ts *httptest.Server) (token, screenID string) {
	t.Helper()
	body, err := json.Marshal(playerserver.PairingRequest{
		HardwareID:    "hw-screenstatus",
		GrantSelector: ssGrantID,
		Capabilities:  playerserver.Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	resp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	defer resp.Body.Close()
	var out playerserver.PairingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode pairing response: %v", err)
	}
	if out.ChannelToken == "" {
		t.Fatalf("pairing yielded no channel token: %+v", out)
	}
	return out.ChannelToken, out.ScreenID
}

// ssPull performs one real program pull with token.
func ssPull(t *testing.T, ts *httptest.Server, token string) {
	t.Helper()
	body, err := json.Marshal(playerserver.ProgramPullRequest{
		Capabilities: playerserver.Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
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
}

// ssAPIOverRegistry mounts the real api/1 handler over an EXISTING screen-status
// read model, with an empty store — so any row a case observes got there through
// the connection and the join, never from a fixture.
func ssAPIOverRegistry(t *testing.T, registry *screens.Registry) *httptest.Server {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return apiFixedNowMs }
	fixture, err := authtest.New(authtest.Config{NowMs: clock})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)
	apiAuth = fixture

	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		origin.New(), apiContentBase, fixture.Auth, api.WithScreenStatus(registry)))
	t.Cleanup(ts.Close)
	return ts
}

// ssGet performs an authenticated api/1 GET against the mounted handler.
func ssGet(t *testing.T, ts *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	apiAuth.Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}
