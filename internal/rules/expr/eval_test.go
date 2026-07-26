package expr

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// evalRaw parses raw and evaluates it against env, failing the test on a parse
// error (parse coverage lives in parse_test.go).
func evalRaw(t *testing.T, raw string, env Env) (any, bool) {
	t.Helper()
	e, perr := Parse(json.RawMessage(raw))
	if perr != nil {
		t.Fatalf("Parse(%s) unexpected parse error: %v", raw, perr)
	}
	return Evaluate(e, env)
}

// baseEnv builds an Env whose snapshot holds one media-player entity Z2 (state
// "playing", volume 42) and one attribute-less sensor entity SENS used for the
// RUL-285 unresolvable-attribute case.
func baseEnv() Env {
	snap := state.Snapshot{
		"01J8Z3K4N5P6Q7R8S9T0V1W2Z2": state.Entity{
			ID:          "01J8Z3K4N5P6Q7R8S9T0V1W2Z2",
			DeviceClass: "media-player",
			State:       "playing",
			Attributes:  map[string]any{"volume": float64(42), "media_title": "  Hi There  "},
		},
		"01J8Z3K4N5P6Q7R8S9T0V1W2Z3": state.Entity{
			ID:          "01J8Z3K4N5P6Q7R8S9T0V1W2Z3",
			DeviceClass: "sensor",
			State:       "ok",
			Attributes:  nil,
		},
	}
	return Env{Snapshot: snap, Reg: registry.FixtureRegistry{}, Clk: clock.NewFakeClock()}
}

// --- RUL-285: default contains an upstream attribute failure ---

func TestEvaluate_RUL285_DefaultContainsAttributeFailure(t *testing.T) {
	env := baseEnv()

	// wrapped: attr() on an entity with no battery_ok attribute fails upstream,
	// default(true) contains it and yields the truthy fallback; not a failure.
	wrapped := `{"expr":"attr('01J8Z3K4N5P6Q7R8S9T0V1W2Z3', 'battery_ok') | default(true)"}`
	val, failed := evalRaw(t, wrapped, env)
	if failed {
		t.Fatalf("wrapped: expected contained (not failed), got failed=true")
	}
	if val != true {
		t.Fatalf("wrapped: expected fallback value true, got %#v", val)
	}

	// unwrapped: the same reference with no default fails closed (RUL-284).
	unwrapped := `{"expr":"attr('01J8Z3K4N5P6Q7R8S9T0V1W2Z3', 'battery_ok')"}`
	_, failed = evalRaw(t, unwrapped, env)
	if !failed {
		t.Fatalf("unwrapped: expected fail-closed (failed=true), got failed=false")
	}
}

// --- literal form ---

func TestEvaluate_Literal(t *testing.T) {
	env := baseEnv()
	for _, raw := range []string{`"hello"`, `42`, `true`, `null`} {
		e, perr := Parse(json.RawMessage(raw))
		if perr != nil {
			t.Fatalf("Parse(%s): %v", raw, perr)
		}
		if _, failed := Evaluate(e, env); failed {
			t.Fatalf("literal %s must never fail", raw)
		}
	}
	v, failed := evalRaw(t, `42`, env)
	if failed || v != float64(42) {
		t.Fatalf("literal 42: got %#v failed=%v", v, failed)
	}
}

// --- sources ---

func TestEvaluate_StateSource(t *testing.T) {
	env := baseEnv()
	v, failed := evalRaw(t, `{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2')"}`, env)
	if failed || v != "playing" {
		t.Fatalf("state: got %#v failed=%v, want playing", v, failed)
	}
	// unresolvable entity fails closed.
	_, failed = evalRaw(t, `{"expr":"state('01J0000000000000000000000X')"}`, env)
	if !failed {
		t.Fatalf("state on unresolvable entity: expected failed=true")
	}
}

func TestEvaluate_AttrSource(t *testing.T) {
	env := baseEnv()
	v, failed := evalRaw(t, `{"expr":"attr('01J8Z3K4N5P6Q7R8S9T0V1W2Z2', 'volume')"}`, env)
	if failed || v != float64(42) {
		t.Fatalf("attr volume: got %#v failed=%v, want 42", v, failed)
	}
	// missing attribute fails closed.
	_, failed = evalRaw(t, `{"expr":"attr('01J8Z3K4N5P6Q7R8S9T0V1W2Z2', 'nope')"}`, env)
	if !failed {
		t.Fatalf("attr missing: expected failed=true")
	}
}

