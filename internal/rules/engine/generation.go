package engine

import (
	"github.com/maaxton/waiveo-next/internal/rules/closure"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/model"
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

// ApplyGeneration makes gen the engine's applied generation, diffing it against
// the currently-applied one to decide which in-flight runs survive the swap
// (RUL-380/381). For each edge-classified rule in gen:
//
//   - a rule that is UNCHANGED from the applied generation (identical canonical
//     compiled structure AND identical frozen closure, RUL-381) carries its
//     existing driven instance forward untouched — its in-flight run continues
//     uninterrupted and its per-(trigger,entity) baseline/hold state is preserved
//     (RUL-303). A bare generation-number bump therefore cancels nothing.
//   - a rule that is NEW, or CHANGED (compiled structure or any closed-over value
//     differs), gets a fresh instance; a changed rule's prior in-flight run is
//     canceled (RUL-380), recorded as a Canceled disposition.
//
// A rule present in the applied generation but absent from gen is REMOVED; its
// in-flight run is canceled too (RUL-380). Each selector/device-class trigger of
// a new/changed rule is fanned out over its frozen matched entity set
// (RUL-011/013) and its frozen closure (variables, selectors, preset batches) is
// wired into evaluation. The engine snapshot is NOT reset: entity state observed
// under the prior generation persists across the swap, so a preserved run's
// deferred tail and an unchanged trigger's durable baseline remain resolvable
// (RUL-303/391).
//
// It returns the Canceled dispositions of every changed-or-removed rule whose
// in-flight run the swap tore down; a swap that cancels nothing returns none.
// (No firing arises from the apply itself — firings arise from Observe/Tick.)
//
// An app-classified rule in gen is skipped (never driven by this edge engine,
// RUL-240) rather than erroring — the generation as a whole still applies.
func (e *Engine) ApplyGeneration(gen Generation) []RunDisposition {
	// Index the currently-applied instances by rule ID so each incoming rule can
	// be diffed against its prior compiled form (RUL-380/381).
	prior := make(map[string]*ruleInstance, len(e.rules))
	for _, ri := range e.rules {
		prior[ri.ruleID] = ri
	}

	var out []RunDisposition
	next := make([]*ruleInstance, 0, len(gen.Rules))
	kept := make(map[string]bool, len(gen.Rules))

	for _, cr := range gen.Rules {
		if cr.Entry.ExecutionClass != "edge" {
			continue // app-classified: never driven by this edge engine (RUL-240).
		}
		id := cr.Rule.ID
		old, existed := prior[id]
		switch {
		case existed && !kept[id] && !ruleChanged(old.rule, old.closure, cr.Rule, cr.Closure):
			// Unchanged across the swap (RUL-380/381): carry the existing instance
			// forward untouched — in-flight run and (trigger,entity) state preserved.
			next = append(next, old)
		default:
			// New or changed (RUL-381): build a fresh instance. A changed rule's
			// prior in-flight run is canceled (RUL-380).
			if existed && !kept[id] && old.run != nil {
				out = append(out, RunDisposition{RuleID: id, Disposition: Canceled, Mode: old.mode})
			}
			next = append(next, newRuleInstance(cr))
		}
		kept[id] = true
	}

	// A prior rule absent from the new generation is removed: cancel its
	// in-flight run (RUL-380). Rules replaced/canceled above are marked kept and
	// are not double-counted here.
	for _, ri := range e.rules {
		if kept[ri.ruleID] {
			continue
		}
		if ri.run != nil {
			out = append(out, RunDisposition{RuleID: ri.ruleID, Disposition: Canceled, Mode: ri.mode})
		}
	}

	e.rules = next
	e.gen = gen.Number
	return out
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
