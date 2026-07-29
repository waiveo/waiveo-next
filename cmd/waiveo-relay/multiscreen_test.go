package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// multiscreen_test.go covers what the relay serves once a program is per screen:
// the sites where a schedule resolution CANNOT be attributed to a screen — every
// site that is not exactly one governed scope node and exactly one screen
// (soleServedScreenID) — and the states where a screen has no program at all.
//
// That attribution rule is the whole reason a resolution is no longer written to
// one shared program slot, and it moved the load onto the app-authored per-screen
// baseline: on those sites the baseline is the ONLY thing that carries an
// operator's edit to a screen, so a generation apply that fails to re-install it
// leaves the whole site frozen on whatever it booted with, while the relay logs
// that it applied and resolved the new generation.

// Multi-screen fixture identities: one site, two screen SCOPE NODES (where
// resolution happens, DAT-001) and two screen IDENTITY ROWS (what a program is
// served to and a channel token resolves to, DAT-004a). They are deliberately
// distinct id spaces — the relay is never handed the placement that joins them,
// which is exactly why it must not attribute a resolution here.
const (
	msOrgBoundID   = "01J8ZMULTIORGBOUND000001"
	msSiteID       = "01J8ZMULTISITE0000000001"
	msScopeNodeA   = "01J8ZMULTISC0PEN0DEA00001"
	msScopeNodeB   = "01J8ZMULTISC0PEN0DEB00001"
	msScreenRowA   = "01J8ZMULTISCREENR0WA00001"
	msScreenRowB   = "01J8ZMULTISCREENR0WB00001"
	msScheduleAID  = "01J8ZMULTISCHEDULEA000001"
	msScheduleBID  = "01J8ZMULTISCHEDULEB000001"
	msDaypartAID   = "01J8ZMULTIDAYPARTA000001"
	msDaypartBID   = "01J8ZMULTIDAYPARTB000001"
	msPlaylistAID  = "01J8ZMULTIPLAYLISTA00001"
	msPlaylistBID  = "01J8ZMULTIPLAYLISTB00001"
	msRelayID      = "01J8ZMULTIRELAYIDENTITY01"
	msScheduleeAst = "sha256:5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c"
)

