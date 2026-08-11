package api_test

// diagnostics_test.go drives the two operator diagnostics reads through the
// real, authenticated mux (parity row 7.4): GET /api/v1/platform-logs and GET
// /api/v1/system-health.
//
// The claims it holds are the ones that could each be wrong independently:
//
//   - AUTHORIZATION. Neither is invocable by a principal who is not the
//     workspace's owner, however broad their ordinary write authority.
//   - THE LOG IS THE REAL PROCESS LOG. A line written through the ordinary
//     `log` package, by code that knows nothing about the buffer, comes back
//     out of the API. That is the whole "tee, don't instrument" bet, and it is
//     asserted end to end rather than against the buffer in isolation.
//   - THE FILTERS FILTER, AND A BAD ONE IS REFUSED. A typo'd level that
//     silently matched nothing would read as a quiet box.
//   - HEALTH IS DERIVED FROM REAL STATE. The summary's `status` is the worst
//     component grade, the relay list is the relay directory, the screen
//     roll-up counts authored rows joined to relay reports, and the disk
//     numbers come from a real statfs.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/platformlog"
	"github.com/maaxton/waiveo-next/internal/app/screens"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// diagEnv is a live handler with both diagnostics collaborators wired, plus a
// handle on the capture buffer and on the screen-status read model so a case can
// arrange the world the health summary is about to describe.
type diagEnv struct {
	*testEnv
	logs     *platformlog.Buffer
	screens  *screens.Registry
	dataDir  string
	relays   []api.PairingRelay
	nowMs    func() int64
	deployed bool
}

const (
	diagRelayA = "relay-diag-a"
	diagRelayB = "relay-diag-b"
)

// newDiagEnv builds the wired env. relays is captured by the directory closure
// so a case can decide connectivity before the first request.
func newDiagEnv(t *testing.T) *diagEnv {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return fixedNowMs }
	reg, err := screens.NewRegistry(clock)
	if err != nil {
		t.Fatalf("screens.NewRegistry: %v", err)
	}
	env := &diagEnv{
		logs:    platformlog.New(64, clock),
		screens: reg,
		dataDir: t.TempDir(),
		nowMs:   clock,
		// One connected relay by default: the interesting cases are the ones
		// that take it away, and a fixture whose baseline is already degraded
		// cannot show a transition.
		relays: []api.PairingRelay{{RelayID: diagRelayA, AdvertisedAddress: "192.0.2.40:7443"}},
	}

	fixture := newAuthFixture(t)
	content := origin.New()
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.Monotonic(),
		content, testContentBase, fixture.Auth,
		api.WithJobRunner(jobs),
		api.WithScreenStatus(reg),
		api.WithPlatformLog(env.logs),
		api.WithPairing(api.PairingRelayDirectory{
			ConnectedRelays: func() []api.PairingRelay { return env.relays },
			RelaySPKI:       func(string) ([]byte, bool) { return nil, false },
		}),
		api.WithSystemHealth(api.SystemHealthConfig{
			StartedAtMs: fixedNowMs - 90_000,
			Version:     "diag-test",
			DataDir:     env.dataDir,
		}),
	))
	t.Cleanup(ts.Close)
	env.testEnv = &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs}
	return env
}

// logsPage reads GET /platform-logs with the given query as the owner.
func (e *diagEnv) logsPage(t *testing.T, query string) map[string]any {
	t.Helper()
	path := "/api/v1/platform-logs"
	if query != "" {
		path += "?" + query
	}
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", path, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, raw)
	}
	return out
}

// health reads GET /system-health as the owner.
func (e *diagEnv) health(t *testing.T) map[string]any {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/system-health", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /system-health = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, raw)
	}
	return out
}

func itemsOf(t *testing.T, page map[string]any) []map[string]any {
	t.Helper()
	raw, _ := page["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item is %T, want an object", it)
		}
		out = append(out, m)
	}
	return out
}

