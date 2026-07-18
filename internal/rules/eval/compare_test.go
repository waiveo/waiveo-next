package eval

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// TestTypedEqualString covers RUL-263 (exact, case-sensitive) and RUL-260
// (no coercion: a non-string value never equals a string, regardless of its
// "obvious" numeric reading).
func TestTypedEqualString(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"exact match", "on", "on", true},
		{"case mismatch", "On", "on", false},
		{"different strings", "on", "off", false},
		{"int refuses to equal its string form", 1, "1", false},
		{"string refuses to equal an int", "1", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TypedEqual(tt.a, tt.b, registry.String); got != tt.want {
				t.Errorf("TypedEqual(%#v, %#v, String) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestTypedEqualNumber covers RUL-270's parsing rule feeding numeric typed
// comparison: both sides are parsed per ParseNumber, so a JSON number and a
// grammar-conforming numeric string compare equal, but a non-conforming
// string or a non-numeric value fails closed to false rather than erroring.
func TestTypedEqualNumber(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"json number equals itself", float64(1), float64(1), true},
		{"int and float64 of same value", int(1), float64(1), true},
		{"json number equals matching numeric string", 42.5, "42.5", true},
		{"non-conforming string never matches", "1e3", 1000.0, false},
		{"boolean is not a number", true, float64(1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TypedEqual(tt.a, tt.b, registry.Number); got != tt.want {
				t.Errorf("TypedEqual(%#v, %#v, Number) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestTypedEqualBoolean covers no-coercion for booleans: "true" (a string)
// never equals true (a bool).
func TestTypedEqualBoolean(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"both true", true, true, true},
		{"both false", false, false, true},
		{"true vs false", true, false, false},
		{"string never coerces to boolean", "true", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TypedEqual(tt.a, tt.b, registry.Boolean); got != tt.want {
				t.Errorf("TypedEqual(%#v, %#v, Boolean) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestTypedEqualUnknownAlwaysFalse covers RUL-262: a comparison against an
// Unknown-typed value (no registry declaration) evaluates as not-matching,
// never an error, even when the two values would otherwise look equal.
func TestTypedEqualUnknownAlwaysFalse(t *testing.T) {
	if TypedEqual("same", "same", registry.Unknown) {
		t.Error("TypedEqual(\"same\", \"same\", Unknown) = true, want false (RUL-262 fail-closed)")
	}
	if TypedEqual(1.0, 1.0, registry.Unknown) {
		t.Error("TypedEqual(1.0, 1.0, Unknown) = true, want false (RUL-262 fail-closed)")
	}
}
