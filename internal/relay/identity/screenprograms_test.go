package identity

import (
	"bytes"
	"path/filepath"
	"testing"
)

// TestServedScreenProgramsEmptyPlaceholderOnFreshStore asserts that a store
// which has never persisted any screen_programs reports the REL-060 empty
// placeholder (`[]`), not a nil/absent value — the offline serve path
// (desiredstate.ServedProgram) then decodes it to an empty program set
// rather than erroring, so a relay that has applied a generation carrying
// no screen_programs still serves cleanly offline.
func TestServedScreenProgramsEmptyPlaceholderOnFreshStore(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	got, err := store.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms() on a fresh store: %v", err)
	}
	if !bytes.Equal(got, []byte("[]")) {
		t.Errorf("LastAppliedScreenPrograms() on a fresh store = %q, want %q (REL-060 empty placeholder)", got, "[]")
	}
}

// TestServedScreenProgramsRoundTrip confirms SetServedScreenPrograms persists
// the applied screen_programs JSON beside the last-applied {generation, hash}
// (REL-055) and LastAppliedScreenPrograms reads back exactly those bytes —
// the signed pointers a screen resolves against its own origin (REL-061),
// never asset bytes (REL-140).
func TestServedScreenProgramsRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The screen_programs facet is a facet of the last-applied generation
	// record, so the row must exist first (SetLastAppliedGeneration creates it).
	if err := store.SetLastAppliedGeneration(50, "sha256:deadbeef"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}

	payload := []byte(`[{"screen_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X6","program_revision":"rev-99","priority":"preempt","display":"content","content":[{"asset_ref":"sha256:cccc","url":"https://app.example/cas/cccc0000","expires_at":1752545000000}]}]`)
	if err := store.SetServedScreenPrograms(payload); err != nil {
		t.Fatalf("SetServedScreenPrograms: %v", err)
	}

	got, err := store.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("LastAppliedScreenPrograms() = %q, want %q", got, payload)
	}
}

// TestServedScreenProgramsSurviveReopen is the REL-055 offline-continuity
// property applied to screen_programs: the persisted program survives a full
// close+reopen of the file-backed store (a power cycle), so a restarted relay
// serves its last-applied screen program purely from durable local storage,
// without first contacting its app peer.
func TestServedScreenProgramsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	payload := []byte(`[{"screen_id":"S1","program_revision":"rev-99","priority":"preempt","display":"content"}]`)

	store1, err := Open(path)
	if err != nil {
		t.Fatalf("Open (1): %v", err)
	}
	if err := store1.SetLastAppliedGeneration(50, "sha256:deadbeef"); err != nil {
		t.Fatalf("SetLastAppliedGeneration: %v", err)
	}
	if err := store1.SetServedScreenPrograms(payload); err != nil {
		t.Fatalf("SetServedScreenPrograms: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close (1): %v", err)
	}

	store2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (2): %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	got, err := store2.LastAppliedScreenPrograms()
	if err != nil {
		t.Fatalf("LastAppliedScreenPrograms() on reopened store: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("LastAppliedScreenPrograms() after reopen = %q, want %q (REL-055 offline continuity)", got, payload)
	}
}
