package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/screens"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screennow_test.go drives PUSH-NOW (parity row 5.7) and LIVE SCREEN STATUS
// (5.8) over real HTTP, through the same handler a console talks to — and, for
// the push, checks the DURABLE half as well as the response, because a route
// that answered 200 and wrote nothing would satisfy every response assertion
// while changing no screen anywhere.

// storedOverride reads a screen row's `override` member straight out of the
// store — the ONE place a screen override lives (DAT-004c). These tests read it
// rather than trusting the response, because a route that answered 200 and wrote
// nothing would satisfy every response assertion while changing no screen.
func storedOverride(t *testing.T, e *testEnv, screenID string) *datamodel.ScreenOverride {
	t.Helper()
	res, found, err := e.store.Get(context.Background(), store.KindScreen, screenID)
	if err != nil {
		t.Fatalf("read screen %s: %v", screenID, err)
	}
	if !found {
		t.Fatalf("screen %s does not exist", screenID)
	}
	var row struct {
		Override *datamodel.ScreenOverride `json:"override"`
	}
	if err := json.Unmarshal(res.Body, &row); err != nil {
		t.Fatalf("decode screen %s: %v", screenID, err)
	}
	return row.Override
}

// pushBody is the ordinary `play` push body: show this cast until it is cleared.
func pushBody(castID string) map[string]any {
	return map[string]any{"mode": "play", "cast_id": castID}
}

// snFixture creates a screen row and a cast at the env's fixture placement node
// and returns both ids.
func snFixture(t *testing.T, e *testEnv) (screenID, castID, node string) {
	t.Helper()
	node = e.placementNode(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, map[string]any{
		"name": "Lobby", "scope_node": node,
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create screen: %d %s", resp.StatusCode, raw)
	}
	screenID = decodeID(t, raw)

	resp, raw = e.do(t, http.MethodPost, "/api/v1/casts", mustJSON(t, map[string]any{
		"name": "Fire Drill", "scope_node": node,
		"slides": []map[string]any{{
			"id": "s1", "duration_ms": 5000,
			"layers": []map[string]any{
				{"kind": "text", "x": 100, "y": 400, "w": 1720, "h": 200, "text": "FIRE DRILL", "font_px": 160, "color": "#ffffff"},
			},
		}},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create cast: %d %s", resp.StatusCode, raw)
	}
	castID = decodeID(t, raw)
	return screenID, castID, node
}

// TestPushNowWritesTheOverrideAndBumpsTheGeneration is the operation's whole
// job: the response describes the override, and the STORE holds it — which is
// what makes the next signed snapshot carry it and every live relay re-pull.
func TestPushNowWritesTheOverrideAndBumpsTheGeneration(t *testing.T) {
	e := newEnv(t)
	screenID, castID, _ := snFixture(t, e)
	ctx := context.Background()

	before, err := e.store.Generation(ctx)
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}

	resp, raw := e.do(t, http.MethodPut, "/api/v1/screens/"+screenID+"/now",
		mustJSON(t, pushBody(castID)), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT now = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["screen_id"] != screenID || body["cast_id"] != castID || body["source"] != "cast" {
		t.Errorf("response = %v, want the pushed cast on this screen", body)
	}

	// The durable half. Without it the console reports a successful push and no
	// screen anywhere changes.
	stored := storedOverride(t, e, screenID)
	if stored == nil || stored.CastID != castID || stored.Mode != datamodel.ScreenOverrideModePlay {
		t.Fatalf("stored override = %+v, want a play override naming the pushed cast", stored)
	}
	if stored.SetAt == 0 {
		t.Error("stored override carries set_at 0 — the console shows an operator how long an override has been in force from this")
	}
	after, err := e.store.Generation(ctx)
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}
	if after <= before {
		t.Fatalf("generation %d after the push, want > %d — nothing nudges the relays without a new generation, so the screen never learns", after, before)
	}
}

// TestClearNowRemovesTheOverrideAndIsSafeToRepeat: the operator can always get
// back to the schedule, and a second click is not an error.
func TestClearNowRemovesTheOverrideAndIsSafeToRepeat(t *testing.T) {
	e := newEnv(t)
	screenID, castID, _ := snFixture(t, e)

	if resp, raw := e.do(t, http.MethodPut, "/api/v1/screens/"+screenID+"/now",
		mustJSON(t, pushBody(castID)), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT now = %d (%s)", resp.StatusCode, raw)
	}

	for i := range 2 {
		resp, raw := e.do(t, http.MethodDelete, "/api/v1/screens/"+screenID+"/now", nil, nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE now #%d = %d, want 204 (%s)", i+1, resp.StatusCode, raw)
		}
	}
	if got := storedOverride(t, e, screenID); got != nil {
		t.Fatalf("override %+v remains after clearing, want none", got)
	}
}

// TestPushNowRefusesABodyThatNamesNothingUsable covers the three ways a caller
// can be wrong about WHAT to show, each with the status that tells them which.
func TestPushNowRefusesABodyThatNamesNothingUsable(t *testing.T) {
	e := newEnv(t)
	screenID, castID, _ := snFixture(t, e)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"no mode at all", map[string]any{"cast_id": castID}, http.StatusUnprocessableEntity},
		{"an unknown mode", map[string]any{"mode": "takeover", "cast_id": castID}, http.StatusUnprocessableEntity},
		{"neither a cast nor a message", map[string]any{"mode": "play"}, http.StatusUnprocessableEntity},
		{"both at once", map[string]any{"mode": "alert", "cast_id": castID, "message": "evacuate"}, http.StatusUnprocessableEntity},
		{"a message under mode play", map[string]any{"mode": "play", "message": "evacuate"}, http.StatusUnprocessableEntity},
		{"a message over 200 characters", map[string]any{"mode": "alert", "message": strings.Repeat("x", 201)}, http.StatusUnprocessableEntity},
		{"a misspelled member", map[string]any{"mode": "play", "castid": castID}, http.StatusUnprocessableEntity},
		{"a negative ttl", map[string]any{"mode": "play", "cast_id": castID, "ttl_seconds": -1}, http.StatusUnprocessableEntity},
		{"a cast that does not exist", map[string]any{"mode": "play", "cast_id": "01J8ZN0SUCHCASTR0W00000001"}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := e.do(t, http.MethodPut, "/api/v1/screens/"+screenID+"/now", mustJSON(t, tc.body), nil)
			if resp.StatusCode != tc.want {
				t.Fatalf("PUT now with %s = %d, want %d (%s)", tc.name, resp.StatusCode, tc.want, raw)
			}
		})
	}

	// A missing SCREEN is a 404: the request is addressed at a resource that is
	// not there, which is a different fault from a body member naming one.
	if resp, _ := e.do(t, http.MethodPut, "/api/v1/screens/01J8ZN0SUCHSCREENR0W000001/now",
		mustJSON(t, pushBody(castID)), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT now at an unknown screen = %d, want 404", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodDelete, "/api/v1/screens/01J8ZN0SUCHSCREENR0W000001/now", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE now at an unknown screen = %d, want 404", resp.StatusCode)
	}
}

