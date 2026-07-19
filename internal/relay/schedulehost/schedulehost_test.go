package schedulehost

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// demoScreenScopeNodeID is the SAME fixture-ULID the feeder's own demo
// schedule (internal/feeder/snapshot, REL-065) attaches its schedule row to
// (its own demoScreenScopeNodeID constant is unexported, so this is a
// byte-exact copy — the cross-package fixture-sharing pattern this codebase
// already uses, e.g. internal/relay/automationhost's testScreenEntity).
const demoScreenScopeNodeID = "01J8Z4DEMOSCREENFIRSTPHOTN"

// buildDemoSection runs the real feeder Build path to obtain the exact
// wire.ScheduleSection a relay receives over the wire (REL-065) — a
// genuinely valid, well-formed section, not a hand-rolled approximation, so
// BuildStore is exercised against the same rows Task 2 authored.
func buildDemoSection(t *testing.T) wire.ScheduleSection {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	snap, err := snapshot.Build([]byte("fixture-image-bytes"), "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	return snap.Sections.Schedule
}

// TestBuildStoreWellFormedDemoSectionGoverns asserts a well-formed carried
// schedule section (the feeder's real demo schedule) builds a RowStore with
// ZERO errors, and Governs reports true for the screen the demo schedule is
// attached to (the stated additive serving policy, Global Constraints).
func TestBuildStoreWellFormedDemoSectionGoverns(t *testing.T) {
	sec := buildDemoSection(t)

	store, errs := BuildStore(sec)
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}
	if len(store.Rows.Schedules) == 0 {
		t.Fatalf("BuildStore(demo section) produced zero schedules, want the demo schedule")
	}
	if !Governs(store, demoScreenScopeNodeID) {
		t.Errorf("Governs(store, %q) = false, want true (the demo schedule is attached to this screen)", demoScreenScopeNodeID)
	}
}

// TestBuildStoreEmptySectionNotGoverned asserts a zero-valued (never-carried)
// schedule section builds a usable, empty RowStore with zero errors, and
// Governs reports false for any screen — today's first-photon state, where
// the relay must keep serving the app-authored screen_programs unchanged.
func TestBuildStoreEmptySectionNotGoverned(t *testing.T) {
	store, errs := BuildStore(wire.ScheduleSection{})
	if len(errs) != 0 {
		t.Fatalf("BuildStore(empty section) errs = %+v, want none", errs)
	}
	if len(store.Rows.Schedules) != 0 {
		t.Fatalf("BuildStore(empty section) produced %d schedules, want 0", len(store.Rows.Schedules))
	}
	if Governs(store, demoScreenScopeNodeID) {
		t.Errorf("Governs(store, %q) = true on an empty section, want false", demoScreenScopeNodeID)
	}
	if Governs(store, "unknown-screen-not-in-tree") {
		t.Errorf("Governs on an unknown node id = true, want false")
	}
}

// TestBuildStoreMalformedScopeNodeDegradesWithoutPanic asserts a scope_nodes
// array with one malformed (unparseable) entry alongside one well-formed
// entry reports an error for the bad one but still builds a usable tree
// carrying the good node — BuildStore must never panic or brick on a bad
// schedule section (degrade-safe).
func TestBuildStoreMalformedScopeNodeDegradesWithoutPanic(t *testing.T) {
	goodNode := datamodel.ScopeNode{
		ID:        "01J8Z9GOODSCOPENODE0000011",
		Kind:      "org",
		Name:      "Good Org",
		Revision:  1,
		CreatedAt: 1752537000000,
		UpdatedAt: 1752537000000,
	}
	goodRaw, err := json.Marshal(goodNode)
	if err != nil {
		t.Fatalf("marshal goodNode: %v", err)
	}

	sec := wire.ScheduleSection{
		ScopeNodes: []json.RawMessage{
			goodRaw,
			json.RawMessage(`{ not valid json`),
		},
	}

	var store datamodel.RowStore
	var errs []datamodel.Error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("BuildStore panicked on a malformed scope node: %v", r)
			}
		}()
		store, errs = BuildStore(sec)
	}()

	if len(errs) == 0 {
		t.Fatalf("BuildStore(one malformed scope node) errs = none, want at least one ROW_MALFORMED error")
	}
	if kind, ok := store.Tree.KindOf(goodNode.ID); !ok || kind != "org" {
		t.Errorf("the good scope node did not survive into the store's tree: KindOf(%q) = (%q, %v), want (\"org\", true)", goodNode.ID, kind, ok)
	}
}

