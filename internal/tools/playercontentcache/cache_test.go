package playercontentcache

import (
	"strings"
	"testing"
)

// cache_test.go guards the content cache (parity row 2.6) in
// player-v3/source/Program.brs.

// TestACacheHitReturnsBeforeAnyFetch is the cache's whole reason to exist.
//
// The saving is not "fetch less often"; it is that a poll naming an asset this
// device already holds performs NO transfer and NO re-hash at all. Both halves
// are asserted by ONE structural fact: wvEnsureContent's hit path returns before
// control can reach the fetch. If a hit fell through to wvHttpGetToFile, the
// cache would still be "working" by every other measure — right bytes, right
// path, right screen — while costing exactly what it did before it existed.
//
// The ordering is checked rather than mere presence because presence is what a
// careless refactor preserves: moving the early return below the fetch, or
// turning it into a flag consulted afterwards, leaves every call site and every
// test that only greps for `cached` perfectly happy.
func TestACacheHitReturnsBeforeAnyFetch(t *testing.T) {
	body := routineBody(t, readBrs(t, programPath), "wvEnsureContent")

	fetchAt := mustCall(t, body, "wvEnsureContent", "wvHttpGetToFile")

	// The hit path: the branch that marks the result cached and returns. Both
	// markers must appear, and both must appear before the fetch.
	cachedAt := -1
	for i, l := range body {
		if strings.Contains(l.text, "r.cached = true") {
			cachedAt = i
			break
		}
	}
	if cachedAt < 0 {
		t.Fatal("wvEnsureContent never reports a cache hit (`r.cached = true`) — callers cannot distinguish a reused asset from a refetched one, and neither can this guard")
	}
	if cachedAt > fetchAt {
		t.Errorf("wvEnsureContent marks a cache hit at line %d, AFTER its fetch at line %d — a hit that still fetches is not a cache",
			body[cachedAt].n, body[fetchAt].n)
	}

	returnAt := -1
	for i := cachedAt; i < fetchAt; i++ {
		if strings.HasPrefix(body[i].text, "return ") || body[i].text == "return" {
			returnAt = i
			break
		}
	}
	if returnAt < 0 {
		t.Errorf("wvEnsureContent has no return between the cache hit (line %d) and the fetch (line %d): control falls through and the asset is downloaded again anyway",
			body[cachedAt].n, body[fetchAt].n)
	}

	// And the hit path must not re-hash either. wvVerifyStoredBytes reads the
	// WHOLE file to sha256 it, which on a video is the single most expensive
	// thing this player does; paying it every ten seconds is half the defect
	// this row exists to fix.
	if verifyAt := indexOfCall(body, "wvVerifyStoredBytes"); verifyAt >= 0 && verifyAt < cachedAt {
		t.Errorf("wvEnsureContent hashes the stored file (line %d) before deciding it has a cache hit (line %d) — the hit path must cost nothing",
			body[verifyAt].n, body[cachedAt].n)
	}
}

// TestTheCacheKeyIsTheContentAddress: entries are keyed on the asset_ref's
// digest, and the local path is derived from it.
//
// This is what makes skipping the re-hash SOUND rather than merely fast. Keyed
// on anything else — a url, a lease id, a position in the playlist — "the same
// key" would stop meaning "the same bytes", and the hit path (which verifies
// nothing) would start presenting content this run never checked. The previous
// index-derived paths are also why nothing could be cached at all: reordering a
// playlist made every file wrong.
func TestTheCacheKeyIsTheContentAddress(t *testing.T) {
	src := readBrs(t, programPath)

	keyFn := routineBody(t, src, "wvAssetRefKey")
	if !contains(keyFn, "sha256:") {
		t.Error("wvAssetRefKey does not derive the key from the `sha256:` content address")
	}

	pathFn := routineBody(t, src, "wvLocalPathForContent")
	if !contains(pathFn, "hexKey") {
		t.Error("wvLocalPathForContent does not derive the local path from the content digest — a path derived from anything else cannot be reused across a reorder")
	}

	ensure := routineBody(t, src, "wvEnsureContent")
	if indexOfCall(ensure, "wvAssetRefKey") < 0 {
		t.Error("wvEnsureContent does not key on the content address")
	}
}

