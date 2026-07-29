package schedulehost

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
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// demoScreenScopeNodeID is the SAME fixture-ULID the feeder's own demo
// schedule (internal/feeder/snapshot, REL-065) attaches its schedule row to
// (its own demoScreenScopeNodeID constant is unexported, so this is a
// byte-exact copy — the cross-package fixture-sharing pattern this codebase
// already uses, e.g. internal/relay/automationhost's testScreenEntity).
const demoScreenScopeNodeID = "01J8Z4DEM0SCREENF1RSTPH0TN"

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
		cascadeOrgAncestorID     = "01J8Z9CASCADE0RGB0VND00001"
		cascadeSiteID            = "01J8Z9CASCADES1TE000000001"
		cascadeScreenID          = "01J8Z9CASCADESCREEN0000001"
		cascadeSiblingID         = "01J8Z9CASCADES1B11NG000001"
		cascadeSiteScheduleID    = "01J8Z9CASCADESCHEDV1ES1T01"
		cascadeSiblingScheduleID = "01J8Z9CASCADESCHEDV1ES1B01"
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

	display, priority, content, programRevision, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 12, 0), "")
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

	display, priority, content, _, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 2, 0), "")
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
	orgBound := "01J8ZATERM1NA10RGB0VND0001"
	siteID := "01J8ZATERM1NA1S1TE00000001"
	screenID := "01J8ZATERM1NA1SCREEN000001"
	scheduleID := "01J8ZATERM1NA1SCHEDV1E0001"
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

	display, priority, content, rev1, err := ProjectLease(store, screenID, demoLocalInstant(t, 12, 0), "")
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

	_, _, _, rev2, err := ProjectLease(store, screenID, demoLocalInstant(t, 18, 0), "")
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

	_, _, _, revMidday, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 12, 0), "")
	if err != nil {
		t.Fatalf("ProjectLease(12:00): %v", err)
	}
	_, _, _, revAfternoon, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 15, 0), "")
	if err != nil {
		t.Fatalf("ProjectLease(15:00): %v", err)
	}
	if revMidday != revAfternoon {
		t.Errorf("programRevision changed within the SAME content daypart: %q (12:00) vs %q (15:00) — a stable daypart must not re-swap", revMidday, revAfternoon)
	}

	_, _, _, revOvernight, err := ProjectLease(store, demoScreenScopeNodeID, demoLocalInstant(t, 2, 0), "")
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
//
// The cert is minted ed25519 directly (not via tlsboot.GenSelfSigned, which now
// serves the browser-facing ECDSA P-256 leaf): the relay's lease-signing
// identity — the cert whose public key a player verifies Leases against
// (PLY-090) — is ed25519, a distinct key from the feeder's TLS serving leaf.
// testServedScreenID is the SCREEN IDENTITY ROW (data-model/1 DAT-004a) these
// cases' resolvers serve. It is deliberately a different value from
// demoScreenScopeNodeID, the SCOPE NODE they resolve at: DAT-004a makes those
// distinct rows with distinct ids, and a test that used one value for both
// would pass even if the resolver conflated them.
const testServedScreenID = "01J8Z9DEM0SCREENR0WF1RSTPH"