// TestBuildStoreMalformedRowDegradesWithoutPanic asserts a scheduling-core
// row array (dayparts) with one malformed entry alongside one well-formed
// entry reports the error but still returns a usable store carrying the good
// row — the same degrade-safe guarantee ValidateRows already provides per
// row, exercised through BuildStore end-to-end with no panic.
func TestBuildStoreMalformedRowDegradesWithoutPanic(t *testing.T) {
	sec := buildDemoSection(t)
	if len(sec.Dayparts) < 2 {
		t.Fatalf("demo section carries %d dayparts, want at least 2 to corrupt one", len(sec.Dayparts))
	}

	// Corrupt one of the two well-formed dayparts, keep the other intact.
	corrupted := make([]json.RawMessage, len(sec.Dayparts))
	copy(corrupted, sec.Dayparts)
	corrupted[0] = json.RawMessage(`{ not valid json`)
	sec.Dayparts = corrupted

	var store datamodel.RowStore
	var errs []datamodel.Error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("BuildStore panicked on a malformed row: %v", r)
			}
		}()
		store, errs = BuildStore(sec)
	}()

	if len(errs) == 0 {
		t.Fatalf("BuildStore(one malformed daypart) errs = none, want at least one ROW_MALFORMED error")
	}
	if len(store.Rows.Dayparts) != 1 {
		t.Fatalf("BuildStore(one malformed daypart) len(Dayparts) = %d, want 1 (the surviving good row)", len(store.Rows.Dayparts))
	}
}

