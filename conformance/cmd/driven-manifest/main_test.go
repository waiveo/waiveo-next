package main

import (
	"reflect"
	"testing"
)

// TestGeneratedEntriesMatchCheckedInManifest is the freshness teeth: it calls
// generate() itself — never `go run` — so it runs under plain `go test ./...`
// with no extra CI step, and fails the instant a driver's live driven/pending
// set diverges from what conformance/driven-manifest.json says on disk, in
// either direction: a driver that starts (or stops) driving a case without
// anyone re-running `-write` breaks this test, not just the traceability
// checker downstream.
func TestGeneratedEntriesMatchCheckedInManifest(t *testing.T) {
	generated, err := generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	checkedIn, err := readManifest(manifestFilePath())
	if err != nil {
		t.Fatalf("readManifest(%s): %v", manifestPath, err)
	}

	for _, contract := range generatedContracts {
		want, ok := generated[contract]
		if !ok {
			t.Errorf("generate() produced no entry for %q — a driver was added to generatedContracts without a generate() case", contract)
			continue
		}
		got, ok := checkedIn[contract]
		if !ok {
			t.Errorf("%s has no entry for %q — run `go run ./conformance/cmd/driven-manifest -write`", manifestPath, contract)
			continue
		}
		if !reflect.DeepEqual(got.Driven, want.Driven) {
			t.Errorf("%s[%q].driven = %v, live driver reports %v — run `go run ./conformance/cmd/driven-manifest -write` to refresh it",
				manifestPath, contract, got.Driven, want.Driven)
		}
		if !reflect.DeepEqual(got.Pending, want.Pending) {
			t.Errorf("%s[%q].pending = %v, live driver reports %v — run `go run ./conformance/cmd/driven-manifest -write` to refresh it",
				manifestPath, contract, got.Pending, want.Pending)
		}
	}

	// The reverse direction: every generated-owned key in the checked-in file
	// must correspond to a driver generate() still knows how to run — a stale
	// leftover entry for a contract no longer generated would silently stop
	// being kept fresh.
	for contract := range checkedIn {
		owned := false
		for _, c := range generatedContracts {
			if c == contract {
				owned = true
				break
			}
		}
		if !owned {
			continue // hand-maintained entry (data-model/1, rules/1, ui-schema/1, ...) — not this tool's concern.
		}
		if _, ok := generated[contract]; !ok {
			t.Errorf("%s has a Go-driver-owned entry %q that generate() no longer produces", manifestPath, contract)
		}
	}
}

// TestMergePreservesHandMaintainedEntries proves merge() never touches a key
// outside generatedContracts — the seam that lets data-model/1, rules/1, and
// ui-schema/1 live in the same file without this tool clobbering them on
// every -write.
func TestMergePreservesHandMaintainedEntries(t *testing.T) {
	handMaintained := Entry{Driver: "hand-maintained example", Driven: []string{"XXX-001"}, Pending: nil}
	existing := Manifest{
		"rules/1": handMaintained,
		"api/1":   {Driver: "stale", Driven: []string{"API-999"}, Pending: nil},
	}
	generated := map[string]Entry{
		"api/1": {Driver: "fresh", Driven: []string{"API-010"}, Pending: []string{}},
	}

	got := merge(existing, generated)

	if !reflect.DeepEqual(got["rules/1"], handMaintained) {
		t.Errorf("merge() altered the hand-maintained rules/1 entry: got %+v, want %+v", got["rules/1"], handMaintained)
	}
	if !reflect.DeepEqual(got["api/1"], generated["api/1"]) {
		t.Errorf("merge() did not overwrite the generated api/1 entry: got %+v, want %+v", got["api/1"], generated["api/1"])
	}
}