// newPlayerServerWithGrants builds a player/1 server holding grants, with the
// relay's own ed25519 signing identity installed (SetSigningKey) exactly as the
// boot path does — so a pull is answerable whether or not any program is ever
// installed on it.
func newPlayerServerWithGrants(t *testing.T, grants ...wire.PairingGrant) *playerserver.Server {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "waiveo-relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	srv, err := playerserver.NewServer(certPEM, grants, playerserver.WallClockMs)
	if err != nil {
		t.Fatalf("playerserver.NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	return srv
}

// newPlayerHTTP mounts srv's player/1 routes behind the same trace middleware
// the real listener uses, and returns the test server. It is separate from
// pairAndPull because these cases pair ONCE and pull SEVERAL times — a one-time
// grant admits exactly one redemption, and the point is to observe what the same
// paired screen is served before and after a live generation apply.
func newPlayerHTTP(t *testing.T, srv *playerserver.Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(apihttp.WithTraceID(mux))
	t.Cleanup(ts.Close)
	return ts
}

// pairForToken redeems selector against ts and returns the minted channel token.
func pairForToken(t *testing.T, ts *httptest.Server, selector string) string {
	t.Helper()
	body, err := json.Marshal(playerserver.PairingRequest{
		HardwareID:    "hw-" + selector,
		GrantSelector: selector,
		Capabilities:  playerserver.Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
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
		t.Fatalf("pairing with selector %q yielded no channel_token: %+v", selector, pr)
	}
	return pr.ChannelToken
}

// pullProgram pulls /player/v1/program with token and returns the issued Lease.
func pullProgram(t *testing.T, ts *httptest.Server, token string) playerserver.LeaseResponse {
	t.Helper()
	body, err := json.Marshal(playerserver.ProgramPullRequest{
		Capabilities: playerserver.Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
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
	return lease
}

// msScreenNode builds one screen-kind scope node under the fixture site.
func msScreenNode(id, name string) datamodel.ScopeNode {
	parent := msSiteID
	return datamodel.ScopeNode{ID: id, Kind: "screen", ParentID: &parent, Name: name, Revision: 1, CreatedAt: 1, UpdatedAt: 1}
}

// msSiteNode builds the fixture site node, which carries the tz/geo the whole
// subtree resolves against (DAT-034).
func msSiteNode() datamodel.ScopeNode {
	tz := "America/Chicago"
	lat := 41.8781
	long := -87.6298
	parent := msOrgBoundID
	return datamodel.ScopeNode{ID: msSiteID, Kind: "site", ParentID: &parent, Name: "Multi Screen Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1}
}

// msScheduleRows builds an all-day content schedule at scopeNode sourcing a
// one-item playlist, so any instant resolves to display:content with a
// recognisable asset — recognisable being the point: if a resolution ever
// reaches a screen it was not attributed to, the served content says so.
func msScheduleRows(scheduleID, daypartID, playlistID, scopeNode string) (datamodel.Schedule, datamodel.Daypart, datamodel.Playlist) {
	schedule := datamodel.Schedule{ID: scheduleID, ScopeNode: scopeNode, Name: "Schedule " + scopeNode, Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	daypart := datamodel.Daypart{
		ID: daypartID, ScheduleID: scheduleID, ScopeNode: scopeNode,
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "00:00:00", EndTime: "23:59:59",
		DisplayPower: "on", PlaylistID: playlistID, Name: "All Day", Revision: 1, CreatedAt: 1, UpdatedAt: 1,
	}
	playlist := datamodel.Playlist{
		ID: playlistID, ScopeNode: scopeNode, Name: "Playlist " + scopeNode,
		Items:    []datamodel.PlaylistItem{{Source: "asset", AssetRef: msScheduleeAst}},
		Revision: 1, CreatedAt: 1, UpdatedAt: 1,
	}
	return schedule, daypart, playlist
}

// msScreenProgram builds one screen identity row's app-authored `screen_programs`
// entry (REL-061), stamped with gen so successive generations are distinguishable
// on the wire a player actually reads.
func msScreenProgram(screenRowID, label, gen string) wire.ScreenProgram {
	return wire.ScreenProgram{
		ScreenID:        screenRowID,
		ProgramRevision: "rev-" + label + "-" + gen,
		Priority:        "scheduled",
		Display:         "content",
		Content: []wire.ContentRef{{
			AssetRef: "sha256:" + label + gen,
			URL:      "https://origin.example/content/" + label + "-" + gen,
		}},
	}
}

// buildMultiScreenApplied is the two-governed-node, two-screen site: the exact
// topology soleServedScreenID refuses to attribute a resolution on. gen labels
// both the desired-state generation and the app-authored program revisions, so
// "which generation is this screen serving" is directly readable off a Lease.
func buildMultiScreenApplied(t *testing.T, gen int64, genLabel string) desiredstate.Applied {
	t.Helper()

	schedA, dayA, listA := msScheduleRows(msScheduleAID, msDaypartAID, msPlaylistAID, msScopeNodeA)
	schedB, dayB, listB := msScheduleRows(msScheduleBID, msDaypartBID, msPlaylistBID, msScopeNodeB)

	sec := wire.ScheduleSection{
		ScopeNodes: marshalRows(t, msSiteNode(), msScreenNode(msScopeNodeA, "Screen A"), msScreenNode(msScopeNodeB, "Screen B")),
		Schedules:  marshalRows(t, schedA, schedB),
		Dayparts:   marshalRows(t, dayA, dayB),
		Playlists:  marshalRows(t, listA, listB),
	}.Normalized()

	return desiredstate.Applied{
		Generation:     gen,
		Schedule:       sec,
		ScreenPrograms: []wire.ScreenProgram{msScreenProgram(msScreenRowA, "a", genLabel), msScreenProgram(msScreenRowB, "b", genLabel)},
		PairingGrants:  []wire.PairingGrant{twoScreenGrant(msScreenRowA), twoScreenGrant(msScreenRowB)},
	}
}

// newMultiScreenHost builds the automation host a live re-pull tick drives
// alongside the schedule driver (rePuller.tick reloads edge rules through it).
func newMultiScreenHost(t *testing.T) *automationhost.Host {
	t.Helper()
	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host, err := automationhost.New(store, deviceclass.Builtin(), loopbackController{}, loopbackResolver, msRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	return host
}

// TestRePullReinstallsTheAppAuthoredBaselineOnAMultiScreenSite is the
// "an API edit MUST change the resolved program" oracle at the topology where
// nothing else can deliver it (REL-056/REL-061).
//
// A site with two governed scope nodes and two screens gets no schedule
// attribution — resolveAndServe deliberately serves no screen rather than
// guessing which node's resolution belongs to which screen — so the ONLY thing
// that carries a new generation to those screens is that generation's own
// app-authored `screen_programs`. The live re-pull path re-drove the schedule
// and the pairing grants and the edge rules, and never re-installed those, so
// every screen on such a site kept serving the BOOT generation's program for the
// life of the process while the relay logged each new generation as applied and
// resolved. A restart was the only fix, and the logs said nothing was wrong.
//
// It drives the real sequence: boot install + apply, then rePuller.tick over a
// strictly higher generation, and observes through player/1's own pair -> program
// surface with the SAME channel token a screen would keep across the apply.
func TestRePullReinstallsTheAppAuthoredBaselineOnAMultiScreenSite(t *testing.T) {
	srv := newPlayerServerWithGrants(t, twoScreenGrant(msScreenRowA), twoScreenGrant(msScreenRowB))
	host := newMultiScreenHost(t)
	nowMs := demoContentHourInstant(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	driver := &scheduleDriver{
		srv:       srv,
		sink:      fakeScheduleSink(),
		site:      hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298},
		tickEvery: scheduleResolverTickInterval,
	}

	// Boot, exactly as main does it: the persisted per-screen baseline first,
	// then the generation's schedule over the top.
	gen7 := buildMultiScreenApplied(t, 7, "gen7")
	serveAppAuthoredPrograms(srv, gen7.Generation, gen7.ScreenPrograms)
	driver.apply(ctx, gen7, nowMs)

	ts := newPlayerHTTP(t, srv)
	tokenA := pairForToken(t, ts, "grant-offline-"+msScreenRowA)
	tokenB := pairForToken(t, ts, "grant-offline-"+msScreenRowB)

	if got := pullProgram(t, ts, tokenA).ProgramRevision; got != "rev-a-gen7" {
		t.Fatalf("fixture: screen A boots serving %q, want rev-a-gen7", got)
	}
	if got := pullProgram(t, ts, tokenB).ProgramRevision; got != "rev-b-gen7" {
		t.Fatalf("fixture: screen B boots serving %q, want rev-b-gen7", got)
	}

	// The operator's edit: a strictly higher generation, pulled and applied live.
	gen8 := buildMultiScreenApplied(t, 8, "gen8")
	puller := &rePuller{
		pull:    func(int64) (desiredstate.Applied, error) { return gen8, nil },
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: gen7.Generation,
	}
	if applied := puller.tick(ctx); !applied {
		t.Fatal("re-pull tick of a higher generation returned applied=false, want true (REL-056 apply)")
	}

	leaseA := pullProgram(t, ts, tokenA)
	leaseB := pullProgram(t, ts, tokenB)

	if leaseA.ProgramRevision != "rev-a-gen8" {
		t.Errorf("screen A serves program_revision %q after generation 8 was applied, want rev-a-gen8 — the applied generation never reached this screen, and only a relay restart would fix it", leaseA.ProgramRevision)
	}
	if leaseB.ProgramRevision != "rev-b-gen8" {
		t.Errorf("screen B serves program_revision %q after generation 8 was applied, want rev-b-gen8", leaseB.ProgramRevision)
	}

	// The CONTENT has to move too: a revision bump a player sees while the
	// content pointer behind it is stale is the same stale screen with a fresh
	// label on it.
	if len(leaseA.Content) != 1 || leaseA.Content[0].URL != "https://origin.example/content/a-gen8" {
		t.Errorf("screen A content = %+v, want the generation-8 asset — the new generation's content never reached the screen", leaseA.Content)
	}
	if len(leaseB.Content) != 1 || leaseB.Content[0].URL != "https://origin.example/content/b-gen8" {
		t.Errorf("screen B content = %+v, want the generation-8 asset", leaseB.Content)
	}

	// And each screen still has its OWN program: a re-install that reached the
	// screens by giving them all one entry would satisfy every check above.
	if leaseA.ProgramRevision == leaseB.ProgramRevision {
		t.Errorf("both screens serve program_revision %q — the per-screen baseline collapsed into one shared program", leaseA.ProgramRevision)
	}
}

// TestSoleServedScreenIDAttributesOnlyWhenTheJoinIsForced pins the decision the
// whole per-screen serve path turns on: WHICH screen a governed scope node's
// schedule resolution may be served to.
//
// Resolution happens at a scope node; a program is served to a screen identity
// row; the join between them is a screen row's own `scope_node` placement, which
// the app peer has and the relay is never sent (REL-065 carries scheduling-core
// rows and scope nodes, no screen identity rows). So an answer is admissible
// only where the inputs FORCE it — one governed node, one screen — and anywhere
// else the honest answer is "I cannot tell", because the alternative is putting
// one screen's schedule on another screen and being unable to notice.
func TestSoleServedScreenIDAttributesOnlyWhenTheJoinIsForced(t *testing.T) {
	prog := func(ids ...string) []wire.ScreenProgram {
		out := make([]wire.ScreenProgram, 0, len(ids))
		for _, id := range ids {
			out = append(out, wire.ScreenProgram{ScreenID: id})
		}
		return out
	}

	cases := []struct {
		name     string
		governed []string
		programs []wire.ScreenProgram
		want     string
	}{
		{"one governed node, one screen — forced", []string{msScopeNodeA}, prog(msScreenRowA), msScreenRowA},
		{"one governed node, no screens — nothing to serve", []string{msScopeNodeA}, nil, ""},
		{"one governed node, two screens — which screen is this node's?", []string{msScopeNodeA}, prog(msScreenRowA, msScreenRowB), ""},
		{"two governed nodes, one screen — which node is this screen's?", []string{msScopeNodeA, msScopeNodeB}, prog(msScreenRowA), ""},
		{"two governed nodes, two screens — neither side forced", []string{msScopeNodeA, msScopeNodeB}, prog(msScreenRowA, msScreenRowB), ""},
		{"no governed node at all", nil, prog(msScreenRowA), ""},
		// A duplicate entry still describes ONE screen. Counting entries rather
		// than distinct ids made a genuinely single-screen site read as
		// ambiguous and silently lose its schedule attribution.
		{"one governed node, two entries for the SAME screen", []string{msScopeNodeA}, prog(msScreenRowA, msScreenRowA), msScreenRowA},
		{"entries around a duplicate", []string{msScopeNodeA}, prog(msScreenRowA, msScreenRowA, msScreenRowB), ""},
		// An entry naming no screen is not a candidate: no channel token ever
		// resolves to an empty screen_id, so counting it would make a
		// one-real-screen site look ambiguous.
		{"one governed node, one screen plus an entry naming none", []string{msScopeNodeA}, prog("", msScreenRowA, ""), msScreenRowA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := soleServedScreenID(tc.governed, tc.programs); got != tc.want {
				t.Errorf("soleServedScreenID(%d node(s), %d entry/entries) = %q, want %q", len(tc.governed), len(tc.programs), got, tc.want)
			}
		})
	}
}

// TestUnattributableResolutionLeavesEveryScreenOnItsAppAuthoredProgram is the
// same refusal observed where it matters — on the wire, at what a paired screen
// is actually served — rather than at the helper that decides it.
//
// Both shapes are covered because they are different mistakes with the same
// consequence: several governed nodes and one screen (whose resolution is this?)
// and one governed node with several screens (which screen is this for?). In
// both, serving the resolution means a screen showing a schedule that was never
// authored for it, and nothing in the system would report that — the schedule
// resolver logs OK either way.
func TestUnattributableResolutionLeavesEveryScreenOnItsAppAuthoredProgram(t *testing.T) {
	site := hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298}
	nowMs := demoContentHourInstant(t)

	t.Run("several governed nodes, one screen", func(t *testing.T) {
		srv := newPlayerServerWithGrants(t, twoScreenGrant(msScreenRowA))
		applied := buildMultiScreenApplied(t, 3, "gen3")
		applied.ScreenPrograms = []wire.ScreenProgram{msScreenProgram(msScreenRowA, "a", "gen3")}

		serveAppAuthoredPrograms(srv, applied.Generation, applied.ScreenPrograms)
		resolvers := bootScheduleResolverAt(applied, srv, fakeScheduleSink(), site, nowMs)
		if len(resolvers) != 2 {
			t.Fatalf("fixture: built %d resolver(s), want 2 governed scope nodes", len(resolvers))
		}

		ts := newPlayerHTTP(t, srv)
		lease := pullProgram(t, ts, pairForToken(t, ts, "grant-offline-"+msScreenRowA))
		if lease.ProgramRevision != "rev-a-gen3" {
			t.Errorf("screen A serves %q, want its own app-authored rev-a-gen3 — with several governed scope nodes the relay cannot tell whose resolution this is, and served one anyway", lease.ProgramRevision)
		}
	})

	t.Run("one governed node, several screens", func(t *testing.T) {
		srv := newPlayerServerWithGrants(t, twoScreenGrant(msScreenRowA), twoScreenGrant(msScreenRowB))
		applied := buildMultiScreenApplied(t, 3, "gen3")
		// One governed node: drop screen B's schedule rows, keep both screens.
		schedA, dayA, listA := msScheduleRows(msScheduleAID, msDaypartAID, msPlaylistAID, msScopeNodeA)
		applied.Schedule = wire.ScheduleSection{
			ScopeNodes: marshalRows(t, msSiteNode(), msScreenNode(msScopeNodeA, "Screen A"), msScreenNode(msScopeNodeB, "Screen B")),
			Schedules:  marshalRows(t, schedA),
			Dayparts:   marshalRows(t, dayA),
			Playlists:  marshalRows(t, listA),
		}.Normalized()

		serveAppAuthoredPrograms(srv, applied.Generation, applied.ScreenPrograms)
		resolvers := bootScheduleResolverAt(applied, srv, fakeScheduleSink(), site, nowMs)
		if len(resolvers) != 1 {
			t.Fatalf("fixture: built %d resolver(s), want exactly 1 governed scope node", len(resolvers))
		}

		ts := newPlayerHTTP(t, srv)
		leaseA := pullProgram(t, ts, pairForToken(t, ts, "grant-offline-"+msScreenRowA))
		leaseB := pullProgram(t, ts, pairForToken(t, ts, "grant-offline-"+msScreenRowB))
		if leaseA.ProgramRevision != "rev-a-gen3" {
			t.Errorf("screen A serves %q, want its own app-authored rev-a-gen3 — with two screens carried, the one governed node's resolution was served to whichever screen came first", leaseA.ProgramRevision)
		}
		if leaseB.ProgramRevision != "rev-b-gen3" {
			t.Errorf("screen B serves %q, want its own app-authored rev-b-gen3", leaseB.ProgramRevision)
		}
	})
}

// TestBootWithNoInstallableScreenProgramServesTheTerminalDefault is the boot
// shape a relay reaches with nothing to install: `screen_programs` empty
// (REL-060's own placeholder), or carrying only entries that name no screen and
// so can never be served to one.
//
// A paired screen pulling against that relay must be answered with
// data-model/1's terminal default (DAT-118) — the defined blank state — and not
// with a failure. It answered 500 INTERNAL until the relay's signing identity
// stopped being a side effect of installing a program: no program meant no key,
// and no key meant no Lease of any kind, including the one the relay can always
// produce.
func TestBootWithNoInstallableScreenProgramServesTheTerminalDefault(t *testing.T) {
	cases := []struct {
		name   string
		served []wire.ScreenProgram
	}{
		{"no screen_programs at all", nil},
		{"only entries naming no screen", []wire.ScreenProgram{
			{ScreenID: "", ProgramRevision: "rev-orphan", Priority: "scheduled", Display: "content"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newPlayerServerWithGrants(t, twoScreenGrant(msScreenRowA))
			serveAppAuthoredPrograms(srv, 5, tc.served)

			ts := newPlayerHTTP(t, srv)
			lease := pullProgram(t, ts, pairForToken(t, ts, "grant-offline-"+msScreenRowA))

			if lease.ProgramRevision != playerserver.TerminalProgramRevision {
				t.Errorf("program_revision = %q, want the DAT-118 terminal sentinel %q", lease.ProgramRevision, playerserver.TerminalProgramRevision)
			}
			if lease.Display != "blank" || len(lease.Content) != 0 {
				t.Errorf("display/content = %q/%+v, want blank with no content (DAT-118)", lease.Display, lease.Content)
			}
			if lease.Signature == "" {
				t.Error("terminal-default Lease carries no signature; PLY-090 admits no unsigned Lease")
			}
		})
	}
}
