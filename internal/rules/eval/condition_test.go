package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// parseConditions decodes a JSON array of condition objects (as authored in
// a rule's `conditions` field) into []model.Member via the public ParseRule.
func parseConditions(t *testing.T, conditionsJSON string) []model.Member {
	t.Helper()
	ruleJSON := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZR","conditions":` + conditionsJSON + `}`
	r, err := model.ParseRule([]byte(ruleJSON))
	if err != nil {
		t.Fatalf("ParseRule(%s): %v", ruleJSON, err)
	}
	return r.Conditions
}

func parseCondition(t *testing.T, conditionJSON string) model.Member {
	t.Helper()
	cs := parseConditions(t, `[`+conditionJSON+`]`)
	if len(cs) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(cs))
	}
	return cs[0]
}

// --- RUL-100: and/or/not composition ---------------------------------------

func TestEvalConditionAnd(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()
	vars := map[string]any{}

	trueLeaf := parseCondition(t, `{"type":"variable","variable":"x","equals":true}`)
	falseLeaf := parseCondition(t, `{"type":"variable","variable":"y","equals":true}`)
	vars["x"] = true
	vars["y"] = false

	and := parseCondition(t, `{"and":[{"type":"variable","variable":"x","equals":true},{"type":"variable","variable":"x","equals":true}]}`)
	if !EvalCondition(reg, snap, clk, vars, and) {
		t.Error("and(true,true) = false, want true")
	}

	andFail := parseCondition(t, `{"and":[{"type":"variable","variable":"x","equals":true},{"type":"variable","variable":"y","equals":true}]}`)
	if EvalCondition(reg, snap, clk, vars, andFail) {
		t.Error("and(true,false) = true, want false")
	}
	_ = trueLeaf
	_ = falseLeaf
}

func TestEvalConditionOr(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()
	vars := map[string]any{"x": true, "y": false}

	or := parseCondition(t, `{"or":[{"type":"variable","variable":"y","equals":true},{"type":"variable","variable":"x","equals":true}]}`)
	if !EvalCondition(reg, snap, clk, vars, or) {
		t.Error("or(false,true) = false, want true")
	}

	orFail := parseCondition(t, `{"or":[{"type":"variable","variable":"y","equals":true},{"type":"variable","variable":"y","equals":true}]}`)
	if EvalCondition(reg, snap, clk, vars, orFail) {
		t.Error("or(false,false) = true, want false")
	}
}

func TestEvalConditionNot(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()
	vars := map[string]any{"x": true}

	not := parseCondition(t, `{"not":{"type":"variable","variable":"x","equals":true}}`)
	if EvalCondition(reg, snap, clk, vars, not) {
		t.Error("not(true) = true, want false")
	}

	notFail := parseCondition(t, `{"not":{"type":"variable","variable":"x","equals":false}}`)
	if !EvalCondition(reg, snap, clk, vars, notFail) {
		t.Error("not(false) = false, want true")
	}
}

// TestEvalConditionNestedComposition exercises and/or/not nested inside one
// another, recursively.
func TestEvalConditionNestedComposition(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()
	vars := map[string]any{"a": true, "b": false, "c": true}

	// (a and c) or (not b) -> true or true -> true; also check a genuinely
	// mixed case: (a and b) or (not b) -> false or true -> true.
	m := parseCondition(t, `{"or":[{"and":[{"type":"variable","variable":"a","equals":true},{"type":"variable","variable":"b","equals":true}]},{"not":{"type":"variable","variable":"b","equals":true}}]}`)
	if !EvalCondition(reg, snap, clk, vars, m) {
		t.Error("nested composition = false, want true")
	}
}

// --- RUL-110: state condition ------------------------------------------------

func mkSnapEntity(id, deviceClass, s string, attrs map[string]any) state.Entity {
	return state.Entity{ID: id, DeviceClass: deviceClass, State: s, Attributes: attrs}
}

