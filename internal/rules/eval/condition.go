package eval

import (
	"encoding/json"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// EvalCondition evaluates one condition Member — a composition (`and`/`or`/
// `not`) or a leaf (`state`/`numeric`/`time`/`variable`) — against a point-in-
// time view of the world (RUL-100). It is a pure function: no fire/hold
// memory of any kind, unlike a trigger's Observe.
//
// A composition recurses over Children: `and` passes iff every child passes,
// `or` passes iff at least one child passes, `not` inverts its single child
// (RUL-100). A leaf's own EntityRef (state/numeric) is resolved against snap,
// which carries ANY relay-visible entity, not only the rule's triggering
// entity (RUL-101) — snap is the caller's concern to populate; EvalCondition
// itself applies no restriction on which entity a leaf may reference (that
// restriction exists only inside the expression grammar, never here).
//
// vars supplies every `variable` condition's comparison value. Per RUL-150/
// RUL-390, for an edge-classified rule this value is resolved once at compile
// time and frozen into the compiled generation — EvalCondition itself never
// performs a live variable lookup; it simply reads whatever vars supplies,
// which is the caller's (the compiled generation's) concern to keep frozen.
//
// clk supplies the wall-clock instant a `time` condition's window (RUL-130)
// is evaluated against; it is read via clk.WallMillis() only — never
// clk.Mono(), since a `time` condition is a wall-clock window, not a
// duration.
//
// Every leaf fails closed (evaluates false) on an unresolvable EntityRef, a
// missing/absent variable, a malformed field, or an unparseable value,
// exactly as RUL-262/271/284 require throughout this contract — never an
// error, never a coercion. A composition leaf type this evaluation core does
// not yet implement (`sun`, `template` — later parts per this task's plan)
// also evaluates false.
func EvalCondition(reg registry.Registry, snap state.Snapshot, clk clock.Clock, vars map[string]any, m model.Member) bool {
	switch m.Composition {
	case "and":
		for _, c := range m.Children {
			if !EvalCondition(reg, snap, clk, vars, c) {
				return false
			}
		}
		return true
	case "or":
		for _, c := range m.Children {
			if EvalCondition(reg, snap, clk, vars, c) {
				return true
			}
		}
		return false
	case "not":
		if len(m.Children) != 1 {
			return false // malformed; fail-closed
		}
		return !EvalCondition(reg, snap, clk, vars, m.Children[0])
	}

	switch m.Type {
	case "state":
		return evalStateCondition(reg, snap, m)
	case "numeric":
		return evalNumericCondition(snap, m)
	case "time":
		return evalTimeCondition(clk, m)
	case "variable":
		return evalVariableCondition(vars, m)
	default:
		// sun/template conditions (RUL-140/151) and any unrecognized leaf are
		// out of this task's scope; fail closed rather than error.
		return false
	}
}

// evalStateCondition implements RUL-110: a state condition's EntityRef is
// resolved against snap (RUL-101, any relay-visible entity); when attribute
// is absent, the entity's canonical state string is compared via the shared
// MatchState (RUL-320/321, identical to a state trigger's own from/to
// matching, RUL-021); when attribute is present, that attribute's own value
// is compared via typed equality (RUL-022's typedBoundMatch, the identical
// function a state trigger's attribute-scoped case uses) — trigger and
// condition matching MUST NOT diverge (RUL-110).
func evalStateCondition(reg registry.Registry, snap state.Snapshot, m model.Member) bool {
	if m.EntityRef == nil || m.EntityRef.EntityID == "" {
		return false
	}
	ent, ok := snap.Get(m.EntityRef.EntityID)
	if !ok {
		return false
	}

	var spec struct {
		Attribute string          `json:"attribute"`
		State     json.RawMessage `json:"state"`
	}
	if len(m.Raw) > 0 {
		if err := json.Unmarshal(m.Raw, &spec); err != nil {
			return false
		}
	}
	expected, ok := decodeBound(spec.State)
	if !ok {
		return false
	}

	if spec.Attribute == "" {
		return MatchState(reg, ent.DeviceClass, ent.State, expected)
	}

	attrVal, present := ent.Attributes[spec.Attribute]
	if !present {
		return false
	}
	vt := reg.AttributeType(ent.DeviceClass, spec.Attribute)
	return typedBoundMatch(attrVal, expected, vt)
}

// evalNumericCondition implements RUL-120: a numeric condition's EntityRef is
// resolved against snap (RUL-101); it performs a level check only (no
// crossing concept — a condition is evaluated at a point in time, not
// observed over a sequence), reusing the identical parsing (ParseNumber,
// RUL-270) and bounds-satisfaction rule (RUL-032, strictly exclusive at both
// endpoints) a numeric trigger uses via NewNumericTrigger's decode + its
// value/satisfies helpers.
func evalNumericCondition(snap state.Snapshot, m model.Member) bool {
	nt, err := NewNumericTrigger(m)
	if err != nil || nt.EntityID == "" {
		return false
	}
	ent, ok := snap.Get(nt.EntityID)
	if !ok {
		return false
	}
	parsed, ok := ParseNumber(nt.value(ent))
	if !ok {
		return false // unparseable, fail-closed (RUL-271); never an error
	}
	return nt.satisfies(parsed)
}

// evalTimeCondition implements RUL-130: passes when the current local
// time-of-day falls within the declared after/before bound(s) — an inclusive
// range when both are present and after <= before; a range wrapping past
// local midnight when after > before. Carries no EntityRef. The wall-clock
// instant is read via clk.WallMillis() and rendered as a time-of-day in UTC
// (this evaluation core has no scope-node timezone input yet — that is a
// later-part concern per RUL-340's "owning scope node's effective timezone").
func evalTimeCondition(clk clock.Clock, m model.Member) bool {
	var spec struct {
		After  string `json:"after"`
		Before string `json:"before"`
	}
	if len(m.Raw) > 0 {
		if err := json.Unmarshal(m.Raw, &spec); err != nil {
			return false
		}
	}
	hasAfter := spec.After != ""
	hasBefore := spec.Before != ""
	if !hasAfter && !hasBefore {
		return false
	}

	var afterSec, beforeSec int
	if hasAfter {
		var ok bool
		afterSec, ok = secondsOfDayFromString(spec.After)
		if !ok {
			return false
		}
	}
	if hasBefore {
		var ok bool
		beforeSec, ok = secondsOfDayFromString(spec.Before)
		if !ok {
			return false
		}
	}

	nowSec := secondsOfDayFromMillis(clk.WallMillis())

	switch {
	case hasAfter && hasBefore:
		if afterSec <= beforeSec {
			return nowSec >= afterSec && nowSec <= beforeSec
		}
		// after > before: wraps past local midnight (RUL-130).
		return nowSec >= afterSec || nowSec <= beforeSec
	case hasAfter:
		return nowSec >= afterSec
	default: // hasBefore only
		return nowSec <= beforeSec
	}
}

// secondsOfDayFromString parses an HH:MM:SS local time-of-day string
// (RUL-130) into seconds-since-local-midnight, reporting ok=false for any
// string that fails to parse in that exact shape (fail-closed, not an error).
func secondsOfDayFromString(s string) (int, bool) {
	t, err := time.Parse("15:04:05", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*3600 + t.Minute()*60 + t.Second(), true
}

// secondsOfDayFromMillis renders a Unix-milliseconds wall-clock instant as
// seconds-since-midnight.
func secondsOfDayFromMillis(wallMillis int64) int {
	t := time.UnixMilli(wallMillis).UTC()
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

// evalVariableCondition implements RUL-150: compares vars[variable] (the
// value the caller supplies — for an edge-classified rule, the value frozen
// into the compiled generation at compile time, RUL-390; EvalCondition itself
// never performs a live lookup) against exactly one of equals/above/below.
// Fails closed (false) when variable is absent from vars, or when a
// numeric (above/below) comparison's either side does not parse as a number
// (RUL-271).
func evalVariableCondition(vars map[string]any, m model.Member) bool {
	var spec struct {
		Variable string          `json:"variable"`
		Equals   json.RawMessage `json:"equals"`
		Above    json.RawMessage `json:"above"`
		Below    json.RawMessage `json:"below"`
	}
	if len(m.Raw) > 0 {
		if err := json.Unmarshal(m.Raw, &spec); err != nil {
			return false
		}
	}
	if spec.Variable == "" {
		return false
	}
	val, present := vars[spec.Variable]
	if !present {
		return false
	}

	if expected, ok := decodeBound(spec.Equals); ok {
		return variableEquals(val, expected)
	}
	if expected, ok := decodeBound(spec.Above); ok {
		vf, vok := ParseNumber(val)
		bf, bok := ParseNumber(expected)
		return vok && bok && vf > bf
	}
	if expected, ok := decodeBound(spec.Below); ok {
		vf, vok := ParseNumber(val)
		bf, bok := ParseNumber(expected)
		return vok && bok && vf < bf
	}
	return false
}

// variableEquals compares a variable's current value against a condition's
// literal comparison value with no cross-type coercion beyond RUL-270's
// number-parsing rule: a bool compares only against a bool, a string only
// against a string, and anything else is attempted as a number comparison via
// ParseNumber, failing closed if either side is not-a-number.
func variableEquals(a, b any) bool {
	switch av := a.(type) {
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	default:
		af, aok := ParseNumber(a)
		bf, bok := ParseNumber(b)
		return aok && bok && af == bf
	}
}
