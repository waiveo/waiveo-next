package rules

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

// loadDrivenManifestEntry reads the checked-in manifest and returns the
// record for the named contract, failing the test if either is missing.
func loadDrivenManifestEntry(t *testing.T, contract string) drivenManifestEntry {
	t.Helper()
	path := filepath.Join("..", "..", "conformance", "driven-manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var manifest map[string]drivenManifestEntry
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	entry, ok := manifest[contract]
	if !ok {
		t.Fatalf("%s has no entry for %q", path, contract)
	}
	return entry
}

// TestRulesDrivenManifestMatchesRuntime proves conformance/driven-manifest.json's
// rules/1 entry has not drifted from what the two rules-1 corpus drivers
// (this package's own compile-time oracle in corpus_test.go and the
// evaluation oracle in eval_corpus_test.go) actually run. Both live inside
// _test.go compilation units the conformance/cmd/driven-manifest generator
// cannot import, so this entry is hand-maintained — this test is what keeps
// it honest: the union of evalCorpusHandlers' keys and expectedCompileRan
// (itself pinned against runtime drift by TestRulesCompileCorpus) must equal
// the manifest's rules/1 driven list exactly, in both directions, and its
// pending list must be empty (rules/1 has no hardware/dep-gated case).
func TestRulesDrivenManifestMatchesRuntime(t *testing.T) {
	seen := map[string]bool{}
	var driven []string
	for id := range evalCorpusHandlers {
		if !seen[id] {
			seen[id] = true
			driven = append(driven, id)
		}
	}
	for _, id := range expectedCompileRan {
		if !seen[id] {
			seen[id] = true
			driven = append(driven, id)
		}
	}
	sort.Strings(driven)

	entry := loadDrivenManifestEntry(t, "rules/1")
	if !reflect.DeepEqual(entry.Driven, driven) {
		t.Errorf("conformance/driven-manifest.json[%q].driven = %v, want %v (evalCorpusHandlers keys ∪ expectedCompileRan) — "+
			"update the hand-maintained entry to match", "rules/1", entry.Driven, driven)
	}
	if len(entry.Pending) != 0 {
		t.Errorf("conformance/driven-manifest.json[%q].pending = %v, want none — rules/1 has no hardware/dep-gated case", "rules/1", entry.Pending)
	}
}