func newTestPlayerServer(t *testing.T) (*playerserver.Server, ed25519.PrivateKey, string) {
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

	const grantID = "grant-test-fixture-000000001"
	// Screen-bound (REL-121a) so the redeemed channel token resolves to
	// testServedScreenID — the SAME screen every resolver in this file is built
	// to serve. A relay serves a program per screen, so an unbound grant would
	// redeem into a fresh opaque screen id and pairAndPull would observe the
	// terminal default instead of whatever the resolver under test served.
	grant := wire.PairingGrant{
		GrantID:                grantID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		ScreenID:               testServedScreenID,
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
	r := NewResolver(store, demoScreenScopeNodeID, testServedScreenID, srv, priv, 1, "")

	midDay := demoLocalInstant(t, 12, 0)
	if _, err := r.ResolveNow(midDay); err != nil {
		t.Fatalf("ResolveNow(mid-day): %v", err)
	}

	_, _, _, wantRevision, err := ProjectLease(store, demoScreenScopeNodeID, midDay, "")
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

// TestResolveNowStaleGenerationDoesNotRevertServedProgram is the regression
// guard for the live re-pull race at the resolver layer (REL-052/056). A
// resolver built for a superseded generation, whose cancelled background loop
// is still mid-resolve when a higher generation has already taken over the
// served program, MUST NOT revert that program when its late ResolveNow lands:
// the resolver stamps its own generation onto SetProgram, which the player
// server fences against a strictly-older write. Here generation 8 is live and a
// leftover generation-7 resolver resolves + writes afterward; the serve must
// stay on generation 8.
func TestResolveNowStaleGenerationDoesNotRevertServedProgram(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}

	srv, priv, grantID := newTestPlayerServer(t)

	// Generation 8 has taken over the served program (the new schedule edit,
	// applied live by the re-pull loop).
	srv.SetProgram(8, testServedScreenID, "gen-8-live", "scheduled", "content", nil, priv)

	// A superseded generation-7 resolver resolves late — its background loop was
	// mid-resolve when generation 8 was applied — and writes via SetProgram.
	stale := NewResolver(store, demoScreenScopeNodeID, testServedScreenID, srv, priv, 7, "")
	if _, err := stale.ResolveNow(demoLocalInstant(t, 12, 0)); err != nil {
		t.Fatalf("ResolveNow(stale generation 7): %v", err)
	}

	lease := pairAndPull(t, srv, grantID, []string{"image", "video"})
	if lease.ProgramRevision != "gen-8-live" {
		t.Errorf("served program_revision = %q, want gen-8-live — a superseded generation-7 resolver reverted the live serve (REL-052/056)", lease.ProgramRevision)
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
	siteBound := "01J8ZBVNRES01VTZS1TEBND001"
	screenID := "01J8ZBVNRES01VTZSCREEN0001"

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
	r := NewResolver(store, screenID, testServedScreenID, nil, nil, 1, "")

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

// demoRuleEntityID / demoContentDaypartID / demoPresetBatchID are byte-exact
// copies of the feeder's own (unexported) demo-schedule fixtures
// (internal/feeder/snapshot/demoschedule.go): the content daypart's bound
// preset batch fires ONE launch device_command against demoRuleEntityID on
// its rising edge (DAT-075). demoScreenScopeNodeID above is the same
// cross-package fixture-sharing pattern.
const (
	demoRuleEntityID     = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	demoContentDaypartID = "01J8Z7DEM0DAYPARTC0NTENT01"
	demoPresetBatchID    = "01J8Z8DEM0PRESETBATCHF1RE1"
)

// recordController records every physical dispatch it receives — the same
// fake-controller pattern internal/relay/automation's own tests use to observe
// what reaches the device plane. It never fails, so a fired preset command's
// dispatch always succeeds (a complete PresetBatchOutcome, DAT-092).
type recordController struct {
	mu   sync.Mutex
	seen []string // "entity/command", in dispatch order
}

func (c *recordController) Dispatch(entityID, command string, params map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, entityID+"/"+command)
	return nil
}

func (c *recordController) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.seen))
	copy(out, c.seen)
	return out
}

// newFakeSink builds a real *automation.CommandSink — the SAME device-plane
// dispatch surface the automation engine fires through (REL-113/115) — wired to
// a recordController so a test can observe exactly which preset commands
// reached the device plane. Every entity resolves to a media-player device, so
// the FixtureRegistry vocabulary (launch/home/keypress/power) accepts the demo
// preset's commands.
func newFakeSink(t *testing.T) (*automation.CommandSink, *recordController) {
	t.Helper()
	ctrl := &recordController{}
	resolve := func(entityID string) (string, string, bool) {
		return entityID + "-device", "media-player", true
	}
	surface := deviceplane.NewCommandSurface(ctrl, registry.FixtureRegistry{}, resolve)
	return automation.NewCommandSink(surface, "01J8ZFAKESINKRELAYID000001"), ctrl
}