// TestGovernsAncestorCascade asserts Governs resolves schedule applicability
// via the DAT-051 ancestor cascade — "its own scope_node is N itself or any
// ancestor of N on the parent_id chain" — not direct-attachment-only. A
// schedule whose scope_node is a SITE ancestor of the screen (DAT-051's own
// paradigmatic example: "a site-wide base schedule governs every screen
// beneath it") MUST govern the screen even though no schedule row is ever
// attached to the screen node itself. A schedule attached to an unrelated
// sibling screen under the same site MUST NOT govern this screen — Governs
// must implement the real ancestor cascade, not degrade into "any schedule
// exists anywhere in the store".
func TestGovernsAncestorCascade(t *testing.T) {
	const (
		cascadeOrgAncestorID     = "01J8Z9CASCADEORGBOUND00001"
		cascadeSiteID            = "01J8Z9CASCADESITE000000001"
		cascadeScreenID          = "01J8Z9CASCADESCREEN0000001"
		cascadeSiblingID         = "01J8Z9CASCADESIBLING000001"
		cascadeSiteScheduleID    = "01J8Z9CASCADESCHEDULESIT01"
		cascadeSiblingScheduleID = "01J8Z9CASCADESCHEDULESIB01"
	)
	orgParentID := cascadeOrgAncestorID
	siteParentID := cascadeSiteID
	siteTZ := "America/New_York"
	siteLat := 40.7128
	siteLong := -74.0060

	nodes := []datamodel.ScopeNode{
		{
			ID: cascadeSiteID, Kind: "site", ParentID: &orgParentID,
			Name: "Cascade Site", TZ: &siteTZ, Lat: &siteLat, Long: &siteLong,
			Revision: 1, CreatedAt: 1, UpdatedAt: 1,
		},
		{
			ID: cascadeScreenID, Kind: "screen", ParentID: &siteParentID,
			Name: "Cascade Screen", Revision: 1, CreatedAt: 1, UpdatedAt: 1,
		},
		{
			ID: cascadeSiblingID, Kind: "screen", ParentID: &siteParentID,
			Name: "Cascade Sibling Screen", Revision: 1, CreatedAt: 1, UpdatedAt: 1,
		},
	}
	tree, treeErrs := datamodel.BuildScopeTree(nodes)
	if len(treeErrs) != 0 {
		t.Fatalf("BuildScopeTree(cascade fixture) errs = %+v, want none", treeErrs)
	}

	t.Run("site-scoped schedule governs a descendant screen", func(t *testing.T) {
		store := datamodel.RowStore{
			Tree: tree,
			Rows: datamodel.RowSet{
				Schedules: []datamodel.Schedule{
					{
						ID: cascadeSiteScheduleID, ScopeNode: cascadeSiteID,
						Name: "Site-wide Base Schedule", Revision: 1, CreatedAt: 1, UpdatedAt: 1,
					},
				},
			},
		}
		if !Governs(store, cascadeScreenID) {
			t.Errorf("Governs(store, %q) = false, want true — the site-scoped schedule is applicable to the screen via the DAT-051 ancestor cascade even though no schedule is attached to the screen directly", cascadeScreenID)
		}
	})

	t.Run("sibling-scoped schedule does not govern an unrelated screen", func(t *testing.T) {
		store := datamodel.RowStore{
			Tree: tree,
			Rows: datamodel.RowSet{
				Schedules: []datamodel.Schedule{
					{
						ID: cascadeSiblingScheduleID, ScopeNode: cascadeSiblingID,
						Name: "Sibling-only Schedule", Revision: 1, CreatedAt: 1, UpdatedAt: 1,
					},
				},
			},
		}
		if Governs(store, cascadeScreenID) {
			t.Errorf("Governs(store, %q) = true, want false — a schedule scoped to an unrelated sibling screen is not an ancestor and MUST NOT govern this screen", cascadeScreenID)
		}
	})
}

// demoSiteTZ is the demo site's fixture effective tz (the same value the
// feeder's own firstPhotonSiteEffective.TZ carries, unexported there, so this
// is a byte-exact copy — the cross-package fixture-sharing pattern
// demoScreenScopeNodeID above already uses). Every demo daypart's local
// coverage is read against this zone, never a box-local one (DAT-034).
const demoSiteTZ = "America/Chicago"

// demoLocalInstant computes a deterministic Unix-ms instant at the given local
// hour/minute in the demo site's effective tz, on a fixed DST-quiet winter date
// (2026-01-15) — the same fixture-instant construction the feeder's own
// demoschedule_test uses, so a content-hour vs overnight instant lands in the
// same daypart the feeder authored (06:00-22:00 content, 22:00-06:00 blank).
func demoLocalInstant(t *testing.T, hour, minute int) int64 {
	t.Helper()
	loc, err := time.LoadLocation(demoSiteTZ)
	if err != nil {
		t.Fatalf("load location %q: %v", demoSiteTZ, err)
	}
	return time.Date(2026, time.January, 15, hour, minute, 0, 0, loc).UnixMilli()
}

// TestProjectLeaseMidDayProjectsContentFromPlaylist asserts a mid-day instant
// (inside the demo content daypart) projects display:content at priority
// scheduled, with one image content item sourced from the effective daypart's
// playlist (DAT-113/115) — the SAME asset_ref the demo playlist item carries.
func TestProjectLeaseMidDayProjectsContentFromPlaylist(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}
	if len(store.Rows.Playlists) == 0 || len(store.Rows.Playlists[0].Items) == 0 {
		t.Fatalf("demo store carries no playlist items to source content from")
	}
	wantAssetRef := store.Rows.Playlists[0].Items[0].AssetRef

	display, priority, content, programRevision, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 12, 0))
	if err != nil {
		t.Fatalf("ProjectLease(mid-day): %v", err)
	}
	if display != "content" {
		t.Errorf("display = %q, want content (DAT-113)", display)
	}
	if priority != "scheduled" {
		t.Errorf("priority = %q, want scheduled (schedule-driven, not the emergency preempt path)", priority)
	}
	if programRevision == "" {
		t.Error("programRevision is empty, want a non-empty deterministic revision")
	}
	if len(content) != 1 {
		t.Fatalf("content has %d items, want 1 (the demo playlist's single asset item)", len(content))
	}
	if content[0].Type != "image" {
		t.Errorf("content[0].type = %q, want image", content[0].Type)
	}
	if content[0].AssetRef != wantAssetRef {
		t.Errorf("content[0].asset_ref = %q, want %q (sourced from the effective daypart's playlist)", content[0].AssetRef, wantAssetRef)
	}
}

