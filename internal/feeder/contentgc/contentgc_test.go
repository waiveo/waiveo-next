package contentgc_test

// Per-guard tests for the content retention sweep.
//
// Every case here is built the same way: establish the state, prove the sweep
// KEEPS the asset, then change exactly the one input the guard reads and prove
// the same sweeper now reclaims it. A guard that is never observed letting go is
// indistinguishable from a sweep that does not work, and a test that only
// asserted retention would pass on both.
//
// The store and the content origin are the real ones — an in-memory SQLite store
// advancing real generations, and a dir-backed origin writing real files — so
// what is faked is only what has to be: the fleet oracle (there is no relay in a
// unit test) and, in one case, a filesystem that refuses to delete.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contentgc"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
)

const (
	testNow       = int64(1_800_000_000_000)
	testScopeNode = "01J8ZH000000000000000000S1"
	testPlaylist  = "01J8ZH000000000000000000P1"
)

// harness is one sweep's world: a real store, a real dir-backed origin, a clock
// the case advances by hand, and a fleet the case moves.
type harness struct {
	t       *testing.T
	store   *store.Store
	origin  *origin.Store
	dir     string
	now     int64
	floor   int64
	known   bool
	sweeper *contentgc.Sweeper
}

func newHarness(t *testing.T, opts ...func(*contentgc.Config)) *harness {
	t.Helper()
	h := &harness{t: t, now: testNow, known: true}

	h.dir = t.TempDir()
	var err error
	h.origin, err = origin.Open(h.dir, origin.WithClock(func() int64 { return h.now }))
	if err != nil {
		t.Fatalf("origin.Open: %v", err)
	}
	h.store, err = store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.store.Close() })
	// The node every fixture playlist is placed at. A row's scope_node is a
	// reference the store resolves (DAT-006), so a playlist placed at an id no
	// node carries is refused — and an org-kind node is the smallest conformant
	// tree a row may sit at (DAT-002/DAT-004).
	if _, err := h.store.Create(context.Background(), store.KindScopeNode, json.RawMessage(
		`{"id":"`+testScopeNode+`","kind":"org","name":"Fixture Org","account_state":"active","entitlements":{}}`)); err != nil {
		t.Fatalf("seed the fixture scope node: %v", err)
	}

	cfg := contentgc.Config{
		Origin:     h.origin,
		References: h.store,
		Fleet:      func(target int64) (bool, bool) { return h.floor == target, h.known },
		NowMs:      func() int64 { return h.now },
	}
	for _, o := range opts {
		o(&cfg)
	}
	h.sweeper, err = contentgc.New(cfg)
	if err != nil {
		t.Fatalf("contentgc.New: %v", err)
	}
	return h
}

// add stores bytes and returns the origin's own key for them.
func (h *harness) add(s string) string {
	h.t.Helper()
	ref, err := h.origin.Add([]byte(s))
	if err != nil {
		h.t.Fatalf("origin.Add: %v", err)
	}
	return strings.TrimPrefix(ref, "sha256:")
}

// reference authors a playlist naming every asset given, advancing the store's
// generation exactly as an api write would.
func (h *harness) reference(id string, hexDigests ...string) {
	h.t.Helper()
	items := make([]datamodel.PlaylistItem, 0, len(hexDigests))
	for _, d := range hexDigests {
		items = append(items, datamodel.PlaylistItem{Source: "asset", AssetRef: "sha256:" + d})
	}
	body, err := json.Marshal(datamodel.Playlist{
		ID: id, ScopeNode: testScopeNode, Name: "Playlist " + id, Items: items,
	})
	if err != nil {
		h.t.Fatalf("marshal playlist: %v", err)
	}
	if _, err := h.store.Create(context.Background(), store.KindPlaylist, body); err != nil {
		h.t.Fatalf("create playlist: %v", err)
	}
}

// converge points the fleet at the store's current generation — the steady state
// on a box whose relay is connected and caught up.
func (h *harness) converge() {
	h.t.Helper()
	gen, err := h.store.Generation(context.Background())
	if err != nil {
		h.t.Fatalf("read generation: %v", err)
	}
	h.floor, h.known = gen, true
}

