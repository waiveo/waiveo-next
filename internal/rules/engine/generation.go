package engine

import (
	"github.com/maaxton/waiveo-next/internal/rules/closure"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// CompiledRule is one rule of a compiled generation: its classification entry
// (Part 1), its parsed authored form, and the frozen closed-over values Part 4's
// closure package computed for it (RUL-390). The engine evaluates the rule
// against Closure — never a live variable/selector/preset lookup — so the same
// generation evaluates identically whether or not the app is reachable
// (RUL-391).
type CompiledRule struct {
	Entry   compile.CompiledRuleEntry
	Rule    model.Rule
	Closure closure.Closure
}

// Generation is the desired state the relay applies as a unit (Generations):
// the set of compiled edge rules plus their frozen closures, tagged with a
// monotonically-advancing generation number (RUL-380/390). ApplyGeneration
// makes a Generation the engine's applied state.
type Generation struct {
	Number int
	Rules  []CompiledRule
}

// ApplyGeneration makes gen the engine's applied generation: it builds a driven
// rule instance for every edge-classified rule in gen — fanning each selector /
// device-class trigger out over its frozen matched entity set (RUL-011/013) and
// wiring each rule's frozen closure (variables, selectors, preset batches) into
// evaluation — and returns the dispositions of any firings the apply itself
// produces (none in this part; firings arise from Observe/Tick).
//
// This part applies a generation fresh — it replaces the driven rule set and
// resets the snapshot, exactly as Load did for a single rule. Generation-swap
// preservation of an unchanged rule's in-flight run and per-(trigger,entity)
// baseline (RUL-380/381/303) is a later task; here every apply starts clean.
//
// An app-classified rule in gen is skipped (never driven by this edge engine,
// RUL-240) rather than erroring — the generation as a whole still applies.
func (e *Engine) ApplyGeneration(gen Generation) []RunDisposition {
	e.gen = gen.Number
	e.rules = e.rules[:0]
	e.snap = state.Snapshot{}
	for _, cr := range gen.Rules {
		if cr.Entry.ExecutionClass != "edge" {
			continue
		}
		e.rules = append(e.rules, newRuleInstance(cr))
	}
	return nil
}

// ruleInstance is one rule the engine currently drives: its identity/mode, its
// parsed rule and frozen closure, its per-(trigger,entity) trigger runtimes
// (selector/device-class triggers already fanned out, RUL-011), and its single
// in-flight run (RUL-190/241/242). Per-rule state is isolated here so the engine
// can drive several rules of one generation independently.
type ruleInstance struct {
	ruleID  string
	mode    string
	rule    model.Rule
	closure closure.Closure

	triggers []*triggerRuntime
	run      *runState
}

// newRuleInstance builds the driven instance for one compiled rule: its mode
// (defaulting to single, RUL-241), and its trigger runtimes with every
// selector/device-class trigger fanned out over the frozen matched set.
func newRuleInstance(cr CompiledRule) *ruleInstance {
	ri := &ruleInstance{
		ruleID:  cr.Rule.ID,
		mode:    cr.Rule.Mode,
		rule:    cr.Rule,
		closure: cr.Closure,
	}
	if ri.mode == "" {
		ri.mode = "single"
	}
	for _, m := range cr.Rule.Triggers {
		ri.triggers = append(ri.triggers, buildTriggers(m, cr.Closure)...)
	}
	return ri
}

// buildTriggers builds the trigger runtime(s) for one trigger member. An
// entity_id (or schedule) trigger yields a single runtime; a selector or
// device-class trigger yields one independent runtime per entity in its frozen
// matched set (RUL-011/013/304) — each keyed to its own subject entity so every
// matched entity carries its own first-observation baseline and `for`-hold.
func buildTriggers(m model.Member, cl closure.Closure) []*triggerRuntime {
	ref := m.EntityRef
	if ref != nil && ref.EntityID == "" && (ref.Selector != "" || ref.DeviceClass != "") {
		key := ref.Selector
		if key == "" {
			key = ref.DeviceClass
		}
		var out []*triggerRuntime
		for _, entityID := range cl.Selectors[key] {
			if tr := newStateOrNumericRuntime(m, entityID); tr != nil {
				out = append(out, tr)
			}
		}
		return out
	}
	if tr := newTriggerRuntime(m); tr != nil {
		return []*triggerRuntime{tr}
	}
	return nil
}

// closurePresetStore adapts a rule's frozen preset batches (RUL-173) to the
// eval.PresetStore a preset_batch action dispatches through, so the edge engine
// executes the compile-time-frozen command list rather than a live preset
// lookup. It layers over an optional live store (the Load compatibility path,
// which computes no closure): a frozen batch always wins, falling back to the
// live store only for a preset the closure did not freeze.
type closurePresetStore struct {
	frozen map[string][]eval.PresetCommand
	live   eval.PresetStore
}

func (s closurePresetStore) Commands(presetID string) ([]eval.PresetCommand, bool) {
	if cmds, ok := s.frozen[presetID]; ok {
		return cmds, true
	}
	if s.live != nil {
		return s.live.Commands(presetID)
	}
	return nil, false
}
