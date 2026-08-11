package playercontentcache

import (
	"fmt"
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

// ─────────────────────────────────────────────────────────────────────────────
// The trim, EXECUTED. Everything below drives the real wvTrimContentCache out of
// the shipped Program.brs (see brsrun_test.go) instead of reading it.
//
// This replaces a structural guard that asserted the trim MENTIONS both caps,
// calls wvDeleteCachedFile, deletes from the map and mentions keepPrev. Every
// one of those was true of a trim that could never evict anything: the shipped
// routine handed its PROTECTED set forward as `keepPrev`, which made the
// protected set the running union of every key ever kept, so `oldestKey` was
// always "" and both caps were dead code from the second poll onwards. A
// reviewer confirmed the guard was blind by making the trim provably incapable
// of eviction and watching the package stay green. Structure was the wrong
// question; these ask what the cache CONTAINS after N polls.
// ─────────────────────────────────────────────────────────────────────────────

// cacheHarness is one player thread's content cache, ready to be trimmed.
type cacheHarness struct {
	in    *interp
	cache *assoc
	// tick mirrors wvEnsureContent's own counter, which is what `used` (the LRU
	// ordering) is drawn from.
}

func newCacheHarness(t *testing.T) *cacheHarness {
	t.Helper()
	in := newInterp(t, programPath)
	cache, ok := in.call("wvContentCache").(*assoc)
	if !ok {
		t.Fatal("wvContentCache() did not return an associative array")
	}
	return &cacheHarness{in: in, cache: cache}
}

func (h *cacheHarness) entries() *assoc {
	e, ok := h.cache.get("entries").(*assoc)
	if !ok {
		panic("the cache has no `entries` map")
	}
	return e
}

// store adds one materialized asset exactly as wvEnsureContent's fetch path does
// (Program.brs: `cache.tick = cache.tick + 1` then
// `cache.entries[key] = { path:, sizeBytes:, used: cache.tick }`), so the LRU
// order these tests assert on is the order the player itself would produce.
func (h *cacheHarness) store(key, path string, sizeBytes int) {
	tick, _ := h.cache.get("tick").(int)
	tick++
	h.cache.set("tick", tick)
	entry := newAssoc()
	entry.set("path", path)
	entry.set("sizeBytes", sizeBytes)
	entry.set("used", tick)
	h.entries().set(key, entry)
}

// poll is one whole Lease: it materializes the named assets and then trims,
// which is the only sequence the player ever performs.
func (h *cacheHarness) poll(t *testing.T, sizeBytes int, keys ...string) {
	t.Helper()
	keep := newAssoc()
	for _, k := range keys {
		h.store(k, "cachefs:/wv_"+k+".bin", sizeBytes)
		keep.set(k, true)
	}
	h.in.call("wvTrimContentCache", keep)
}

func (h *cacheHarness) holds(key string) bool { return h.entries().has(key) }

func (h *cacheHarness) totalBytes() int {
	total := 0
	for _, k := range h.entries().keyList() {
		e := h.entries().get(k).(*assoc)
		n, _ := e.get("sizeBytes").(int)
		total += n
	}
	return total
}

const mib = 1024 * 1024

// TestTheTrimEvictsPastTheEntryCapAndUnlinksTheBytes: the count cap is real.
//
// A hundred posters blow the entry count while using almost no space, so the
// cap has to bite on count alone — and the eviction has to UNLINK, not merely
// forget: dropping the entry without deleting the file is the version of this
// bug that looks completely correct in review, because the cache reports itself
// bounded while the filesystem fills anyway.
func TestTheTrimEvictsPastTheEntryCapAndUnlinksTheBytes(t *testing.T) {
	h := newCacheHarness(t)
	maxEntries := h.in.call("wvContentCacheMaxEntries").(int)

	// One Lease that references far more assets than the cap allows, then two
	// ordinary Leases naming a single new asset each. TWO are needed, not one:
	// the wide program is the previous generation after the first of them and
	// still carries its grace, and only becomes evictable once a further program
	// change has retired it.
	wide := make([]string, 0, maxEntries+4)
	for i := 0; i < maxEntries+4; i++ {
		wide = append(wide, fmt.Sprintf("image:%02d", i))
	}
	stored := len(wide) + 2
	h.poll(t, 4096, wide...)
	h.poll(t, 4096, "image:new")
	h.poll(t, 4096, "image:newer")

	if got := h.entries().count(); got > maxEntries {
		t.Errorf("after three polls the cache holds %d entries, cap is %d — the entry cap is not enforced", got, maxEntries)
	}
	if !h.holds("image:newer") {
		t.Error("the trim evicted the asset THIS Lease is about to play")
	}
	// The oldest unprotected keys are the ones that went, and their bytes with
	// them.
	if h.holds("image:00") {
		t.Error("the least-recently-used entry survived: eviction is not LRU-ordered, or is not happening at all")
	}
	if len(h.in.deleted) == 0 {
		t.Fatal("nothing was handed to roFileSystem.Delete: the trim forgot entries without reclaiming a single byte")
	}
	for _, path := range h.in.deleted {
		if !strings.HasPrefix(path, "cachefs:/wv_") {
			t.Errorf("the trim deleted %q, which is not a file this player wrote", path)
		}
	}
	if h.entries().count()+len(h.in.deleted) != stored {
		t.Errorf("%d entries remain and %d files were deleted, but %d were stored: an entry left the map without its bytes being reclaimed",
			h.entries().count(), len(h.in.deleted), stored)
	}
}

// TestTheTrimEvictsPastTheByteCap: the byte cap is real, and independent.
//
// Two long videos blow the budget as two entries, which no count cap can see.
func TestTheTrimEvictsPastTheByteCap(t *testing.T) {
	h := newCacheHarness(t)
	maxBytes := h.in.call("wvContentCacheMaxBytes").(int)

	// Three clips of 40 MiB each: three entries (well under the count cap) and
	// 120 MiB (over the byte cap), reached one Lease at a time the way a screen
	// cycling through a playlist reaches it.
	h.poll(t, 40*mib, "video:a")
	h.poll(t, 40*mib, "video:b")
	h.poll(t, 40*mib, "video:c")

	if got := h.totalBytes(); got > maxBytes {
		t.Errorf("the cache holds %d bytes, cap is %d — the byte cap is not enforced", got, maxBytes)
	}
	if h.holds("video:a") {
		t.Error("the oldest clip survived a cache that is over its byte budget")
	}
	if !h.holds("video:c") {
		t.Error("the trim evicted the clip THIS Lease is about to play")
	}
	if len(h.in.deleted) != 1 || h.in.deleted[0] != "cachefs:/wv_video:a.bin" {
		t.Errorf("deleted %v, want exactly the oldest clip's file", h.in.deleted)
	}
}

// TestTheTrimGrantsExactlyOneGenerationOfGrace is the defect, stated directly.
//
// The previous Lease's assets are protected because the scene may still be
// rendering the outgoing program — a Video node decoding from a file — so a
// program change must not delete under it. That grace is ONE generation. If
// `keepPrev` is fed the protected set rather than this Lease's own keys it
// becomes the union of every key ever kept, every entry is protected forever,
// and both caps stop existing: the shipped bug, which no structural guard could
// see.
//
// The sizes are chosen so the cap is ALREADY exceeded at the second poll: two
// 60 MiB clips are 24 MiB over budget, so a trim with no grace would evict the
// outgoing program's clip right there, and a trim with a running-union keepPrev
// never evicts anything at the third. Both halves are therefore load-bearing —
// the outgoing program's clip SURVIVES, and the one before it does NOT.
func TestTheTrimGrantsExactlyOneGenerationOfGrace(t *testing.T) {
	h := newCacheHarness(t)
	maxBytes := h.in.call("wvContentCacheMaxBytes").(int)

	h.poll(t, 60*mib, "video:gen1")
	h.poll(t, 60*mib, "video:gen2")
	if h.totalBytes() <= maxBytes {
		t.Fatal("two clips no longer put the cache over its byte cap, so nothing here is under eviction pressure and the grace is not being tested")
	}
	if !h.holds("video:gen1") {
		t.Fatal("the previous Lease's clip was evicted while the scene may still be decoding from it — the one generation of grace is gone")
	}
	h.poll(t, 60*mib, "video:gen3")

	if !h.holds("video:gen3") || !h.holds("video:gen2") {
		t.Errorf("the current or previous Lease's clip was evicted: entries = %v", h.entries().keyList())
	}
	if h.holds("video:gen1") {
		t.Errorf("two generations back is still protected, so `keepPrev` is accumulating rather than being replaced by THIS Lease's keys — both caps are inert. entries = %v", h.entries().keyList())
	}
}

// TestTheCacheStaysBoundedAcrossManyPolls is the property the whole subsystem
// exists for: an unattended screen that has cycled through hundreds of distinct
// assets still fits in its caps.
//
// The fault this prevents has a months-long fuse — nothing is wrong until
// cachefs is full, and then wvHttpGetToFile fails, the poll errors, and the
// screen is frozen on its last program fleet-wide, for a reason nobody connects
// to a caching change.
func TestTheCacheStaysBoundedAcrossManyPolls(t *testing.T) {
	h := newCacheHarness(t)
	maxEntries := h.in.call("wvContentCacheMaxEntries").(int)
	maxBytes := h.in.call("wvContentCacheMaxBytes").(int)

	const polls = 200
	for i := 0; i < polls; i++ {
		h.poll(t, 8*mib, fmt.Sprintf("image:asset%03d", i))
		if got := h.entries().count(); got > maxEntries {
			t.Fatalf("poll %d: %d entries held, cap is %d", i, got, maxEntries)
		}
		if got := h.totalBytes(); got > maxBytes {
			t.Fatalf("poll %d: %d bytes held, cap is %d", i, got, maxBytes)
		}
	}
	if h.entries().count() == polls {
		t.Fatal("every asset ever fetched is still cached")
	}
	if want := polls - h.entries().count(); len(h.in.deleted) != want {
		t.Errorf("%d files unlinked over %d polls, want %d — entries left the map without their bytes", len(h.in.deleted), polls, want)
	}
}

// TestTheTrimNeverEvictsWhatIsOnScreen: over budget is not a licence to delete a
// file that is being played, and being over budget with nothing evictable must
// TERMINATE rather than spin.
//
// A single program larger than the byte cap must still play — that is why the
// caps alone could never be the bound — so the trim's last word is the keep-set,
// not the cap.
func TestTheTrimNeverEvictsWhatIsOnScreen(t *testing.T) {
	h := newCacheHarness(t)
	maxBytes := h.in.call("wvContentCacheMaxBytes").(int)

	// One Lease whose own content exceeds the byte cap on its own.
	h.poll(t, 60*mib, "video:featureA", "video:featureB")

	if h.totalBytes() <= maxBytes {
		t.Fatal("this case no longer puts the cache over its byte cap, so it is not testing the over-budget path")
	}
	if !h.holds("video:featureA") || !h.holds("video:featureB") {
		t.Error("the trim evicted content the current Lease is playing to satisfy a cap")
	}
	if len(h.in.deleted) != 0 {
		t.Errorf("the trim deleted %v while every entry was in use", h.in.deleted)
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
