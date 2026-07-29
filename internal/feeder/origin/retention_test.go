package origin

// The retention surface: Entries (what a sweep decides over) and Remove (the
// single-asset reclamation it drives). An internal test package, because two of
// these cases reach the in-flight bookkeeping Remove consults, which is not, and
// should not be, exported.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const retNow = int64(1_700_000_000_000)

func fixedClock(ms *int64) Option { return WithClock(func() int64 { return *ms }) }

// TestEntriesReportsEveryAssetWithItsSizeAndArrival pins the enumeration a sweep
// decides over: every stored asset, in a stable order, with the size and the
// arrival instant the sweep's windows are measured against.
func TestEntriesReportsEveryAssetWithItsSizeAndArrival(t *testing.T) {
	now := retNow
	o := New(fixedClock(&now))
	first := []byte("first asset")
	second := []byte("the second asset, which is longer")
	refA, err := o.Add(first)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	now += 5_000
	refB, err := o.Add(second)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	entries := o.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries returned %d, want 2", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].HexDigest >= entries[i].HexDigest {
			t.Fatalf("Entries is not in digest order: %v", entries)
		}
	}
	byDigest := map[string]Entry{}
	for _, e := range entries {
		byDigest[e.HexDigest] = e
	}
	a := byDigest[strings.TrimPrefix(refA, "sha256:")]
	b := byDigest[strings.TrimPrefix(refB, "sha256:")]
	if a.SizeBytes != len(first) || b.SizeBytes != len(second) {
		t.Fatalf("sizes = %d/%d, want %d/%d", a.SizeBytes, b.SizeBytes, len(first), len(second))
	}
	if a.StoredAtMs != retNow || b.StoredAtMs != retNow+5_000 {
		t.Fatalf("stored-at = %d/%d, want %d/%d", a.StoredAtMs, b.StoredAtMs, retNow, retNow+5_000)
	}
}

