package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/compile"
)

// corpusCase is the subset of a conformance/corpora/rules-1 case this compiler
// FRONT-END oracle reads. A case is coverable here iff it supplies a single
// input.rule and its expected block carries a compile-time signal this front-end
// implements: `compiles` (flat compile), `rule_execution_class`, or top-level
// `execution_class` (classification). Everything else — evaluation `results`,
// expression cross-entity (EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE), multi-rule
// scenario cases — belongs to later engine parts and is skipped + logged here
// (union honesty, matching the player1/relay1 drivers).
type corpusCase struct {
	CaseID string `json:"case_id"`
	Input  struct {
		Rule json.RawMessage `json:"rule"`
	} `json:"input"`
	Expected struct {
		Compiles *bool `json:"compiles"`
		Error    *struct {
			Code  string `json:"code"`
			Field string `json:"field"`
		} `json:"error"`
		RuleExecutionClass string `json:"rule_execution_class"`
		ExecutionClass     string `json:"execution_class"`
		AppClassReasons    []struct {
			Field string `json:"field"`
			Type  string `json:"type"`
		} `json:"app_class_reasons"`
		SilentlyTreatedAsAppClass *bool `json:"silently_treated_as_app_class"`
	} `json:"expected"`
}

// errorsThisFrontEndEmits are the compile-error codes the front-end can produce;
// a compiles:false case expecting any other code needs a later part and is
// deferred rather than failed.
var errorsThisFrontEndEmits = map[string]bool{
	"UNKNOWN_VOCABULARY_MEMBER": true,
	"ENTITY_REF_AMBIGUOUS":      true,
	"MODE_MAX_MISSING":          true,
	"MODE_MAX_NOT_APPLICABLE":   true,
}

// expectedCompileRan pins which rules-1 cases the front-end oracle actually
// compiles-and-asserts, since which cases qualify is computed at runtime from
// each case's OWN expected block (hasCompileSignal, above) rather than from a
// hard-coded case list. Without this pin, a corpus edit could silently move a
// case from ran to deferred (or the reverse) and nothing would notice — this
// literal, plus the assertion at the end of TestRulesCompileCorpus, is what
// makes that movement loud. TestRulesDrivenManifestMatchesRuntime (in
// driven_manifest_test.go) unions this list with evalCorpusHandlers' keys and
// checks the result against conformance/driven-manifest.json's rules/1 entry
// — so a change here that isn't mirrored in the manifest also fails there.
var expectedCompileRan = []string{
	"RUL-001-unknown-vocabulary-member-rejected",
	"RUL-101-corroborating-and-cross-entity-conditions",
	"RUL-182-choose-app-branch-forces-app-class",
}

func TestRulesCompileCorpus(t *testing.T) {
	dir := filepath.Join("..", "..", "conformance", "corpora", "rules-1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	var ran, deferred int
	var ranIDs []string
	var deferredIDs []string
	for _, ent := range entries {
		if filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", ent.Name(), err)
		}
		var c corpusCase
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatalf("parse %s: %v", ent.Name(), err)
		}

		expectedClass := c.Expected.RuleExecutionClass
		if expectedClass == "" {
			expectedClass = c.Expected.ExecutionClass
		}
		hasCompileSignal := c.Expected.Compiles != nil || expectedClass != "" || len(c.Expected.AppClassReasons) > 0
		// Defer: no single rule, no compile signal, or a compiles:false case
		// whose expected error this front-end does not emit.
		if len(c.Input.Rule) == 0 || !hasCompileSignal ||
			(c.Expected.Compiles != nil && !*c.Expected.Compiles && c.Expected.Error != nil && !errorsThisFrontEndEmits[c.Expected.Error.Code]) {
			deferred++
			deferredIDs = append(deferredIDs, c.CaseID)
			continue
		}

		ran++
		ranIDs = append(ranIDs, c.CaseID)
		t.Run(c.CaseID, func(t *testing.T) {
			entry, cerr := compile.Compile(c.Input.Rule)
			gotCompiles := cerr == nil

			if c.Expected.Compiles != nil && gotCompiles != *c.Expected.Compiles {
				t.Fatalf("compiles=%v, want %v (err=%v)", gotCompiles, *c.Expected.Compiles, cerr)
			}
			if c.Expected.Error != nil && errorsThisFrontEndEmits[c.Expected.Error.Code] {
				if cerr == nil {
					t.Fatalf("want error %s, got success", c.Expected.Error.Code)
				}
				if cerr.Code != c.Expected.Error.Code {
					t.Fatalf("error code = %s, want %s", cerr.Code, c.Expected.Error.Code)
				}
				if c.Expected.Error.Field != "" && cerr.Field != c.Expected.Error.Field {
					t.Fatalf("error field = %s, want %s", cerr.Field, c.Expected.Error.Field)
				}
			}
			if c.Expected.SilentlyTreatedAsAppClass != nil && *c.Expected.SilentlyTreatedAsAppClass {
				t.Fatalf("corpus expects a rule NEVER silently treated as app-class")
			}

			// Classification assertions (only meaningful when it compiled).
			if gotCompiles && expectedClass != "" && entry.ExecutionClass != expectedClass {
				t.Fatalf("execution_class = %s, want %s", entry.ExecutionClass, expectedClass)
			}
			for _, want := range c.Expected.AppClassReasons {
				found := false
				for _, got := range entry.AppClassReasons {
					if got.Field == want.Field && got.Type == want.Type {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("app_class_reasons missing {field:%q type:%q}; got %+v", want.Field, want.Type, entry.AppClassReasons)
				}
			}
		})
	}
	if ran == 0 {
		t.Fatal("no compile-time corpus cases ran — corpus path or shape wrong")
	}
	sort.Strings(ranIDs)
	if !reflect.DeepEqual(ranIDs, expectedCompileRan) {
		t.Errorf("compile-time driven set = %v, want %v — a corpus edit moved a case across the ran/deferred line; "+
			"update expectedCompileRan (and, if driven cases changed, conformance/driven-manifest.json's rules/1 entry) to match",
			ranIDs, expectedCompileRan)
	}
	t.Logf("front-end oracle: ran %d compile-time case(s), deferred %d to later engine parts: %v", ran, deferred, deferredIDs)
}