// --- string filters ---

func TestEvaluate_StringFilters(t *testing.T) {
	env := baseEnv()
	if v, failed := evalRaw(t, `{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2') | upper"}`, env); failed || v != "PLAYING" {
		t.Fatalf("upper: got %#v failed=%v", v, failed)
	}
	if v, failed := evalRaw(t, `{"expr":"'HELLO' | lower"}`, env); failed || v != "hello" {
		t.Fatalf("lower: got %#v failed=%v", v, failed)
	}
	if v, failed := evalRaw(t, `{"expr":"attr('01J8Z3K4N5P6Q7R8S9T0V1W2Z2', 'media_title') | trim"}`, env); failed || v != "Hi There" {
		t.Fatalf("trim: got %#v failed=%v", v, failed)
	}
	// upper on a non-string fails closed.
	if _, failed := evalRaw(t, `{"expr":"42 | upper"}`, env); !failed {
		t.Fatalf("upper on number: expected failed=true")
	}
}

// --- numeric filters ---

func TestEvaluate_Round(t *testing.T) {
	env := baseEnv()
	cases := []struct {
		raw  string
		want float64
	}{
		{`{"expr":"2.5 | round"}`, 3},   // half rounds up toward +inf
		{`{"expr":"-2.5 | round"}`, -2}, // ties toward +inf
		{`{"expr":"2.4 | round"}`, 2},   //
		{`{"expr":"2.345 | round(2)"}`, 2.35},
		{`{"expr":"1.244 | round(2)"}`, 1.24},
	}
	for _, c := range cases {
		v, failed := evalRaw(t, c.raw, env)
		if failed {
			t.Fatalf("%s: unexpected failure", c.raw)
		}
		got, _ := v.(float64)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("%s: got %v want %v", c.raw, got, c.want)
		}
	}
}

func TestEvaluate_Abs(t *testing.T) {
	env := baseEnv()
	if v, failed := evalRaw(t, `{"expr":"-3.5 | abs"}`, env); failed || v != float64(3.5) {
		t.Fatalf("abs: got %#v failed=%v", v, failed)
	}
}

func TestEvaluate_IntTruncate(t *testing.T) {
	env := baseEnv()
	cases := []struct {
		raw  string
		want float64
	}{
		{`{"expr":"2.9 | int"}`, 2},   // truncate toward zero
		{`{"expr":"-2.9 | int"}`, -2}, // toward zero, not floor
		{`{"expr":"'7.8' | int"}`, 7}, // string parsed per RUL-270
	}
	for _, c := range cases {
		v, failed := evalRaw(t, c.raw, env)
		if failed || v != c.want {
			t.Fatalf("%s: got %#v failed=%v want %v", c.raw, v, failed, c.want)
		}
	}
	// non-number fails per RUL-271.
	if _, failed := evalRaw(t, `{"expr":"'abc' | int"}`, env); !failed {
		t.Fatalf("int('abc'): expected failed=true")
	}
	if _, failed := evalRaw(t, `{"expr":"true | int"}`, env); !failed {
		t.Fatalf("int(true): expected failed=true")
	}
}

func TestEvaluate_Float(t *testing.T) {
	env := baseEnv()
	if v, failed := evalRaw(t, `{"expr":"'3.14' | float"}`, env); failed || v != float64(3.14) {
		t.Fatalf("float('3.14'): got %#v failed=%v", v, failed)
	}
	// exponent notation is not-a-number per RUL-270.
	if _, failed := evalRaw(t, `{"expr":"'1e3' | float"}`, env); !failed {
		t.Fatalf("float('1e3'): expected failed=true")
	}
}

// --- duration / convert (RUL-291) ---

func TestEvaluate_Duration(t *testing.T) {
	env := baseEnv()
	cases := []struct {
		raw  string
		want float64 // seconds
	}{
		{`{"expr":"5 | duration('min')"}`, 300},
		{`{"expr":"1 | duration('h')"}`, 3600},
		{`{"expr":"500 | duration('ms')"}`, 0.5},
		{`{"expr":"90 | duration('s')"}`, 90},
	}
	for _, c := range cases {
		v, failed := evalRaw(t, c.raw, env)
		if failed {
			t.Fatalf("%s: unexpected failure", c.raw)
		}
		if math.Abs(v.(float64)-c.want) > 1e-9 {
			t.Fatalf("%s: got %v want %v", c.raw, v, c.want)
		}
	}
	// unknown unit fails closed.
	if _, failed := evalRaw(t, `{"expr":"5 | duration('furlong')"}`, env); !failed {
		t.Fatalf("duration bogus unit: expected failed=true")
	}
}