func (h *harness) sweep() contentgc.Result {
	h.t.Helper()
	h.converge()
	res, err := h.sweeper.Sweep(context.Background())
	if err != nil {
		h.t.Fatalf("sweep: %v", err)
	}
	return res
}

// age advances the clock past both windows, which is what an asset that has been
// sitting unreferenced since the previous sweep experiences.
func (h *harness) age() {
	h.now += contentgc.DefaultMinAssetAgeMs
}

func (h *harness) onDisk(hexDigest string) bool {
	h.t.Helper()
	_, err := os.Stat(filepath.Join(h.dir, hexDigest))
	return err == nil
}

// TestFirstObservationNeverReclaims pins the "one sweep is not enough" guard: the
// sweep that first sees an asset unreferenced only marks it, however old the
// bytes are and however converged the fleet is.
func TestFirstObservationNeverReclaims(t *testing.T) {
	h := newHarness(t)
	digest := h.add("never referenced by anything")
	h.age()

	first := h.sweep()
	if first.Reclaimed != 0 {
		t.Fatalf("the first sweep reclaimed %d asset(s); one observation must never delete", first.Reclaimed)
	}
	if first.Retained[contentgc.ReasonFirstObservation] != 1 {
		t.Fatalf("retained = %v, want the asset held as %q", first.Retained, contentgc.ReasonFirstObservation)
	}
	if !h.onDisk(digest) {
		t.Fatal("the asset was deleted on its first observation")
	}

	// The ONLY thing that changes: a second sweep, with the unreferenced window
	// elapsed. Same sweeper, same asset, same fleet.
	h.now += contentgc.DefaultMinUnreferencedAgeMs
	second := h.sweep()
	if second.Reclaimed != 1 {
		t.Fatalf("the second sweep reclaimed %d asset(s), want 1 (retained: %v) — "+
			"the case above proves nothing unless this one reclaims", second.Reclaimed, second.Retained)
	}
	if h.onDisk(digest) {
		t.Fatal("the asset survived a second, eligible sweep")
	}
}

// TestUnreferencedWindowMustElapse separates the two windows: an asset old enough
// by MinAssetAge is still held until it has been unreferenced for
// MinUnreferencedAge, and released the moment it has.
func TestUnreferencedWindowMustElapse(t *testing.T) {
	h := newHarness(t)
	digest := h.add("old bytes, only just unreferenced")
	h.age()

	h.sweep() // marks

	h.now += contentgc.DefaultMinUnreferencedAgeMs - 1
	held := h.sweep()
	if held.Reclaimed != 0 {
		t.Fatalf("reclaimed %d asset(s) one millisecond inside the unreferenced window", held.Reclaimed)
	}
	if held.Retained[contentgc.ReasonMarkTooRecent] != 1 {
		t.Fatalf("retained = %v, want %q", held.Retained, contentgc.ReasonMarkTooRecent)
	}

	h.now++
	released := h.sweep()
	if released.Reclaimed != 1 {
		t.Fatalf("reclaimed %d asset(s) at the window boundary, want 1 (retained: %v)", released.Reclaimed, released.Retained)
	}
	if h.onDisk(digest) {
		t.Fatal("the asset survived past its window")
	}
}

// TestAssetAgeWindowMustElapse is the same shape for the other window: an asset
// observed unreferenced across many sweeps is still held while its BYTES are
// young, and released once they are not.
func TestAssetAgeWindowMustElapse(t *testing.T) {
	h := newHarness(t)
	digest := h.add("recently uploaded, not yet scheduled")

	h.sweep() // marks
	h.now += contentgc.DefaultMinUnreferencedAgeMs
	held := h.sweep()
	if held.Reclaimed != 0 {
		t.Fatalf("reclaimed %d freshly uploaded asset(s)", held.Reclaimed)
	}
	if held.Retained[contentgc.ReasonTooNew] != 1 {
		t.Fatalf("retained = %v, want %q", held.Retained, contentgc.ReasonTooNew)
	}

	h.now = testNow + contentgc.DefaultMinAssetAgeMs
	released := h.sweep()
	if released.Reclaimed != 1 {
		t.Fatalf("reclaimed %d asset(s) once the bytes aged out, want 1 (retained: %v)", released.Reclaimed, released.Retained)
	}
	if h.onDisk(digest) {
		t.Fatal("the asset survived past its age window")
	}
}

