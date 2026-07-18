// Package closure computes an edge-classified rule's compile-time closure: the
// set of values RUL-390 mandates be resolved once at compile time and frozen
// into the compiled generation, never re-resolved live by the edge engine.
//
// Three kinds are frozen (RUL-390): every `variable` condition's value observed
// at compile time (RUL-150), keyed by variable name; every trigger/action
// `EntityRef` that is a `selector` or `device_class` filter, resolved to its
// matched entity set (RUL-013), keyed by the filter string; and every
// `preset_batch` action's referenced command list (RUL-173), keyed by
// `preset_id` — an unresolvable preset_id is a PRESET_NOT_FOUND compile error.
// An `entity_id` EntityRef addresses a single concrete entity and needs no
// closure. An action's `params` Expression is deliberately NOT frozen here
// (RUL-393 — it is live-evaluated at run time, a deliberate exception to
// RUL-390's freeze list).
//
// Frozen values evaluate stale-but-defined even while the engine is offline
// (RUL-391); a variable write, membership change, or preset edit compiles a NEW
// generation with the new closed-over values (RUL-392) rather than mutating an
// applied one.
package closure

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// codePresetNotFound is the Error-taxonomy code (contracts/rules-1.md) for a
// preset_batch whose preset_id does not resolve to an existing preset row at
// compile time.
const codePresetNotFound = "PRESET_NOT_FOUND"

// Env is the compile-time environment Compute reads closed-over values from:
// the current variable values, the selector/device-class resolvers that answer
// a filter's matched entity set, and the preset store that answers a preset's
// command list. Each resolver is the live source consulted exactly once, at
// compile time; the resulting Closure is what the edge engine evaluates against
// thereafter (RUL-390).
type Env struct {
	Variables          map[string]any
	ResolveSelector    func(selector string) []string
	ResolveDeviceClass func(deviceClass string) []string
	PresetCommands     func(presetID string) ([]eval.PresetCommand, bool)
}

// Closure is a rule's frozen closed-over values (RUL-390), the `closed_over`
// block of a compiled generation's per-rule entry. Variables is keyed by
// variable name; Selectors is keyed by the selector/device-class filter string
// (RUL-011/013's matched entity set); PresetBatches is keyed by preset_id.
type Closure struct {
	Variables     map[string]any
	Selectors     map[string][]string
	PresetBatches map[string][]eval.PresetCommand
}

// Compute walks a rule and freezes its closed-over values into a Closure
// (RUL-390). It resolves: every `variable` condition (anywhere — including
// nested and/or/not compositions and choose-branch guards) into its
// compile-time value (RUL-150); every `selector`/`device_class` trigger or
// action EntityRef into its matched entity set (RUL-013); and every
// `preset_batch` action's command list (RUL-173), returning a PRESET_NOT_FOUND
// CompileError for an unresolvable preset_id. `entity_id` refs are left
// un-closured, and an action's `params` Expression is not frozen (RUL-393).
func Compute(rule model.Rule, env Env) (Closure, *compile.CompileError) {
	c := Closure{
		Variables:     map[string]any{},
		Selectors:     map[string][]string{},
		PresetBatches: map[string][]eval.PresetCommand{},
	}

	// Triggers: freeze selector/device_class matched sets (RUL-013).
	for _, t := range rule.Triggers {
		freezeRef(&c, env, t.EntityRef)
	}

	// Conditions: freeze every `variable` value (RUL-150), recursing through
	// and/or/not compositions.
	for _, cond := range rule.Conditions {
		freezeCondition(&c, env, cond)
	}

	// Actions: freeze action-side EntityRefs + preset command lists, recursing
	// through choose branches/default.
	for _, a := range rule.Actions {
		if ce := freezeAction(&c, env, a); ce != nil {
			return Closure{}, ce
		}
	}

	return c, nil
}

// freezeRef freezes a selector/device_class EntityRef's matched entity set into
// Selectors, keyed by the filter string (RUL-013). An entity_id ref (or a nil
// ref) is left un-closured.
func freezeRef(c *Closure, env Env, ref *model.EntityRef) {
	if ref == nil {
		return
	}
	switch {
	case ref.Selector != "":
		var set []string
		if env.ResolveSelector != nil {
			set = env.ResolveSelector(ref.Selector)
		}
		c.Selectors[ref.Selector] = set
	case ref.DeviceClass != "":
		var set []string
		if env.ResolveDeviceClass != nil {
			set = env.ResolveDeviceClass(ref.DeviceClass)
		}
		c.Selectors[ref.DeviceClass] = set
	}
}

// freezeCondition freezes a `variable` condition's compile-time value (RUL-150),
// recursing through and/or/not compositions to reach every nested variable
// condition. A variable absent from env.Variables is left out of the closure
// (it evaluates fail-closed at run time, RUL-150); no error is raised.
func freezeCondition(c *Closure, env Env, m model.Member) {
	if m.Composition != "" {
		for _, child := range m.Children {
			freezeCondition(c, env, child)
		}
		return
	}
	if m.Type != "variable" {
		return
	}
	var spec struct {
		Variable string `json:"variable"`
	}
	if len(m.Raw) > 0 {
		if err := json.Unmarshal(m.Raw, &spec); err != nil {
			return
		}
	}
	if spec.Variable == "" {
		return
	}
	if v, ok := env.Variables[spec.Variable]; ok {
		c.Variables[spec.Variable] = v
	}
}

// freezeAction freezes an action's closed-over values: a device-affecting
// action's selector/device_class EntityRef (RUL-013) and a preset_batch's
// command list (RUL-173), recursing through a choose action's branch guards
// (variable conditions), branch actions, and default actions. An action's
// `params` Expression is deliberately not frozen (RUL-393).
func freezeAction(c *Closure, env Env, m model.Member) *compile.CompileError {
	freezeRef(c, env, m.EntityRef)

	switch m.Type {
	case "preset_batch":
		return freezePreset(c, env, m)
	case "choose":
		for _, b := range m.Branches {
			freezeCondition(c, env, b.Condition)
			for _, a := range b.Actions {
				if ce := freezeAction(c, env, a); ce != nil {
					return ce
				}
			}
		}
		for _, a := range m.Default {
			if ce := freezeAction(c, env, a); ce != nil {
				return ce
			}
		}
	}
	return nil
}

// freezePreset freezes a preset_batch's command list keyed by preset_id
// (RUL-173), returning PRESET_NOT_FOUND when the preset_id does not resolve to
// an existing preset row at compile time. A missing preset resolver, or a
// preset_batch with no preset_id, is treated as unresolvable (fail-closed).
func freezePreset(c *Closure, env Env, m model.Member) *compile.CompileError {
	var spec struct {
		PresetID string `json:"preset_id"`
	}
	if len(m.Raw) > 0 {
		if err := json.Unmarshal(m.Raw, &spec); err != nil {
			return &compile.CompileError{Code: codePresetNotFound, Message: err.Error()}
		}
	}
	var (
		cmds []eval.PresetCommand
		ok   bool
	)
	if env.PresetCommands != nil {
		cmds, ok = env.PresetCommands(spec.PresetID)
	}
	if !ok {
		return &compile.CompileError{
			Code:    codePresetNotFound,
			Message: "preset_batch preset_id does not resolve to an existing preset row: " + spec.PresetID,
		}
	}
	c.PresetBatches[spec.PresetID] = cmds
	return nil
}