// TestProjectLeaseOvernightProjectsBlankNoContent asserts an overnight instant
// (inside the demo blank daypart) projects display:blank with NO content
// (DAT-114/115), still at priority scheduled — a blank daypart is a
// schedule-driven state, powered on showing nothing.
func TestProjectLeaseOvernightProjectsBlankNoContent(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}

	display, priority, content, _, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 2, 0))
	if err != nil {
		t.Fatalf("ProjectLease(overnight): %v", err)
	}
	if display != "blank" {
		t.Errorf("display = %q, want blank (DAT-114)", display)
	}
	if priority != "scheduled" {
		t.Errorf("priority = %q, want scheduled", priority)
	}
	if len(content) != 0 {
		t.Errorf("content has %d items, want 0 (a blank daypart shows nothing, DAT-115)", len(content))
	}
}

// governedTerminalStore builds a store whose schedule GOVERNS the screen (a
// schedule is attached to it) but that never HOLDS — it declares no dayparts
// and no fallback — so resolution terminates at the DAT-118 terminal default:
// display:blank, powered on, no content, never a box-local substitution.
func governedTerminalStore(t *testing.T) (datamodel.RowStore, string) {
	t.Helper()
	tz := demoSiteTZ
	lat := 41.8781
	long := -87.6298
	orgBound := "01J8ZATERMINALORGBOUND0001"
	siteID := "01J8ZATERMINALSITE00000001"
	screenID := "01J8ZATERMINALSCREEN000001"
	scheduleID := "01J8ZATERMINALSCHEDULE0001"
	siteParent := siteID

	nodes := []datamodel.ScopeNode{
		{ID: siteID, Kind: "site", ParentID: &orgBound, Name: "Terminal Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
		{ID: screenID, Kind: "screen", ParentID: &siteParent, Name: "Terminal Screen", Revision: 1, CreatedAt: 1, UpdatedAt: 1},
	}
	tree, treeErrs := datamodel.BuildScopeTree(nodes)
	if len(treeErrs) != 0 {
		t.Fatalf("BuildScopeTree(terminal fixture) errs = %+v, want none", treeErrs)
	}
	store := datamodel.RowStore{
		Tree: tree,
		Rows: datamodel.RowSet{
			Schedules: []datamodel.Schedule{
				{ID: scheduleID, ScopeNode: screenID, Name: "Governs But Nothing Holds", Revision: 1, CreatedAt: 1, UpdatedAt: 1},
			},
		},
	}
	return store, screenID
}

// TestProjectLeaseTerminalDefaultBlank asserts a governed screen with nothing
// holding (no daypart, no fallback) projects the DAT-118 terminal default:
// display:blank, no content, at a deterministic (stable) programRevision so the
// screen never spuriously re-swaps while parked at the terminal blank.
func TestProjectLeaseTerminalDefaultBlank(t *testing.T) {
	store, screenID := governedTerminalStore(t)

	display, priority, content, rev1, err := ProjectLease(store, screenID, demoLocalInstant(t, 12, 0))
	if err != nil {
		t.Fatalf("ProjectLease(terminal): %v", err)
	}
	if display != "blank" {
		t.Errorf("display = %q, want blank (DAT-118 terminal default)", display)
	}
	if priority != "scheduled" {
		t.Errorf("priority = %q, want scheduled", priority)
	}
	if len(content) != 0 {
		t.Errorf("content has %d items, want 0 (terminal default shows nothing, DAT-118)", len(content))
	}

	_, _, _, rev2, err := ProjectLease(store, screenID, demoLocalInstant(t, 18, 0))
	if err != nil {
		t.Fatalf("ProjectLease(terminal, second instant): %v", err)
	}
	if rev1 != rev2 {
		t.Errorf("terminal programRevision is not stable across instants: %q vs %q", rev1, rev2)
	}
}

// TestProjectLeaseProgramRevisionStableWithinDaypartChangesAcross asserts the
// programRevision is a deterministic function of effective-daypart identity: it
// is byte-identical across two resolves inside the SAME daypart (no spurious
// program swap) and differs across a daypart boundary (the player swaps).
func TestProjectLeaseProgramRevisionStableWithinDaypartChangesAcross(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}

	_, _, _, revMidday, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 12, 0))
	if err != nil {
		t.Fatalf("ProjectLease(12:00): %v", err)
	}
	_, _, _, revAfternoon, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 15, 0))
	if err != nil {
		t.Fatalf("ProjectLease(15:00): %v", err)
	}
	if revMidday != revAfternoon {
		t.Errorf("programRevision changed within the SAME content daypart: %q (12:00) vs %q (15:00) — a stable daypart must not re-swap", revMidday, revAfternoon)
	}

	_, _, _, revOvernight, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 2, 0))
	if err != nil {
		t.Fatalf("ProjectLease(02:00): %v", err)
	}
	if revMidday == revOvernight {
		t.Errorf("programRevision unchanged across a daypart boundary: content=%q overnight=%q — a daypart change must yield a new revision", revMidday, revOvernight)
	}
}

