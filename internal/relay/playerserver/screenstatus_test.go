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
