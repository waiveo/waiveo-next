package datamodel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// drivenManifestEntry mirrors one contract's record in
// conformance/driven-manifest.json (conformance/cmd/driven-manifest's own
// Entry type) — duplicated here rather than imported because that command is
// `package main` and cannot be imported from another package.
type drivenManifestEntry struct {
	Driver  string   `json:"driver"`
	Driven  []string `json:"driven"`
	Pending []string `json:"pending"`
}

// TestDataModel1DrivenManifestMatchesRuntime proves conformance/driven-manifest.json's
// data-model/1 entry has not drifted from what corpusHandlers actually runs.
// This driver lives inside a _test.go compilation unit the
// conformance/cmd/driven-manifest generator cannot import, so its manifest
// entry is hand-maintained — this test is what keeps it honest: a case added
// to (or removed from) corpusHandlers without updating the checked-in
// manifest entry fails here, in either direction. data-model/1 drives its
// full frozen corpus (TestDataModel1Corpus already asserts corpusHandlers'
// keys == the corpus directory 1:1), so pending is always empty.
func TestDataModel1DrivenManifestMatchesRuntime(t *testing.T) {
	driven := make([]string, 0, len(corpusHandlers))
	for id := range corpusHandlers {
		driven = append(driven, id)
	}
	sort.Strings(driven)

	path := filepath.Join("..", "..", "conformance", "driven-manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var manifest map[string]drivenManifestEntry
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	entry, ok := manifest["data-model/1"]
	if !ok {
		t.Fatalf("%s has no entry for %q", path, "data-model/1")
	}

	if !reflect.DeepEqual(entry.Driven, driven) {
		t.Errorf("conformance/driven-manifest.json[%q].driven = %v, want %v (corpusHandlers keys) — update the hand-maintained entry to match",
			"data-model/1", entry.Driven, driven)
	}
	if len(entry.Pending) != 0 {
		t.Errorf("conformance/driven-manifest.json[%q].pending = %v, want none — data-model/1 has no hardware/dep-gated case", "data-model/1", entry.Pending)
	}
}
