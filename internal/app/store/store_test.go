package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// Fixture ULIDs (fixture-ULID convention; no secrets, no real placement data
// beyond a documentation-range geo). These mirror the demo tree's shape: a site
// scope node, a screen under it, and a schedule/playlist/daypart on the screen.
const (
	siteNodeID   = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	orgParentID  = "01J8Z0DEM00RGANCEST0RB0VND"
	screenNodeID = "01J8Z4DEM0SCREENF1RSTPH0TN"
	scheduleID   = "01J8Z5DEM0SCHEDV1ETW0DAYPT"
	playlistID   = "01J8Z6DEM0P1AY11STC0NTENT1"
	daypartID    = "01J8Z7DEM0DAYPARTC0NTENT01"

	siteTZ   = "America/Chicago"
	siteLat  = 41.8781
	siteLong = -87.6298
)

func strp(s string) *string   { return &s }
func f64p(f float64) *float64 { return &f }
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

func openMem(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func gen(t *testing.T, s *store.Store) int64 {
	t.Helper()
	g, err := s.Generation(context.Background())
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}
	return g
}

// orgNode is the tree's root. It is a REAL row every fixture tree starts from:
// the store validates the FULL tree on every scope-node write (DAT-002), so a
// site whose parent_id names a node that was never created is a validation
// failure, not a tolerated subtree boundary.
func orgNode() datamodel.ScopeNode {
	return datamodel.ScopeNode{
		ID:   orgParentID,
		Kind: "org",
		Name: "Demo Org",
		// Both mandatory on this kind (DAT-010/013), and the full-tree validator
		// every scope-node write runs now says so.
		AccountState: "active",
		Entitlements: json.RawMessage(`{}`),
	}
}

func siteNode() datamodel.ScopeNode {
	return datamodel.ScopeNode{
		ID:       siteNodeID,
		Kind:     "site",
		ParentID: strp(orgParentID),
		Name:     "Demo Site",
		TZ:       strp(siteTZ),
		Lat:      f64p(siteLat),
		Long:     f64p(siteLong),
	}
}

// seedOrg creates the org root a fixture site hangs off.
func seedOrg(t *testing.T, s *store.Store) {
	t.Helper()
	if _, err := s.Create(context.Background(), store.KindScopeNode, mustJSON(t, orgNode())); err != nil {
		t.Fatalf("create org node: %v", err)
	}
}

// seedPlacementNode creates ONE org-kind scope node carrying the given id: the
// smallest conformant tree there is, and enough for a row to be placed at.
//
// It is the fixture for a test whose subject is a ROW rather than the tree. A
// row's scope_node is a reference the store now resolves (DAT-006), so placing a
// fixture row at an id no node ever carried is authoring the dangling state the
// store refuses — and DAT-004 says a row may sit at a node of ANY kind, an org
// included, so no site/screen scaffolding is needed to give it somewhere to be.
func seedPlacementNode(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if _, err := s.Create(context.Background(), store.KindScopeNode, mustJSON(t, datamodel.ScopeNode{
		ID: id, Kind: "org", Name: "Fixture Placement Org",
		AccountState: "active", Entitlements: json.RawMessage(`{}`),
	})); err != nil {
		t.Fatalf("seed placement node %s: %v", id, err)
	}
}

func screenNode() datamodel.ScopeNode {
	return datamodel.ScopeNode{
		ID:       screenNodeID,
		Kind:     "screen",
		ParentID: strp(siteNodeID),
		Name:     "Demo Screen",
	}
}

// seedSiteScreen creates the org + site + screen scope nodes — THREE committed
// writes, for a test counting generations or commit hooks.
func seedSiteScreen(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	seedOrg(t, s)
	if _, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, siteNode())); err != nil {
		t.Fatalf("create site node: %v", err)
	}
	if _, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, screenNode())); err != nil {
		t.Fatalf("create screen node: %v", err)
	}
}

func TestOpenStartsAtGenerationZero(t *testing.T) {
	s := openMem(t)
	if g := gen(t, s); g != 0 {
		t.Fatalf("fresh store generation = %d, want 0", g)
	}
}