// TestReReferencingResetsTheUnreferencedClock is the guard against an asset that
// comes back into use resuming a clock from the last time it was idle.
//
// It matters because the clock is what stands in for "anything still in flight
// has drained". An asset that was referenced ten minutes ago has leases and
// renders out there now; it must serve the whole window again, not inherit
// credit from an earlier idle stretch.
func TestReReferencingResetsTheUnreferencedClock(t *testing.T) {
	h := newHarness(t)
	digest := h.add("dropped, restored, dropped again")
	h.age()

	h.sweep() // marks

	// Most of the window passes, and then the asset is referenced again.
	h.now += contentgc.DefaultMinUnreferencedAgeMs - 1
	h.reference(testPlaylist, digest)
	if res := h.sweep(); res.Retained[contentgc.ReasonReferenced] != 1 {
		t.Fatalf("retained = %v, want the asset held as %q", res.Retained, contentgc.ReasonReferenced)
	}

	// Dropped again. The old mark must be gone: one more millisecond would have
	// been enough under the ORIGINAL mark, and must not be enough now.
	if err := h.store.Delete(context.Background(), store.KindPlaylist, testPlaylist, 1); err != nil {
		t.Fatalf("delete playlist: %v", err)
	}
	h.now++
	if res := h.sweep(); res.Retained[contentgc.ReasonFirstObservation] != 1 {
		t.Fatalf("retained = %v, want the asset re-marked as %q — the clock did not reset", res.Retained, contentgc.ReasonFirstObservation)
	}
	h.now += contentgc.DefaultMinUnreferencedAgeMs - 1
	if res := h.sweep(); res.Reclaimed != 0 {
		t.Fatalf("reclaimed %d asset(s) on a stale mark from before it was referenced again", res.Reclaimed)
	}
	h.now++
	if res := h.sweep(); res.Reclaimed != 1 {
		t.Fatalf("reclaimed %d asset(s) after a full fresh window, want 1 (retained: %v)", res.Reclaimed, res.Retained)
	}
}

// TestPerSweepCapBoundsTheBlastRadius pins the last guard: however many assets
// are eligible, one sweep reclaims at most MaxReclaimPerSweep of them.
//
// It is not an optimization. It is the bound on how much a wrong decision — in
// any of the three guards above, or in the reference read they rest on — can
// destroy before an hour passes and somebody can look.
func TestPerSweepCapBoundsTheBlastRadius(t *testing.T) {
	h := newHarness(t, func(c *contentgc.Config) { c.MaxReclaimPerSweep = 1 })
	for _, s := range []string{"garbage one", "garbage two", "garbage three"} {
		h.add(s)
	}
	h.age()

	h.sweep() // marks all three
	h.now += contentgc.DefaultMinUnreferencedAgeMs
	capped := h.sweep()
	if capped.Reclaimed != 1 {
		t.Fatalf("reclaimed %d asset(s) under a cap of 1", capped.Reclaimed)
	}
	if capped.Retained[contentgc.ReasonBatchLimit] != 2 {
		t.Fatalf("retained = %v, want 2 held as %q", capped.Retained, contentgc.ReasonBatchLimit)
	}

	// The remaining two are not stranded: the next sweep takes one more, and so
	// on. A cap that permanently kept assets would be a leak wearing a guard's
	// clothes.
	next := h.sweep()
	if next.Reclaimed != 1 {
		t.Fatalf("the following sweep reclaimed %d, want 1 — capped assets must not be stranded", next.Reclaimed)
	}
}

// refusingOrigin is a content origin whose Remove always fails: a read-only
// mount, a permissions problem, a filesystem in trouble.
type refusingOrigin struct {
	inner contentgc.ContentOrigin
	err   error
}

func (r refusingOrigin) Entries() []origin.Entry       { return r.inner.Entries() }
func (r refusingOrigin) Remove(hexDigest string) error { return r.err }