// serviceNamed returns one named entry of the health summary's services list.
func serviceNamed(t *testing.T, h map[string]any, name string) map[string]any {
	t.Helper()
	svcs, _ := h["services"].([]any)
	for _, s := range svcs {
		m, _ := s.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("no service named %q in %v", name, h["services"])
	return nil
}

// TestALineWrittenThroughTheOrdinaryLoggerReachesTheAPI is the end-to-end claim
// the whole capture design rests on: code that knows nothing about this feature,
// calling `log.Printf`, is diagnosable through the console.
//
// It writes through a *log.Logger teed at the buffer — exactly how the feeder
// installs it — rather than into the buffer directly, so the stdlib's own
// date/time prefix really is in the bytes and really is stripped.
func TestALineWrittenThroughTheOrdinaryLoggerReachesTheAPI(t *testing.T) {
	e := newDiagEnv(t)
	e.orgRoot(t)

	lg := log.New(e.logs, "", log.LstdFlags)
	lg.Printf("waiveo-relay: the ECP command to 192.0.2.31 failed after 12s")

	page := e.logsPage(t, "")
	items := itemsOf(t, page)
	if len(items) != 1 {
		t.Fatalf("got %d record(s), want 1: %v", len(items), items)
	}
	got := items[0]
	if got["source"] != "waiveo-relay" {
		t.Errorf("source = %v, want waiveo-relay", got["source"])
	}
	if got["level"] != "error" {
		t.Errorf("level = %v, want error", got["level"])
	}
	if got["message"] != "the ECP command to 192.0.2.31 failed after 12s" {
		t.Errorf("message = %v", got["message"])
	}
	// The stdlib's local-time prefix must not have survived into the text: the
	// page renders `ts_ms` beside it and two clocks on one row is how an
	// operator correlates the wrong pair of events.
	if raw, _ := got["raw"].(string); raw != "waiveo-relay: the ECP command to 192.0.2.31 failed after 12s" {
		t.Errorf("raw = %q, want the line with the logger's date/time prefix removed", raw)
	}
	if ts, _ := got["ts_ms"].(float64); int64(ts) != fixedNowMs {
		t.Errorf("ts_ms = %v, want the capture clock %d", got["ts_ms"], fixedNowMs)
	}
}

// TestTheLogPagePublishesWhatItIsNotShowing. A diagnostics page that let an
// operator conclude "no errors" from a buffer that had already discarded them
// would be worse than no page at all.
func TestTheLogPagePublishesWhatItIsNotShowing(t *testing.T) {
	e := newDiagEnv(t)
	e.orgRoot(t)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(e.logs, "waiveo-feeder: line %d\n", i)
	}
	page := e.logsPage(t, "limit=5")
	if got := int(page["capacity"].(float64)); got != 64 {
		t.Errorf("capacity = %d, want the buffer's size", got)
	}
	if got := int(page["retained"].(float64)); got != 64 {
		t.Errorf("retained = %d, want 64", got)
	}
	if got := int64(page["dropped"].(float64)); got != 36 {
		t.Errorf("dropped = %d, want 36 — an operator must be able to tell a quiet box from a wrapped buffer", got)
	}
	if got := int(page["matched"].(float64)); got != 64 {
		t.Errorf("matched = %d, want 64 (the page shows 5 OF 64)", got)
	}
	if got := int64(page["retained_from_ms"].(float64)); got != fixedNowMs {
		t.Errorf("retained_from_ms = %d, want the oldest surviving record's instant", got)
	}
	if len(itemsOf(t, page)) != 5 {
		t.Errorf("limit=5 returned %d record(s)", len(itemsOf(t, page)))
	}
}

