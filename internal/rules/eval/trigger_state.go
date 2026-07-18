package eval

import (
	"encoding/json"
	"reflect"

	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// StateTrigger is one decoded `state` trigger (RUL-020): its subject entity id
// plus the optional Attribute / From / To that select which of RUL-023's four
// fire rules applies. From and To carry the authored bound as encoding/json
// decodes it (a scalar string, a []any of strings, or — for an attribute —
// a JSON number/bool), and HasFrom / HasTo record presence so an absent bound
// is distinguished from a present-but-null one (both treated as absent). The
// `for` hold (RUL-024/026) and the EntityRef selector/device_class forms
// (RUL-010/011) are the concern of later evaluation tasks; this type carries
// only the entity_id subject and the from/to/attribute fire selectors.
type StateTrigger struct {
	EntityID  string
	Attribute string
	From      any
	HasFrom   bool
	To        any
	HasTo     bool
}

// TriggerBaseline is the durable per-(trigger, entity) last-observed value a
// state trigger diffs its next observation against (RUL-300/304). Known is
// true once a durable value exists for this (trigger, entity) pair — set by a
// prior observation, or seeded from the entity's durable pre-restart value on
// an engine restart / re-enrolment. A false Known marks a genuine
// first-ever observation with no prior to transition from: it is recorded
// (never treated as an absent baseline that could synthesize a transition,
// RUL-300) but never fires. State holds the last canonical state string; Attr
// / AttrKnown hold the last value of an attribute-scoped trigger's named
// attribute (RUL-022/023 cases 1–2), unused for a state-scoped or unscoped
// trigger.
type TriggerBaseline struct {
	Known     bool
	State     string
	Attr      any
	AttrKnown bool
}

// NewStateTrigger builds a StateTrigger from a decoded `state` trigger member
// (RUL-020). The subject entity_id comes from the member's EntityRef; the
// attribute/from/to fire selectors come from the member's retained raw JSON.
// A `from`/`to` present as JSON null is treated as absent (RUL-023 keys on
// presence of a usable bound).
func NewStateTrigger(m model.Member) (*StateTrigger, error) {
	st := &StateTrigger{}
	if m.EntityRef != nil {
		st.EntityID = m.EntityRef.EntityID
	}
	var raw struct {
		Attribute string          `json:"attribute"`
		From      json.RawMessage `json:"from"`
		To        json.RawMessage `json:"to"`
	}
	if len(m.Raw) > 0 {
		if err := json.Unmarshal(m.Raw, &raw); err != nil {
			return nil, err
		}
	}
	st.Attribute = raw.Attribute
	if v, ok := decodeBound(raw.From); ok {
		st.From = v
		st.HasFrom = true
	}
	if v, ok := decodeBound(raw.To); ok {
		st.To = v
		st.HasTo = true
	}
	return st, nil
}

// decodeBound decodes a from/to bound into `any`, reporting ok=false when the
// field is absent or an explicit JSON null (both mean "no bound" for RUL-023).
func decodeBound(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// Observe applies RUL-023's four-way fire rule to a single observation of the
// trigger's subject entity, diffing against prev (the durable last value,
// RUL-300/304) and returning whether the trigger fires plus the next baseline
// to persist. It is a pure function of (prev, obs): the engine owns baseline
// persistence and the first-observation seeding.
//
// Fail-closed throughout (RUL-262/271): an unresolvable comparison evaluates
// as not-matched, never an error.
func (t *StateTrigger) Observe(reg registry.Registry, prev TriggerBaseline, obs state.Observation) (bool, TriggerBaseline) {
	cur := obs.Entity

	// The next baseline always records the current durable value, whether or
	// not the trigger fires.
	next := TriggerBaseline{Known: true, State: cur.State}
	var curAttr any
	var curAttrPresent bool
	if t.Attribute != "" {
		curAttr, curAttrPresent = cur.Attributes[t.Attribute]
		next.Attr = curAttr
		next.AttrKnown = curAttrPresent
	}

	// First-ever observation (no durable prior): never a transition, even if
	// the value equals to: (RUL-300/302/304). Record and do not fire.
	if !prev.Known {
		return false, next
	}

	attrScoped := t.Attribute != ""
	bounded := t.HasFrom || t.HasTo

	switch {
	case !attrScoped && !bounded: // RUL-023 case 4: unscoped
		// Fires on any reported change — state string or any attribute
		// (RUL-023 case 4: "the state string or any attribute moved").
		return obs.StateChanged || len(obs.ChangedAttrs) > 0, next

	case !attrScoped && bounded: // RUL-023 case 3: state-scoped
		// MUST NOT fire on a tick whose state string is unchanged, even if
		// the unchanged value equals to: (RUL-023 case 3 / RUL-330).
		if !obs.StateChanged {
			return false, next
		}
		return t.matchStateBounds(reg, cur.DeviceClass, prev.State, cur.State), next

	case attrScoped && bounded: // RUL-023 case 1: attribute-scoped, bounded
		// Fires only when the attribute's OWN value transitions matching
		// from/to; never on a state-only change (RUL-023 case 1).
		if !curAttrPresent {
			return false, next
		}
		if !prev.AttrKnown || attrEqual(reg, cur.DeviceClass, t.Attribute, prev.Attr, curAttr) {
			return false, next // no attribute transition
		}
		return t.matchAttrBounds(reg, cur.DeviceClass, prev.Attr, curAttr), next

	default: // attrScoped && !bounded — RUL-023 case 2: attribute-scoped, unbounded
		// Fires whenever the attribute's value changes to a new value
		// (RUL-023 case 2), independent of the state string.
		if !curAttrPresent {
			return false, next
		}
		changed := !prev.AttrKnown || !attrEqual(reg, cur.DeviceClass, t.Attribute, prev.Attr, curAttr)
		return changed, next
	}
}

// matchStateBounds applies RUL-021 state-string matching: from (if present)
// against the prior state and to (if present) against the new state, each via
// the shared MatchState (scalar group-expands, array exact). An absent bound
// is unconstrained.
func (t *StateTrigger) matchStateBounds(reg registry.Registry, deviceClass, oldState, newState string) bool {
	if t.HasFrom && !MatchState(reg, deviceClass, oldState, t.From) {
		return false
	}
	if t.HasTo && !MatchState(reg, deviceClass, newState, t.To) {
		return false
	}
	return true
}

// matchAttrBounds applies RUL-022 attribute matching: from (if present)
// against the prior attribute value and to (if present) against the new value,
// each via typed equality — no semantic-group expansion. An absent bound is
// unconstrained. A bound authored as an array matches by typed membership
// (any element equal); a scalar bound is a single typed comparison.
func (t *StateTrigger) matchAttrBounds(reg registry.Registry, deviceClass string, oldVal, newVal any) bool {
	vt := reg.AttributeType(deviceClass, t.Attribute)
	if t.HasFrom && !typedBoundMatch(oldVal, t.From, vt) {
		return false
	}
	if t.HasTo && !typedBoundMatch(newVal, t.To, vt) {
		return false
	}
	return true
}

// typedBoundMatch reports whether value equals bound under type vt (RUL-022),
// treating an array bound as typed membership. Fail-closed for any other shape.
func typedBoundMatch(value, bound any, vt registry.ValueType) bool {
	switch b := bound.(type) {
	case []any:
		for _, e := range b {
			if TypedEqual(value, e, vt) {
				return true
			}
		}
		return false
	case []string:
		for _, e := range b {
			if TypedEqual(value, e, vt) {
				return true
			}
		}
		return false
	default:
		return TypedEqual(value, bound, vt)
	}
}

// attrEqual reports whether two successive values of a named attribute are the
// same value, for the RUL-023 case 1/2 "did it change" test. A registry-typed
// attribute uses typed equality (RUL-022); an untyped attribute falls back to
// a structural comparison so a composite value is compared by content.
func attrEqual(reg registry.Registry, deviceClass, attr string, a, b any) bool {
	vt := reg.AttributeType(deviceClass, attr)
	if vt == registry.Unknown {
		return reflect.DeepEqual(a, b)
	}
	return TypedEqual(a, b, vt)
}