// TestARefusedRemovalKeepsTheAssetAndKeepsSweeping pins the failure posture: an
// asset the filesystem will not delete stays served, its failure is reported,
// and the sweep goes on to the others rather than aborting on the first one.
func TestARefusedRemovalKeepsTheAssetAndKeepsSweeping(t *testing.T) {
	h := newHarness(t)
	digest := h.add("the asset the filesystem refuses to delete")
	h.age()
	h.sweep()
	h.now += contentgc.DefaultMinUnreferencedAgeMs

	failing, err := contentgc.New(contentgc.Config{
		Origin:     refusingOrigin{inner: h.origin, err: errors.New("read-only file system")},
		References: h.store,
		Fleet:      func(target int64) (bool, bool) { return h.floor == target, h.known },
		NowMs:      func() int64 { return h.now },
	})
	if err != nil {
		t.Fatalf("contentgc.New: %v", err)
	}
	h.converge()
	// Two sweeps: the first marks (this sweeper has its own marks), the second
	// tries and fails.
	if _, err := failing.Sweep(context.Background()); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	h.now += contentgc.DefaultMinUnreferencedAgeMs
	res, err := failing.Sweep(context.Background())
	if err != nil {
		t.Fatalf("a per-asset removal failure aborted the whole sweep: %v", err)
	}
	if res.Reclaimed != 0 {
		t.Fatalf("reported %d reclaimed while every removal failed", res.Reclaimed)
	}
	if res.Retained[contentgc.ReasonRemoveFailed] != 1 {
		t.Fatalf("retained = %v, want %q", res.Retained, contentgc.ReasonRemoveFailed)
	}
	if len(res.RemoveErrors) != 1 || !strings.Contains(res.RemoveErrors[0].Error(), "read-only") {
		t.Fatalf("RemoveErrors = %v, want the filesystem's own error carried", res.RemoveErrors)
	}
	if !h.onDisk(digest) {
		t.Fatal("the asset is gone from disk although its removal was reported as failed")
	}
}

// TestAnUnreadableReferenceSetReclaimsNothing pins the abort posture: a sweep
// that cannot establish what is referenced deletes nothing at all, rather than
// treating an unreadable reference set as an empty one.
func TestAnUnreadableReferenceSetReclaimsNothing(t *testing.T) {
	h := newHarness(t, func(c *contentgc.Config) { c.References = brokenRefs{} })
	digest := h.add("unreferenced, but the store cannot say so")
	h.age()
	h.now += contentgc.DefaultMinUnreferencedAgeMs

	if _, err := h.sweeper.Sweep(context.Background()); err == nil {
		t.Fatal("a sweep whose reference read failed reported success")
	}
	if !h.onDisk(digest) {
		t.Fatal("an asset was deleted by a sweep that could not read what is referenced")
	}
}

type brokenRefs struct{}

func (brokenRefs) Generation(context.Context) (int64, error) { return 0, nil }

func (brokenRefs) WithContentReferences(context.Context, func(store.ContentReferences) error) error {
	return errors.New("the playlist table could not be read")
}

// TestNewRefusesAWiringThatCannotBeSafe pins the constructor's refusals. A nil
// fleet oracle in particular must not default to "converged": a deployment that
// forgot to wire it would then reclaim against the current generation with no
// idea what the fleet is serving, and would look exactly like one that had.
func TestNewRefusesAWiringThatCannotBeSafe(t *testing.T) {
	base := func() contentgc.Config {
		return contentgc.Config{
			Origin:     origin.New(),
			References: brokenRefs{},
			Fleet:      func(int64) (bool, bool) { return true, true },
		}
	}
	for name, mutate := range map[string]func(*contentgc.Config){
		"no origin":      func(c *contentgc.Config) { c.Origin = nil },
		"no references":  func(c *contentgc.Config) { c.References = nil },
		"no fleet floor": func(c *contentgc.Config) { c.Fleet = nil },
	} {
		cfg := base()
		mutate(&cfg)
		if _, err := contentgc.New(cfg); err == nil {
			t.Errorf("contentgc.New accepted a config with %s", name)
		}
	}
}