func TestEvalConditionStateLeafScalarGroupExpansion(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	const entID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"
	snap := state.Snapshot{entID: mkSnapEntity(entID, "media-player", "playing", nil)}

	m := parseCondition(t, `{"type":"state","entity_id":"`+entID+`","state":"on"}`)
	if !EvalCondition(reg, snap, clk, nil, m) {
		t.Error(`state condition "on" against actual "playing" = false, want true (group expansion, RUL-321)`)
	}

	mArray := parseCondition(t, `{"type":"state","entity_id":"`+entID+`","state":["on"]}`)
	if EvalCondition(reg, snap, clk, nil, mArray) {
		t.Error(`state condition ["on"] against actual "playing" = true, want false (array never group-expands, RUL-321)`)
	}
}

func TestEvalConditionStateLeafAttribute(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	const entID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z6"
	snap := state.Snapshot{entID: mkSnapEntity(entID, "media-player", "idle", map[string]any{"powered_on": true})}

	m := parseCondition(t, `{"type":"state","entity_id":"`+entID+`","attribute":"powered_on","state":true}`)
	if !EvalCondition(reg, snap, clk, nil, m) {
		t.Error("attribute condition powered_on=true against true = false, want true")
	}

	mFail := parseCondition(t, `{"type":"state","entity_id":"`+entID+`","attribute":"powered_on","state":false}`)
	if EvalCondition(reg, snap, clk, nil, mFail) {
		t.Error("attribute condition powered_on=false against true = true, want false")
	}
}

func TestEvalConditionStateLeafUnresolvableEntityFailsClosed(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	snap := state.Snapshot{} // empty: entity not present
	m := parseCondition(t, `{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z9","state":"on"}`)
	if EvalCondition(reg, snap, clk, nil, m) {
		t.Error("state condition against an absent entity = true, want false (fail-closed)")
	}
}

// --- RUL-120: numeric condition (level check, no crossing) ------------------

func TestEvalConditionNumericLeafLevelCheck(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	const entID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z3"
	snap := state.Snapshot{entID: mkSnapEntity(entID, "media-player", "playing", map[string]any{"volume": 30.0})}

	above := parseCondition(t, `{"type":"numeric","entity_id":"`+entID+`","attribute":"volume","above":20}`)
	if !EvalCondition(reg, snap, clk, nil, above) {
		t.Error("numeric condition volume>20 against 30 = false, want true")
	}

	below := parseCondition(t, `{"type":"numeric","entity_id":"`+entID+`","attribute":"volume","below":20}`)
	if EvalCondition(reg, snap, clk, nil, below) {
		t.Error("numeric condition volume<20 against 30 = true, want false")
	}

	// Strictly exclusive bounds (RUL-032/120): equal to the bound never
	// satisfies.
	exact := parseCondition(t, `{"type":"numeric","entity_id":"`+entID+`","attribute":"volume","above":30}`)
	if EvalCondition(reg, snap, clk, nil, exact) {
		t.Error("numeric condition volume>30 against 30 = true, want false (strictly exclusive)")
	}
}

func TestEvalConditionNumericLeafUnparseableFailsClosed(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	const entID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z3"
	snap := state.Snapshot{entID: mkSnapEntity(entID, "media-player", "playing", map[string]any{"volume": "loud"})}

	m := parseCondition(t, `{"type":"numeric","entity_id":"`+entID+`","attribute":"volume","above":20}`)
	if EvalCondition(reg, snap, clk, nil, m) {
		t.Error("numeric condition against an unparseable value = true, want false (fail-closed, RUL-271)")
	}
}

// --- RUL-130: time condition (local HH:MM:SS window, midnight wrap) --------

func TestEvalConditionTimeLeafNormalRange(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	m := parseCondition(t, `{"type":"time","after":"09:00:00","before":"17:00:00"}`)

	clk := clock.NewFakeClock()
	clk.SetWall(12 * 3600 * 1000) // 12:00:00 UTC
	if !EvalCondition(reg, snap, clk, nil, m) {
		t.Error("time condition 09:00-17:00 at 12:00 = false, want true")
	}

	clk.SetWall(8 * 3600 * 1000) // 08:00:00
	if EvalCondition(reg, snap, clk, nil, m) {
		t.Error("time condition 09:00-17:00 at 08:00 = true, want false")
	}

	clk.SetWall(17 * 3600 * 1000) // exactly 17:00:00 -- inclusive
	if !EvalCondition(reg, snap, clk, nil, m) {
		t.Error("time condition 09:00-17:00 at 17:00:00 (inclusive bound) = false, want true")
	}
}

