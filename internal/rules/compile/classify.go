package compile

import (
	"fmt"

	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/vocab"
)

// CompiledRuleEntry is a rule's classification result (RUL Wire shapes,
// RUL-390): its execution_class and, when app, every app-causing member
// (RUL-006). This front-end omits closed_over (Compile-time closure is a later
// part); classify only fills the classification fields.
type CompiledRuleEntry struct {
	RuleID          string      `json:"rule_id"`
	ExecutionClass  string      `json:"execution_class"`
	AppClassReasons []AppReason `json:"app_class_reasons,omitempty"`
}

// AppReason is one RUL-006 per-member "why this rule is app": the offending
// member's JSON field path, its type (or the mode's value), and a reason.
type AppReason struct {
	Field  string `json:"field"`
	Type   string `json:"type,omitempty"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason"`
}

// Classify implements RUL-002 as a total function: a rule is edge iff every
// trigger, every condition (recursively through and/or/not), every action
// (recursively through choose branches + default), and its mode are edge —
// otherwise app, with EVERY app-causing member listed (RUL-006). It assumes the
// rule already passed Validate (no unknown members); an unknown type reaching
// here contributes no reason (it is never App), which is why Compile validates
// first.
func Classify(r model.Rule) CompiledRuleEntry {
	var reasons []AppReason
	for i, m := range r.Triggers {
		classifyMember(m, vocab.TriggerKind, fmt.Sprintf("triggers[%d]", i), &reasons)
	}
	for i, m := range r.Conditions {
		classifyMember(m, vocab.ConditionKind, fmt.Sprintf("conditions[%d]", i), &reasons)
	}
	for i, m := range r.Actions {
		classifyMember(m, vocab.ActionKind, fmt.Sprintf("actions[%d]", i), &reasons)
	}
	if vocab.ModeClass(r.Mode) == vocab.App {
		reasons = append(reasons, AppReason{
			Field:  "mode",
			Value:  r.Mode,
			Reason: fmt.Sprintf("%s is app-only (RUL-240)", r.Mode),
		})
	}

	entry := CompiledRuleEntry{RuleID: r.ID, ExecutionClass: "edge"}
	if len(reasons) > 0 {
		entry.ExecutionClass = "app"
		entry.AppClassReasons = reasons
	}
	return entry
}

func classifyMember(m model.Member, kind vocab.Kind, path string, reasons *[]AppReason) {
	// Composition (and/or/not): edge iff all children edge (RUL-100). Recurse;
	// the composition itself carries no class.
	if m.Composition != "" {
		for i, child := range m.Children {
			var childPath string
			if m.Composition == "not" {
				childPath = path + ".not"
			} else {
				childPath = fmt.Sprintf("%s.%s[%d]", path, m.Composition, i)
			}
			classifyMember(child, vocab.ConditionKind, childPath, reasons)
		}
		return
	}

	if vocab.ClassOf(kind, m.Type) == vocab.App {
		*reasons = append(*reasons, AppReason{
			Field:  path,
			Type:   m.Type,
			Reason: fmt.Sprintf("%s is app-class unconditionally", m.Type),
		})
	}

	// choose: edge iff every branch action AND every default action is edge
	// (RUL-180/182). Branch guard conditions do not affect action-class.
	if kind == vocab.ActionKind && m.Type == "choose" {
		for j, b := range m.Branches {
			classifyMember(b.Condition, vocab.ConditionKind, fmt.Sprintf("%s.branches[%d].condition", path, j), reasons)
			for k, a := range b.Actions {
				classifyMember(a, vocab.ActionKind, fmt.Sprintf("%s.branches[%d].actions[%d]", path, j, k), reasons)
			}
		}
		for k, a := range m.Default {
			classifyMember(a, vocab.ActionKind, fmt.Sprintf("%s.default[%d]", path, k), reasons)
		}
	}
}