// TestAReferenceTheSweeperNeverSawStillResetsTheClock is the case the
// re-referencing test above cannot reach, and it is the one that destroys data.
//
// The mark was cleared only when a sweep OBSERVED the reference. A reference
// created and removed entirely BETWEEN two sweeps is never observed — so the
// clock kept running from an observation taken before the asset was in use, and
// an asset referenced sixty seconds ago was reclaimed on the next sweep. A
// player holding a lease naming it then fetched a 404, and the bytes were gone
// for good: an operator who toggles an asset out of a playlist and back the next
// day finds it unreferencable and has to re-upload.
//
// Every write that removes a reference advances the generation, so a mark taken
// at a different generation is evidence about a reference set this sweeper never
// saw, and must not be counted toward the window.
func TestAReferenceTheSweeperNeverSawStillResetsTheClock(t *testing.T) {
	h := newHarness(t)
	digest := h.add("referenced and unreferenced between two sweeps")
	h.age()

	h.sweep() // marks it unreferenced

	// Almost the whole window passes with no sweep running. Inside that gap the
	// asset is referenced and then unreferenced again — the sweeper sees neither.
	h.now += contentgc.DefaultMinUnreferencedAgeMs - 60_000
	h.reference(testPlaylist, digest)
	if err := h.store.Delete(context.Background(), store.KindPlaylist, testPlaylist, 1); err != nil {
		t.Fatalf("delete playlist: %v", err)
	}
	h.now += 60_000

	// The window has now elapsed against the ORIGINAL mark. It must not count:
	// the asset stopped being referenced sixty seconds ago, not a day ago.
	res := h.sweep()
	if res.Reclaimed != 0 {
		t.Fatalf("reclaimed %d asset(s) referenced 60s ago — the mark survived a reference the sweeper never observed", res.Reclaimed)
	}
	if res.Retained[contentgc.ReasonFirstObservation] != 1 {
		t.Fatalf("retained = %v, want the asset re-marked as %q", res.Retained, contentgc.ReasonFirstObservation)
	}

	// And it is reclaimable once a full window really has elapsed with no
	// intervening reference — the guard delays reclamation, it does not prevent it.
	h.now += contentgc.DefaultMinUnreferencedAgeMs
	if res := h.sweep(); res.Reclaimed != 1 {
		t.Fatalf("reclaimed %d after a genuinely quiet full window, want 1 (retained=%v)", res.Reclaimed, res.Retained)
	}
}

// TestSweepReportsThePlaylistRowCountItActedOn pins the carry-out that makes the
// feeder's zero-row note possible.
//
// The number is deliberately not consulted inside the pass — zero rows is a
// legitimate workspace state and a guard on it would refuse the sweeper's main
// job — so nothing about reclamation changes when it is wrong. That is exactly
// why it needs its own test: a Result field no decision reads is one a refactor
// can quietly stop populating, and the only symptom would be a warning that
// never fires again.
func TestSweepReportsThePlaylistRowCountItActedOn(t *testing.T) {
	h := newHarness(t)
	kept, dropped := h.add("an asset a playlist names"), h.add("an asset nothing names")
	h.reference("01J8ZH000000000000000000R1", kept)
	h.reference("01J8ZH000000000000000000R2", kept)

	if got := h.sweep().PlaylistRows; got != 2 {
		t.Errorf("a sweep over two playlist rows reported PlaylistRows=%d, want 2 — the feeder's "+
			"zero-row reclamation note reads this, so a wrong count silences it or fires it falsely", got)
	}
	_ = dropped
}

// TestSweepOverNoPlaylistsReportsZeroRows is the half the note actually keys on.
// A count that were hardcoded to the number of assets, or to the digest-set size,
// would pass the test above and fail here.
func TestSweepOverNoPlaylistsReportsZeroRows(t *testing.T) {
	h := newHarness(t)
	orphan := h.add("an asset no playlist has ever named")

	first := h.sweep()
	if first.PlaylistRows != 0 {
		t.Fatalf("a sweep over a workspace with no playlists reported PlaylistRows=%d, want 0", first.PlaylistRows)
	}

	// And the same sweep must still reclaim: the count is reported, never a guard.
	// If a future change gates on it, this fails and says why.
	h.age()
	if second := h.sweep(); second.Reclaimed != 1 {
		t.Errorf("a sweep over a zero-playlist workspace reclaimed %d unreferenced asset(s), want 1 — "+
			"PlaylistRows must be REPORTED and never gate reclamation, or content uploaded before it is "+
			"scheduled would accumulate forever", second.Reclaimed)
	}
	if h.onDisk(orphan) {
		t.Error("the unreferenced asset survived a sweep of a workspace with no playlists")
	}
}
