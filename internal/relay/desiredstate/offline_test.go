package desiredstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

const rel061Corpus = "../../../conformance/corpora/relay-1/REL-061-valid-preempt-priority-screen-program-offline.json"

// rel061Doc decodes exactly the REL-061 fields the offline-serve oracle reads:
// the persisted last-applied generation, the persisted screen_programs, and
// the expected served entry (priority/display/revision), plus the flags that
// name the offline property itself.
type rel061Doc struct {
	Input struct {
		RelayAppPeerConnectivity string `json:"relay_app_peer_connectivity"`
		PersistedLastApplied     struct {
			Generation int64  `json:"generation"`
			Hash       string `json:"hash"`
		} `json:"relay_persisted_last_applied"`
		PersistedScreenPrograms []wire.ScreenProgram `json:"persisted_sections_screen_programs"`
	} `json:"input"`
	Expected struct {
		ServedFromPersistedSnapshotWithoutAppPeer bool   `json:"served_from_persisted_snapshot_without_app_peer"`
		ScreenProgramEntryPriority                string `json:"screen_program_entry_priority"`
		ScreenProgramEntryDisplay                 string `json:"screen_program_entry_display"`
		ProgramRevisionDelivered                  string `json:"program_revision_delivered"`
		RequiresLiveAppPeerConnection             bool   `json:"requires_live_app_peer_connection"`
	} `json:"expected"`
}

func loadRel061(t *testing.T) rel061Doc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel061Corpus))
	if err != nil {
		t.Fatalf("reading REL-061 corpus: %v", err)
	}
	var doc rel061Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding REL-061 corpus: %v", err)
	}
	return doc
}

// TestServedProgramOfflineFromPersistedSnapshot is the REL-061 differential
// oracle: a relay whose app peer is DISCONNECTED serves a screen's program
// purely from its own last-applied, durably persisted desired-state snapshot.
// The scenario is staged straight from the corpus fixture — the persisted
// {generation, hash} and the persisted screen_programs entry carrying
// priority: preempt / display: content — and ServedProgram is then invoked
// with NO feeder in existence at all: its only input is the identity store,
// so the emergency-takeover assignment (rev-99, preempt, content) reaches the
// serve path with no live app-peer connection required (REL-055/061).
func TestServedProgramOfflineFromPersistedSnapshot(t *testing.T) {
	doc := loadRel061(t)

	// Corpus self-checks: this oracle only means anything if the fixture
	// actually declares the offline property it asserts.
	if doc.Input.RelayAppPeerConnectivity != "disconnected" {
		t.Fatalf("corpus self-check: REL-061 input.relay_app_peer_connectivity = %q, want \"disconnected\"", doc.Input.RelayAppPeerConnectivity)
	}
	if !doc.Expected.ServedFromPersistedSnapshotWithoutAppPeer {
		t.Fatal("corpus self-check: REL-061 expects served_from_persisted_snapshot_without_app_peer:true")
	}
	if doc.Expected.RequiresLiveAppPeerConnection {
		t.Fatal("corpus self-check: REL-061 expects requires_live_app_peer_connection:false")
	}

	// Stage the relay's OWN durable operational store as if a prior generation
	// had already been applied and persisted — the only state the serve path
	// is allowed to read here. No feeder, no connection, no app peer.
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.SetLastAppliedGeneration(doc.Input.PersistedLastApplied.Generation, doc.Input.PersistedLastApplied.Hash); err != nil {
		t.Fatalf("seed SetLastAppliedGeneration: %v", err)
	}
	programsJSON, err := json.Marshal(doc.Input.PersistedScreenPrograms)
	if err != nil {
		t.Fatalf("marshal persisted screen_programs: %v", err)
	}
	if err := store.SetServedScreenPrograms(programsJSON); err != nil {
		t.Fatalf("seed SetServedScreenPrograms: %v", err)
	}

	// The serve path: ServedProgram's SOLE input is the store — there is no
	// feeder URL parameter, so it structurally cannot contact an app peer.
	served, err := ServedProgram(store)
	if err != nil {
		t.Fatalf("ServedProgram: %v", err)
	}
	if len(served) != 1 {
		t.Fatalf("ServedProgram returned %d screen_programs, want 1", len(served))
	}
	entry := served[0]

	if entry.Priority != doc.Expected.ScreenProgramEntryPriority {
		t.Errorf("served priority = %q, want %q (REL-061 preempt carried opaquely through offline continuity)", entry.Priority, doc.Expected.ScreenProgramEntryPriority)
	}
	if entry.Display != doc.Expected.ScreenProgramEntryDisplay {
		t.Errorf("served display = %q, want %q (REL-061 display carried opaquely through offline continuity)", entry.Display, doc.Expected.ScreenProgramEntryDisplay)
	}
	if entry.ProgramRevision != doc.Expected.ProgramRevisionDelivered {
		t.Errorf("served program_revision = %q, want %q", entry.ProgramRevision, doc.Expected.ProgramRevisionDelivered)
	}
	if !reflect.DeepEqual(entry, doc.Input.PersistedScreenPrograms[0]) {
		t.Errorf("served entry = %+v, want the persisted entry unmodified %+v (opaque carriage, REL-061/062/065)", entry, doc.Input.PersistedScreenPrograms[0])
	}
}