// TestTickCrossingIntoContentFiresPresetOnceThenHolds is the Task-5 rising-edge
// proof (DAT-075/111/119): ticking at an overnight instant (the blank daypart,
// which binds no preset) fires nothing; crossing into the content daypart fires
// its bound preset EXACTLY ONCE through the device plane (a rising edge of
// effective-daypart identity); and a second tick still inside that content
// daypart fires NOTHING — the STATE projection is level-triggered every tick,
// the preset is edge-triggered only, so a hold never re-fires.
func TestTickCrossingIntoContentFiresPresetOnceThenHolds(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}
	srv, priv, _ := newTestPlayerServer(t)
	r := NewResolver(store, demoScreenScopeNodeID, testServedScreenID, srv, priv, 1, "")
	sink, ctrl := newFakeSink(t)

	// Overnight: the blank daypart holds and binds no preset — nothing fires.
	r.Tick(demoLocalInstant(t, 2, 0), sink)
	if calls := ctrl.calls(); len(calls) != 0 {
		t.Fatalf("overnight tick dispatched %v, want nothing (the blank daypart binds no preset)", calls)
	}

	// Cross the boundary into the content daypart: its bound preset fires once.
	r.Tick(demoLocalInstant(t, 12, 0), sink)
	if calls := ctrl.calls(); len(calls) != 1 || calls[0] != demoRuleEntityID+"/launch" {
		t.Fatalf("crossing into the content daypart dispatched %v, want exactly [%s/launch] (DAT-075 rising edge)", calls, demoRuleEntityID)
	}

	// Stay inside the SAME content daypart: level-triggered STATE, no re-fire.
	r.Tick(demoLocalInstant(t, 15, 0), sink)
	if calls := ctrl.calls(); len(calls) != 1 {
		t.Fatalf("a second tick inside the same content daypart dispatched %v, want still exactly 1 (a hold must not re-fire, DAT-119)", calls)
	}
}

// TestFirePresetDispatchesBatchAndCollectsCompleteOutcome asserts FirePreset
// resolves the fired preset batch's commands, dispatches each through the reused
// device-plane sink, and collects a DAT-092 PresetBatchOutcome — complete when
// every command succeeds, with one per-command result carrying the target entity
// and command name.
func TestFirePresetDispatchesBatchAndCollectsCompleteOutcome(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}
	r := NewResolver(store, demoScreenScopeNodeID, testServedScreenID, nil, nil, 1, "") // FirePreset never touches the player server
	sink, ctrl := newFakeSink(t)

	fire := &datamodel.PresetFire{DaypartID: demoContentDaypartID, PresetBatchID: demoPresetBatchID}
	out := r.FirePreset(fire, sink)

	if out.Outcome != "complete" {
		t.Errorf("outcome = %q, want complete (every command succeeded, DAT-092)", out.Outcome)
	}
	if len(out.Results) != 1 {
		t.Fatalf("outcome has %d per-command results, want 1 (the demo batch's single command)", len(out.Results))
	}
	res := out.Results[0]
	if !res.OK || res.Target != demoRuleEntityID || res.Command != "launch" {
		t.Errorf("result = %+v, want an ok launch on %s", res, demoRuleEntityID)
	}
	if calls := ctrl.calls(); len(calls) != 1 || calls[0] != demoRuleEntityID+"/launch" {
		t.Errorf("device plane saw %v, want exactly [%s/launch]", calls, demoRuleEntityID)
	}
}

// TestFirePresetNilFireDispatchesNothing asserts a nil fire (no rising edge, or
// a masked/absent preset) dispatches nothing and collects an empty outcome —
// the level-triggered STATE projection continues, but no preset event is
// emitted (DAT-075).
func TestFirePresetNilFireDispatchesNothing(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}
	r := NewResolver(store, demoScreenScopeNodeID, testServedScreenID, nil, nil, 1, "")
	sink, ctrl := newFakeSink(t)

	out := r.FirePreset(nil, sink)
	if len(out.Results) != 0 {
		t.Errorf("nil-fire outcome has %d results, want 0 (nothing fired)", len(out.Results))
	}
	if calls := ctrl.calls(); len(calls) != 0 {
		t.Errorf("nil fire dispatched %v, want nothing", calls)
	}
}