// TestTheLogFiltersNarrowAndABadLevelIsRefused.
//
// The refusal is the load-bearing half. A level parameter that silently matched
// nothing on a typo would answer an empty page, and an empty diagnostics page
// reads as "the box is quiet" — the opposite of the truth, and the single worst
// answer this surface can give.
func TestTheLogFiltersNarrowAndABadLevelIsRefused(t *testing.T) {
	e := newDiagEnv(t)
	e.orgRoot(t)
	fmt.Fprint(e.logs, "waiveo-feeder: listening on :7420\n")
	fmt.Fprint(e.logs, "waiveo-relay: ECP command failed after 12s\n")
	fmt.Fprint(e.logs, "waiveo-relay: retrying the app peer connection\n")

	if got := int(e.logsPage(t, "level=error")["matched"].(float64)); got != 1 {
		t.Errorf("level=error matched %d, want 1", got)
	}
	if got := int(e.logsPage(t, "source=waiveo-relay")["matched"].(float64)); got != 2 {
		t.Errorf("source=waiveo-relay matched %d, want 2", got)
	}
	if got := int(e.logsPage(t, "contains=ecp")["matched"].(float64)); got != 1 {
		t.Errorf("contains=ecp matched %d, want 1 (case-insensitive)", got)
	}

	resp, raw := e.do(t, http.MethodGet, "/api/v1/platform-logs?level=critical", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("level=critical = %d, want 400 — a silently-non-matching filter reads as a quiet box (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")

	for _, bad := range []string{"limit=0", "limit=1001", "limit=abc"} {
		resp, raw := e.do(t, http.MethodGet, "/api/v1/platform-logs?"+bad, nil, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400 (body %s)", bad, resp.StatusCode, raw)
		}
	}
}

// TestTheSourceListIsTheWHOLEBufferSoAFilterCanBeUndone. A source control built
// from filtered results can only ever offer the option already chosen — a dead
// end an operator cannot leave without editing the URL. This is the console
// dead-end shape, caught at the surface that feeds it.
func TestTheSourceListIsTheWHOLEBufferSoAFilterCanBeUndone(t *testing.T) {
	e := newDiagEnv(t)
	e.orgRoot(t)
	fmt.Fprint(e.logs, "waiveo-feeder: listening\n")
	fmt.Fprint(e.logs, "waiveo-relay: ECP failed\n")
	fmt.Fprint(e.logs, "http: TLS handshake error\n")

	page := e.logsPage(t, "source=waiveo-feeder")
	srcs, _ := page["sources"].([]any)
	if len(srcs) != 3 {
		t.Fatalf("sources under a filter = %v, want all three retained sources", srcs)
	}
	counts, _ := page["level_counts"].(map[string]any)
	if int(counts["error"].(float64)) != 2 {
		t.Errorf("level_counts.error = %v under a source filter, want the buffer's own count (2)", counts["error"])
	}
}

// TestNeitherDiagnosticsReadIsOpenToANonOwner. An admin bound at one site would
// otherwise get the whole deployment's process log and every relay's address —
// a map of a deployment they have authority over one corner of.
func TestNeitherDiagnosticsReadIsOpenToANonOwner(t *testing.T) {
	e := newDiagEnv(t)
	org := e.orgRoot(t)
	site := e.createNode(t, siteUnder(org))
	admin := e.principalWith(t, roleAt{node: site, role: auth.RoleAdmin})

	for _, path := range []string{"/api/v1/platform-logs", "/api/v1/system-health"} {
		resp, raw := e.as(t, admin, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s as a site admin = %d, want 403 (body %s)", path, resp.StatusCode, raw)
		}
	}
	// And an owner bound at the ORG gets both.
	for _, path := range []string{"/api/v1/platform-logs", "/api/v1/system-health"} {
		resp, _ := e.do(t, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s as the owner = %d, want 200", path, resp.StatusCode)
		}
	}
}

// TestHealthReportsTheWorstComponentGrade — the summary must never read `ok`
// while a component reads `down`, because it is the one line an operator trusts.
func TestHealthReportsTheWorstComponentGrade(t *testing.T) {
	e := newDiagEnv(t)
	e.orgRoot(t)

	// Baseline: a relay connected, no screens authored. Nothing is wrong.
	h := e.health(t)
	if h["status"] != "ok" {
		t.Fatalf("baseline status = %v, want ok: %v", h["status"], h["services"])
	}
	if serviceNamed(t, h, "screens")["status"] != "ok" {
		t.Errorf("a box with no screens authored yet is not a fault; it must not greet its owner with a red banner")
	}

	// Take the relays away. The relays are the ONLY path to every screen and
	// device, so this is `down`, and the summary must follow.
	e.relays = nil
	h = e.health(t)
	if got := serviceNamed(t, h, "relay-plane")["status"]; got != "down" {
		t.Errorf("relay-plane with nothing connected = %v, want down", got)
	}
	if h["status"] != "down" {
		t.Errorf("summary status = %v while a component is down, want down", h["status"])
	}
	if relays, _ := h["relays"].([]any); len(relays) != 0 {
		t.Errorf("relays = %v, want empty", relays)
	}
}

// TestHealthListsTheConnectedRelaysWithTheirScreenCounts. A connected relay
// reporting zero screens is a real failure, and a distinct one from a relay that
// is not connected — the list has to be able to show the difference.
func TestHealthListsTheConnectedRelaysWithTheirScreenCounts(t *testing.T) {
	e := newDiagEnv(t)
	org := e.orgRoot(t)
	screenID := e.createScreenRow(t, org, "Lobby")

	e.relays = []api.PairingRelay{
		{RelayID: diagRelayB, AdvertisedAddress: "192.0.2.41:7443"},
		{RelayID: diagRelayA, AdvertisedAddress: "192.0.2.40:7443"},
	}
	if err := e.screens.ApplyScreenStatus(diagRelayA, fixedNowMs, []wire.ScreenStatusEntry{{
		ScreenID: screenID, Paired: true, LastPullAgeMs: 2_000,
		LastAckAgeMs: 2_000, LastRenderStartAgeMs: 2_000,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}

	h := e.health(t)
	relays, _ := h["relays"].([]any)
	if len(relays) != 2 {
		t.Fatalf("relays = %v, want 2", relays)
	}
	first, _ := relays[0].(map[string]any)
	if first["relay_id"] != diagRelayA {
		t.Errorf("relays are not sorted by id: first is %v", first["relay_id"])
	}
	if first["address"] != "192.0.2.40:7443" {
		t.Errorf("address = %v, want the advertised address a player is told to dial", first["address"])
	}
	if int(first["screen_count"].(float64)) != 1 {
		t.Errorf("relay-a screen_count = %v, want 1", first["screen_count"])
	}
	second, _ := relays[1].(map[string]any)
	if int(second["screen_count"].(float64)) != 0 {
		t.Errorf("relay-b screen_count = %v, want 0 — a connected relay reporting nothing must be visible as such", second["screen_count"])
	}
}

// TestTheScreenRollUpCountsAuthoredRowsNotJustReports. A screen no relay has
// ever mentioned is the most alarming row there is, and a roll-up built from
// reports alone is silent about exactly it.
func TestTheScreenRollUpCountsAuthoredRowsNotJustReports(t *testing.T) {
	e := newDiagEnv(t)
	org := e.orgRoot(t)
	live := e.createScreenRow(t, org, "Lobby")
	e.createScreenRow(t, org, "Never switched on")

	if err := e.screens.ApplyScreenStatus(diagRelayA, fixedNowMs, []wire.ScreenStatusEntry{{
		ScreenID: live, Paired: true, LastPullAgeMs: 2_000,
		LastAckAgeMs: screens.NeverObserved, LastRenderStartAgeMs: screens.NeverObserved,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}

	h := e.health(t)
	sc, _ := h["screens"].(map[string]any)
	if int(sc["total"].(float64)) != 2 {
		t.Fatalf("total = %v, want 2 authored rows", sc["total"])
	}
	if int(sc["live"].(float64)) != 1 {
		t.Errorf("live = %v, want 1", sc["live"])
	}
	if int(sc["never_seen"].(float64)) != 1 {
		t.Errorf("never_seen = %v, want 1 — the row a report-only count cannot see", sc["never_seen"])
	}
	if int(sc["paired"].(float64)) != 1 {
		t.Errorf("paired = %v, want 1", sc["paired"])
	}
	if int64(sc["live_window_ms"].(float64)) != screens.LiveWindowMs {
		t.Errorf("live_window_ms = %v, want the threshold the roll-up was decided by (%d)", sc["live_window_ms"], screens.LiveWindowMs)
	}
	if got := serviceNamed(t, h, "screens")["status"]; got != "degraded" {
		t.Errorf("screens service with 1 of 2 live = %v, want degraded", got)
	}
	if h["status"] != "degraded" {
		t.Errorf("summary = %v, want degraded", h["status"])
	}
}

// TestAScreenFetchingContentIsItsOwnRowAndItsOwnCount is the pair-completion for
// the read model's third reachability state: a state the model can produce and
// the surfaces cannot name is a state that reaches an operator as whichever
// neighbour the `default` branch happened to be (here: `never_seen`, for a
// screen that has demonstrably been seen).
//
// It drives ONE fixture through BOTH surfaces — the per-screen list and the
// fleet roll-up — because those are the two places the enum is consumed.
func TestAScreenFetchingContentIsItsOwnRowAndItsOwnCount(t *testing.T) {
	e := newDiagEnv(t)
	org := e.orgRoot(t)
	fetching := e.createScreenRow(t, org, "Lobby (downloading a new video)")
	e.createScreenRow(t, org, "Never switched on")

	// Pulled a Lease well past the live window and not acknowledged it: the
	// player is downloading and verifying content, and the previous program is
	// still on the wall.
	if err := e.screens.ApplyScreenStatus(diagRelayA, fixedNowMs, []wire.ScreenStatusEntry{{
		ScreenID: fetching, Paired: true,
		LastPullAgeMs:        screens.LiveWindowMs + 20_000,
		LastAckAgeMs:         screens.LiveWindowMs + 30_000, // the PREVIOUS cycle's ack
		LastRenderStartAgeMs: screens.NeverObserved,
		// ONE outstanding pull: the fetch is serialised inside the player's poll
		// loop, so a screen materialising content has made no further pull.
		UnackedPulls:    1,
		ProgramRevision: "rev-new", ContentCount: 1,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}

	// The per-screen surface.
	resp, raw := e.do(t, http.MethodGet, "/api/v1/screen-status", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET screen-status = %d (%s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	mustUnmarshal(t, raw, &page)
	var row map[string]any
	for _, it := range page.Items {
		if it["screen_id"] == fetching {
			row = it
		}
	}
	if row == nil {
		t.Fatalf("screen-status did not list %s: %v", fetching, page.Items)
	}
	if row["reachability"] != string(screens.ReachabilityFetching) {
		t.Errorf("reachability = %v, want %q — a screen mid content transfer is neither confirmed live nor a screen to go and look at",
			row["reachability"], screens.ReachabilityFetching)
	}
	if got := int64(row["content_transfer_window_ms"].(float64)); got != screens.ContentTransferWindowMs {
		t.Errorf("content_transfer_window_ms = %d, want %d — the second line the judgement was drawn at must be publishable, or nobody can check it",
			got, screens.ContentTransferWindowMs)
	}
	// The third line, and the count it is read against. `fetching` is decided by
	// three inputs now, and a consumer given two of them cannot redraw the line
	// or tell "downloading a video" from "asking forever and confirming nothing".
	if got := int64(row["fetching_max_unacked_pulls"].(float64)); got != screens.MaxFetchingUnackedPulls {
		t.Errorf("fetching_max_unacked_pulls = %d, want %d", got, screens.MaxFetchingUnackedPulls)
	}
	if got := int(row["unacked_pulls"].(float64)); got != 1 {
		t.Errorf("unacked_pulls = %d, want 1 — the observation the bound is applied to, and the number that tells an operator whether a screen is transferring or failing", got)
	}

	// The fleet roll-up.
	h := e.health(t)
	sc, _ := h["screens"].(map[string]any)
	if int(sc["fetching"].(float64)) != 1 {
		t.Errorf("fetching = %v, want 1", sc["fetching"])
	}
	if int(sc["never_seen"].(float64)) != 1 {
		t.Errorf("never_seen = %v, want 1 (the OTHER screen) — a fetching screen counted here is the default-branch bug this test exists for", sc["never_seen"])
	}
	if int(sc["stale"].(float64)) != 0 || int(sc["live"].(float64)) != 0 {
		t.Errorf("live/stale = %v/%v, want 0/0", sc["live"], sc["stale"])
	}
	if got := int64(sc["content_transfer_window_ms"].(float64)); got != screens.ContentTransferWindowMs {
		t.Errorf("roll-up content_transfer_window_ms = %d, want %d — the roll-up published a `fetching` COUNT and not the line it was counted at, so a consumer that wanted to treat those screens as stale had a number it could not reinterpret",
			got, screens.ContentTransferWindowMs)
	}
	// And the fleet is not `down`: a transferring screen is still talking to its
	// relay and still showing its previous program.
	if got := serviceNamed(t, h, "screens")["status"]; got != "degraded" {
		t.Errorf("screens service with one fetching and one never-seen = %v, want degraded", got)
	}
}

// TestAFleetFailingEveryContentFetchIsDOWN is the roll-up's half of the 2026-08
// finding, and the reason it was not merely a wording bug on a card.
//
// `down` is graded on `Live == 0 && Fetching == 0`. While `fetching` meant only
// "the last pull is unacknowledged", every screen of a site whose content origin
// was unreachable sat permanently in that state — 200 on every program pull, a
// failed fetch, no ack, retry at the backoff cap, age reset every time — so the
// clause could never hold. A whole dark site read `degraded`, indefinitely, and
// the alarm was off for exactly the failure it exists for.
func TestAFleetFailingEveryContentFetchIsDown(t *testing.T) {
	e := newDiagEnv(t)
	org := e.orgRoot(t)
	broken := e.createScreenRow(t, org, "Lobby (403 on every content fetch)")

	// The observation such a screen presents at the top of its backoff sawtooth:
	// a pull comfortably INSIDE the content-transfer window (so no age bound can
	// expire it), never acknowledged, and a long tail of abandoned Leases.
	pullAge := screens.LiveWindowMs + 20_000
	if pullAge > screens.ContentTransferWindowMs {
		t.Fatalf("fixture no longer models the finding: pull age %d is outside the transfer window %d", pullAge, screens.ContentTransferWindowMs)
	}
	if err := e.screens.ApplyScreenStatus(diagRelayA, fixedNowMs, []wire.ScreenStatusEntry{{
		ScreenID: broken, Paired: true,
		LastPullAgeMs:        pullAge,
		LastAckAgeMs:         screens.NeverObserved,
		LastRenderStartAgeMs: screens.NeverObserved,
		UnackedPulls:         31,
		ProgramRevision:      "rev-new", ContentCount: 1,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}

	h := e.health(t)
	sc, _ := h["screens"].(map[string]any)
	if got := int(sc["fetching"].(float64)); got != 0 {
		t.Fatalf("fetching = %d, want 0: a screen that has abandoned 31 Leases is not collecting content, and while it counted as fetching this fleet could never be graded down", got)
	}
	if got := int(sc["stale"].(float64)); got != 1 {
		t.Fatalf("stale = %d, want 1", got)
	}
	svc := serviceNamed(t, h, "screens")
	if svc["status"] != "down" {
		t.Errorf("a fleet whose every screen is failing every content fetch = %v, want down: %v", svc["status"], svc["detail"])
	}
}

// TestHealthMeasuresARealFilesystem. The disk is the failure that actually kills
// this appliance, and a headroom number nobody measured is worse than none.
func TestHealthMeasuresARealFilesystem(t *testing.T) {
	e := newDiagEnv(t)
	e.orgRoot(t)
	h := e.health(t)
	st, _ := h["storage"].(map[string]any)
	if st["path"] != e.dataDir {
		t.Errorf("storage.path = %v, want the wired data dir", st["path"])
	}
	total, ok := st["total_bytes"].(float64)
	if !ok || total <= 0 {
		t.Fatalf("storage.total_bytes = %v; a real filesystem is not zero-sized", st["total_bytes"])
	}
	if free, ok := st["free_bytes"].(float64); !ok || free < 0 || free > total {
		t.Errorf("storage.free_bytes = %v of total %v", st["free_bytes"], total)
	}
	if st["status"] == "unknown" {
		t.Errorf("storage.status = unknown with a real directory wired; the statfs never ran")
	}
}

// TestUptimeAndVersionComeFromTheDeployment, and the uptime is the number that
// most often explains a fleet that all went dark at once.
func TestUptimeAndVersionComeFromTheDeployment(t *testing.T) {
	e := newDiagEnv(t)
	e.orgRoot(t)
	h := e.health(t)
	if got := int64(h["uptime_ms"].(float64)); got != 90_000 {
		t.Errorf("uptime_ms = %d, want 90000", got)
	}
	if h["version"] != "diag-test" {
		t.Errorf("version = %v", h["version"])
	}
	if got := int64(h["checked_at_ms"].(float64)); got != fixedNowMs {
		t.Errorf("checked_at_ms = %d", got)
	}
}

// TestAnUnwiredDeploymentDegradesRatherThan404s: both routes mount, authorize
// and answer honestly with no collaborators — the same posture the device plane
// and the screen-status read model take. The distinguishing fact is `capacity:
// 0`, which is what separates "captures nothing" from "captures, nothing
// happened".
func TestAnUnwiredDeploymentDegradesRatherThan404s(t *testing.T) {
	e := newEnv(t)
	e.orgRoot(t)

	resp, raw := e.do(t, http.MethodGet, "/api/v1/platform-logs", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("platform-logs unwired = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if int(page["capacity"].(float64)) != 0 {
		t.Errorf("capacity = %v with no buffer wired, want 0 — the fact that distinguishes an uncaptured deployment from a quiet one", page["capacity"])
	}
	if items, _ := page["items"].([]any); items == nil {
		t.Error("items is null; an empty list must serialize as [] so no consumer nil-checks a list it can iterate")
	}

	resp, raw = e.do(t, http.MethodGet, "/api/v1/system-health", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("system-health unwired = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var h map[string]any
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := int64(h["uptime_ms"].(float64)); got != screens.NeverObserved {
		t.Errorf("uptime_ms = %d with no start time wired, want the never-observed sentinel %d — never 0, which reads as 'just restarted'", got, screens.NeverObserved)
	}
	st, _ := h["storage"].(map[string]any)
	if st["status"] != "unknown" {
		t.Errorf("storage.status = %v with no data dir wired, want unknown", st["status"])
	}
	if _, present := st["free_bytes"]; present {
		t.Error("free_bytes is present for an unmeasured filesystem; a 0 there renders as a FULL disk and manufactures the emergency this check exists to detect")
	}
}

// TestDiagnosticsAreRefusedWithNoWorkspace. Both are `owner` AT the workspace's
// org node; with no org node there is no workspace to be the owner of, and the
// honest answer is 404 rather than a summary of a deployment that does not
// exist yet.
func TestDiagnosticsAreRefusedWithNoWorkspace(t *testing.T) {
	e := newDiagEnv(t)
	for _, path := range []string{"/api/v1/platform-logs", "/api/v1/system-health"} {
		resp, raw := e.do(t, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with no org node = %d, want 404 (body %s)", path, resp.StatusCode, raw)
		}
	}
}

// createScreenRow creates a screen row at node and returns its id — the authored
// half the health roll-up counts.
func (e *testEnv) createScreenRow(t *testing.T, node, name string) string {
	t.Helper()
	body := mustJSON(t, map[string]any{"scope_node": node, "name": name, "labels": map[string]string{}})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", body, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create screen %q: %d %s", name, resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// TestAFleetThatHasNEVERBeenSeenIsDegradedNotDown — the distinction the read
// model itself is built around, carried into the roll-up's grade.
//
// A screen nobody has ever heard from has not been switched on yet; one that WAS
// being seen and went quiet is a fleet going dark. Collapsing them makes every
// deployment's first hour — screens authored, TVs not yet plugged in — a red
// banner about a failure that has not happened, which is how a health page stops
// being read.
func TestAFleetThatHasNeverBeenSeenIsDegradedNotDown(t *testing.T) {
	e := newDiagEnv(t)
	org := e.orgRoot(t)
	seen := e.createScreenRow(t, org, "Lobby")
	e.createScreenRow(t, org, "Not plugged in yet")

	// Nothing reported at all: two authored screens, both never seen.
	h := e.health(t)
	svc := serviceNamed(t, h, "screens")
	if svc["status"] != "degraded" {
		t.Errorf("a fleet that has never been seen = %v, want degraded (never-seen is waiting, not broken): %v", svc["status"], svc["detail"])
	}
	if detail, _ := svc["detail"].(string); !strings.Contains(detail, "has ever been seen") {
		t.Errorf("detail = %q, want it to say none has ever been seen", detail)
	}

	// Now one of them HAS been seen, and has gone quiet. That is a fleet going
	// dark, and it is `down`.
	// The pull is ACKNOWLEDGED (an ack 500 ms after it), which is what a healthy
	// iteration of the shipped player leaves behind. Without the ack the most
	// recent pull is outstanding, and an outstanding pull is how the read model
	// recognises a screen mid content transfer — `fetching`, not `stale`, which
	// is a different fixture and a different assertion.
	if err := e.screens.ApplyScreenStatus(diagRelayA, fixedNowMs-screens.LiveWindowMs-60_000, []wire.ScreenStatusEntry{{
		ScreenID: seen, Paired: true, LastPullAgeMs: 1_000,
		LastAckAgeMs: 500, LastRenderStartAgeMs: screens.NeverObserved,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}
	h = e.health(t)
	sc, _ := h["screens"].(map[string]any)
	if int(sc["stale"].(float64)) != 1 {
		t.Fatalf("fixture: stale = %v, want 1 (the report must have aged past the live window)", sc["stale"])
	}
	svc = serviceNamed(t, h, "screens")
	if svc["status"] != "down" {
		t.Errorf("a fleet with a screen that WAS seen and went quiet = %v, want down: %v", svc["status"], svc["detail"])
	}
}