// TestTheCacheIsTrimmedOnEverySuccessfulLease guards the bound.
//
// An unbounded cache on an unattended device is a fault with a months-long fuse:
// nothing is wrong until the filesystem is full, and then everything is wrong at
// once, fleet-wide, for a reason nobody connects to a caching change. So the
// trim must be CALLED, exactly once, on the success path — after the last item
// (trimming per item could evict an asset a later item in the same Lease is
// about to reuse) and before the Lease is published.
func TestTheCacheIsTrimmedOnEverySuccessfulLease(t *testing.T) {
	body := routineBody(t, readBrs(t, programPath), "wvDoProgram")

	if n := countCalls(body, "wvTrimContentCache"); n != 1 {
		t.Fatalf("wvDoProgram calls wvTrimContentCache %d times, want exactly 1 (on the success path, after the item loop)", n)
	}
	trimAt := mustCall(t, body, "wvDoProgram", "wvTrimContentCache")

	okAt := -1
	for i, l := range body {
		if l.text == "r.ok = true" {
			okAt = i
		}
	}
	if okAt < 0 {
		t.Fatal("wvDoProgram never sets r.ok = true — this guard no longer recognises its success path")
	}
	if trimAt > okAt {
		t.Errorf("wvDoProgram trims the cache at line %d, after marking the Lease ok at line %d", body[trimAt].n, body[okAt].n)
	}

	// The last content-resolving call must come BEFORE the trim: a trim that ran
	// mid-loop could delete a file a later item in the same Lease needs.
	lastEnsure := -1
	for i, l := range body {
		if strings.Contains(l.text, "wvEnsureContent(") {
			lastEnsure = i
		}
	}
	if lastEnsure < 0 {
		t.Fatal("wvDoProgram no longer resolves content through wvEnsureContent")
	}
	if lastEnsure > trimAt {
		t.Errorf("wvDoProgram resolves content at line %d, after trimming at line %d — the trim can evict an asset this same Lease still needs",
			body[lastEnsure].n, body[trimAt].n)
	}
}

// TestTheTrimHonoursBothCapsAndActuallyDeletes: the trim enforces an entry cap
// AND a byte cap, and reclaims space rather than merely forgetting.
//
// Both caps are needed and neither implies the other — a hundred posters blow
// the count while using no space, two long videos blow the budget as two
// entries. And dropping an entry from the map without unlinking the file is the
// version of this bug that looks completely correct in review: the cache reports
// itself as bounded while the filesystem fills anyway.
func TestTheTrimHonoursBothCapsAndActuallyDeletes(t *testing.T) {
	body := routineBody(t, readBrs(t, programPath), "wvTrimContentCache")

	for _, cap := range []string{"wvContentCacheMaxEntries", "wvContentCacheMaxBytes"} {
		if indexOfCall(body, cap) < 0 {
			t.Errorf("wvTrimContentCache does not consult %s", cap)
		}
	}
	if indexOfCall(body, "wvDeleteCachedFile") < 0 {
		t.Error("wvTrimContentCache never deletes a file: dropping the entry alone leaves the bytes on the device forever, so the cache reports itself bounded while the filesystem fills")
	}
	if !contains(body, "cache.entries.Delete(") {
		t.Error("wvTrimContentCache never removes an evicted entry from the map: the cache would keep claiming to hold a file it just deleted, and the next hit would hand a player a path to nothing")
	}
	// The previous generation's keys are protected, so a program change cannot
	// delete a file the scene may still be rendering from.
	if !contains(body, "keepPrev") {
		t.Error("wvTrimContentCache does not protect the previous Lease's assets: a program change can delete the file a Video node is still decoding")
	}
}

// TestTheCacheSweepsStaleFilesAtStartup guards the cold-start half of the bound.
//
// In-run the cache knows every file it wrote and can trim them; across a restart
// it knows nothing, so anything already on disk under this player's prefix is
// unaccounted-for and must be reclaimed. Without this, files accumulate across
// reboots forever, which on an unattended fleet is the only way this cache could
// ever fill a device — the caps cannot see a file no entry describes.
func TestTheCacheSweepsStaleFilesAtStartup(t *testing.T) {
	src := readBrs(t, programPath)

	if indexOfCall(routineBody(t, src, "wvContentCache"), "wvSweepContentCacheDir") < 0 {
		t.Error("wvContentCache does not sweep the cache directory when it is created; files from previous runs are never reclaimed")
	}

	sweep := routineBody(t, src, "wvSweepContentCacheDir")
	if indexOfCall(sweep, "wvContentCachePrefix") < 0 {
		t.Error("wvSweepContentCacheDir does not scope its deletions to this player's own filename prefix — a sweep that deletes by any broader rule is deleting other software's files")
	}
	if indexOfCall(sweep, "wvLegacyContentCachePrefix") < 0 {
		t.Error("wvSweepContentCacheDir does not collect the legacy per-index files an upgraded device still holds; nothing else ever will")
	}
}
