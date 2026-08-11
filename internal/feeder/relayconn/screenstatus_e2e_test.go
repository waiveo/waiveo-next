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
			UnackedPulls:         st.UnackedPulls,
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
	ssAck(t, s.playerTS, token, ssPull(t, s.playerTS, token))

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
	// Pull AND acknowledge, which is one whole iteration of the shipped player.
	// The ack matters to what this case asserts: an unacknowledged pull is how
	// the read model recognises a screen that is still transferring content, and
	// a fixture that never acks would be asserting `stale` about a screen this
	// model correctly calls `fetching`.
	ssAck(t, s.playerTS, token, ssPull(t, s.playerTS, token))
	s.sendStatus(t)
	waitFor(t, 5*time.Second, func() bool { return len(s.registry.Statuses()) == 1 },
		"the app peer never held a screen status")

	if got := s.registry.Statuses()[0].Reachability; got != screens.ReachabilityLive {
		t.Fatalf("fixture: reachability right after a report = %q, want live", got)
	}

	// A silence longer than the live window passes with no further report — a
	// disconnected relay, a wedged reporter, a dead box. The app has stopped
	// learning anything, and says so.
	//
	// The silence is expressed relative to screens.LiveWindowMs, not as a
	// literal two minutes: the window is DERIVED from the player's measured pull
	// cadence (internal/shared/wire/screencadence.go), so a literal here would
	// silently become an assertion that a screen inside the window is stale the
	// next time that cadence is re-measured.
	const silence = screens.LiveWindowMs + 60_000
	s.setNow(1_700_000_000_000 + silence)
	got := s.registry.Statuses()[0]
	if got.Reachability != screens.ReachabilityStale {
		t.Errorf("reachability %dms after the last report = %q, want stale — a view that never ages reports a dead fleet as healthy forever", silence, got.Reachability)
	}
	if got.ReportAgeMs < silence {
		t.Errorf("report_age = %d, want at least %d — the field that tells an operator the RELAY went quiet rather than the screen", got.ReportAgeMs, silence)
	}
}