// TestScreenStatusJoinsTheRowTheOverrideAndTheRelaysObservation is 5.8's central
// case AND the proof that 5.7's console affordance has something to render: one
// request returns what the screen is, what a relay has observed of it, and
// whether a human has overridden it.
func TestScreenStatusJoinsTheRowTheOverrideAndTheRelaysObservation(t *testing.T) {
	registry, err := screens.NewRegistry(func() int64 { return fixedNowMs })
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e := newEnvWithOptions(t, api.WithScreenStatus(registry))
	screenID, castID, node := snFixture(t, e)

	// A second screen the relay has NEVER reported — the alarming row, and the
	// one a list built from relay reports alone would be silent about.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, map[string]any{
		"name": "Back Office", "scope_node": node,
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create the second screen: %d %s", resp.StatusCode, raw)
	}
	unseenID := decodeID(t, raw)

	if err := registry.ApplyScreenStatus("relay-1", fixedNowMs-5_000, []wire.ScreenStatusEntry{{
		ScreenID: screenID, Paired: true,
		LastPullAgeMs: 2_000, LastAckAgeMs: 2_100, LastRenderStartAgeMs: 2_500,
		ProgramRevision: "rev-live", Priority: "preempt", Display: "content", ContentCount: 1,
		RenderAssetRef: "sha256:aa",
	}, {
		// A screen no ROW exists for: dropped, because a console cannot name it
		// and an operator has nothing to act on.
		ScreenID: "01J8ZN0R0WF0RTH1SSCREEN001", LastPullAgeMs: 1_000,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}

	if resp, raw := e.do(t, http.MethodPut, "/api/v1/screens/"+screenID+"/now",
		mustJSON(t, pushBody(castID)), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT now = %d (%s)", resp.StatusCode, raw)
	}

	resp, raw = e.do(t, http.MethodGet, "/api/v1/screen-status", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET screen-status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("%d status row(s), want 2 (one per authored screen, the unmatched report dropped): %v", len(page.Items), page.Items)
	}

	byID := map[string]map[string]any{}
	for _, it := range page.Items {
		byID[it["screen_id"].(string)] = it
	}

	live := byID[screenID]
	if live == nil {
		t.Fatalf("no status row for the reported screen: %v", page.Items)
	}
	if live["reachability"] != "live" {
		t.Errorf("reachability = %v, want live (pulled 2s before a report 5s old)", live["reachability"])
	}
	// 2000 reported + 5000 of report age.
	if live["last_pull_age_ms"] != float64(7_000) {
		t.Errorf("last_pull_age_ms = %v, want 7000 — the report's own age must be added, or a disconnected relay's screens read healthy forever", live["last_pull_age_ms"])
	}
	if live["report_age_ms"] != float64(5_000) {
		t.Errorf("report_age_ms = %v, want 5000", live["report_age_ms"])
	}
	if live["name"] != "Lobby" {
		t.Errorf("name = %v, want the authored row's Lobby — the join to the screen row is what makes the row nameable", live["name"])
	}
	if live["render_asset_ref"] != "sha256:aa" {
		t.Errorf("render_asset_ref = %v, want sha256:aa", live["render_asset_ref"])
	}
	now, ok := live["now"].(map[string]any)
	if !ok {
		t.Fatalf("the pushed screen's row carries no `now` member: %v", live)
	}
	if now["cast_id"] != castID || now["source"] != "cast" || now["mode"] != "play" {
		t.Errorf("now = %v, want the pushed cast", now)
	}

	unseen := byID[unseenID]
	if unseen == nil {
		t.Fatalf("no status row for the never-reported screen — the most alarming row on the page is the one that must not be omitted")
	}
	if unseen["reachability"] != "never_seen" {
		t.Errorf("an un-reported screen reads %v, want never_seen", unseen["reachability"])
	}
	for _, field := range []string{"last_pull_age_ms", "last_ack_age_ms", "last_render_start_age_ms", "report_age_ms"} {
		if unseen[field] != float64(-1) {
			t.Errorf("an un-reported screen's %s = %v, want -1 (never) — 0 would read as 'just now', the most misleading possible answer", field, unseen[field])
		}
	}
	if _, has := unseen["now"]; has {
		t.Errorf("an un-pushed screen carries a `now` member: %v", unseen)
	}
}

// TestScreenStatusIsHonestWithNoRelayPlaneWired: the route mounts and answers
// truthfully in a deployment that has no relay read model — every authored
// screen listed, every one never_seen. A 404 or an empty page would both be
// lies of a different kind.
func TestScreenStatusIsHonestWithNoRelayPlaneWired(t *testing.T) {
	e := newEnv(t) // no api.WithScreenStatus
	screenID, _, _ := snFixture(t, e)

	resp, raw := e.do(t, http.MethodGet, "/api/v1/screen-status", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET screen-status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0]["screen_id"] != screenID {
		t.Fatalf("items = %v, want the one authored screen", page.Items)
	}
	if page.Items[0]["reachability"] != "never_seen" {
		t.Errorf("reachability = %v with no relay plane, want never_seen", page.Items[0]["reachability"])
	}
}

// TestScreenStatusPublishesTheThresholdItJudgedBy: an operator (or a different
// console) that disagrees with this platform's live/stale line must be able to
// draw its own, which needs both the raw ages and the threshold.
func TestScreenStatusPublishesTheThresholdItJudgedBy(t *testing.T) {
	e := newEnv(t)
	snFixture(t, e)

	_, raw := e.do(t, http.MethodGet, "/api/v1/screen-status", nil, nil)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := page.Items[0]["live_window_ms"]; got != float64(screens.LiveWindowMs) {
		t.Errorf("live_window_ms = %v, want %d", got, screens.LiveWindowMs)
	}
}

// TestPushNowIsScopedToTheScreensOwnPlacement: pushing content changes what a
// physical display in a physical room shows, so it is authorized as a WRITE at
// the screen row's placement (SEC-005) — and a screen outside the caller's
// visible set answers 404, exactly as an id naming nothing does, so this surface
// cannot be used to probe which screens exist elsewhere in the tree.
func TestPushNowIsScopedToTheScreensOwnPlacement(t *testing.T) {
	e := newEnv(t)
	screenID, castID, _ := snFixture(t, e)

	// A principal whose authority is bound at a DIFFERENT site — one the screen
	// above does not sit under. `owner` there, so nothing observed below can be
	// explained by a coarse role floor: the only variable is WHERE the authority
	// is bound.
	elsewhere := e.createNode(t, datamodel.ScopeNode{
		Kind: "site", ParentID: strp(e.orgRoot(t)), Name: "Elsewhere",
		TZ: strp(siteTZ), Lat: f64p(siteLat), Long: f64p(siteLong),
	})
	outsider := e.principalAt(t, elsewhere)

	resp, _ := e.as(t, outsider, http.MethodPut, "/api/v1/screens/"+screenID+"/now",
		mustJSON(t, pushBody(castID)), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT now at a screen outside the caller's subtree = %d, want 404 (indistinguishable from an id that names nothing)", resp.StatusCode)
	}
	resp, _ = e.as(t, outsider, http.MethodDelete, "/api/v1/screens/"+screenID+"/now", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE now at a screen outside the caller's subtree = %d, want 404", resp.StatusCode)
	}

	// And nothing was written by either refusal.
	if got := storedOverride(t, e, screenID); got != nil {
		t.Fatalf("override %+v written by a refused push, want none", got)
	}

	// The same screen IS pushable by a caller whose authority covers it, so the
	// refusals above are the scoping and not a route that refuses everyone.
	if resp, raw := e.do(t, http.MethodPut, "/api/v1/screens/"+screenID+"/now",
		mustJSON(t, pushBody(castID)), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT now as the root-bound principal = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if got := storedOverride(t, e, screenID); got == nil || got.CastID != castID {
		t.Errorf("stored override = %+v, want the pushed cast", got)
	}
}
