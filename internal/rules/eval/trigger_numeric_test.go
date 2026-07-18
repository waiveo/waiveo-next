package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// parseNumericTrigger decodes one standalone `numeric` trigger JSON object
// into a model.Member (via the public ParseRule) and builds a NumericTrigger
// from it.
func parseNumericTrigger(t *testing.T, triggerJSON string) *NumericTrigger {
	t.Helper()
	ruleJSON := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZR","triggers":[` + triggerJSON + `]}`
	r, err := model.ParseRule([]byte(ruleJSON))
	if err != nil {
		t.Fatalf("ParseRule(%s): %v", ruleJSON, err)
	}
	if len(r.Triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(r.Triggers))
	}
	nt, err := NewNumericTrigger(r.Triggers[0])
	if err != nil {
		t.Fatalf("NewNumericTrigger: %v", err)
	}
	return nt
}

func numericEntity(attrs map[string]any) state.Entity {
	return state.Entity{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z3", DeviceClass: "sensor", Attributes: attrs}
}

// TestNewNumericTriggerClassification checks decoding of entity_id,
// attribute, above/below presence (RUL-030).
func TestNewNumericTriggerClassification(t *testing.T) {
	entity := "01J8Z3K4N5P6Q7R8S9T0V1W2Z3"
	nt := parseNumericTrigger(t, `{"type":"numeric","entity_id":"`+entity+`","attribute":"cpu_temp","above":75}`)
	if nt.EntityID != entity {
		t.Errorf("EntityID = %q, want %q", nt.EntityID, entity)
	}
	if nt.Attribute != "cpu_temp" {
		t.Errorf("Attribute = %q, want cpu_temp", nt.Attribute)
	}
	if !nt.HasAbove || nt.Above != 75 {
		t.Errorf("Above = %v hasAbove=%v, want 75/true", nt.Above, nt.HasAbove)
	}
	if nt.HasBelow {
		t.Errorf("HasBelow = true, want false (below absent)")
	}

	both := parseNumericTrigger(t, `{"type":"numeric","entity_id":"`+entity+`","above":10,"below":20}`)
	if !both.HasAbove || both.Above != 10 || !both.HasBelow || both.Below != 20 {
		t.Errorf("both bounds = above:%v/%v below:%v/%v, want 10/true 20/true", both.Above, both.HasAbove, both.Below, both.HasBelow)
	}
	if both.Attribute != "" {
		t.Errorf("Attribute = %q, want empty (canonical state compared)", both.Attribute)
	}
}

// TestNumericFirstObservationLevelCheck is RUL-033/302: the first-ever
// observation (no prior parsed value) fires via a level check.
func TestNumericFirstObservationLevelCheck(t *testing.T) {
	nt := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","above":75}`)

	obs := state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(82)})}
	fired, next := nt.Observe(nil, obs)
	if !fired {
		t.Error("first observation above threshold did not fire, want fire (level check)")
	}
	if next == nil || *next != 82 {
		t.Errorf("next parsed = %v, want 82", next)
	}

	// A first-ever observation NOT satisfying the bound must not fire either.
	nt2 := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","above":75}`)
	fired2, next2 := nt2.Observe(nil, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(60)})})
	if fired2 {
		t.Error("first observation below threshold fired, want no fire")
	}
	if next2 == nil || *next2 != 60 {
		t.Errorf("next parsed = %v, want 60", next2)
	}
}

// TestNumericNoRefireWithoutCrossing is RUL-033: staying above the threshold
// on a subsequent observation must not refire (no crossing).
func TestNumericNoRefireWithoutCrossing(t *testing.T) {
	nt := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","above":75}`)
	prev := float64(82)
	fired, next := nt.Observe(&prev, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(83)})})
	if fired {
		t.Error("subsequent still-above observation fired, want no refire (no crossing)")
	}
	if next == nil || *next != 83 {
		t.Errorf("next parsed = %v, want 83", next)
	}
}

// TestNumericCrossingRefires is RUL-033: a genuine crossing (prior did not
// satisfy, current does) fires.
func TestNumericCrossingRefires(t *testing.T) {
	nt := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","above":75}`)
	prev := float64(60)
	fired, next := nt.Observe(&prev, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(90)})})
	if !fired {
		t.Error("crossing from below to above did not fire, want fire")
	}
	if next == nil || *next != 90 {
		t.Errorf("next parsed = %v, want 90", next)
	}
}

// TestNumericDropDoesNotFire covers the corpus's seq 3: dropping out of
// range never fires (only entering does).
func TestNumericDropDoesNotFire(t *testing.T) {
	nt := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","above":75}`)
	prev := float64(83)
	fired, next := nt.Observe(&prev, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(60)})})
	if fired {
		t.Error("dropping below threshold fired, want no fire")
	}
	if next == nil || *next != 60 {
		t.Errorf("next parsed = %v, want 60", next)
	}
}