// TestCreateDuplicateIDIsValidationError: a Create whose id already names a row is
// rejected as a *ValidationError (a well-defined client Problem the api layer maps
// to VALIDATION_FAILED), never a raw PRIMARY KEY constraint error, and writes
// nothing — the generation does not advance.
func TestCreateDuplicateIDIsValidationError(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	seedOrg(t, s)
	if _, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, siteNode())); err != nil {
		t.Fatalf("first create: %v", err)
	}
	genBefore := gen(t, s)

	_, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, siteNode()))
	var verr *store.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("duplicate-id create error = %v, want *store.ValidationError", err)
	}
	if len(verr.Errors) == 0 || verr.Errors[0].Field != "id" {
		t.Fatalf("duplicate-id validation errors = %+v, want an id-field error", verr.Errors)
	}
	if g := gen(t, s); g != genBefore {
		t.Fatalf("generation advanced on rejected duplicate create: %d -> %d", genBefore, g)
	}
}

// TestWriteGuardRunsAtomicallyAndCanReject: a WriteGuard sees the pre-write row
// snapshot and, when it rejects, its error propagates verbatim and nothing is
// written (generation unchanged) — the store enforces a check-then-write invariant
// atomically inside the write transaction rather than trusting a caller's earlier
// snapshot.
func TestWriteGuardRunsAtomicallyAndCanReject(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s) // the org root + one site + one screen already present
	genBefore := gen(t, s)

	sentinel := errors.New("guard rejected")
	sawRows := -1
	guard := func(existing []store.Resource) error {
		sawRows = len(existing)
		return sentinel
	}

	// A create the guard rejects: error is returned verbatim, nothing written.
	dupSite := siteNode()
	dupSite.ID = "01J8Z9SEC0NDS1TEN0DEF0RGRD"
	_, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, dupSite), guard)
	if !errors.Is(err, sentinel) {
		t.Fatalf("guarded create error = %v, want the sentinel verbatim", err)
	}
	if sawRows != 3 {
		t.Fatalf("guard saw %d existing rows, want 3 (the org+site+screen snapshot)", sawRows)
	}
	if g := gen(t, s); g != genBefore {
		t.Fatalf("generation advanced on guard-rejected create: %d -> %d", genBefore, g)
	}

	// A passing guard lets the write through.
	if _, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, dupSite), func([]store.Resource) error { return nil }); err != nil {
		t.Fatalf("guarded create with passing guard: %v", err)
	}
	if g := gen(t, s); g != genBefore+1 {
		t.Fatalf("generation after accepted guarded create = %d, want %d", g, genBefore+1)
	}
}

func TestCreateScopeNodeBumpsRevisionAndGeneration(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	seedOrg(t, s)
	// The org root's own write already bumped the generation, so what this test
	// asserts is ONE further bump — not an absolute value that would have to be
	// re-counted every time the fixture tree grows a node.
	genBefore := gen(t, s)
	res, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, siteNode()))
	if err != nil {
		t.Fatalf("create site node: %v", err)
	}
	if res.Revision != 1 {
		t.Fatalf("created revision = %d, want 1", res.Revision)
	}
	if res.ID != siteNodeID {
		t.Fatalf("created id = %q, want %q", res.ID, siteNodeID)
	}
	if g := gen(t, s); g != genBefore+1 {
		t.Fatalf("generation after one create = %d, want %d", g, genBefore+1)
	}

	// The stored body carries the store-owned revision.
	var body struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatalf("unmarshal stored body: %v", err)
	}
	if body.Revision != 1 {
		t.Fatalf("stored body revision = %d, want 1", body.Revision)
	}
}

func TestCreateScheduleValidatedAndStored(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)

	sched := datamodel.Schedule{ID: scheduleID, ScopeNode: screenNodeID, Name: "Demo Schedule"}
	res, err := s.Create(ctx, store.KindSchedule, mustJSON(t, sched))
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if res.Revision != 1 {
		t.Fatalf("schedule revision = %d, want 1", res.Revision)
	}
	// org + site + screen + schedule = four writes, generation 4.
	if g := gen(t, s); g != 4 {
		t.Fatalf("generation after seed+schedule = %d, want 4", g)
	}

	got, ok, err := s.Get(ctx, store.KindSchedule, scheduleID)
	if err != nil || !ok {
		t.Fatalf("Get schedule: ok=%v err=%v", ok, err)
	}
	if got.ScopeNode != screenNodeID {
		t.Fatalf("stored schedule scope_node = %q, want %q", got.ScopeNode, screenNodeID)
	}
}

