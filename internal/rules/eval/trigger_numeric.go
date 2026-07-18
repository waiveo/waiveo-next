package eval

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// NumericTrigger is one decoded `numeric` trigger (RUL-030): its subject
// entity id plus the optional Attribute and the above/below bounds RUL-032
// defines satisfaction against. At least one of Above/Below is present by
// RUL-030 (compile-time enforced); this type simply carries whichever bounds
// were authored, each a literal JSON number decoded at construction time —
// never an expression (RUL-030). The `for` hold (RUL-024/033, reused
// identically) and the EntityRef selector/device_class forms are the
// concern of later evaluation tasks; this type carries only the fire
// selectors RUL-031/032/033 need.
type NumericTrigger struct {
	EntityID  string
	Attribute string
	Above     float64
	HasAbove  bool
	Below     float64
	HasBelow  bool
}

// NewNumericTrigger builds a NumericTrigger from a decoded `numeric` trigger
// member (RUL-030). The subject entity_id comes from the member's EntityRef;
// attribute/above/below come from the member's retained raw JSON. An
// above/below present as JSON null, or that fails to decode as a number, is
// treated as absent (fail-closed, RUL-262): construction never errors on a
// malformed bound, it simply leaves that bound unconstrained.
func NewNumericTrigger(m model.Member) (*NumericTrigger, error) {
	nt := &NumericTrigger{}
	if m.EntityRef != nil {
		nt.EntityID = m.EntityRef.EntityID
	}
	var raw struct {
		Attribute string          `json:"attribute"`
		Above     json.RawMessage `json:"above"`
		Below     json.RawMessage `json:"below"`
	}
	if len(m.Raw) > 0 {
		if err := json.Unmarshal(m.Raw, &raw); err != nil {
			return nil, err
		}
	}
	nt.Attribute = raw.Attribute
	if v, ok := decodeBound(raw.Above); ok {
		if f, pok := ParseNumber(v); pok {
			nt.Above = f
			nt.HasAbove = true
		}
	}
	if v, ok := decodeBound(raw.Below); ok {
		if f, pok := ParseNumber(v); pok {
			nt.Below = f
			nt.HasBelow = true
		}
	}
	return nt, nil
}

// value resolves the compared value for one observation of the trigger's
// subject entity (RUL-031): the named attribute's value when Attribute is
// set, otherwise the entity's canonical state string.
func (t *NumericTrigger) value(e state.Entity) any {
	if t.Attribute != "" {
		return e.Attributes[t.Attribute]
	}
	return e.State
}

// satisfies applies RUL-032: strictly greater than Above (if declared) and
// strictly less than Below (if declared), both endpoints excluded; when
// both bounds are declared, both must hold simultaneously.
func (t *NumericTrigger) satisfies(v float64) bool {
	if t.HasAbove && !(v > t.Above) {
		return false
	}
	if t.HasBelow && !(v < t.Below) {
		return false
	}
	return true
}

// Observe applies RUL-033/302's level-then-crossing fire rule to a single
// observation of the trigger's subject entity. It is a pure function of
// (prevParsed, obs): prevParsed is the immediately preceding observation's
// parsed value, or nil when none exists — either because this is the
// entity's first-ever observation for this trigger, or because the most
// recent prior observation failed to parse (RUL-031), leaving no prior
// value to cross from (RUL-302). Observe returns the fire decision plus the
// parsed value to pass as prevParsed on the next call.
//
// Fail-closed throughout (RUL-031/271): a value that fails to parse as a
// number never satisfies either bound, on every observation, and yields a
// nil newParsed — it is never treated as an evaluation error and never
// coerced.
func (t *NumericTrigger) Observe(prevParsed *float64, obs state.Observation) (bool, *float64) {
	parsed, ok := ParseNumber(t.value(obs.Entity))
	if !ok {
		return false, nil
	}

	satisfied := t.satisfies(parsed)
	next := parsed

	if prevParsed == nil {
		// No prior parsed value to cross from (RUL-302): a level check, not
		// a crossing. This covers both a genuine first-ever observation and
		// the first valid observation after a run of unparseable ones.
		return satisfied, &next
	}

	// A crossing: the immediately preceding parsed value did not satisfy and
	// the current one does (RUL-033).
	crossed := !t.satisfies(*prevParsed) && satisfied
	return crossed, &next
}