func TestEvaluate_Convert(t *testing.T) {
	env := baseEnv()
	cases := []struct {
		raw  string
		want float64
	}{
		{`{"expr":"1 | convert('h','min')"}`, 60},
		{`{"expr":"2 | convert('h','s')"}`, 7200},
		{`{"expr":"1000 | convert('ms','s')"}`, 1},
	}
	for _, c := range cases {
		v, failed := evalRaw(t, c.raw, env)
		if failed {
			t.Fatalf("%s: unexpected failure", c.raw)
		}
		if math.Abs(v.(float64)-c.want) > 1e-9 {
			t.Fatalf("%s: got %v want %v", c.raw, v, c.want)
		}
	}
	// cross-class (unit not in the duration class) fails closed (RUL-291).
	if _, failed := evalRaw(t, `{"expr":"1 | convert('h','byte')"}`, env); !failed {
		t.Fatalf("convert cross-class: expected failed=true")
	}
}

// --- now / elapsed ---

func TestEvaluate_NowTrusted(t *testing.T) {
	fc := clock.NewFakeClock()
	fc.SetWall(1_700_000_000_000)
	env := baseEnv()
	env.Clk = fc
	v, failed := evalRaw(t, `{"expr":"now()"}`, env)
	if failed || v != float64(1_700_000_000_000) {
		t.Fatalf("now trusted: got %#v failed=%v", v, failed)
	}
}

func TestEvaluate_NowUntrustedFloored(t *testing.T) {
	fc := clock.NewFakeClock()
	fc.SetWall(1_700_000_000_000) // an unverified wall reading
	env := baseEnv()
	env.Clk = fc
	env.TrustUntrusted = true
	env.ClockUntrustedFloor = 1_600_000_000_000
	v, failed := evalRaw(t, `{"expr":"now()"}`, env)
	if failed || v != float64(1_600_000_000_000) {
		t.Fatalf("now untrusted: expected the floor, got %#v failed=%v", v, failed)
	}
}

func TestEvaluate_Elapsed(t *testing.T) {
	fc := clock.NewFakeClock()
	fc.SetWall(10_000)
	env := baseEnv()
	env.Clk = fc
	// elapsed(4000ms) with now=10000ms -> 6 seconds.
	v, failed := evalRaw(t, `{"expr":"4000 | elapsed"}`, env)
	if failed || v.(float64) != 6 {
		t.Fatalf("elapsed: got %#v failed=%v want 6", v, failed)
	}
}

// --- fail-closed propagation and default as the sole container ---

func TestEvaluate_FailClosedPropagates(t *testing.T) {
	env := baseEnv()
	// A filter applied after a failed stage stays failed.
	if _, failed := evalRaw(t, `{"expr":"state('01J0000000000000000000000X') | upper"}`, env); !failed {
		t.Fatalf("failure must propagate through a downstream filter")
	}
}

func TestEvaluate_DefaultContainsMultiStageFailure(t *testing.T) {
	env := baseEnv()
	// The source fails, upper propagates the failure, default contains it.
	v, failed := evalRaw(t, `{"expr":"state('01J0000000000000000000000X') | upper | default('x')"}`, env)
	if failed || v != "x" {
		t.Fatalf("default must contain an upstream (multi-stage) failure: got %#v failed=%v", v, failed)
	}
}

func TestEvaluate_DefaultNullAbsent(t *testing.T) {
	env := baseEnv()
	// A null (not a failure) upstream value also triggers the fallback.
	v, failed := evalRaw(t, `{"expr":"null | default('fb')"}`, env)
	if failed || v != "fb" {
		t.Fatalf("default on null: got %#v failed=%v", v, failed)
	}
	// A present, non-null value passes through unchanged.
	v, failed = evalRaw(t, `{"expr":"'keep' | default('fb')"}`, env)
	if failed || v != "keep" {
		t.Fatalf("default on present value: got %#v failed=%v", v, failed)
	}
}