func TestUpdateStaleRevisionRejectedNoWrite(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)
	sched := datamodel.Schedule{ID: scheduleID, ScopeNode: screenNodeID, Name: "Demo Schedule"}
	if _, err := s.Create(ctx, store.KindSchedule, mustJSON(t, sched)); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	genBefore := gen(t, s)

	_, err := s.Update(ctx, store.KindSchedule, scheduleID, 99, mustJSON(t, map[string]string{"name": "Renamed"}))
	if err == nil {
		t.Fatalf("stale-revision update succeeded, want ErrRevisionMismatch")
	}
	if !errors.Is(err, store.ErrRevisionMismatch) {
		t.Fatalf("stale-revision update error = %v, want ErrRevisionMismatch", err)
	}
	var rme *store.RevisionMismatchError
	if !errors.As(err, &rme) {
		t.Fatalf("stale-revision update error not a *RevisionMismatchError: %v", err)
	}
	if rme.Current != 1 {
		t.Fatalf("RevisionMismatchError.Current = %d, want 1", rme.Current)
	}

	if g := gen(t, s); g != genBefore {
		t.Fatalf("generation changed on rejected update: %d -> %d", genBefore, g)
	}
	// The stored row is untouched (still revision 1, original name).
	got, _, err := s.Get(ctx, store.KindSchedule, scheduleID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Revision != 1 {
		t.Fatalf("row revision after rejected update = %d, want 1", got.Revision)
	}
}