// TestApplyPersistsScreenProgramsForOfflineServe asserts the write half of
// the offline-continuity loop: a successful live apply persists the applied
// screen_programs into the durable store, so a subsequent ServedProgram —
// with no further app-peer contact — returns exactly the array the verified
// snapshot carried (REL-055/061).
func TestApplyPersistsScreenProgramsForOfflineServe(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, []wire.PairingGrant{grant.Mint()})
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}

	served, err := ServedProgram(store)
	if err != nil {
		t.Fatalf("ServedProgram after apply: %v", err)
	}
	if !reflect.DeepEqual(served, snap.Sections.ScreenPrograms) {
		t.Errorf("ServedProgram after apply = %+v, want the applied snapshot's screen_programs %+v", served, snap.Sections.ScreenPrograms)
	}
	if !reflect.DeepEqual(served, applied.ScreenPrograms) {
		t.Errorf("ServedProgram after apply = %+v, want Applied.ScreenPrograms %+v (same persisted array)", served, applied.ScreenPrograms)
	}
}

// TestApplyAppliesGenerationAndProgramsInLockstep is the REL-056 atomic-swap
// property observed at the VerifyAndApply boundary: a single successful apply
// leaves the persisted last-applied generation AND the persisted
// screen_programs both reflecting the SAME snapshot — never the new
// generation paired with a prior generation's program. VerifyAndApply
// persists them through the store's single atomic apply-unit
// (identity.ApplyGeneration), so the two facets can never read back
// cross-generation.
func TestApplyAppliesGenerationAndProgramsInLockstep(t *testing.T) {
	img := loadTestImage(t)
	id := testFeederIdentity(t)

	snap, err := snapshot.Build(img, "https://origin.example", id, []wire.PairingGrant{grant.Mint()})
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}

	ts := newTestFeeder(t, id)
	store := enrolledStore(t, ts)

	applied, err := applySnapshot(t, store, snap)
	if err != nil {
		t.Fatalf("VerifyAndApply: %v", err)
	}

	// The persisted generation must equal the generation whose programs were
	// persisted — read the two facets back independently and confirm they agree.
	gen, _, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration: gen=%d ok=%v err=%v", gen, ok, err)
	}
	if gen != applied.Generation {
		t.Errorf("persisted generation %d != applied generation %d", gen, applied.Generation)
	}
	served, err := ServedProgram(store)
	if err != nil {
		t.Fatalf("ServedProgram: %v", err)
	}
	if !reflect.DeepEqual(served, snap.Sections.ScreenPrograms) {
		t.Errorf("persisted screen_programs = %+v, want the pulled generation's %+v (REL-056: generation and programs applied in lockstep)", served, snap.Sections.ScreenPrograms)
	}
}

// TestServedProgramEmptyOnFreshStore asserts a store that has never applied a
// generation serves an empty (non-nil-erroring) program set — the REL-060
// empty-placeholder decode, so a never-synced relay's serve path is a clean
// no-program rather than a hard error.
func TestServedProgramEmptyOnFreshStore(t *testing.T) {
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	served, err := ServedProgram(store)
	if err != nil {
		t.Fatalf("ServedProgram on a fresh store: %v", err)
	}
	if len(served) != 0 {
		t.Errorf("ServedProgram on a fresh store returned %d entries, want 0", len(served))
	}
}