func TestEvalConditionTimeLeafMidnightWrap(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	// after > before: wraps past local midnight (RUL-130).
	m := parseCondition(t, `{"type":"time","after":"22:00:00","before":"06:00:00"}`)

	clk := clock.NewFakeClock()
	clk.SetWall(23 * 3600 * 1000) // 23:00:00 -- inside the wrapped range
	if !EvalCondition(reg, snap, clk, nil, m) {
		t.Error("wrapped time condition at 23:00 = false, want true")
	}

	clk.SetWall(2 * 3600 * 1000) // 02:00:00 -- inside the wrapped range (past midnight)
	if !EvalCondition(reg, snap, clk, nil, m) {
		t.Error("wrapped time condition at 02:00 = false, want true")
	}

	clk.SetWall(12 * 3600 * 1000) // 12:00:00 -- outside the wrapped range
	if EvalCondition(reg, snap, clk, nil, m) {
		t.Error("wrapped time condition at 12:00 = true, want false")
	}
}

func TestEvalConditionTimeLeafOneSidedBound(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}

	afterOnly := parseCondition(t, `{"type":"time","after":"20:00:00"}`)
	clk := clock.NewFakeClock()
	clk.SetWall(21 * 3600 * 1000)
	if !EvalCondition(reg, snap, clk, nil, afterOnly) {
		t.Error("after-only time condition at 21:00 (after 20:00) = false, want true")
	}
	clk.SetWall(1 * 3600 * 1000)
	if EvalCondition(reg, snap, clk, nil, afterOnly) {
		t.Error("after-only time condition at 01:00 (before 20:00) = true, want false")
	}

	beforeOnly := parseCondition(t, `{"type":"time","before":"06:00:00"}`)
	clk.SetWall(3 * 3600 * 1000)
	if !EvalCondition(reg, snap, clk, nil, beforeOnly) {
		t.Error("before-only time condition at 03:00 (before 06:00) = false, want true")
	}
	clk.SetWall(10 * 3600 * 1000)
	if EvalCondition(reg, snap, clk, nil, beforeOnly) {
		t.Error("before-only time condition at 10:00 (after 06:00) = true, want false")
	}
}

// --- RUL-150: variable condition (compares against the supplied/closed-over
// value; no live lookup performed by this function itself) ------------------

func TestEvalConditionVariableLeafEquals(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()

	m := parseCondition(t, `{"type":"variable","variable":"guest_mode","equals":false}`)
	if !EvalCondition(reg, snap, clk, map[string]any{"guest_mode": false}, m) {
		t.Error("variable condition guest_mode==false against false = false, want true")
	}
	if EvalCondition(reg, snap, clk, map[string]any{"guest_mode": true}, m) {
		t.Error("variable condition guest_mode==false against true = true, want false")
	}
}

func TestEvalConditionVariableLeafAboveBelow(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()

	above := parseCondition(t, `{"type":"variable","variable":"threshold","above":10}`)
	if !EvalCondition(reg, snap, clk, map[string]any{"threshold": 15.0}, above) {
		t.Error("variable condition threshold>10 against 15 = false, want true")
	}
	if EvalCondition(reg, snap, clk, map[string]any{"threshold": 5.0}, above) {
		t.Error("variable condition threshold>10 against 5 = true, want false")
	}

	below := parseCondition(t, `{"type":"variable","variable":"threshold","below":10}`)
	if !EvalCondition(reg, snap, clk, map[string]any{"threshold": 5.0}, below) {
		t.Error("variable condition threshold<10 against 5 = false, want true")
	}
}

func TestEvalConditionVariableLeafMissingFailsClosed(t *testing.T) {
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()

	m := parseCondition(t, `{"type":"variable","variable":"nope","equals":true}`)
	if EvalCondition(reg, snap, clk, map[string]any{}, m) {
		t.Error("variable condition against a missing variable = true, want false (fail-closed)")
	}
}

// --- RUL-101 corpus: corroborating-and-cross-entity-conditions -------------

