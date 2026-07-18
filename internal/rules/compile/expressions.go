package compile

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/maaxton/waiveo-next/internal/rules/expr"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// validateExpressions checks every Expression an action carries — a `log`
// action's `message` (RUL-200) and each action `params` value (RUL-393),
// recursively through `choose`. Two taxonomy codes come out of the same pass:
//
//   - UNKNOWN_FILTER (RUL-290) for a filter outside the closed list. This is
//     class-agnostic (RUL-283) — the filter list is shared between edge and app.
//   - EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE (RUL-282) for an edge-classified
//     rule whose Expression sources an entity other than its trigger subject.
//     App-class rules and `template` contexts are exempt (RUL-282/070/151);
//     template triggers/conditions already force a rule app-class, so gating on
//     the rule's overall class subsumes the template exemption.
//
// It returns the first violation in document order, or nil. Parse failures
// other than an unknown filter (a malformed pipeline, an invalid source) carry
// no compile taxonomy code and fail closed at evaluation instead (RUL-284), so
// they are not rejected here.
func validateExpressions(r model.Rule) *CompileError {
	var sites []exprSite
	for i, a := range r.Actions {
		collectExprSites(a, fmt.Sprintf("actions[%d]", i), &sites)
	}
	if len(sites) == 0 {
		return nil
	}

	isEdge := Classify(r).ExecutionClass == "edge"
	allowed, dynamic := triggerSubjects(r)

	for _, s := range sites {
		e, perr := expr.Parse(s.raw)
		if perr != nil {
			if perr.Code == expr.CodeUnknownFilter {
				return &CompileError{Code: codeUnknownFilter, Field: s.path, Message: perr.Message}
			}
			continue // malformed/invalid-source: no taxonomy code, fails closed at eval (RUL-284)
		}
		// The cross-entity restriction applies only to edge rules, and only when
		// every trigger subject is a statically-known literal entity; a dynamic
		// (selector/device_class) subject is decided at runtime (RUL-011) and is
		// left to eval-time enforcement.
		if !isEdge || dynamic {
			continue
		}
		for _, id := range expr.SourceEntities(e) {
			if !allowed[id] {
				return &CompileError{
					Code:    codeEdgeExpressionCrossEntity,
					Field:   s.path,
					Message: fmt.Sprintf("an edge rule's Expression may source only its trigger subject entity, not %q (RUL-282)", id),
				}
			}
		}
	}
	return nil
}

// exprSite is one Expression's location: the raw JSON of the Expression and its
// RUL-006 field path for error addressing.
type exprSite struct {
	path string
	raw  json.RawMessage
}

// collectExprSites appends every Expression site an action carries, recursing
// through choose branch/default actions. A `log` message (RUL-200) and each
// `params` value (RUL-393) are Expression sites; params keys are visited in
// sorted order for a deterministic first-violation.
func collectExprSites(m model.Member, path string, out *[]exprSite) {
	var probe struct {
		Message json.RawMessage            `json:"message"`
		Params  map[string]json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(m.Raw, &probe) // shape already validated; a bad shape yields no sites

	if m.Type == "log" && len(probe.Message) > 0 {
		*out = append(*out, exprSite{path + ".message", probe.Message})
	}
	if len(probe.Params) > 0 {
		keys := make([]string, 0, len(probe.Params))
		for k := range probe.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			*out = append(*out, exprSite{path + ".params." + k, probe.Params[k]})
		}
	}

	if m.Type == "choose" {
		for j, b := range m.Branches {
			for k, a := range b.Actions {
				collectExprSites(a, fmt.Sprintf("%s.branches[%d].actions[%d]", path, j, k), out)
			}
		}
		for k, a := range m.Default {
			collectExprSites(a, fmt.Sprintf("%s.default[%d]", path, k), out)
		}
	}
}

// triggerSubjects returns the set of literal entity IDs the rule's triggers
// subject on, and whether any trigger has a dynamic (selector/device_class or
// subjectless) subject whose firing entity is only known at runtime (RUL-011).
// The edge cross-entity check (RUL-282) is decidable at compile time only when
// no trigger is dynamic.
func triggerSubjects(r model.Rule) (allowed map[string]bool, dynamic bool) {
	allowed = map[string]bool{}
	for _, t := range r.Triggers {
		if t.EntityRef != nil && t.EntityRef.EntityID != "" {
			allowed[t.EntityRef.EntityID] = true
		} else {
			dynamic = true
		}
	}
	return allowed, dynamic
}