// TestAScreenThatPullsAndNeverAcksReachesANonFetchingState is the 2026-08 wall,
// driven through the WHOLE pipeline — real player/1 pulls at a real relay, a
// real `screen.status` frame over a real connection, the app peer's real read
// model — because that is where the finding lived and every layer of it had to
// be wrong at once for the console to say what it said.
//
// The screen answers its program pull (so the relay stamps a fresh `lastPullMs`
// on every retry) and never acknowledges (the shipped player returns before
// `wvAckLease` when a content fetch fails). Bounded on pull AGE alone, that
// screen read `fetching` on every sample forever: its age reset before it could
// expire, and its pull was permanently unacknowledged.
//
// Three pulls with no ack is past the tolerance, which is the point of the count
// — a duration cannot express "and it keeps doing it".
func TestAScreenThatPullsAndNeverAcksReachesANonFetchingState(t *testing.T) {
	s := newSSStack(t)

	token, screenID := ssPair(t, s.playerTS)
	s.player.SetProgram(1, screenID, "rev-new", "scheduled", "content", []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa", URL: "https://origin.example/content/aa"},
	})

	// One pull, unacknowledged, reported while it is still young: this is a
	// screen that could genuinely be materialising the Lease it was just handed,
	// and it must NOT be called stale.
	s.setNow(1_700_000_004_000)
	ssPull(t, s.playerTS, token)
	s.setNow(1_700_000_004_000 + screens.LiveWindowMs + 1_000)
	s.sendStatus(t)
	waitFor(t, 5*time.Second, func() bool { return len(s.registry.Statuses()) == 1 },
		"the app peer never held a screen status")
	if got := s.registry.Statuses()[0]; got.Reachability != screens.ReachabilityFetching {
		t.Fatalf("one outstanding pull past the live window reads %q, want fetching (unacked_pulls %d) — a screen downloading a video must not be reported as a screen to go and look at",
			got.Reachability, got.UnackedPulls)
	}

	// It keeps pulling and keeps not acknowledging. Each pull re-stamps the
	// relay's clock, so the AGE bound is never reached — only the count moves.
	for i := 0; i < int(screens.MaxFetchingUnackedPulls); i++ {
		s.setNow(1_700_000_010_000 + int64(i)*60_000)
		ssPull(t, s.playerTS, token)
	}
	// Reported at the top of the screen's own backoff sawtooth, which is where a
	// broken wall spends most of its samples and the only place the fleet-dark
	// grade can be reached from.
	s.setNow(1_700_000_010_000 + int64(screens.MaxFetchingUnackedPulls)*60_000 + screens.LiveWindowMs + 1_000)
	s.sendStatus(t)

	var got screens.Status
	waitFor(t, 5*time.Second, func() bool {
		st := s.registry.Statuses()
		if len(st) != 1 {
			return false
		}
		got = st[0]
		return got.UnackedPulls > int(screens.MaxFetchingUnackedPulls)
	}, "the app peer never saw the unacknowledged-pull count climb past the tolerance")

	if got.Reachability == screens.ReachabilityFetching {
		t.Fatalf("after %d pulls with no acknowledgement the screen still reads `fetching` (pull age %d, transfer window %d).\n"+
			"Nothing is being fetched. This is the 2026-08 wall: 200 on every program pull, a failed content fetch, no ack, retry "+
			"forever — and while `fetching` captured it the console said 'Collecting content' about a dead screen and the fleet "+
			"roll-up could never grade a whole site of them `down`.", got.UnackedPulls, got.LastPullAgeMs, screens.ContentTransferWindowMs)
	}
	// `rejected`, not `stale`. The screen is answering every poll — nothing about
	// it is unheard-from — and calling it stale is what sent a whole review round
	// looking at the live window instead of at the content it kept refusing. Both
	// keep it out of `live` and `fetching`, which is what the fleet-dark grade
	// needs; this one also says what to go and fix.
	if got.Reachability != screens.ReachabilityRejected {
		t.Fatalf("reachability = %q, want rejected: the pull is %dms old (the screen is in contact) with %d pulls outstanding and nothing ever confirmed",
			got.Reachability, got.LastPullAgeMs, got.UnackedPulls)
	}
	if got.LastPullAgeMs > screens.ContentTransferWindowMs {
		t.Fatalf("fixture no longer proves the finding: the pull age (%d) is outside the transfer window (%d), so the AGE bound would have expired `fetching` on its own and the count is not what this test is measuring",
			got.LastPullAgeMs, screens.ContentTransferWindowMs)
	}
}