type rul101Corpus struct {
	Input struct {
		Rule struct {
			Conditions []json.RawMessage `json:"conditions"`
		} `json:"rule"`
		Evaluations []struct {
			ID               string `json:"id"`
			PoweredOn        bool   `json:"powered_on"`
			OtherEntityState string `json:"other_entity_state"`
		} `json:"evaluations"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			ID             string `json:"id"`
			ConditionsPass bool   `json:"conditions_pass"`
		} `json:"results"`
	} `json:"expected"`
}

// TestEvalConditionRUL101CorroboratingAndCrossEntity replays
// RUL-101-corroborating-and-cross-entity-conditions.json: an `and` of a
// same-entity attribute condition (corroborating the trigger's subject) and a
// structured condition on a wholly different entity, per RUL-101's
// unrestricted structured-condition EntityRef.
func TestEvalConditionRUL101CorroboratingAndCrossEntity(t *testing.T) {
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "rules-1", "RUL-101-corroborating-and-cross-entity-conditions.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c rul101Corpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	ruleJSON := `{"id":"x","conditions":` + mustMarshalConditions(t, c.Input.Rule.Conditions) + `}`
	r, err := model.ParseRule([]byte(ruleJSON))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if len(r.Conditions) != 2 {
		t.Fatalf("expected 2 top-level conditions, got %d", len(r.Conditions))
	}
	and := model.Member{Composition: "and", Children: r.Conditions}

	const triggerEntity = "01J8Z3K4N5P6Q7R8S9T0V1W2Z6"
	const otherEntity = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"

	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()

	for _, ev := range c.Input.Evaluations {
		ev := ev
		t.Run(ev.ID, func(t *testing.T) {
			snap := state.Snapshot{
				triggerEntity: mkSnapEntity(triggerEntity, "media-player", "idle", map[string]any{"powered_on": ev.PoweredOn}),
				otherEntity:   mkSnapEntity(otherEntity, "media-player", ev.OtherEntityState, nil),
			}
			var want bool
			found := false
			for _, r := range c.Expected.Results {
				if r.ID == ev.ID {
					want = r.ConditionsPass
					found = true
				}
			}
			if !found {
				t.Fatalf("no expected result for evaluation %q", ev.ID)
			}
			got := EvalCondition(reg, snap, clk, nil, and)
			if got != want {
				t.Errorf("EvalCondition(%s) = %v, want %v", ev.ID, got, want)
			}
		})
	}
}

func mustMarshalConditions(t *testing.T, raws []json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(raws)
	if err != nil {
		t.Fatalf("marshal conditions: %v", err)
	}
	return string(b)
}

// --- RUL-150 corpus: variable-condition-constant-folded ---------------------

// TestEvalConditionRUL150VariableConstantFolded replays
// RUL-150-variable-condition-constant-folded.json: the variable condition
// evaluates against the compile-time-frozen closed-over value (vars, as this
// function receives it), never a "live" value — this test simply never
// updates vars after the seq-3 live_variable_write event, modeling that the
// edge engine (a later task) never re-resolves it live within one generation.
func TestEvalConditionRUL150VariableConstantFolded(t *testing.T) {
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "rules-1", "RUL-150-variable-condition-constant-folded.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c struct {
		Input struct {
			Rule struct {
				Conditions []json.RawMessage `json:"conditions"`
			} `json:"rule"`
		} `json:"input"`
		Expected struct {
			Results []struct {
				Seq            int  `json:"seq"`
				ConditionsPass bool `json:"conditions_pass"`
			} `json:"results"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	ruleJSON := `{"id":"x","conditions":` + mustMarshalConditions(t, c.Input.Rule.Conditions) + `}`
	r, err := model.ParseRule([]byte(ruleJSON))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if len(r.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(r.Conditions))
	}

	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()
	// The frozen closed-over generation-1 value (RUL-390): never updated by
	// the corpus's later live_variable_write event.
	frozenVars := map[string]any{"guest_mode": false}

	for _, want := range c.Expected.Results {
		want := want
		got := EvalCondition(reg, snap, clk, frozenVars, r.Conditions[0])
		if got != want.ConditionsPass {
			t.Errorf("seq %d: EvalCondition = %v, want %v", want.Seq, got, want.ConditionsPass)
		}
	}
}
