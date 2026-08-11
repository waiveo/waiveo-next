package compile

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
	"github.com/maaxton/waiveo-next/internal/rules/vocab"
)

// Validate returns the FIRST structural violation in r, or nil. It checks, in
// document order: every trigger, condition (recursively through and/or/not),
// and action (recursively through choose) is a closed-vocabulary member
// (RUL-001) with an unambiguous EntityRef where one is carried (RUL-010); then
// the rule's mode is known and its mode/max coupling holds (RUL-244/240).
// Classification (edge/app) is a separate, always-total step (classify.go);
// Validate is only about hard structural rejects.
func Validate(r model.Rule) *CompileError {
	for i, m := range r.Triggers {
		if e := validateMember(m, vocab.TriggerKind, fmt.Sprintf("triggers[%d]", i)); e != nil {
			return e
		}
	}
	for i, m := range r.Conditions {
		if e := validateMember(m, vocab.ConditionKind, fmt.Sprintf("conditions[%d]", i)); e != nil {
			return e
		}
	}
	for i, m := range r.Actions {
		if e := validateMember(m, vocab.ActionKind, fmt.Sprintf("actions[%d]", i)); e != nil {
			return e
		}
	}

	if vocab.ModeClass(r.Mode) == vocab.Unknown {
		return &CompileError{Code: codeUnknownVocabularyMember, Field: "mode", Message: fmt.Sprintf("%q is not a member of the closed mode vocabulary", r.Mode)}
	}
	if e := validateModeMax(r); e != nil {
		return e
	}
	// Expression-level checks (RUL-200/393 sites): an unknown filter (RUL-290)
	// and, for edge rules, the cross-entity source restriction (RUL-282). Run
	// last, after every structural member/mode reject.
	if e := validateExpressions(r); e != nil {
		return e
	}
	return nil
}

// validateMember recursively validates one member. kind selects the vocabulary
// the leaf type is checked against; path is the member's JSON address (RUL-006).
func validateMember(m model.Member, kind vocab.Kind, path string) *CompileError {
	// Composition (and/or/not): not itself a vocabulary leaf — recurse its
	// children as conditions (compositions appear only in conditions).
	if m.Composition != "" {
		for i, child := range m.Children {
			var childPath string
			if m.Composition == "not" {
				childPath = path + ".not"
			} else {
				childPath = fmt.Sprintf("%s.%s[%d]", path, m.Composition, i)
			}
			if e := validateMember(child, vocab.ConditionKind, childPath); e != nil {
				return e
			}
		}
		return nil
	}

	if vocab.ClassOf(kind, m.Type) == vocab.Unknown {
		return &CompileError{
			Code:    codeUnknownVocabularyMember,
			Field:   path + ".type",
			Message: fmt.Sprintf("%q is not a member of the closed %s vocabulary", m.Type, kindName(kind)),
		}
	}

	if m.EntityRef != nil && m.EntityRef.Present() != 1 {
		return &CompileError{
			Code:    codeEntityRefAmbiguous,
			Field:   path,
			Message: "an EntityRef MUST declare exactly one of entity_id/selector/device_class",
		}
	}

	// A signage action (RUL-234/235) targets SCREENS, so it carries a ScreenRef
	// (RUL-233) rather than an EntityRef and is held to that shape's own
	// exactly-one rule. The EntityRef check above does not cover it: `screen_id`
	// is not an EntityRef member at all, so an action naming BOTH a screen_id and
	// a selector decodes as a perfectly well-formed selector-only EntityRef and
	// passes — the exact ambiguity RUL-233 refuses, reaching the executor as a
	// silent pick of one of the two targets the author named.
	if kind == vocab.ActionKind && vocab.IsSignageAction(m.Type) {
		if e := validateScreenRef(m, path); e != nil {
			return e
		}
	}

	// A schedule trigger (time/time_pattern/sun) may carry a `misfire` policy; a
	// PRESENT value MUST be one of the closed set (RUL-350). An absent field is
	// legal and defaults to skip (RUL-354). Only triggers carry misfire.
	if kind == vocab.TriggerKind && (m.Type == "time" || m.Type == "time_pattern" || m.Type == "sun") {
		if e := validateMisfire(m, path); e != nil {
			return e
		}
	}

	// choose: recurse branch conditions, branch actions, and default actions.
	if kind == vocab.ActionKind && m.Type == "choose" {
		for j, b := range m.Branches {
			if e := validateMember(b.Condition, vocab.ConditionKind, fmt.Sprintf("%s.branches[%d].condition", path, j)); e != nil {
				return e
			}
			for k, a := range b.Actions {
				if e := validateMember(a, vocab.ActionKind, fmt.Sprintf("%s.branches[%d].actions[%d]", path, j, k)); e != nil {
					return e
				}
			}
		}
		for k, a := range m.Default {
			if e := validateMember(a, vocab.ActionKind, fmt.Sprintf("%s.default[%d]", path, k)); e != nil {
				return e
			}
		}
	}
	return nil
}

