package origin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// purge_test.go covers Purge, which had no tests at all. It is the erasure
// path — the call that destroys a whole workspace's content — so its refusals
// matter more than most, not less.

// TestPurgeRefusesWhileAnAddIsInFlight pins the concurrency refusal.
//
// Add writes its file OUTSIDE the lock and publishes after, so emptying the map
// underneath one leaves the re-publish pointing at a file this call already
// unlinked: an asset advertised in memory with no bytes on disk, served until
// the next restart and then silently gone. Remove already refuses in that
// window and is tested; Purge refuses for the same reason and was not.
//
// Refusing is the right answer for an erasure specifically: the caller is
// destroying a workspace and needs to know it did not complete, rather than be
// told it did while an upload raced past it.
func TestPurgeRefusesWhileAnAddIsInFlight(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ref, err := o.Add([]byte("an asset a racing purge must not orphan"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	hexDigest := strings.TrimPrefix(ref, "sha256:")

	// Stand in the middle of an Add, exactly as the Remove case does.
	o.mu.Lock()
	o.adding[hexDigest]++
	o.mu.Unlock()

	if err := o.Purge(); err == nil {
		t.Fatal("Purge completed while an add was in flight — the add's publish would then advertise an asset " +
			"whose file this call had already unlinked")
	}

	// Nothing was destroyed: a refused erasure must be a no-op, not a partial one.
	if !o.Has(hexDigest) {
		t.Error("the asset stopped being advertised despite the refusal")
	}
	if _, err := os.Stat(filepath.Join(dir, hexDigest)); err != nil {
		t.Errorf("the asset's file was unlinked despite the refusal: %v", err)
	}

	// The refusal is a deferral, not a permanent exemption: once the add
	// completes the same purge proceeds.
	o.addDone(hexDigest)
	if err := o.Purge(); err != nil {
		t.Fatalf("Purge after the add completed: %v", err)
	}
	if o.Has(hexDigest) {
		t.Error("the asset survived a completed purge")
	}
	if _, err := os.Stat(filepath.Join(dir, hexDigest)); !os.IsNotExist(err) {
		t.Errorf("the asset's file survived a completed purge: %v", err)
	}
}

// TestPurgeOnAnInMemoryStoreClearsWithoutTouchingDisk pins the other guard: a
// store with no directory holds its content in memory only, so the file loop is
// skipped rather than run against an empty path.
//
// Without the check the loop would call os.Remove on bare digests relative to
// the process's working directory — paths that belong to nobody. The refusal
// there would be swallowed as os.IsNotExist and the purge would report success,
// so the failure is silent rather than loud, which is why it is worth holding.
func TestPurgeOnAnInMemoryStoreClearsWithoutTouchingDisk(t *testing.T) {
	o := New()
	ref, err := o.Add([]byte("in-memory content"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	hexDigest := strings.TrimPrefix(ref, "sha256:")
	if !o.Has(hexDigest) {
		t.Fatal("the in-memory store did not advertise what it was given")
	}

	if err := o.Purge(); err != nil {
		t.Fatalf("Purge on an in-memory store: %v", err)
	}
	if o.Has(hexDigest) {
		t.Error("the in-memory store still advertises content after a purge")
	}
}

// TestPurgeRemovesEveryStoredAsset is the control for both: a purge with nothing
// in flight really does erase, so neither refusal above is satisfied by a Purge
// that does nothing.
func TestPurgeRemovesEveryStoredAsset(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var digests []string
	for _, body := range []string{"first", "second", "third"} {
		ref, err := o.Add([]byte(body))
		if err != nil {
			t.Fatalf("Add(%s): %v", body, err)
		}
		digests = append(digests, strings.TrimPrefix(ref, "sha256:"))
	}

	if err := o.Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	for _, d := range digests {
		if o.Has(d) {
			t.Errorf("%s is still advertised after a purge", d)
		}
		if _, err := os.Stat(filepath.Join(dir, d)); !os.IsNotExist(err) {
			t.Errorf("%s's file survived a purge: %v", d, err)
		}
	}
}
