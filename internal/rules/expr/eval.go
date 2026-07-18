package expr

import (
	"math"
	"strings"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// Env is the evaluation environment a pipeline Expression resolves against
// (RUL-281/284/290): the live entity Snapshot its state/attr sources read, the
// device-class Registry that types attribute values (RUL-260), and the Clock
// its now()/elapsed read. On the edge, now() is subject to the clock-trust
// floor (RUL-370/275): when TrustUntrusted is set the engine's wall clock is
// unverified, so now() reports ClockUntrustedFloor — the persisted best-known
// time floor — instead of an unverified wall reading, exactly as the engine's
// floored schedule clock does; a duration hold is never floored and is not
// involved here.
type Env struct {
	Snapshot            state.Snapshot
	Reg                 registry.Registry
	Clk                 clock.Clock
	ClockUntrustedFloor int64
	TrustUntrusted      bool

	// EdgeScoped/ScopeEntity enforce the edge cross-entity restriction (RUL-282)
	// at eval time as defense in depth behind compile.Validate: when EdgeScoped
	// is set, a state/attr source referencing any entity other than ScopeEntity
	// — the firing trigger's subject — fails closed (RUL-284), even if that
	// entity resolves in the snapshot. App-class evaluation leaves EdgeScoped
	// false and reads any relay-visible entity (RUL-282/283).
	EdgeScoped  bool
	ScopeEntity string
}

// inScope reports whether an entity reference is admissible under the edge
// cross-entity restriction (RUL-282): always true off the edge (EdgeScoped
// false), otherwise only the firing trigger's own subject entity.
func (env Env) inScope(id string) bool {
	return !env.EdgeScoped || id == env.ScopeEntity
}

// nowMillis is the effective current wall instant for now()/elapsed on the edge
// (RUL-275/290/370): the floor while the clock is untrusted, the wall reading
// otherwise. It never returns a reading earlier than the floor while untrusted.
func (env Env) nowMillis() int64 {
	if env.TrustUntrusted {
		return env.ClockUntrustedFloor
	}
	return env.Clk.WallMillis()
}

// Evaluate resolves a parsed Expression to a value, or fails closed (RUL-284).
// A literal Expression is returned as-is and never fails. A pipeline evaluates
// its source (RUL-281) then each filter stage in order (RUL-290/292): any
// source or filter failure — an unresolvable entity/attribute, a number-parse
// failure (RUL-271), or a filter argument outside its domain — sets failed and
// short-circuits every later stage, EXCEPT a `default(value, fallback)` filter,
// the sole exception RUL-284 admits (RUL-285): it contains an upstream failure
// (or a null/absent upstream value) and yields its fallback, clearing failed so
// the pipeline continues normally. The caller treats failed==true as
// not-matched/false (fail closed), recorded for operator visibility.
func Evaluate(e Expr, env Env) (value any, failed bool) {
	if e.Literal != nil {
		return *e.Literal, false
	}
	if e.Pipeline == nil {
		// A well-formed Expr is always one form or the other; a zero Expr is a
		// caller error — fail closed rather than panic.
		return nil, true
	}
	val, failed := evalSource(e.Pipeline.Source, env)
	for _, f := range e.Pipeline.Filters {
		val, failed = applyFilter(f, val, failed, env)
	}
	return val, failed
}

// evalSource resolves a pipeline's source (RUL-281): a literal used as-is, or
// one of the state/attr/now source calls read against env. An unresolvable
// entity or a missing attribute fails closed (RUL-284).
func evalSource(s Source, env Env) (any, bool) {
	if s.Literal != nil {
		return *s.Literal, false
	}
	if s.Call == nil {
		return nil, true
	}
	c := s.Call
	switch c.Name {
	case "state":
		if len(c.Args) != 1 {
			return nil, true
		}
		id, ok := c.Args[0].Value.(string)
		if !ok {
			return nil, true
		}
		if !env.inScope(id) {
			return nil, true // outside the edge entity scope (RUL-282/284)
		}
		ent, ok := env.Snapshot.Get(id)
		if !ok {
			return nil, true // unresolvable entity (RUL-284)
		}
		return ent.State, false
	case "attr":
		if len(c.Args) != 2 {
			return nil, true
		}
		id, idOK := c.Args[0].Value.(string)
		name, nameOK := c.Args[1].Value.(string)
		if !idOK || !nameOK {
			return nil, true
		}
		if !env.inScope(id) {
			return nil, true // outside the edge entity scope (RUL-282/284)
		}
		ent, ok := env.Snapshot.Get(id)
		if !ok {
			return nil, true // unresolvable entity (RUL-284)
		}
		v, present := ent.Attributes[name]
		if !present {
			return nil, true // unresolvable attribute (RUL-284/285)
		}
		return v, false
	case "now":
		if len(c.Args) != 0 {
			return nil, true
		}
		return float64(env.nowMillis()), false
	default:
		// The parser restricts a source Call to state/attr/now; defense in depth.
		return nil, true
	}
}

// applyFilter applies one filter stage to the prior stage's (val, failed)
// output (RUL-292). `default` is handled specially — it is the sole filter that
// consumes the upstream failed flag and can clear it (RUL-285). Every other
// filter short-circuits on an already-failed upstream (propagating the
// failure), then runs and fails closed if its own argument is outside its
// domain (RUL-284).
func applyFilter(f Call, val any, failed bool, env Env) (any, bool) {
	if f.Name == "default" {
		// default(value, fallback): fallback when the upstream value failed to
		// evaluate (RUL-285's contained-failure exception) or is null/absent,
		// else the upstream value. Exactly one explicit argument, the fallback;
		// the value is the piped upstream output (RUL-292).
		if len(f.Args) != 1 {
			return nil, true
		}
		if failed || val == nil {
			return f.Args[0].Value, false
		}
		return val, false
	}

	// Fail-closed propagation (RUL-284): a filter after a failed stage stays
	// failed and never runs.
	if failed {
		return nil, true
	}

	out, ok := runFilter(f, val, env)
	if !ok {
		return nil, true
	}
	return out, false
}

// durationUnitSeconds maps each member of the v1 duration unit class (RUL-291)
// to its length in seconds. Membership in this map is membership in the sole v1
// unit class; a unit absent from it is not a duration unit.
var durationUnitSeconds = map[string]float64{
	"ms":  0.001,
	"s":   1,
	"min": 60,
	"h":   3600,
}

// runFilter computes one non-default filter over the piped value (RUL-290). It
// returns ok=false to fail closed (RUL-284): a type/domain mismatch, a
// number-parse failure (RUL-271), or a bad unit argument.
func runFilter(f Call, val any, env Env) (any, bool) {
	switch f.Name {
	case "upper", "lower", "trim":
		s, ok := val.(string)
		if !ok || len(f.Args) != 0 {
			return nil, false
		}
		switch f.Name {
		case "upper":
			return strings.ToUpper(s), true
		case "lower":
			return strings.ToLower(s), true
		default:
			return strings.TrimSpace(s), true
		}
	case "round":
		n, ok := eval.ParseNumber(val)
		if !ok {
			return nil, false
		}
		prec := 0
		switch len(f.Args) {
		case 0:
			// precision defaults to 0
		case 1:
			p, ok := eval.ParseNumber(f.Args[0].Value)
			if !ok || p != math.Trunc(p) || p < 0 {
				return nil, false
			}
			prec = int(p)
		default:
			return nil, false
		}
		return roundHalfUp(n, prec), true
	case "abs":
		n, ok := eval.ParseNumber(val)
		if !ok || len(f.Args) != 0 {
			return nil, false
		}
		return math.Abs(n), true
	case "int":
		n, ok := eval.ParseNumber(val)
		if !ok || len(f.Args) != 0 {
			return nil, false
		}
		return math.Trunc(n), true // truncate toward zero (RUL-290)
	case "float":
		n, ok := eval.ParseNumber(val)
		if !ok || len(f.Args) != 0 {
			return nil, false
		}
		return n, true
	case "elapsed":
		ts, ok := eval.ParseNumber(val)
		if !ok || len(f.Args) != 0 {
			return nil, false
		}
		return (float64(env.nowMillis()) - ts) / 1000, true // seconds (RUL-290)
	case "duration":
		n, ok := eval.ParseNumber(val)
		if !ok || len(f.Args) != 1 {
			return nil, false
		}
		unit, ok := f.Args[0].Value.(string)
		if !ok {
			return nil, false
		}
		secs, ok := durationUnitSeconds[unit]
		if !ok {
			return nil, false // outside the duration unit class (RUL-291)
		}
		return n * secs, true // normalized to seconds (RUL-290)
	case "convert":
		n, ok := eval.ParseNumber(val)
		if !ok || len(f.Args) != 2 {
			return nil, false
		}
		from, fromOK := f.Args[0].Value.(string)
		to, toOK := f.Args[1].Value.(string)
		if !fromOK || !toOK {
			return nil, false
		}
		fromSecs, fromMember := durationUnitSeconds[from]
		toSecs, toMember := durationUnitSeconds[to]
		if !fromMember || !toMember {
			return nil, false // not both in the same unit class (RUL-291)
		}
		return n * fromSecs / toSecs, true
	default:
		// The parser restricts filter names to the RUL-290 closed list; defense
		// in depth against an unknown filter reaching evaluation.
		return nil, false
	}
}

// roundHalfUp rounds n to prec decimal places with ties rounding toward
// positive infinity (RUL-290's round row: "half rounds up").
func roundHalfUp(n float64, prec int) float64 {
	scale := math.Pow(10, float64(prec))
	return math.Floor(n*scale+0.5) / scale
}
