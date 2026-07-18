package schedule

import (
	"reflect"
	"testing"
)

// TestApplyMisfireSkip: the default/explicit `skip` policy drops every missed
// occurrence — none fire late (RUL-351/354). An absent policy ("") resolves to
// skip.
func TestApplyMisfireSkip(t *testing.T) {
	for _, policy := range []string{"", "skip"} {
		if got := ApplyMisfire(policy, []int64{7000, 8000, 9000}); got != nil {
			t.Fatalf("ApplyMisfire(%q, 3 missed) = %+v, want nil (skip drops all)", policy, got)
		}
	}
}

// TestApplyMisfireCatchUpOnceCollapses: `catch_up_once` collapses any number of
// missed occurrences into exactly one fire marked misfire_caught, at the most
// recent missed instant (RUL-352).
func TestApplyMisfireCatchUpOnceCollapses(t *testing.T) {
	got := ApplyMisfire("catch_up_once", []int64{7000, 8000, 9000})
	want := []Fire{{Wall: 9000, MisfireCaught: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ApplyMisfire(catch_up_once, [7000,8000,9000]) = %+v, want %+v", got, want)
	}
	// A single missed occurrence still collapses to exactly one caught fire.
	if got := ApplyMisfire("catch_up_once", []int64{5000}); !reflect.DeepEqual(got, []Fire{{Wall: 5000, MisfireCaught: true}}) {
		t.Fatalf("ApplyMisfire(catch_up_once, [5000]) = %+v, want one caught fire at 5000", got)
	}
}

// TestApplyMisfireFireEachChronological: `fire_each` produces one caught fire per
// missed occurrence, in ascending chronological order (RUL-353).
func TestApplyMisfireFireEachChronological(t *testing.T) {
	got := ApplyMisfire("fire_each", []int64{7000, 8000, 9000})
	want := []Fire{
		{Wall: 7000, MisfireCaught: true},
		{Wall: 8000, MisfireCaught: true},
		{Wall: 9000, MisfireCaught: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ApplyMisfire(fire_each, ...) = %+v, want %+v", got, want)
	}
	// Out-of-order input is normalized to chronological order (RUL-353).
	if got := ApplyMisfire("fire_each", []int64{9000, 7000, 8000}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ApplyMisfire(fire_each, unordered) = %+v, want ascending %+v", got, want)
	}
}

// TestApplyMisfireNoMissed: an empty missed set never fires under any policy.
func TestApplyMisfireNoMissed(t *testing.T) {
	for _, policy := range []string{"", "skip", "catch_up_once", "fire_each"} {
		if got := ApplyMisfire(policy, nil); got != nil {
			t.Fatalf("ApplyMisfire(%q, nil) = %+v, want nil", policy, got)
		}
	}
}

// TestApplyMisfireUnknownFailsClosed: an unrecognized policy (compile rejects it
// as MISFIRE_INVALID before it reaches here) fires nothing — no-fire beats
// mis-fire (RUL-350, fail-closed).
func TestApplyMisfireUnknownFailsClosed(t *testing.T) {
	if got := ApplyMisfire("bogus", []int64{1, 2, 3}); got != nil {
		t.Fatalf("ApplyMisfire(bogus, ...) = %+v, want nil (fail-closed)", got)
	}
}

// TestValidMisfire: exactly the three declared policies are valid; an absent ("")
// or unknown value is not (the MISFIRE_INVALID predicate, RUL-350).
func TestValidMisfire(t *testing.T) {
	for _, ok := range []string{"skip", "catch_up_once", "fire_each"} {
		if !ValidMisfire(ok) {
			t.Fatalf("ValidMisfire(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "SKIP", "catch_up", "fire", "always"} {
		if ValidMisfire(bad) {
			t.Fatalf("ValidMisfire(%q) = true, want false", bad)
		}
	}
}