// maskedStore builds a single-screen store carrying TWO overlapping schedules
// that both HOLD at noon (America/Chicago): a base schedule (priority 0,
// 06:00-22:00) binding maskedEntity/home, and a higher-precedence holiday
// schedule (priority 100, 10:00-14:00) binding effectiveEntity/launch. At noon
// the holiday layer is effective and the base daypart is MASKED (DAT-111): only
// the effective daypart's preset must ever fire (DAT-075).
const (
	maskedSiteID           = "01J8ZCMASKEDS1TE0000000001"
	maskedScreenID         = "01J8ZCMASKEDSCREEN00000001"
	maskedBaseScheduleID   = "01J8ZCMASKEDBASESCHEDV1E01"
	maskedHolidayScheduleI = "01J8ZCMASKEDH011SCHEDV1E01"
	maskedBaseDaypartID    = "01J8ZCMASKEDBASEDAYPART001"
	maskedHolidayDaypartID = "01J8ZCMASKEDH011DAYPART001"
	maskedPresetID         = "01J8ZCMASKEDPRESETBASE0001"
	effectivePresetID      = "01J8ZCEFFECT1VEPRESETH0101"
	maskedEntity           = "01J8ZCMASKEDENTITYBASE0001"
	effectiveEntity        = "01J8ZCEFFECTIVEENTITYHOL01"
)

