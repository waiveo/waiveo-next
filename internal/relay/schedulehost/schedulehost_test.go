package schedulehost

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// demoScreenScopeNodeID is the SAME fixture-ULID the feeder's own demo
// schedule (internal/feeder/snapshot, REL-065) attaches its schedule row to
// (its own demoScreenScopeNodeID constant is unexported, so this is a
// byte-exact copy — the cross-package fixture-sharing pattern this codebase
// already uses, e.g. internal/relay/automationhost's testScreenEntity).
const demoScreenScopeNodeID = "01J8Z4DEMOSCREENFIRSTPHOTN"

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