// TestReAddingDoesNotRefreshTheArrivalStamp pins the immutability of the arrival
// instant. The feeder re-adds its placeholder image on every boot, and a client
// retrying an upload re-adds the same bytes; if either reset the stamp, the
// minimum-asset-age guard would never expire on exactly the assets that get
// re-added most, and content would accumulate forever behind a guard that looked
// like it was working.
func TestReAddingDoesNotRefreshTheArrivalStamp(t *testing.T) {
	now := retNow
	dir := t.TempDir()
	o, err := Open(dir, fixedClock(&now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	asset := []byte("re-added on every boot")
	if _, err := o.Add(asset); err != nil {
		t.Fatalf("Add: %v", err)
	}
	now += 30 * 24 * 60 * 60 * 1000
	if _, err := o.Add(asset); err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	if got := o.Entries()[0].StoredAtMs; got != retNow {
		t.Fatalf("stored-at after a re-add = %d, want the original %d", got, retNow)
	}
}

// TestOpenTakesTheArrivalStampFromTheFileMtime pins the property that makes the
// minimum-asset-age guard survive a restart: a reloaded asset's age comes from
// its file, not from the moment the process happened to start. Without it, a box
// that reboots more often than the guard's window would reset every asset's age
// on every boot and never reclaim anything.
func TestOpenTakesTheArrivalStampFromTheFileMtime(t *testing.T) {
	dir := t.TempDir()
	asset := []byte("written in a previous lifetime")
	now := retNow
	first, err := Open(dir, fixedClock(&now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ref, err := first.Add(asset)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	hexDigest := strings.TrimPrefix(ref, "sha256:")

	// Backdate the file, as a real asset written a month ago would be.
	past := time.UnixMilli(retNow - 30*24*60*60*1000)
	if err := os.Chtimes(filepath.Join(dir, hexDigest), past, past); err != nil {
		t.Fatalf("backdate the asset file: %v", err)
	}

	later := retNow + 10_000
	reopened, err := Open(dir, fixedClock(&later))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Entries()[0].StoredAtMs
	if got != past.UnixMilli() {
		t.Fatalf("stored-at after a restart = %d, want the file's mtime %d (not the boot clock %d)",
			got, past.UnixMilli(), later)
	}
}

// TestRemoveDropsBothRepresentations pins that a reclamation is complete: the
// file is unlinked AND the bytes stop being served. Leaving either behind is a
// half-reclamation — disk that never frees, or content that reappears at the next
// boot.
func TestRemoveDropsBothRepresentations(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ref, err := o.Add([]byte("reclaimed"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	hexDigest := strings.TrimPrefix(ref, "sha256:")

	if err := o.Remove(hexDigest); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if o.Has(hexDigest) || o.Serve(hexDigest) != nil {
		t.Fatal("the removed asset is still advertised in memory")
	}
	if _, err := os.Stat(filepath.Join(dir, hexDigest)); !os.IsNotExist(err) {
		t.Fatalf("the removed asset's file is still on disk (stat err %v); it would be reloaded at the next boot", err)
	}
	if len(o.Entries()) != 0 {
		t.Fatalf("Entries still reports %d asset(s) after removing the only one", len(o.Entries()))
	}
	// Removing what is not there is not an error: a sweep that raced a previous
	// removal has nothing to report.
	if err := o.Remove(hexDigest); err != nil {
		t.Fatalf("Remove of an absent asset = %v, want nil", err)
	}
}

// TestRemoveRefusesWhileAnAddIsInFlight is the guard against the worst outcome
// this type can produce: an asset advertised in memory with no bytes on disk.
//
// Add writes the file outside the write lock on purpose — a multi-megabyte upload
// must not stall every concurrent Serve — so a removal landing in that window
// would unlink the file the Add is about to publish. The asset would then serve
// normally, be schedulable, pass every playlist validation, and vanish at the
// next restart, taking every playlist that referenced it with it.
func TestRemoveRefusesWhileAnAddIsInFlight(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	asset := []byte("published and reclaimed at the same instant")
	ref, err := o.Add(asset)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	hexDigest := strings.TrimPrefix(ref, "sha256:")

	// Stand in the middle of an Add: declare it in flight exactly as Add does,
	// which is the state the file-writing window leaves the store in.
	o.mu.Lock()
	o.adding[hexDigest]++
	o.mu.Unlock()

	if err := o.Remove(hexDigest); err != ErrAddInFlight {
		t.Fatalf("Remove during an in-flight add = %v, want ErrAddInFlight", err)
	}
	if !o.Has(hexDigest) {
		t.Fatal("the asset stopped being advertised despite the refusal")
	}
	if _, err := os.Stat(filepath.Join(dir, hexDigest)); err != nil {
		t.Fatalf("the asset's file was unlinked despite the refusal: %v", err)
	}

	// Once the add completes, the same removal proceeds — the refusal is a
	// deferral, not a permanent exemption.
	o.addDone(hexDigest)
	if err := o.Remove(hexDigest); err != nil {
		t.Fatalf("Remove after the add completed = %v, want nil", err)
	}
}

// TestConcurrentAddAndRemoveNeverAdvertiseMissingBytes is the same invariant
// under real concurrency rather than a staged one: whatever interleaving the
// scheduler picks, an asset this store says it has must have bytes on disk behind
// it.
func TestConcurrentAddAndRemoveNeverAdvertiseMissingBytes(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	asset := []byte("added and reclaimed, over and over")
	hexDigest := ""
	if ref, err := o.Add(asset); err != nil {
		t.Fatalf("Add: %v", err)
	} else {
		hexDigest = strings.TrimPrefix(ref, "sha256:")
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = o.Add(asset) }()
		go func() { defer wg.Done(); _ = o.Remove(hexDigest) }()
	}
	wg.Wait()

	if o.Has(hexDigest) {
		if _, err := os.Stat(filepath.Join(dir, hexDigest)); err != nil {
			t.Fatalf("the store advertises %s but its file is gone (%v): it would serve until the next restart and then vanish, "+
				"stranding every playlist that referenced it", hexDigest, err)
		}
	}
}