// TestAFirstEverTransferAfterBlankLeasesIsNotStale is the OTHER end of the same
// count, driven through the same whole stack, and it is the case the bound broke
// when it was added.
//
// A freshly paired screen has no program, so the relay answers every pull with
// terminalDefault: `display: blank`, empty content (DAT-118). The shipped player
// refuses an empty content array and returns before `wvAckLease`, exactly as it
// does on a failed fetch — so those pulls are never acknowledged. While they
// counted, a box six seconds past pairing (two pulls, at the player's 2 s/4 s
// backoff) was already at the tolerance, and the very FIRST program an operator
// assigned — one Lease, one genuine 90-second video download — pushed it over.
//
//	PROBE: reachability="stale" last_pull_age_ms=90000 unacked_pulls=3
//	       live_window=52000 transfer_window=172000 max_unacked=2
//
// A one-screen site then graded `down` (internal/app/api/diagnostics.go) on a box
// that was working perfectly, and the console cell said the opposite of what was
// happening. Nothing about the screen was wrong; the counter was counting pulls
// with nothing outstanding to fetch.
func TestAFirstEverTransferAfterBlankLeasesIsNotStale(t *testing.T) {
	s := newSSStack(t)

	token, screenID := ssPair(t, s.playerTS)

	// No program assigned yet. The player pulls on its startup backoff and gets
	// the terminal default each time — nothing to fetch, nothing to acknowledge.
	s.setNow(1_700_000_002_000)
	ssPull(t, s.playerTS, token)
	s.setNow(1_700_000_006_000)
	ssPull(t, s.playerTS, token)

	// The operator assigns the screen its first program: one video. The screen
	// collects it, and the download takes 90 seconds — one Lease in flight, well
	// inside the transfer window, which is precisely `fetching`.
	s.player.SetProgram(1, screenID, "rev-first", "scheduled", "content", []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa", URL: "https://origin.example/content/aa"},
	})
	s.setNow(1_700_000_010_000)
	ssPull(t, s.playerTS, token)
	s.setNow(1_700_000_010_000 + 90_000)
	s.sendStatus(t)

	var got screens.Status
	waitFor(t, 5*time.Second, func() bool {
		st := s.registry.Statuses()
		if len(st) != 1 {
			return false
		}
		got = st[0]
		return got.LastPullAgeMs > 0
	}, "the app peer never held a screen status for the transferring screen")

	if got.UnackedPulls != 1 {
		t.Fatalf("unacked_pulls = %d during a first-ever transfer, want 1: the two blank Leases served before it are being counted as outstanding pulls, and nothing in them can ever be acknowledged",
			got.UnackedPulls)
	}
	if got.Reachability != screens.ReachabilityFetching {
		t.Fatalf("a screen 90s into its FIRST content download reads %q, want fetching (unacked_pulls %d, pull age %d, transfer window %d).\n"+
			"This is a working box on its first program. Reading `stale` here sends an operator to a site with nothing wrong with it, and the "+
			"fleet roll-up grades a one-screen deployment `down`.",
			got.Reachability, got.UnackedPulls, got.LastPullAgeMs, screens.ContentTransferWindowMs)
	}
	// The fixture has to still be measuring the COUNT, not the age: if the pull
	// had aged out of the transfer window this case would pass for the wrong
	// reason and stop proving anything about blank Leases.
	if got.LastPullAgeMs > screens.ContentTransferWindowMs {
		t.Fatalf("fixture no longer proves the finding: the pull age (%d) is outside the transfer window (%d), so the age bound decides this case and the blank-Lease count is not what is being measured",
			got.LastPullAgeMs, screens.ContentTransferWindowMs)
	}
}

// TestAReportReplacesTheRelaysWholeView: a screen that has left a relay's view
// (its session dropped, its program removed) must leave the app peer's view too,
// which only a full-set replace expresses.
func TestAReportReplacesTheRelaysWholeView(t *testing.T) {
	s := newSSStack(t)

	token, screenID := ssPair(t, s.playerTS)
	s.player.SetProgram(1, screenID, "rev-live", "scheduled", "content", nil)
	ssAck(t, s.playerTS, token, ssPull(t, s.playerTS, token))
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
// ssPull pulls a program and returns the Lease's own id, so a case can go on to
// acknowledge it the way the shipped player does.
func ssPull(t *testing.T, ts *httptest.Server, token string) string {
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
	var lease struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		t.Fatalf("decode the Lease: %v", err)
	}
	if lease.LeaseID == "" {
		t.Fatal("the Lease carried no lease_id; nothing can be acknowledged")
	}
	return lease.LeaseID
}

// ssAck acknowledges a Lease (PLY-091), which the shipped player does at the end
// of every successful iteration (player-v3/source/Program.brs, wvAckLease).
//
// Fixtures that pull without acking are not modelling this player, and after the
// screen-status model learned to tell "pulled, not yet acknowledged" from
// "quiet" they are not modelling a healthy screen either — an unacknowledged
// pull is precisely what `fetching` means.
func ssAck(t *testing.T, ts *httptest.Server, token, leaseID string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"lease_id": leaseID, "accepted": true})
	if err != nil {
		t.Fatalf("marshal the ack: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/player/v1/lease/ack", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build the ack request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /player/v1/lease/ack: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("lease ack status = %d, want 200 or 204", resp.StatusCode)
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
