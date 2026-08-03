package auth

import (
	"testing"
)

// timingboundaries_test.go pins the instants these two controls turn on and off.
//
// Every case here survived a boundary-mutation sweep — the operator that flips
// `<` to `<=` rather than disabling a guard. Both subjects are decided ENTIRELY
// by a comparison against a clock, so an off-by-one is the only way they can be
// wrong, and it is the one thing nothing was checking.

// TestClockFloorAdvancesOnlyOnAStrictlyHigherReading pins SEC-066's "a lower
// verified reading is simply not news" — and its equal case, which is the part
// a comparison flip moves.
//
// Advance REPORTS whether it advanced, and the report is not cosmetic: a caller
// acts on it. Treating an equal reading as an advance would claim progress that
// did not happen and persist a file that did not change, on every repeat of the
// same verified timestamp.
func TestClockFloorAdvancesOnlyOnAStrictlyHigherReading(t *testing.T) {
	const base = int64(1_752_537_600_000)
	c, err := OpenClockFloor(t.TempDir(), func() int64 { return base })
	if err != nil {
		t.Fatalf("OpenClockFloor: %v", err)
	}

	if advanced, err := c.Advance(base, TimeSourceVerifiable); err != nil || !advanced {
		t.Fatalf("first Advance = (%v, %v); want it to set the floor", advanced, err)
	}
	if got := c.FloorMs(); got != base {
		t.Fatalf("floor = %d, want %d", got, base)
	}

	for _, tc := range []struct {
		name string
		ts   int64
		want bool
	}{
		{"one ms below the floor", base - 1, false},
		{"exactly the floor", base, false},
		{"one ms above the floor", base + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			advanced, err := c.Advance(tc.ts, TimeSourceVerifiable)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if advanced != tc.want {
				t.Errorf("Advance(%s) reported advanced=%v, want %v — the floor moves on a STRICTLY higher "+
					"verified reading, and a repeat of the same instant is not news", tc.name, advanced, tc.want)
			}
		})
	}
	// The floor ends where the only real advance left it.
	if got := c.FloorMs(); got != base+1 {
		t.Errorf("floor = %d, want %d", got, base+1)
	}
}

// TestLockoutDurationCapAndOverflowGuard pins the two boundaries in the
// doubling, which together decide how long a credential stays locked.
//
// The cap is inclusive of maxMs: a computed duration of exactly maxMs is kept
// rather than clamped to it, which is the same value either way — so the case
// that matters is the one either side. The overflow guard is the interesting
// half: baseMs << shift on a large base wraps NEGATIVE, and a negative duration
// would set lockedTil BEHIND nowMs, unlocking the credential the instant it was
// meant to be locked.
func TestLockoutDurationCapAndOverflowGuard(t *testing.T) {
	const threshold = 3
	const now = int64(1_752_537_600_000)

	t.Run("doubling is capped at maxMs", func(t *testing.T) {
		l := NewLockout(threshold, 1_000, 4_000)
		var last int64
		for i := 0; i < threshold+6; i++ {
			last = l.Fail("k", now)
		}
		if last != 4_000 {
			t.Errorf("a long failure run locked for %d ms, want the %d ms cap", last, int64(4_000))
		}
	})

	t.Run("an overflowing doubling never yields a non-positive lock", func(t *testing.T) {
		// A base this large overflows int64 within the shift cap, which is what
		// the `dur <= 0` half of the guard exists for.
		l := NewLockout(threshold, 1<<60, 60_000)
		var dur int64
		for i := 0; i < threshold+8; i++ {
			dur = l.Fail("k", now)
		}
		if dur <= 0 {
			t.Fatalf("a failure run produced a lock of %d ms — a non-positive duration sets lockedTil BEHIND "+
				"nowMs, so the credential is unlocked at the instant it was meant to be locked", dur)
		}
		// And the credential really is locked afterwards.
		if locked, _ := l.Locked("k", now); !locked {
			t.Error("the credential is not locked after a failure run that should have locked it")
		}
	})
}

// TestLockoutEvictionTreatsAnExpiredLockAsEvictable pins the eviction boundary.
//
// evictOneLocked skips an entry whose lock is still resting on it. At exactly
// lockedTil the lock has LIFTED — Locked() says so, using `<=` — so eviction has
// to agree, or the two disagree about the same instant and a table under
// pressure keeps entries it is entitled to drop.
func TestLockoutEvictionTreatsAnExpiredLockAsEvictable(t *testing.T) {
	const now = int64(1_752_537_600_000)
	l := NewLockout(3, 1_000, 1_000)

	// Lock one key, then ask both questions at the instant the lock ends.
	for i := 0; i < 5; i++ {
		l.Fail("locked-key", now)
	}
	expiry := now + 1_000

	if locked, _ := l.Locked("locked-key", expiry); locked {
		t.Fatal("Locked() reports a lock still in force at exactly its expiry — the two boundaries must agree")
	}

	l.mu.Lock()
	evicted := l.evictOneLocked(expiry)
	l.mu.Unlock()
	if !evicted {
		t.Error("eviction refused an entry whose lock expired at exactly this instant, while Locked() reports it " +
			"unlocked — the two disagree about the same millisecond")
	}
}

// Five comparisons in these two subjects survive a boundary flip and are
// EQUIVALENT rather than unheld. Recorded so the next sweep does not re-open
// them, with the reason each is equivalent rather than merely unkilled:
//
//   - OpenClockFloor's `st.FloorMs < 0`. A persisted floor of exactly 0 is
//     indistinguishable from no floor: both leave floorMs at 0, which is what
//     rejecting it also does.
//   - Now's `floor > wall`. At equality the two branches return the same value.
//   - Fail's `shift > lockoutMaxShift` and `dur > l.maxMs`. Both clamp, so at
//     the boundary they assign a value to itself.
//   - evictOneLocked's `e.lastMs < victimAt`. This picks the least recently
//     seen entry; at a tie it chooses between entries of equal age, and Go's
//     map iteration already leaves that order unspecified.
//
// The tests above still pin what those lines are FOR — the cap, the overflow
// guard, and eviction agreeing with Locked() about the same instant — they
// simply cannot distinguish these rewrites, because nothing can.
