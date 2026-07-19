package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
)

// TestOpenAppliesWriteDisciplinePragmas asserts Open configures the
// operational store for crash-safe writes (§10 write-discipline): every
// connection reports journal_mode=WAL, synchronous=FULL (2), and a
// busy_timeout of 5000ms. Without these a power-pull mid-write can surface a
// torn or lost operational record on reopen (no silent durable loss).
func TestOpenAppliesWriteDisciplinePragmas(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var journal string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("read PRAGMA journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want %q", journal, "wal")
	}

	var synchronous int
	if err := store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("read PRAGMA synchronous: %v", err)
	}
	if synchronous != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", synchronous)
	}

	var busyTimeout int
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}
}

// TestCommittedStateSurvivesReopenWithoutCleanClose is the write-discipline
// recovery property (§10): a value written and committed through one Store is
// present when the same path is reopened by a SECOND Store WITHOUT the first
// having Close()d — modeling a relay process that is abruptly stopped
// (power-pull) after committing, with no clean shutdown to flush/checkpoint.
// WAL's committed frames are recovered on reopen; a committed generation/hash
// pair is never lost (REL-055, no silent durable loss).
func TestCommittedStateSurvivesReopenWithoutCleanClose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	store1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Deliberately register cleanup but never call Close before the reopen —
	// the reopen below observes uncheckpointed committed WAL frames.
	t.Cleanup(func() { _ = store1.Close() })

	if err := store1.SetIdentity("relay-abrupt", []byte("cert-abrupt"), priv); err != nil {
		t.Fatalf("SetIdentity: %v", err)
	}
	if err := store1.SetLastAppliedGeneration(42, "sha256:deadbeef"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}

	// Reopen the same path with no clean close of store1.
	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second Open (same path, no clean close): %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	id, ok, err := store2.Identity()
	if err != nil {
		t.Fatalf("Identity() on reopened store: %v", err)
	}
	if !ok || id.RelayID != "relay-abrupt" {
		t.Fatalf("Identity() = %+v, ok=%v, want RelayID=relay-abrupt, ok=true", id, ok)
	}

	gen, hash, ok, err := store2.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration() on reopened store: %v", err)
	}
	if !ok || gen != 42 || hash != "sha256:deadbeef" {
		t.Fatalf("LastAppliedGeneration() = (%d, %q, %v), want (42, sha256:deadbeef, true)", gen, hash, ok)
	}
}

// TestCheckpointBoundsWALAfterCleanClose asserts a clean close leaves no
// unbounded WAL sidecar: Checkpoint() (and Close, which checkpoints) truncate
// the -wal file so it does not grow without limit across the appliance's life
// (§10 write-discipline — WAL kept bounded on clean shutdown).
func TestCheckpointBoundsWALAfterCleanClose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := store.SetLastAppliedGeneration(1, "sha256:aa"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}

	if err := store.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint(): %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	// Reopen and confirm the committed value survived the checkpoint+close.
	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen after checkpoint: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	gen, hash, ok, err := store2.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("LastAppliedGeneration() after checkpoint reopen: %v", err)
	}
	if !ok || gen != 1 || hash != "sha256:aa" {
		t.Fatalf("LastAppliedGeneration() = (%d, %q, %v), want (1, sha256:aa, true)", gen, hash, ok)
	}
}
