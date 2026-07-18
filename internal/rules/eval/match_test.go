package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// TestMatchStateScalarExpandsGroup exercises RUL-320/321's scalar path
// directly against the fixture registry's real media-player group name
// ("on", members on/playing/idle/paused/buffering — NOT the plan's
// illustrative "active"): a scalar expected value of "on" matches actual
// "playing" via group membership even though the literal strings differ.
func TestMatchStateScalarExpandsGroup(t *testing.T) {
	reg := registry.FixtureRegistry{}
	if !MatchState(reg, "media-player", "playing", "on") {
		t.Error(`MatchState(media-player, "playing", "on") = false, want true (playing is a member of the on-group)`)
	}
	if !MatchState(reg, "media-player", "on", "on") {
		t.Error(`MatchState(media-player, "on", "on") = false, want true (exact match)`)
	}
	if MatchState(reg, "media-player", "standby", "on") {
		t.Error(`MatchState(media-player, "standby", "on") = true, want false (standby is in the off-group)`)
	}
}

// TestMatchStateArrayExactOnly exercises RUL-321's array path: an array
// expected value matches only exact literal membership, never semantic-group
// expansion, even for a state that would otherwise group-match a scalar.
func TestMatchStateArrayExactOnly(t *testing.T) {
	reg := registry.FixtureRegistry{}
	if MatchState(reg, "media-player", "playing", []string{"on"}) {
		t.Error(`MatchState(media-player, "playing", ["on"]) = true, want false (array never group-expands)`)
	}
	if !MatchState(reg, "media-player", "on", []string{"on"}) {
		t.Error(`MatchState(media-player, "on", ["on"]) = false, want true (literal membership)`)
	}
	if !MatchState(reg, "media-player", "playing", []string{"paused", "playing"}) {
		t.Error(`MatchState(media-player, "playing", ["paused","playing"]) = false, want true (literal membership)`)
	}
	// []any is what encoding/json produces for an array field decoded into
	// `any` — MatchState must accept that shape identically to []string.
	if MatchState(reg, "media-player", "playing", []any{"on"}) {
		t.Error(`MatchState(media-player, "playing", []any{"on"}) = true, want false (array never group-expands)`)
	}
	if !MatchState(reg, "media-player", "on", []any{"on"}) {
		t.Error(`MatchState(media-player, "on", []any{"on"}) = false, want true (literal membership, []any)`)
	}
}

func TestMatchStateUnknownExpectedTypeFails(t *testing.T) {
	reg := registry.FixtureRegistry{}
	if MatchState(reg, "media-player", "on", 42) {
		t.Error("MatchState with a non-string/array expected value = true, want false")
	}
}

// scalarArrayCorpusCase mirrors
// conformance/corpora/rules-1/RUL-321-scalar-expands-array-exact.json: its
// own fixture registry, its scalar ("on") and array (["on"]) triggers, and
// its sequenced state_transition events with per-event scalar_fired/
// array_fired expectations.
type scalarArrayCorpusCase struct {
	Input struct {
		Fixture struct {
			DeviceClasses map[string]struct {
				SemanticGroups map[string][]string `json:"semantic_groups"`
			} `json:"device_classes"`
			Entities map[string]struct {
				DeviceClass string `json:"device_class"`
			} `json:"entities"`
		} `json:"fixture"`
		Triggers []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			EntityID string `json:"entity_id"`
			To       any    `json:"to"`
		} `json:"triggers"`
		Events []struct {
			Seq  int    `json:"seq"`
			TMs  int64  `json:"t_ms"`
			Kind string `json:"kind"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq         int  `json:"seq"`
			ScalarFired bool `json:"scalar_fired"`
			ArrayFired  bool `json:"array_fired"`
		} `json:"results"`
	} `json:"expected"`
}

// fixtureRegistryFromCorpus is a minimal registry.Registry built directly
// from a corpus case's own inline input.fixture, so this test does not rely
// on registry.FixtureRegistry happening to match — it exercises MatchState
// against exactly the semantic groups the corpus case itself declares (per
// RUL-321-scalar-expands-array-exact's cross-contract-alignment note, this
// currently coincides with registry.FixtureRegistry, but the test wires the
// corpus's own fixture, not that coincidence).
type fixtureRegistryFromCorpus struct {
	groups map[string]map[string][]string // deviceClass -> groupName -> members
}

func (r fixtureRegistryFromCorpus) SemanticGroups(deviceClass, state string) []string {
	var out []string
	for name, members := range r.groups[deviceClass] {
		for _, m := range members {
			if m == state {
				out = append(out, name)
				break
			}
		}
	}
	return out
}
func (r fixtureRegistryFromCorpus) AttributeType(deviceClass, attr string) registry.ValueType { return registry.Unknown }
func (r fixtureRegistryFromCorpus) ChangeEmission(deviceClass, attr string) string             { return "" }
func (r fixtureRegistryFromCorpus) CommandExists(deviceClass, command string) bool             { return false }

var _ registry.Registry = fixtureRegistryFromCorpus{}

// TestMatchStateScalarExpandsArrayExactCorpus replays
// RUL-321-scalar-expands-array-exact.json's sequenced events against
// MatchState for both the scalar "on" trigger and the array ["on"] trigger,
// diffing scalar_fired/array_fired against expected.results — the named
// oracle for this task's array-vs-scalar corpus case.
func TestMatchStateScalarExpandsArrayExactCorpus(t *testing.T) {
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "rules-1", "RUL-321-scalar-expands-array-exact.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c scalarArrayCorpusCase
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	reg := fixtureRegistryFromCorpus{groups: map[string]map[string][]string{}}
	for class, decl := range c.Input.Fixture.DeviceClasses {
		reg.groups[class] = decl.SemanticGroups
	}

	var scalarExpected, arrayExpected any
	deviceClass := ""
	for _, trig := range c.Input.Triggers {
		switch trig.ID {
		case "scalar":
			scalarExpected = trig.To
		case "array":
			arrayExpected = trig.To
		}
		if ent, ok := c.Input.Fixture.Entities[trig.EntityID]; ok {
			deviceClass = ent.DeviceClass
		}
	}
	if scalarExpected == nil || arrayExpected == nil || deviceClass == "" {
		t.Fatalf("corpus case missing scalar/array trigger or device class: %+v", c.Input.Triggers)
	}

	for _, want := range c.Expected.Results {
		want := want
		ev := c.Input.Events[want.Seq-1]
		t.Run(ev.Kind, func(t *testing.T) {
			if gotScalar := MatchState(reg, deviceClass, ev.To, scalarExpected); gotScalar != want.ScalarFired {
				t.Errorf("seq %d: scalar MatchState(%q, %v) = %v, want %v", want.Seq, ev.To, scalarExpected, gotScalar, want.ScalarFired)
			}
			if gotArray := MatchState(reg, deviceClass, ev.To, arrayExpected); gotArray != want.ArrayFired {
				t.Errorf("seq %d: array MatchState(%q, %v) = %v, want %v", want.Seq, ev.To, arrayExpected, gotArray, want.ArrayFired)
			}
		})
	}
}