func TestUpdateCorrectRevisionBumps(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)
	sched := datamodel.Schedule{ID: scheduleID, ScopeNode: screenNodeID, Name: "Demo Schedule"}
	if _, err := s.Create(ctx, store.KindSchedule, mustJSON(t, sched)); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	genBefore := gen(t, s)

	res, err := s.Update(ctx, store.KindSchedule, scheduleID, 1, mustJSON(t, map[string]string{"name": "Renamed Schedule"}))
	if err != nil {
		t.Fatalf("update schedule: %v", err)
	}
	if res.Revision != 2 {
		t.Fatalf("updated revision = %d, want 2", res.Revision)
	}
	if g := gen(t, s); g != genBefore+1 {
		t.Fatalf("generation after update = %d, want %d", g, genBefore+1)
	}

	var body struct {
		Name     string `json:"name"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatalf("unmarshal updated body: %v", err)
	}
	if body.Name != "Renamed Schedule" {
		t.Fatalf("patched name = %q, want Renamed Schedule", body.Name)
	}
	if body.Revision != 2 {
		t.Fatalf("stored body revision = %d, want 2", body.Revision)
	}
}

func TestSchedulingWriteRejectsDanglingReference(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)
	sched := datamodel.Schedule{ID: scheduleID, ScopeNode: screenNodeID, Name: "Demo Schedule"}
	if _, err := s.Create(ctx, store.KindSchedule, mustJSON(t, sched)); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	genBefore := gen(t, s)

	// A daypart referencing a playlist that does not exist must be rejected by
	// datamodel.ValidateRows (DAT-075 REFERENCE_INVALID) before commit.
	dangling := datamodel.Daypart{
		ID:           daypartID,
		ScheduleID:   scheduleID,
		ScopeNode:    screenNodeID,
		DaysOfWeek:   []int{0, 1, 2, 3, 4, 5, 6},
		StartTime:    "06:00:00",
		EndTime:      "22:00:00",
		DisplayPower: "on",
		PlaylistID:   playlistID, // never created
	}
	_, err := s.Create(ctx, store.KindDaypart, mustJSON(t, dangling))
	if err == nil {
		t.Fatalf("create dangling daypart succeeded, want VALIDATION_FAILED")
	}
	var ve *store.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("dangling daypart error not a *ValidationError: %v", err)
	}
	if len(ve.Errors) == 0 {
		t.Fatalf("ValidationError carried no datamodel errors")
	}

	// Nothing was stored and the generation is unchanged.
	rows, err := s.List(ctx, store.KindDaypart, store.ListFilter{})
	if err != nil {
		t.Fatalf("List dayparts: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("dangling daypart was stored: %d rows", len(rows))
	}
	if g := gen(t, s); g != genBefore {
		t.Fatalf("generation changed on rejected write: %d -> %d", genBefore, g)
	}
}

func TestListReturnsIDOrder(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)

	// Insert three playlists out of id order; List must return them sorted by id.
	ids := []string{
		"01J8ZC000000000000000000C3",
		"01J8ZC000000000000000000A1",
		"01J8ZC000000000000000000B2",
	}
	for _, id := range ids {
		pl := datamodel.Playlist{ID: id, ScopeNode: screenNodeID, Name: "PL " + id, Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:deadbeef"}}}
		if _, err := s.Create(ctx, store.KindPlaylist, mustJSON(t, pl)); err != nil {
			t.Fatalf("create playlist %s: %v", id, err)
		}
	}

	rows, err := s.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil {
		t.Fatalf("List playlists: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("List returned %d playlists, want 3", len(rows))
	}
	want := []string{
		"01J8ZC000000000000000000A1",
		"01J8ZC000000000000000000B2",
		"01J8ZC000000000000000000C3",
	}
	for i, r := range rows {
		if r.ID != want[i] {
			t.Fatalf("List[%d].ID = %q, want %q (not id-ordered)", i, r.ID, want[i])
		}
	}
}

func TestDeleteRemovesAndBumpsGeneration(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)
	// An unreferenced playlist can be deleted freely.
	pl := datamodel.Playlist{ID: playlistID, ScopeNode: screenNodeID, Name: "Standalone", Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:cafef00d"}}}
	if _, err := s.Create(ctx, store.KindPlaylist, mustJSON(t, pl)); err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	genBefore := gen(t, s)

	if err := s.Delete(ctx, store.KindPlaylist, playlistID, 1); err != nil {
		t.Fatalf("delete playlist: %v", err)
	}
	if g := gen(t, s); g != genBefore+1 {
		t.Fatalf("generation after delete = %d, want %d", g, genBefore+1)
	}
	if _, ok, err := s.Get(ctx, store.KindPlaylist, playlistID); err != nil || ok {
		t.Fatalf("playlist still present after delete: ok=%v err=%v", ok, err)
	}
}

func TestDeleteStaleRevisionRejected(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)
	pl := datamodel.Playlist{ID: playlistID, ScopeNode: screenNodeID, Name: "Standalone", Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:cafef00d"}}}
	if _, err := s.Create(ctx, store.KindPlaylist, mustJSON(t, pl)); err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	genBefore := gen(t, s)

	err := s.Delete(ctx, store.KindPlaylist, playlistID, 99)
	if !errors.Is(err, store.ErrRevisionMismatch) {
		t.Fatalf("stale delete error = %v, want ErrRevisionMismatch", err)
	}
	if g := gen(t, s); g != genBefore {
		t.Fatalf("generation changed on rejected delete: %d -> %d", genBefore, g)
	}
	if _, ok, _ := s.Get(ctx, store.KindPlaylist, playlistID); !ok {
		t.Fatalf("playlist removed despite stale-revision delete")
	}
}

func TestDeleteReferencedPlaylistRejected(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)
	sched := datamodel.Schedule{ID: scheduleID, ScopeNode: screenNodeID, Name: "Demo Schedule"}
	if _, err := s.Create(ctx, store.KindSchedule, mustJSON(t, sched)); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	pl := datamodel.Playlist{ID: playlistID, ScopeNode: screenNodeID, Name: "Referenced", Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:cafef00d"}}}
	if _, err := s.Create(ctx, store.KindPlaylist, mustJSON(t, pl)); err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	dp := datamodel.Daypart{
		ID: daypartID, ScheduleID: scheduleID, ScopeNode: screenNodeID,
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "06:00:00", EndTime: "22:00:00",
		DisplayPower: "on", PlaylistID: playlistID,
	}
	if _, err := s.Create(ctx, store.KindDaypart, mustJSON(t, dp)); err != nil {
		t.Fatalf("create daypart: %v", err)
	}
	genBefore := gen(t, s)

	// Deleting the playlist a daypart references leaves a dangling reference, so
	// the delete must be rejected by validation and roll back.
	err := s.Delete(ctx, store.KindPlaylist, playlistID, 1)
	var ve *store.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("delete of referenced playlist error = %v, want *ValidationError", err)
	}
	if g := gen(t, s); g != genBefore {
		t.Fatalf("generation changed on rejected delete: %d -> %d", genBefore, g)
	}
	if _, ok, _ := s.Get(ctx, store.KindPlaylist, playlistID); !ok {
		t.Fatalf("referenced playlist was deleted despite dangling reference")
	}
}

func TestDesiredStateRows(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)
	sched := datamodel.Schedule{ID: scheduleID, ScopeNode: screenNodeID, Name: "Demo Schedule"}
	if _, err := s.Create(ctx, store.KindSchedule, mustJSON(t, sched)); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	pl := datamodel.Playlist{ID: playlistID, ScopeNode: screenNodeID, Name: "PL", Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:cafef00d"}}}
	if _, err := s.Create(ctx, store.KindPlaylist, mustJSON(t, pl)); err != nil {
		t.Fatalf("create playlist: %v", err)
	}

	nodes, rows, site, generation, err := s.DesiredStateRows(ctx)
	if err != nil {
		t.Fatalf("DesiredStateRows: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("DesiredStateRows scope nodes = %d, want 3 (org + site + screen)", len(nodes))
	}
	if len(rows.Schedules) != 1 {
		t.Fatalf("DesiredStateRows schedules = %d, want 1", len(rows.Schedules))
	}
	if len(rows.Playlists) != 1 {
		t.Fatalf("DesiredStateRows playlists = %d, want 1", len(rows.Playlists))
	}
	if site.TZ != siteTZ || site.Lat != siteLat || site.Long != siteLong {
		t.Fatalf("site_effective = %+v, want {%s %v %v}", site, siteTZ, siteLat, siteLong)
	}
	if generation != gen(t, s) {
		t.Fatalf("DesiredStateRows generation = %d, out of sync with Generation()", generation)
	}

	// The derived rows round-trip through datamodel validation with zero errors.
	if _, errs := datamodel.ValidateRows(rows); len(errs) != 0 {
		t.Fatalf("DesiredStateRows rows failed validation: %v", errs)
	}
	if _, errs := datamodel.BuildScopeTree(nodes); len(errs) != 0 {
		t.Fatalf("DesiredStateRows scope tree failed validation: %v", errs)
	}
}

// TestConcurrentWritesSerialized exercises the serialized write path: N
// concurrent playlist creates must all commit, with the generation strictly
// monotonic and no lost bump. Each playlist is independent (no cross-row
// constraints), so every write is a legitimate success — the generation must
// therefore end exactly at the seed count plus N.
func TestConcurrentWritesSerialized(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s) // generation now 3 (org + site + screen)
	const seedGen = 3

	const n = 24
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct 26-char fixture id per goroutine (zero-padded index).
			id := "01J8ZP1AY11ST" + padID(i)
			pl := datamodel.Playlist{ID: id, ScopeNode: screenNodeID, Name: "PL", Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:cafef00d"}}}
			if _, err := s.Create(ctx, store.KindPlaylist, mustJSON(t, pl)); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent create error: %v", err)
	}

	rows, err := s.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil {
		t.Fatalf("List playlists: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("stored playlists = %d, want %d (a concurrent write was lost)", len(rows), n)
	}
	if g := gen(t, s); g != seedGen+n {
		t.Fatalf("generation = %d, want %d; a bump was lost or double-counted under concurrency", g, seedGen+n)
	}
}

// TestDesiredStateGenerationBindsConsistentEdgeRuleContent guards the
// (generation, edge-rule content) binding DesiredState's caller relies on
// (REL-053/075: a signed {generation, hash} pair must correspond to exactly one
// atomic store state). Every write in this test is a distinct, compile-clean
// edge-automation Create — each commits exactly one generation bump together
// with exactly one new edge rule row, atomically, in the SAME transaction
// (automations.go) — so at ANY single true store instant the two quantities are
// the same number: generation == stored edge-rule count. A storm of concurrent
// creates races a storm of concurrent DesiredState() reads; every DesiredState()
// result, whenever it lands, must report that same equality.
//
// Regression: DesiredState used to compose two SEPARATELY read-locked calls
// (DesiredStateRows, whose RUnlock released the store's lock, then a later,
// independent RLock/RUnlock in EdgeRuleBodies) rather than one RLock section
// spanning both reads. A create landing in the gap between them committed (and
// bumped the generation) after the first read but before the second, so the
// composite result carried the FIRST read's (now stale) generation alongside the
// SECOND, later read's fresher edge-rule content — a generation/content pair
// that never described one atomic snapshot.
func TestDesiredStateGenerationBindsConsistentEdgeRuleContent(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	const writers = 150
	const readers = 8

	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	for i := 0; i < readers; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ds, err := s.DesiredState(ctx)
				if err != nil {
					t.Errorf("DesiredState: %v", err)
					return
				}
				if int64(len(ds.EdgeRules.Rules)) != ds.Generation {
					t.Errorf("DesiredState generation=%d but EdgeRules carried %d rule(s), want equal — "+
						"a write landed between the store's two independently-locked reads, binding a stale "+
						"generation to fresher edge-rule content (REL-053/075)",
						ds.Generation, len(ds.EdgeRules.Rules))
				}
			}
		}()
	}

	var writerWG sync.WaitGroup
	for i := 0; i < writers; i++ {
		writerWG.Add(1)
		go func(i int) {
			defer writerWG.Done()
			id := "01J8ZDSGENEDG" + padID(i)
			if _, err := s.Create(ctx, store.KindAutomation, edgeAutomation(id)); err != nil {
				t.Errorf("create automation %d: %v", i, err)
			}
		}(i)
	}
	writerWG.Wait()
	close(stop)
	readerWG.Wait()

	if g := gen(t, s); g != writers {
		t.Fatalf("generation after %d automation creates = %d, want %d", writers, g, writers)
	}
	bodies, _, finalGen, err := s.EdgeRuleBodies(ctx)
	if err != nil {
		t.Fatalf("EdgeRuleBodies: %v", err)
	}
	if len(bodies) != writers {
		t.Fatalf("EdgeRuleBodies after storm = %d, want %d", len(bodies), writers)
	}
	if finalGen != int64(writers) {
		t.Fatalf("EdgeRuleBodies generation after storm = %d, want %d", finalGen, writers)
	}
}

// padID renders i as a fixed-width 13-char suffix so every generated fixture id
// is a distinct 26-char string. digits is restricted to the Crockford base32
// alphabet (no I, L, O, U) so a suffix concatenated onto a 13-char valid-ULID
// prefix stays a syntactically valid canonical ULID (DAT-005a).
func padID(i int) string {
	const digits = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	buf := []byte("0000000000000")
	j := len(buf) - 1
	for i > 0 && j >= 0 {
		buf[j] = digits[i%len(digits)]
		i /= len(digits)
		j--
	}
	return string(buf)
}

// TestOnCommitHookFiresOnCommittedWritesOnly pins the post-commit seam the
// feeder hangs its live-connection nudge on (relay/1 REL-057): the hook runs
// exactly once per COMMITTED write, never for a rejected/rolled-back one, and
// it runs OUTSIDE the store's write lock — a hook that re-reads the store
// (exactly what the feeder's snapshot provider does) must not deadlock.
func TestOnCommitHookFiresOnCommittedWritesOnly(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	var mu sync.Mutex
	fired := 0
	var genInHook int64
	s.OnCommit(func() {
		// Re-read the store from inside the hook: proves the hook runs after
		// the write lock is released (a hook under the lock deadlocks here).
		g, err := s.Generation(ctx)
		if err != nil {
			t.Errorf("Generation inside OnCommit hook: %v", err)
		}
		mu.Lock()
		fired++
		genInHook = g
		mu.Unlock()
	})

	seedSiteScreen(t, s) // three committed writes (org + site + screen)
	mu.Lock()
	if fired != 3 {
		t.Fatalf("hook fired %d time(s) after three committed writes, want 3", fired)
	}
	if genInHook != 3 {
		t.Fatalf("generation observed inside the hook = %d, want the committed 3", genInHook)
	}
	mu.Unlock()

	// A rejected write (a duplicate-id Create rolls the tx back, see
	// TestCreateDuplicateIDIsValidationError) fires nothing.
	if _, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, siteNode())); err == nil {
		t.Fatal("duplicate-id create was accepted, want validation failure")
	}
	mu.Lock()
	if fired != 3 {
		t.Fatalf("hook fired %d time(s) after a rolled-back write, want still 3 (no nudge for nothing)", fired)
	}
	mu.Unlock()
}
