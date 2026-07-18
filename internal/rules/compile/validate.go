package compile

import (
	"fmt"

	"github.com/maaxton/waiveo-next/internal/rules/model"
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