// validateScreenRef enforces SCREEN_REF_AMBIGUOUS (RUL-233): a signage action
// MUST declare exactly one of `screen_id` or `selector`. Both is ambiguous —
// nothing in the contract ranks one over the other, so an executor picking
// either would be inventing a rule — and neither names no target at all, which
// is an action that can never do anything rather than one that does nothing
// today (a selector matching zero screens IS legal, and is the way to express
// "whatever currently matches", RUL-233).
//
// The probe reads the action's raw JSON rather than model.Member.EntityRef
// because `screen_id` is not an EntityRef member: the decoder never sees it, so
// the EntityRef arity check cannot speak to this at all.
func validateScreenRef(m model.Member, path string) *CompileError {
	var probe struct {
		ScreenID string `json:"screen_id"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(m.Raw, &probe); err != nil {
		return nil // malformed JSON is not this check's concern (parse handled it)
	}
	n := 0
	if probe.ScreenID != "" {
		n++
	}
	if probe.Selector != "" {
		n++
	}
	if n == 1 {
		return nil
	}
	return &CompileError{
		Code:    codeScreenRefAmbiguous,
		Field:   path,
		Message: "a signage action's ScreenRef MUST declare exactly one of screen_id/selector (RUL-233)",
	}
}

// validateMisfire enforces MISFIRE_INVALID (RUL-350): a schedule trigger's
// `misfire`, when PRESENT, must be one of `catch_up_once`, `skip`, `fire_each`.
// An absent field is legal (defaults to skip, RUL-354), so presence is decided by
// a pointer probe against the trigger's raw JSON.
func validateMisfire(m model.Member, path string) *CompileError {
	var probe struct {
		Misfire *string `json:"misfire"`
	}
	if err := json.Unmarshal(m.Raw, &probe); err != nil {
		return nil // malformed JSON is not this check's concern (parse handled it)
	}
	if probe.Misfire == nil || schedule.ValidMisfire(*probe.Misfire) {
		return nil
	}
	return &CompileError{
		Code:    codeMisfireInvalid,
		Field:   path + ".misfire",
		Message: fmt.Sprintf("%q is not one of catch_up_once, skip, fire_each", *probe.Misfire),
	}
}

// validateModeMax enforces RUL-244: parallel requires max; a non-null max under
// any non-parallel mode is not applicable.
func validateModeMax(r model.Rule) *CompileError {
	if r.Mode == "parallel" {
		if r.Max == nil {
			return &CompileError{Code: codeModeMaxMissing, Field: "max", Message: "mode \"parallel\" requires max"}
		}
		return nil
	}
	if r.Max != nil {
		return &CompileError{Code: codeModeMaxNotApplicable, Field: "max", Message: fmt.Sprintf("max is only applicable to mode \"parallel\", not %q", r.Mode)}
	}
	return nil
}

func kindName(k vocab.Kind) string {
	switch k {
	case vocab.TriggerKind:
		return "trigger"
	case vocab.ConditionKind:
		return "condition"
	case vocab.ActionKind:
		return "action"
	default:
		return "member"
	}
}
