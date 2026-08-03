package events

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

// boundaries_test.go pins values exactly ON a limit.
//
// These came out of a mutation sweep that flips comparisons (> to >=, <= to <)
// rather than disabling guards. Disabling asks "is the rule enforced at all";
// flipping asks "is its boundary right" — the off-by-one an inclusive/exclusive
// mix-up produces. Every case below survived that flip, which means nothing
// exercised the value at the limit, which is exactly where limits are got wrong.

// TestEmptySchemaRestrictionImposesNone pins EVT-124's "an empty or nil slice
// imposes none".
//
// NewFilter builds its lookup set only when schemas is non-empty, and that
// emptiness check is the whole rule: build the set unconditionally and an empty
// restriction becomes an empty ALLOWLIST, so a subscriber that asked for no
// schema restriction receives NOTHING. The wrong behaviour is total silence on a
// healthy connection, which is the hardest kind of fault to attribute.
func TestEmptySchemaRestrictionImposesNone(t *testing.T) {
	all := func(string) bool { return true }
	env := Envelope{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", Schema: SchemaAutomationRun, ScopeNode: "01J8Z5A0B1C2D3E4F5G6H7Z5A0"}

	for _, tc := range []struct {
		name    string
		schemas []string
	}{
		{"nil", nil},
		{"empty", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFilter(all, apiselector.Selector{}, nil, tc.schemas)
			if !f.Allows(env) {
				t.Errorf("a subscriber with a %s schema restriction was sent nothing — an empty restriction is "+
					"NO restriction (EVT-124), and the failure mode is a silent stream on a healthy connection", tc.name)
			}
		})
	}

	// The control: a NON-empty restriction really does restrict, so the rule
	// above is about emptiness rather than about the filter never applying.
	f := NewFilter(all, apiselector.Selector{}, nil, []string{SchemaContentPlayed})
	if f.Allows(env) {
		t.Error("a schema restriction naming another schema still admitted this event")
	}
	if !f.Allows(Envelope{ID: env.ID, Schema: SchemaContentPlayed, ScopeNode: env.ScopeNode}) {
		t.Error("a schema restriction refused the very schema it names")
	}
}

// TestRotationOverlapIsInclusiveAtItsDeadline pins EVT-158's window boundary.
//
// inRotationOverlap is, by its own doc, "the one place that boundary is
// evaluated, so emission and acceptance can never disagree about when the prior
// secret stops working" — which makes the exact instant load-bearing on both
// sides at once. The window is INCLUSIVE: at exactly rotationOverlapMs after the
// rotation the prior secret still works, and one millisecond later it does not.
func TestRotationOverlapIsInclusiveAtItsDeadline(t *testing.T) {
	const overlap = 60_000
	const rotatedAt = 1_752_537_600_000

	e := NewEndpointState("secret-old", 5, 1_000, 60_000, overlap)
	e.RotateSecret("secret-new", rotatedAt)

	for _, tc := range []struct {
		name string
		now  int64
		want bool
	}{
		{"the instant of rotation", rotatedAt, true},
		{"one ms inside the window", rotatedAt + overlap - 1, true},
		{"exactly at the deadline", rotatedAt + overlap, true},
		{"one ms past the deadline", rotatedAt + overlap + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.inRotationOverlap(tc.now); got != tc.want {
				t.Errorf("inRotationOverlap at %s = %v, want %v — emission and acceptance both read this, so an "+
					"off-by-one here drops a receiver's prior-secret verification a millisecond early on both sides",
					tc.name, got, tc.want)
			}
		})
	}

	// And before any rotation there is no overlap at all, whatever the clock says.
	fresh := NewEndpointState("only-secret", 5, 1_000, 60_000, overlap)
	if fresh.inRotationOverlap(rotatedAt + 1) {
		t.Error("an endpoint that never rotated reported itself inside a rotation overlap")
	}
}

// TestBackoffDoublesPerAttemptAndFlattensAtTheCap pins the loop bound in
// NextBackoffMs, which decides how many times the base is doubled for a given
// attempt number. A loop that runs one iteration too many or too few produces a
// schedule that is wrong from the second retry onward, and nothing about the
// shape looks wrong.
func TestBackoffDoublesPerAttemptAndFlattensAtTheCap(t *testing.T) {
	const base, cap = int64(1_000), int64(8_000)
	e := NewEndpointState("secret", 5, base, cap, 60_000)

	for _, tc := range []struct {
		attempt int
		want    int64
	}{
		{1, 1_000}, // the first retry is the base, undoubled
		{2, 2_000},
		{3, 4_000},
		{4, 8_000},  // reaches the cap exactly
		{5, 8_000},  // and flattens there
		{50, 8_000}, // still flat, however far it runs
	} {
		if got := e.NextBackoffMs(tc.attempt); got != tc.want {
			t.Errorf("NextBackoffMs(%d) = %d, want %d", tc.attempt, got, tc.want)
		}
	}

	// Attempt numbering is 1-based; anything below that is clamped rather than
	// producing a shorter-than-base delay.
	for _, attempt := range []int{0, -1} {
		if got := e.NextBackoffMs(attempt); got != base {
			t.Errorf("NextBackoffMs(%d) = %d, want the base %d", attempt, got, base)
		}
	}
}

// Four comparisons in this file's subjects survive a boundary flip and are
// EQUIVALENT rather than unheld. Verified by flipping each and running the
// package, and recorded so the next sweep does not re-open them:
//
//   - NewFilter's `len(schemas) > 0`. Allows re-checks `len(f.schemas) > 0`
//     before consulting the set, so an empty non-nil map behaves exactly as a
//     nil one and building it unconditionally changes nothing observable.
//   - NextBackoffMs's `attempt < 1`. Clamping at 1 versus at 1-or-below is the
//     same clamp: attempt 1 is already 1.
//   - Its loop guard `backoff < cap`. Doubling once more at exactly the cap is
//     undone by the clamp on the next line.
//   - That clamp's own `backoff > cap`. Assigning cap to a value already equal
//     to cap is a no-op.
//
// The tests above still pin what those lines are FOR — EVT-124's empty
// restriction, 1-based attempt numbering, and the doubling schedule — they
// simply cannot distinguish these particular rewrites, because nothing can.
