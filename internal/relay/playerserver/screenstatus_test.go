package playerserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screenstatus_test.go drives parity row 5.8's relay half through the REAL
// player/1 surface — pair, pull, ack, render/start over HTTP — rather than by
// poking the observation record directly. That matters because the whole claim
// of this feature is that the status is a by-product of serving a screen: a test
// that called the recorders itself would still pass if they were never wired
// into the handlers, which is precisely the "surface that accepts work it never
// performs" shape.

// statusClock is a settable clock so a test can advance time between a pull and
// a snapshot and observe the age grow, rather than sleeping.
type statusClock struct{ ms atomic.Int64 }

func (c *statusClock) now() int64  { return c.ms.Load() }
func (c *statusClock) set(v int64) { c.ms.Store(v) }

// newStatusServer builds a player server on a settable clock, with a redeemable
// grant and the relay's own Lease-signing identity installed, mounted on an HTTP
// test surface — the same construction the real boot path performs.
func newStatusServer(t *testing.T, clk *statusClock) (*Server, *httptest.Server, wire.PairingGrant) {
	t.Helper()
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrant()
	grant.IssuedAt = clk.now()
	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, clk.now)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	return srv, newPairingTestServer(t, srv), grant
}

// statusPair redeems the grant and returns the minted channel token and the
// screen_id it resolves to.
func statusPair(t *testing.T, ts *httptest.Server, grant wire.PairingGrant) (token, screenID string) {
	t.Helper()
	body, err := json.Marshal(PairingRequest{
		HardwareID:    "hw-status",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "slide"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	resp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	defer resp.Body.Close()
	var out PairingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode pairing response: %v", err)
	}
	if out.ChannelToken == "" {
		t.Fatalf("pairing yielded no channel token: %+v", out)
	}
	return out.ChannelToken, out.ScreenID
}

// statusPost sends an authenticated POST to a player/1 route and asserts 200.
func statusPost(t *testing.T, ts *httptest.Server, token, path string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s body: %v", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build %s request: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200", path, resp.StatusCode)
	}
}