func maskedStore(t *testing.T) datamodel.RowStore {
	t.Helper()
	tz := demoSiteTZ
	lat := 41.8781
	long := -87.6298
	orgBound := "01J8ZCMASKED0RGB0VND000001"
	siteParent := maskedSiteID
	basePriority := 0
	holidayPriority := 100

	nodes := []datamodel.ScopeNode{
		{ID: maskedSiteID, Kind: "site", ParentID: &orgBound, Name: "Masked Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
		{ID: maskedScreenID, Kind: "screen", ParentID: &siteParent, Name: "Masked Screen", Revision: 1, CreatedAt: 1, UpdatedAt: 1},
	}
	tree, treeErrs := datamodel.BuildScopeTree(nodes)
	if len(treeErrs) != 0 {
		t.Fatalf("BuildScopeTree(masked fixture) errs = %+v, want none", treeErrs)
	}

	allWeek := []int{0, 1, 2, 3, 4, 5, 6}
	return datamodel.RowStore{
		Tree: tree,
		Rows: datamodel.RowSet{
			Schedules: []datamodel.Schedule{
				{ID: maskedBaseScheduleID, ScopeNode: maskedScreenID, Name: "Base", Priority: &basePriority, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
				{ID: maskedHolidayScheduleI, ScopeNode: maskedScreenID, Name: "Holiday", Priority: &holidayPriority, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
			},
			Dayparts: []datamodel.Daypart{
				{ID: maskedBaseDaypartID, ScheduleID: maskedBaseScheduleID, ScopeNode: maskedScreenID, DaysOfWeek: allWeek, StartTime: "06:00:00", EndTime: "22:00:00", DisplayPower: "on", PresetBatchID: maskedPresetID, Name: "Base Hours", Revision: 1, CreatedAt: 1, UpdatedAt: 1},
				{ID: maskedHolidayDaypartID, ScheduleID: maskedHolidayScheduleI, ScopeNode: maskedScreenID, DaysOfWeek: allWeek, StartTime: "10:00:00", EndTime: "14:00:00", DisplayPower: "on", PresetBatchID: effectivePresetID, Name: "Holiday Hours", Revision: 1, CreatedAt: 1, UpdatedAt: 1},
			},
			PresetBatches: []datamodel.PresetBatch{
				{PresetID: maskedPresetID, ScopeNode: maskedScreenID, Name: "Masked Base Preset", Commands: []datamodel.PresetCommand{{EntityID: maskedEntity, Command: "home"}}, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
				{PresetID: effectivePresetID, ScopeNode: maskedScreenID, Name: "Effective Holiday Preset", Commands: []datamodel.PresetCommand{{EntityID: effectiveEntity, Command: "launch", Params: json.RawMessage(`{"channel":"holiday"}`)}}, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
			},
		},
	}
}

// TestTickMaskedDaypartDoesNotFire asserts a masked daypart NEVER fires its
// preset (DAT-075/111): at noon both the base and holiday dayparts hold, but the
// higher-precedence holiday is the effective one, so only its preset
// (effectiveEntity/launch) fires — the masked base's preset (maskedEntity/home)
// must never reach the device plane. This holds by construction because
// PresetTransition keys on the node-level effective state, not per-schedule
// candidates.
func TestTickMaskedDaypartDoesNotFire(t *testing.T) {
	store := maskedStore(t)
	srv, priv, _ := newTestPlayerServer(t)
	r := NewResolver(store, maskedScreenID, testServedScreenID, srv, priv, 1, "")
	sink, ctrl := newFakeSink(t)

	r.Tick(demoLocalInstant(t, 12, 0), sink)

	calls := ctrl.calls()
	if len(calls) != 1 || calls[0] != effectiveEntity+"/launch" {
		t.Fatalf("masked tick dispatched %v, want exactly [%s/launch] — only the effective daypart's preset fires (DAT-075/111)", calls, effectiveEntity)
	}
	for _, c := range calls {
		if c == maskedEntity+"/home" {
			t.Fatalf("the MASKED daypart's preset fired (%s) — a masked daypart must NEVER fire (DAT-111)", c)
		}
	}
}

// TestLoopDrivesTickOnInjectedTicks asserts the resolution loop drives Tick on
// each tick delivered by an injected channel (a time.Ticker's channel in the
// binary, a manual channel here) — no wall-clock sleeps: an overnight tick then
// a content-daypart tick fire the rising-edge preset exactly once, a second
// content tick does not re-fire, and the loop returns when its context is
// cancelled.
func TestLoopDrivesTickOnInjectedTicks(t *testing.T) {
	store, errs := BuildStore(buildDemoSection(t))
	if len(errs) != 0 {
		t.Fatalf("BuildStore(demo section) errs = %+v, want none", errs)
	}
	srv, priv, _ := newTestPlayerServer(t)
	r := NewResolver(store, demoScreenScopeNodeID, testServedScreenID, srv, priv, 1, "")
	sink, ctrl := newFakeSink(t)

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Loop(ctx, ticks, sink)
		close(done)
	}()

	// Each unbuffered send completes only once the loop has received it, and the
	// loop processes ticks one at a time, so these drive three ordered Ticks
	// deterministically without any sleep.
	ticks <- time.UnixMilli(demoLocalInstant(t, 2, 0))  // overnight: no fire
	ticks <- time.UnixMilli(demoLocalInstant(t, 12, 0)) // rising edge: fire once
	ticks <- time.UnixMilli(demoLocalInstant(t, 15, 0)) // hold: no re-fire

	cancel()
	<-done

	if calls := ctrl.calls(); len(calls) != 1 || calls[0] != demoRuleEntityID+"/launch" {
		t.Fatalf("Loop over the injected ticks dispatched %v, want exactly [%s/launch]", calls, demoRuleEntityID)
	}
}

// resumeMisfire fixture constants: a single schedule + one all-day daypart
// (00:00:00-23:59:59, so ANY test instant lands inside it — modeling a relay
// restart landing inside an already-active daypart's window, DAT-075's boot/
// resume case) bound to one preset batch.
const (
	resumeMisfireSiteID     = "01J8ZRM1SF1RES1TE000000010"
	resumeMisfireScreenID   = "01J8ZRM1SF1RESCREEN0000001"
	resumeMisfireScheduleID = "01J8ZRM1SF1RESCHEDV1E00001"
	resumeMisfireDaypartID  = "01J8ZRM1SF1REDAYPART000001"
	resumeMisfirePresetID   = "01J8ZRM1SF1REPRESET0000001"
	resumeMisfireEntity     = "01J8ZRM1SF1REENT1TY0000001"
)

// resumeMisfireStore builds a single-screen store with one all-day daypart
// whose effective misfire is daypartMisfire (empty string for the DAT-121
// catch_up_once default) bound to a preset batch — the DAT-075/076/094/121
// boot/resume-misfire regression fixture.
func resumeMisfireStore(t *testing.T, daypartMisfire string) datamodel.RowStore {
	t.Helper()
	tz := demoSiteTZ
	lat := 41.8781
	long := -87.6298
	orgBound := "01J8ZRM1SF1RE0RGB0VND00001"
	siteParent := resumeMisfireSiteID

	nodes := []datamodel.ScopeNode{
		{ID: resumeMisfireSiteID, Kind: "site", ParentID: &orgBound, Name: "Resume Misfire Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
		{ID: resumeMisfireScreenID, Kind: "screen", ParentID: &siteParent, Name: "Resume Misfire Screen", Revision: 1, CreatedAt: 1, UpdatedAt: 1},
	}
	tree, treeErrs := datamodel.BuildScopeTree(nodes)
	if len(treeErrs) != 0 {
		t.Fatalf("BuildScopeTree(resume misfire fixture) errs = %+v, want none", treeErrs)
	}

	allWeek := []int{0, 1, 2, 3, 4, 5, 6}
	return datamodel.RowStore{
		Tree: tree,
		Rows: datamodel.RowSet{
			Schedules: []datamodel.Schedule{
				{ID: resumeMisfireScheduleID, ScopeNode: resumeMisfireScreenID, Name: "Resume Misfire Schedule", Revision: 1, CreatedAt: 1, UpdatedAt: 1},
			},
			Dayparts: []datamodel.Daypart{
				{ID: resumeMisfireDaypartID, ScheduleID: resumeMisfireScheduleID, ScopeNode: resumeMisfireScreenID, DaysOfWeek: allWeek, StartTime: "00:00:00", EndTime: "23:59:59", DisplayPower: "on", PresetBatchID: resumeMisfirePresetID, Misfire: daypartMisfire, Name: "All Day", Revision: 1, CreatedAt: 1, UpdatedAt: 1},
			},
			PresetBatches: []datamodel.PresetBatch{
				{PresetID: resumeMisfirePresetID, ScopeNode: resumeMisfireScreenID, Name: "Resume Misfire Preset", Commands: []datamodel.PresetCommand{{EntityID: resumeMisfireEntity, Command: "home"}}, Revision: 1, CreatedAt: 1, UpdatedAt: 1},
			},
		},
	}
}

// TestTickBootSkipMisfireSuppressesResumeFireButStillProjectsState is the
// DAT-075/076/094/121 boot-resume regression: a daypart declaring
// misfire:"skip" MUST NOT re-fire its bound preset batch on a boot/resume
// tick that lands inside its already-active window (a site declares "skip"
// specifically so a relay restart never re-toggles a device) — while the
// level-triggered STATE projection (DAT-119) still resolves and is tracked
// exactly as an ordinary Tick would; only the preset fire is gated.
func TestTickBootSkipMisfireSuppressesResumeFireButStillProjectsState(t *testing.T) {
	store := resumeMisfireStore(t, "skip")
	srv, priv, _ := newTestPlayerServer(t)
	r := NewResolver(store, resumeMisfireScreenID, testServedScreenID, srv, priv, 1, "")
	sink, ctrl := newFakeSink(t)

	r.TickBoot(demoLocalInstant(t, 12, 0), sink)

	if calls := ctrl.calls(); len(calls) != 0 {
		t.Fatalf("TickBoot with misfire:skip dispatched %v, want nothing — a skip misfire suppresses the resume-edge fire (DAT-075/076/121)", calls)
	}
	if r.prev == nil || r.prev.Daypart == nil || r.prev.Daypart.ID != resumeMisfireDaypartID {
		t.Fatalf("TickBoot did not resolve/track the STATE projection even though the preset fire was suppressed (DAT-119: STATE is level-triggered, only the preset is edge-gated)")
	}
}

// TestTickBootDefaultMisfireStillFiresResumeEdge asserts TickBoot's misfire
// gate suppresses ONLY an explicit "skip" — the DAT-121 default
// (catch_up_once, an unset misfire) still fires the resume edge exactly as
// Tick always has, so a boot/resume landing inside a preset-bound daypart
// that declares no misfire keeps collapsing any missed transitions into one
// current-state fire, unchanged from today's behavior.
func TestTickBootDefaultMisfireStillFiresResumeEdge(t *testing.T) {
	store := resumeMisfireStore(t, "") // no explicit misfire -> catch_up_once (DAT-121)
	srv, priv, _ := newTestPlayerServer(t)
	r := NewResolver(store, resumeMisfireScreenID, testServedScreenID, srv, priv, 1, "")
	sink, ctrl := newFakeSink(t)

	r.TickBoot(demoLocalInstant(t, 12, 0), sink)

	if calls := ctrl.calls(); len(calls) != 1 || calls[0] != resumeMisfireEntity+"/home" {
		t.Fatalf("TickBoot with the default (catch_up_once) misfire dispatched %v, want exactly [%s/home] (DAT-121 default still fires the resume edge)", calls, resumeMisfireEntity)
	}
}
