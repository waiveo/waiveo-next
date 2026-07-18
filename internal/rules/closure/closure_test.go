package closure

import (
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// fixture ULIDs (contracts/rules-1.md corpus convention).
const (
	entA     = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"
	entB     = "01J8Z3K4N5P6Q7R8S9T0V1W2Z6"
	presetID = "01J8Z3K4N5P6Q7R8S9T0V1W2Z5"
)

// envFrom builds an Env whose resolvers answer from the given maps.
func envFrom(vars map[string]any, sels map[string][]string, dcs map[string][]string, presets map[string][]eval.PresetCommand) Env {
	return Env{
		Variables: vars,
		ResolveSelector: func(s string) []string {
			return sels[s]
		},
		ResolveDeviceClass: func(dc string) []string {
			return dcs[dc]
		},
		PresetCommands: func(id string) ([]eval.PresetCommand, bool) {
			c, ok := presets[id]
			return c, ok
		},
	}
}

func mustParse(t *testing.T, raw string) model.Rule {
	t.Helper()
	r, err := model.ParseRule([]byte(raw))
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	return r
}

// A rule with a variable condition + a selector trigger + a preset_batch action
// freezes all three closed-over kinds (RUL-150/013/173/390).
func TestComputeFreezesAllThreeKinds(t *testing.T) {
	rule := mustParse(t, `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers": [{ "type": "state", "selector": "label==lobby-screens", "to": ["on"] }],
		"conditions": [{ "type": "variable", "variable": "guest_mode", "equals": false }],
		"actions": [{ "type": "preset_batch", "preset_id": "`+presetID+`" }]
	}`)

	presetCmds := []eval.PresetCommand{{EntityID: entA, Command: "power_on"}}
	env := envFrom(
		map[string]any{"guest_mode": false},
		map[string][]string{"label==lobby-screens": {entA, entB}},
		nil,
		map[string][]eval.PresetCommand{presetID: presetCmds},
	)

	cl, ce := Compute(rule, env)
	if ce != nil {
		t.Fatalf("unexpected compile error: %v", ce)
	}

	// Variable frozen from env (RUL-150/390).
	if got, ok := cl.Variables["guest_mode"]; !ok || got != false {
		t.Fatalf("Variables[guest_mode] = %v (ok=%v); want false", got, ok)
	}
	// Selector matched-set frozen keyed by the filter string (RUL-013/390).
	if got := cl.Selectors["label==lobby-screens"]; !reflect.DeepEqual(got, []string{entA, entB}) {
		t.Fatalf("Selectors[label==lobby-screens] = %v; want [%s %s]", got, entA, entB)
	}
	// Preset command list frozen keyed by preset_id (RUL-173/390).
	if got := cl.PresetBatches[presetID]; !reflect.DeepEqual(got, presetCmds) {
		t.Fatalf("PresetBatches[%s] = %v; want %v", presetID, got, presetCmds)
	}
}

// An unresolvable preset_id errors PRESET_NOT_FOUND at compile time (RUL-173).
func TestComputeMissingPresetErrors(t *testing.T) {
	rule := mustParse(t, `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers": [{ "type": "state", "entity_id": "`+entA+`", "to": ["on"] }],
		"actions": [{ "type": "preset_batch", "preset_id": "`+presetID+`" }]
	}`)
	// env resolves NO presets.
	env := envFrom(nil, nil, nil, nil)

	cl, ce := Compute(rule, env)
	if ce == nil {
		t.Fatalf("expected PRESET_NOT_FOUND, got closure %+v", cl)
	}
	if ce.Code != "PRESET_NOT_FOUND" {
		t.Fatalf("error code = %q; want PRESET_NOT_FOUND", ce.Code)
	}
}

// The RUL-150 corpus case: the variable's compile-time value is folded into the
// generation as a constant.
func TestComputeRUL150VariableConstantFolded(t *testing.T) {
	rule := mustParse(t, `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers": [{ "type": "state", "entity_id": "`+entA+`", "to": ["on"] }],
		"conditions": [{ "type": "variable", "variable": "guest_mode", "equals": false }],
		"actions": [{ "type": "device_command", "entity_id": "`+entA+`", "command": "launch" }]
	}`)
	env := envFrom(map[string]any{"guest_mode": false}, nil, nil, nil)

	cl, ce := Compute(rule, env)
	if ce != nil {
		t.Fatalf("unexpected compile error: %v", ce)
	}
	got, ok := cl.Variables["guest_mode"]
	if !ok || got != false {
		t.Fatalf("frozen guest_mode = %v (ok=%v); want false (RUL-150)", got, ok)
	}
	// entity_id action ref is NOT closured (only selector/device_class are).
	if len(cl.Selectors) != 0 {
		t.Fatalf("Selectors = %v; want empty for entity_id refs", cl.Selectors)
	}
}

// A device_class EntityRef freezes its matched entity set keyed by the class
// string (RUL-013/390), and an entity_id ref adds nothing to Selectors.
func TestComputeDeviceClassFrozenEntityIDLeftAlone(t *testing.T) {
	rule := mustParse(t, `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers": [
			{ "type": "state", "device_class": "media_player", "to": ["on"] },
			{ "type": "state", "entity_id": "`+entA+`", "to": ["on"] }
		],
		"actions": [{ "type": "device_command", "entity_id": "`+entA+`", "command": "launch" }]
	}`)
	env := envFrom(nil, nil, map[string][]string{"media_player": {entA, entB}}, nil)

	cl, ce := Compute(rule, env)
	if ce != nil {
		t.Fatalf("unexpected compile error: %v", ce)
	}
	if got := cl.Selectors["media_player"]; !reflect.DeepEqual(got, []string{entA, entB}) {
		t.Fatalf("Selectors[media_player] = %v; want [%s %s]", got, entA, entB)
	}
	if len(cl.Selectors) != 1 {
		t.Fatalf("Selectors has %d keys; want 1 (entity_id ref must not be closured)", len(cl.Selectors))
	}
}

// Variable conditions nested inside and/or/not compositions are frozen (RUL-150).
func TestComputeVariableInNestedComposition(t *testing.T) {
	rule := mustParse(t, `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers": [{ "type": "state", "entity_id": "`+entA+`", "to": ["on"] }],
		"conditions": [{ "and": [
			{ "not": { "type": "variable", "variable": "away", "equals": true } },
			{ "or": [{ "type": "variable", "variable": "brightness", "above": 50 }] }
		]}],
		"actions": [{ "type": "device_command", "entity_id": "`+entA+`", "command": "launch" }]
	}`)
	env := envFrom(map[string]any{"away": false, "brightness": 80}, nil, nil, nil)

	cl, ce := Compute(rule, env)
	if ce != nil {
		t.Fatalf("unexpected compile error: %v", ce)
	}
	if v, ok := cl.Variables["away"]; !ok || v != false {
		t.Fatalf("frozen away = %v (ok=%v); want false", v, ok)
	}
	if v, ok := cl.Variables["brightness"]; !ok || v != 80 {
		t.Fatalf("frozen brightness = %v (ok=%v); want 80", v, ok)
	}
}

// Closed-over kinds reachable only through a choose action are frozen: a
// variable in a branch guard, a selector in a branch action, a preset in the
// default (RUL-150/013/173).
func TestComputeClosuresInsideChoose(t *testing.T) {
	rule := mustParse(t, `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers": [{ "type": "state", "entity_id": "`+entA+`", "to": ["on"] }],
		"actions": [{ "type": "choose",
			"branches": [{
				"condition": { "type": "variable", "variable": "guest_mode", "equals": true },
				"actions": [{ "type": "device_command", "selector": "label==lobby-screens", "command": "launch" }]
			}],
			"default": [{ "type": "preset_batch", "preset_id": "`+presetID+`" }]
		}]
	}`)
	presetCmds := []eval.PresetCommand{{EntityID: entB, Command: "power_off"}}
	env := envFrom(
		map[string]any{"guest_mode": true},
		map[string][]string{"label==lobby-screens": {entA}},
		nil,
		map[string][]eval.PresetCommand{presetID: presetCmds},
	)

	cl, ce := Compute(rule, env)
	if ce != nil {
		t.Fatalf("unexpected compile error: %v", ce)
	}
	if v, ok := cl.Variables["guest_mode"]; !ok || v != true {
		t.Fatalf("frozen guest_mode = %v (ok=%v); want true", v, ok)
	}
	if got := cl.Selectors["label==lobby-screens"]; !reflect.DeepEqual(got, []string{entA}) {
		t.Fatalf("Selectors[label==lobby-screens] = %v; want [%s]", got, entA)
	}
	if got := cl.PresetBatches[presetID]; !reflect.DeepEqual(got, presetCmds) {
		t.Fatalf("PresetBatches[%s] = %v; want %v", presetID, got, presetCmds)
	}
}

// RUL-393: an action params Expression is explicitly NOT frozen. A device_command
// carrying a params object must not be treated as a closed-over value here.
func TestComputeDoesNotFreezeParams(t *testing.T) {
	rule := mustParse(t, `{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers": [{ "type": "state", "entity_id": "`+entA+`", "to": ["on"] }],
		"actions": [{ "type": "device_command", "entity_id": "`+entA+`", "command": "set", "params": { "level": { "expr": "state(`+entA+`)" } } }]
	}`)
	env := envFrom(nil, nil, nil, nil)

	cl, ce := Compute(rule, env)
	if ce != nil {
		t.Fatalf("unexpected compile error: %v", ce)
	}
	if len(cl.Variables) != 0 || len(cl.Selectors) != 0 || len(cl.PresetBatches) != 0 {
		t.Fatalf("nothing should be frozen; got %+v", cl)
	}
}