// TestNumericStrictEndpointsExcluded is RUL-032: a value equal to `above` or
// `below` does not satisfy — both endpoints excluded.
func TestNumericStrictEndpointsExcluded(t *testing.T) {
	above := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","above":75}`)
	fired, _ := above.Observe(nil, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(75)})})
	if fired {
		t.Error("value equal to above:75 fired, want no fire (strict, endpoint excluded)")
	}

	below := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","below":20}`)
	fired2, _ := below.Observe(nil, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(20)})})
	if fired2 {
		t.Error("value equal to below:20 fired, want no fire (strict, endpoint excluded)")
	}

	// Both bounds: value strictly between, endpoints excluded.
	between := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","above":10,"below":20}`)
	firedLow, _ := between.Observe(nil, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(10)})})
	if firedLow {
		t.Error("value equal to above-endpoint 10 fired for a two-bound trigger, want no fire")
	}
	firedHigh, _ := between.Observe(nil, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(20)})})
	if firedHigh {
		t.Error("value equal to below-endpoint 20 fired for a two-bound trigger, want no fire")
	}
	firedMid, _ := between.Observe(nil, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(15)})})
	if !firedMid {
		t.Error("value strictly between 10 and 20 did not fire, want fire")
	}
}

// TestNumericUnparseableNeverSatisfies is RUL-031/271: an unparseable value
// never satisfies either bound and must fail closed, not error.
func TestNumericUnparseableNeverSatisfies(t *testing.T) {
	nt := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","attribute":"cpu_temp","above":75}`)

	// First observation with a non-numeric string value: must not fire, and
	// must not record a bogus parsed baseline.
	fired, next := nt.Observe(nil, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": "hot"})})
	if fired {
		t.Error("unparseable first observation fired, want no fire (fail closed)")
	}
	if next != nil {
		t.Errorf("next parsed = %v, want nil (no baseline recorded for an unparseable value)", *next)
	}

	// A prior valid parsed value, followed by an unparseable observation:
	// must not fire, and must not carry the stale prior forward as a
	// crossing baseline (RUL-302: no prior value to cross from once the
	// chain of valid parses breaks).
	prev := float64(60)
	fired2, next2 := nt.Observe(&prev, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": nil})})
	if fired2 {
		t.Error("unparseable subsequent observation fired, want no fire")
	}
	if next2 != nil {
		t.Errorf("next parsed after unparseable = %v, want nil", *next2)
	}

	// After an unparseable gap, the next valid observation performs a fresh
	// level check (no prior value survives to cross from), not a crossing.
	fired3, next3 := nt.Observe(next2, state.Observation{Entity: numericEntity(map[string]any{"cpu_temp": float64(90)})})
	if !fired3 {
		t.Error("first valid observation after an unparseable gap did not fire, want fire (fresh level check)")
	}
	if next3 == nil || *next3 != 90 {
		t.Errorf("next parsed = %v, want 90", next3)
	}
}

// TestNumericCanonicalStateWhenAttributeAbsent is RUL-031: the compared
// value is the entity's canonical state string when `attribute` is absent.
func TestNumericCanonicalStateWhenAttributeAbsent(t *testing.T) {
	nt := parseNumericTrigger(t, `{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","above":50}`)
	reg := registry.FixtureRegistry{}
	_ = reg
	obs := state.Observation{Entity: state.Entity{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z3", DeviceClass: "sensor", State: "82"}}
	fired, next := nt.Observe(nil, obs)
	if !fired {
		t.Error("canonical state string \"82\" parsed as 82 > 50 did not fire, want fire")
	}
	if next == nil || *next != 82 {
		t.Errorf("next parsed = %v, want 82", next)
	}
}

// --- RUL-033 corpus case ---

type rul033Case struct {
	Input struct {
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq   int     `json:"seq"`
			TMs   int64   `json:"t_ms"`
			Kind  string  `json:"kind"`
			Value float64 `json:"value"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq   int  `json:"seq"`
			Fired bool `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

// TestRUL033NumericFirstObservationLevelCheckCorpus replays the named corpus
// case exactly: first-ever observation fires via a level check, a
// still-above observation does not refire, a drop below does not fire, and a
// genuine crossing back above refires.
func TestRUL033NumericFirstObservationLevelCheckCorpus(t *testing.T) {
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "rules-1", "RUL-033-numeric-first-observation-level-check.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul033Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	nt, err := buildNumericTrigger(c.Input.Trigger)
	if err != nil {
		t.Fatalf("build trigger: %v", err)
	}

	var prev *float64
	resultBySeq := map[int]bool{}
	for _, ev := range c.Input.Events {
		obs := state.Observation{Entity: numericEntity(map[string]any{nt.Attribute: ev.Value})}
		fired, next := nt.Observe(prev, obs)
		prev = next
		resultBySeq[ev.Seq] = fired
	}

	for _, want := range c.Expected.Results {
		got := resultBySeq[want.Seq]
		if got != want.Fired {
			t.Errorf("seq %d: fired=%v want %v", want.Seq, got, want.Fired)
		}
	}
}

// buildNumericTrigger builds a NumericTrigger from a raw trigger JSON object.
func buildNumericTrigger(raw json.RawMessage) (*NumericTrigger, error) {
	r, err := model.ParseRule([]byte(`{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZR","triggers":[` + string(raw) + `]}`))
	if err != nil {
		return nil, err
	}
	return NewNumericTrigger(r.Triggers[0])
}