// statusPull performs an authenticated program pull and returns the Lease.
func statusPull(t *testing.T, ts *httptest.Server, token string) LeaseResponse {
	t.Helper()
	raw, err := json.Marshal(ProgramPullRequest{
		Capabilities: Capabilities{ContentTypes: []string{"image", "slide"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal program pull: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/player/v1/program", bytes.NewReader(raw))
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
	var lease LeaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	return lease
}

// statusOf returns the single status entry for screenID, or fails.
func statusOf(t *testing.T, srv *Server, screenID string) ScreenStatus {
	t.Helper()
	for _, st := range srv.ScreenStatuses() {
		if st.ScreenID == screenID {
			return st
		}
	}
	t.Fatalf("no screen status for %q; got %+v", screenID, srv.ScreenStatuses())
	return ScreenStatus{}
}

// TestScreenStatusRecordsTheRealPlayerFlow is the capability's central case: a
// screen pairs, pulls, acknowledges and reports a render, and every one of those
// four facts is observable afterwards — with ages measured against a clock the
// test advances, so an age that was merely stamped at snapshot time would read 0
// and fail.
func TestScreenStatusRecordsTheRealPlayerFlow(t *testing.T) {
	clk := &statusClock{}
	clk.set(1_700_000_000_000)
	srv, ts, grant := newStatusServer(t, clk)

	token, screenID := statusPair(t, ts, grant)

	// Before any pull: the relay knows the screen (it holds a live session) but
	// has never been contacted by its player. Distinguishing those two is the
	// whole point of the `paired` field.
	pre := statusOf(t, srv, screenID)
	if !pre.Paired {
		t.Error("paired = false immediately after redemption, want true")
	}
	if pre.LastPullAgeMs != neverObserved {
		t.Errorf("last_pull age before any pull = %d, want %d (never)", pre.LastPullAgeMs, neverObserved)
	}

	srv.SetProgram(1, screenID, "rev-1", PriorityScheduled, DisplayContent, []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa", URL: "https://origin.example/content/aa"},
	})

	clk.set(1_700_000_010_000)
	lease := statusPull(t, ts, token)

	clk.set(1_700_000_011_000)
	statusPost(t, ts, token, "/player/v1/lease/ack", LeaseAckRequest{LeaseID: lease.LeaseID, Accepted: true})

	clk.set(1_700_000_012_000)
	statusPost(t, ts, token, "/player/v1/render/start", RenderStartRequest{
		LeaseID: lease.LeaseID, AssetRef: "sha256:aa", TS: clk.now(),
	})

	// Snapshot five seconds after the render report.
	clk.set(1_700_000_017_000)
	got := statusOf(t, srv, screenID)

	if got.LastPullAgeMs != 7_000 {
		t.Errorf("last_pull age = %dms, want 7000 (pulled at +10s, read at +17s)", got.LastPullAgeMs)
	}
	if got.LastAckAgeMs != 6_000 {
		t.Errorf("last_ack age = %dms, want 6000", got.LastAckAgeMs)
	}
	if got.LastRenderStartAgeMs != 5_000 {
		t.Errorf("last_render_start age = %dms, want 5000", got.LastRenderStartAgeMs)
	}
	if got.ProgramRevision != "rev-1" || got.Priority != PriorityScheduled || got.Display != DisplayContent {
		t.Errorf("status describes program %q/%q/%q, want rev-1/scheduled/content", got.ProgramRevision, got.Priority, got.Display)
	}
	if got.ContentCount != 1 {
		t.Errorf("content_count = %d, want 1", got.ContentCount)
	}
	if got.RenderAssetRef != "sha256:aa" {
		t.Errorf("render_asset_ref = %q, want sha256:aa — the one field that is evidence of playback rather than of intent", got.RenderAssetRef)
	}
}

// TestScreenStatusReportsWhatTheScreenWasHandedNotWhatIsWaiting: the status
// describes the program the screen actually RECEIVED. A status read off the
// currently served program instead would report a screen as showing content it
// has never collected, which is wrong exactly while a screen is failing — the
// only time anybody reads this surface.
func TestScreenStatusReportsWhatTheScreenWasHandedNotWhatIsWaiting(t *testing.T) {
	clk := &statusClock{}
	clk.set(1_700_000_000_000)
	srv, ts, grant := newStatusServer(t, clk)
	token, screenID := statusPair(t, ts, grant)

	srv.SetProgram(1, screenID, "rev-collected", PriorityScheduled, DisplayContent, []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa", URL: "https://origin.example/content/aa"},
	})
	statusPull(t, ts, token)

	// An operator edits the screen's content; the screen has not polled since.
	srv.SetProgram(2, screenID, "rev-waiting", PriorityPreempt, DisplayContent, []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:bb", URL: "https://origin.example/content/bb"},
		{Type: "image", AssetRef: "sha256:cc", URL: "https://origin.example/content/cc"},
	})

	got := statusOf(t, srv, screenID)
	if got.ProgramRevision != "rev-collected" {
		t.Errorf("status reports program_revision %q, want rev-collected — it must describe what the screen HAS, not what is waiting for it", got.ProgramRevision)
	}
	if got.ContentCount != 1 {
		t.Errorf("content_count = %d, want 1 (the collected program's), not the waiting program's 2", got.ContentCount)
	}
}

// TestScreenStatusFiltersByWhatThePlayerCanDraw: a player that declares only
// `image` and is served a slide-only program receives an EMPTY Lease, and the
// status has to say so. Reading the content count off the served program instead
// would report three slides on a screen showing nothing.
func TestScreenStatusFiltersByWhatThePlayerCanDraw(t *testing.T) {
	clk := &statusClock{}
	clk.set(1_700_000_000_000)
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrant()
	grant.IssuedAt = clk.now()
	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, clk.now)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	ts := newPairingTestServer(t, srv)

	// This player declares `image` only — no `slide`.
	body, err := json.Marshal(PairingRequest{
		HardwareID:    "hw-old",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	resp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	defer resp.Body.Close()
	var pr PairingResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode pairing response: %v", err)
	}

	srv.SetProgram(1, pr.ScreenID, "rev-slides", PriorityScheduled, DisplayContent, []wire.LeaseContent{
		{Type: "slide"}, {Type: "slide"}, {Type: "slide"},
	})

	// Pull declaring `image` only, matching the pairing declaration.
	raw, err := json.Marshal(ProgramPullRequest{Capabilities: Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"}})
	if err != nil {
		t.Fatalf("marshal pull: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/player/v1/program", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build pull: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+pr.ChannelToken)
	pullResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	defer pullResp.Body.Close()

	got := statusOf(t, srv, pr.ScreenID)
	if got.ContentCount != 0 {
		t.Errorf("content_count = %d for a player that can draw none of the served items, want 0 — the status must describe the Lease that was handed over, not the program behind it", got.ContentCount)
	}
	// …and it owes no acknowledgement for it. The same reasoning the count rests
	// on applies here: a player handed nothing it can draw fetches nothing, acks
	// nothing, and must not be accumulating outstanding pulls for it.
	if got.UnackedPulls != 0 {
		t.Errorf("unacked_pulls = %d after a pull the capability filter emptied, want 0 — there is nothing in that Lease to confirm, so counting it makes the screen look like it is failing to fetch content it was never sent", got.UnackedPulls)
	}
}

// TestABlankLeaseIsNotAnOutstandingPull is the first-ever-transfer defect, at the
// place the count is kept.
//
// A Lease with no content gets no acknowledgement — the shipped player returns
// from wvDoProgram before wvAckLease when the content array is empty
// (Program.brs), exactly as it does when a fetch fails — so every blank Lease
// this counted was an outstanding pull that could never be discharged. And blank
// Leases are the ORDINARY case twice over: terminalDefault is what a paired
// screen pulls until an operator assigns it a program, and a scheduled `blank`
// is the same empty array every night.
//
// The consequence is at the far end of the pipeline: past the tolerance the app
// peer's read model calls the screen `stale` (internal/app/screens) and the fleet
// roll-up grades a one-screen site `down` (internal/app/api/diagnostics.go) —
// while that screen is downloading its first video perfectly normally. This test
// is the relay-side half; the whole-stack pin is relayconn's
// TestAFirstEverTransferAfterBlankLeasesIsNotStale.
func TestABlankLeaseIsNotAnOutstandingPull(t *testing.T) {
	clk := &statusClock{}
	clk.set(1_700_000_000_000)
	srv, ts, grant := newStatusServer(t, clk)
	token, screenID := statusPair(t, ts, grant)

	// No program at all: every pull is answered with terminalDefault, the blank
	// Lease DAT-118 defines for a screen the relay holds nothing for.
	for i := 0; i < 4; i++ {
		clk.set(1_700_000_000_000 + int64(i)*2_000)
		lease := statusPull(t, ts, token)
		if lease.ProgramRevision != TerminalProgramRevision {
			t.Fatalf("pull %d was answered with %q, want the terminal default — this test is no longer exercising the blank Lease", i, lease.ProgramRevision)
		}
	}
	if got := statusOf(t, srv, screenID); got.UnackedPulls != 0 {
		t.Fatalf("unacked_pulls = %d after 4 terminal-default pulls, want 0.\n"+
			"A blank Lease carries nothing to fetch and gets no ack, so counting one is counting an obligation the screen does not have — "+
			"and this is what every freshly paired screen pulls until it is given a program.", got.UnackedPulls)
	}

	// A scheduled `blank` program is the same shape from the other direction: an
	// authored program, with an empty content array.
	srv.SetProgram(1, screenID, "rev-overnight", PriorityScheduled, DisplayBlank, nil)
	clk.set(1_700_000_010_000)
	statusPull(t, ts, token)
	if got := statusOf(t, srv, screenID); got.UnackedPulls != 0 {
		t.Fatalf("unacked_pulls = %d after a scheduled-blank pull, want 0 — this one happens on a schedule, every night, on every screen that is switched off overnight", got.UnackedPulls)
	}

	// Now the operator assigns the screen its first real program. Exactly ONE
	// Lease is in flight, and the count has to say so: this is the number the
	// read model reads as "transferring" rather than "failing".
	srv.SetProgram(2, screenID, "rev-first", PriorityScheduled, DisplayContent, []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:vv", URL: "https://origin.example/content/vv"},
	})
	clk.set(1_700_000_020_000)
	statusPull(t, ts, token)
	got := statusOf(t, srv, screenID)
	if got.UnackedPulls != 1 {
		t.Fatalf("unacked_pulls = %d during the screen's first real transfer, want exactly 1 (wire.OutstandingPullsWhileTransferring).\n"+
			"Anything above the tolerance reads `stale` at the console for a screen that is doing its job.", got.UnackedPulls)
	}
	if int64(got.UnackedPulls) > wire.ScreenFetchingMaxUnackedPulls {
		t.Fatalf("unacked_pulls = %d is past the %d the read model tolerates: the first content a box ever collects would be reported as a failure",
			got.UnackedPulls, wire.ScreenFetchingMaxUnackedPulls)
	}
}

// TestAnAckClearsTheOutstandingPullCount is the RESET half of the count, which
// had no coverage at all: deleting `l.unackedPulls = 0` from noteLeaseAck left
// the whole suite green, and a count that only ever climbs turns every screen
// stale after two ordinary program changes.
//
// Driven over the real /player/v1/lease/ack route rather than by calling the
// recorder, for the reason the top of this file gives: a reset wired to nothing
// looks exactly like a reset.
func TestAnAckClearsTheOutstandingPullCount(t *testing.T) {
	clk := &statusClock{}
	clk.set(1_700_000_000_000)
	srv, ts, grant := newStatusServer(t, clk)
	token, screenID := statusPair(t, ts, grant)

	srv.SetProgram(1, screenID, "rev-1", PriorityScheduled, DisplayContent, []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa", URL: "https://origin.example/content/aa"},
	})

	// Two pulls, neither acknowledged: the count is the only signal that says so,
	// because the pull AGE resets on every retry.
	clk.set(1_700_000_002_000)
	statusPull(t, ts, token)
	clk.set(1_700_000_006_000)
	lease := statusPull(t, ts, token)
	if got := statusOf(t, srv, screenID); got.UnackedPulls != 2 {
		t.Fatalf("unacked_pulls = %d after two unacknowledged pulls, want 2 — the increment half is what this test's reset is measured against", got.UnackedPulls)
	}

	// The screen finishes the transfer and acknowledges.
	clk.set(1_700_000_008_000)
	statusPost(t, ts, token, "/player/v1/lease/ack", LeaseAckRequest{LeaseID: lease.LeaseID, Accepted: true})

	got := statusOf(t, srv, screenID)
	if got.UnackedPulls != 0 {
		t.Fatalf("unacked_pulls = %d after an acknowledgement, want 0.\n"+
			"The count means 'content-bearing pulls served since the last ack'. If an ack does not clear it, it becomes a lifetime pull "+
			"counter: every screen crosses the %d-pull tolerance after a couple of ordinary program changes and reads `stale` forever, "+
			"which is W2-18's symptom with the opposite cause.", got.UnackedPulls, wire.ScreenFetchingMaxUnackedPulls)
	}
	if got.LastAckAgeMs != 0 {
		t.Errorf("last_ack age = %d immediately after the ack, want 0", got.LastAckAgeMs)
	}

	// And it counts again from there — a reset that also disabled the counter
	// would satisfy the assertion above and hide the failing screen instead.
	clk.set(1_700_000_012_000)
	statusPull(t, ts, token)
	if again := statusOf(t, srv, screenID); again.UnackedPulls != 1 {
		t.Fatalf("unacked_pulls = %d for the pull AFTER the ack, want 1 — the reset must clear the count, not switch the counter off", again.UnackedPulls)
	}
}

// TestScreenStatusIncludesAConfiguredScreenThatHasNeverPulled is the alarming
// row an operator most needs, and the one a report built from observations alone
// would be silent about: a screen the relay is holding a program for that has
// never once come to collect it.
func TestScreenStatusIncludesAConfiguredScreenThatHasNeverPulled(t *testing.T) {
	clk := &statusClock{}
	clk.set(1_700_000_000_000)
	certPEM, _, _, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, nil, clk.now)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetProgram(1, "screen-never-seen", "rev-waiting", PriorityScheduled, DisplayContent, []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa"},
	})

	got := statusOf(t, srv, "screen-never-seen")
	if got.Paired {
		t.Error("paired = true for a screen holding no session, want false")
	}
	if got.LastPullAgeMs != neverObserved || got.LastAckAgeMs != neverObserved || got.LastRenderStartAgeMs != neverObserved {
		t.Errorf("a never-contacted screen reports ages (%d, %d, %d), want all %d",
			got.LastPullAgeMs, got.LastAckAgeMs, got.LastRenderStartAgeMs, neverObserved)
	}
	// It still says what the screen is CONFIGURED to show — "configured to show
	// X, never collected it" is a far more actionable card than a blank one.
	if got.ProgramRevision != "rev-waiting" || got.ContentCount != 1 {
		t.Errorf("a never-contacted screen reports program %q with %d item(s), want rev-waiting with 1",
			got.ProgramRevision, got.ContentCount)
	}
}

// TestScreenStatusesAreOrderedAndPerScreen: two screens' statuses do not bleed
// into one another, and the list order is stable so a console table does not
// reshuffle every ten seconds.
func TestScreenStatusesAreOrderedAndPerScreen(t *testing.T) {
	clk := &statusClock{}
	clk.set(1_700_000_000_000)
	certPEM, _, _, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, nil, clk.now)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetProgram(1, "screen-b", "rev-b", PriorityScheduled, DisplayContent, nil)
	srv.SetProgram(1, "screen-a", "rev-a", PriorityPreempt, DisplayContent, nil)

	got := srv.ScreenStatuses()
	if len(got) != 2 {
		t.Fatalf("%d status entries, want 2: %+v", len(got), got)
	}
	if got[0].ScreenID != "screen-a" || got[1].ScreenID != "screen-b" {
		t.Fatalf("statuses came back in order %q, %q — want screen_id order", got[0].ScreenID, got[1].ScreenID)
	}
	if got[0].Priority != PriorityPreempt || got[1].Priority != PriorityScheduled {
		t.Errorf("priorities crossed between screens: %q / %q", got[0].Priority, got[1].Priority)
	}
}