// newTestPlayerServer builds a real playerserver.Server with one redeemable
// pairing grant and returns it alongside the ed25519 signing key SetProgram
// signs its Leases with and the grant's selector — enough to pair a player and
// pull the served program back over player/1's own HTTP surface.
func newTestPlayerServer(t *testing.T) (*playerserver.Server, ed25519.PrivateKey, string) {
	t.Helper()
	certPEM, keyPEM := tlsboot.GenSelfSigned()

	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("GenSelfSigned key did not PEM-decode to a PRIVATE KEY block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParsePKCS8PrivateKey: %v", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("parsed key is %T, want ed25519.PrivateKey", key)
	}

	const grantID = "grant-test-fixture-000000001"
	grant := wire.PairingGrant{
		GrantID:                grantID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		TTL:                    900,
		RedemptionMode:         "one-time",
		IssuedAt:               time.Now().UnixMilli(),
	}
	srv, err := playerserver.NewServer(certPEM, []wire.PairingGrant{grant})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, priv, grantID
}

// pairAndPull pairs a player against srv (redeeming grantID) and pulls the
// served program, returning the issued Lease — the black-box way to observe
// what SetProgram configured, through player/1's own pair -> program flow.
func pairAndPull(t *testing.T, srv *playerserver.Server, grantID string, contentTypes []string) playerserver.LeaseResponse {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(apihttp.WithTraceID(mux))
	t.Cleanup(ts.Close)

	pairBody, err := json.Marshal(playerserver.PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grantID,
		Capabilities:  playerserver.Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	pairResp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(pairBody))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	defer pairResp.Body.Close()
	var pr playerserver.PairingResponse
	if err := json.NewDecoder(pairResp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode pairing response: %v", err)
	}
	if pr.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pr)
	}

	pullBody, err := json.Marshal(playerserver.ProgramPullRequest{
		Capabilities: playerserver.Capabilities{ContentTypes: contentTypes, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal program pull: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/player/v1/program", bytes.NewReader(pullBody))
	if err != nil {
		t.Fatalf("build program request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+pr.ChannelToken)
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

// TestResolveNowServesResolvedProgramViaSetProgram asserts ResolveNow projects
// the current instant and calls the player server's SetProgram, so a player
// pulling its program then sees the schedule-resolved Lease: display:content,
// priority scheduled, the demo asset ref, and the SAME programRevision
// ProjectLease derives for that instant (proving the two share one projection).
func TestResolveNowServesResolvedProgramViaSetProgram(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}
	wantAssetRef := store.Rows.Playlists[0].Items[0].AssetRef

	srv, priv, grantID := newTestPlayerServer(t)
	r := NewResolver(store, demoScreenScopeNodeID, srv, priv)

	midDay := demoLocalInstant(t, 12, 0)
	if _, err := r.ResolveNow(midDay); err != nil {
		t.Fatalf("ResolveNow(mid-day): %v", err)
	}

	_, _, _, wantRevision, err := ProjectLease(store, demoScreenScopeNodeID, midDay)
	if err != nil {
		t.Fatalf("ProjectLease(mid-day): %v", err)
	}

	lease := pairAndPull(t, srv, grantID, []string{"image", "video"})
	if lease.Display != "content" {
		t.Errorf("served display = %q, want content (SetProgram was not wired to the resolved state)", lease.Display)
	}
	if lease.Priority != "scheduled" {
		t.Errorf("served priority = %q, want scheduled", lease.Priority)
	}
	if lease.ProgramRevision != wantRevision {
		t.Errorf("served program_revision = %q, want %q (ResolveNow must serve ProjectLease's revision)", lease.ProgramRevision, wantRevision)
	}
	if len(lease.Content) != 1 {
		t.Fatalf("served content has %d items, want 1", len(lease.Content))
	}
	if lease.Content[0].Type != "image" || lease.Content[0].AssetRef != wantAssetRef {
		t.Errorf("served content[0] = %+v, want type image with asset_ref %q", lease.Content[0], wantAssetRef)
	}
}

// unresolvableTZStore builds a store whose screen resolves to a tz that is not
// a loadable IANA zone — so datamodel.Resolve returns an error (DAT-034: the
// platform NEVER substitutes box-local state) rather than a defined state.
func unresolvableTZStore(t *testing.T) (datamodel.RowStore, string) {
	t.Helper()
	badTZ := "Not/ARealZone"
	lat := 1.0
	long := 1.0
	siteBound := "01J8ZBUNRESOLVTZSITEBND001"
	screenID := "01J8ZBUNRESOLVTZSCREEN0001"

	nodes := []datamodel.ScopeNode{
		{ID: screenID, Kind: "screen", ParentID: &siteBound, Name: "Bad TZ Screen", TZ: &badTZ, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
	}
	tree, _ := datamodel.BuildScopeTree(nodes)
	return datamodel.RowStore{Tree: tree}, screenID
}

// TestResolveNowUnresolvableTZLeavesSetProgramUncalledAndErrors asserts that on
// an unresolvable effective tz (DAT-034) ResolveNow does NOT call SetProgram
// and surfaces the error — never a box-local substitution. The Resolver is
// given a nil *playerserver.Server: had ResolveNow called SetProgram it would
// panic dereferencing nil, so a clean error return with no panic is proof the
// serve path was left untouched.
func TestResolveNowUnresolvableTZLeavesSetProgramUncalledAndErrors(t *testing.T) {
	store, screenID := unresolvableTZStore(t)
	r := NewResolver(store, screenID, nil, nil)

	var fire *datamodel.PresetFire
	var err error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("ResolveNow panicked — it called SetProgram on the nil server instead of erroring (DAT-034): %v", rec)
			}
		}()
		fire, err = r.ResolveNow(demoLocalInstant(t, 12, 0))
	}()

	if err == nil {
		t.Fatal("ResolveNow err = nil, want an unresolvable-tz error (DAT-034, no box-local fallback)")
	}
	if fire != nil {
		t.Errorf("ResolveNow fire = %+v, want nil on the error path", fire)
	}
}
