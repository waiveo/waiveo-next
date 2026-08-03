package packs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

const hourMsForTest = int64(3_600_000)

// holdboundary_test.go pins the instant a staged-rollout hold ends.
//
// CHI-030's hold is evaluated at RESOLUTION time against an injected clock, so
// the whole rule is one comparison — and a boundary sweep found that comparison
// unheld. Off by one here is not cosmetic: a publisher who staged a rollout for
// N hours has an artifact that becomes installable N hours after publication,
// and a resolver holding it a millisecond longer is a resolver whose own
// eligibility window disagrees with the registry's.

// resolveWithEntry publishes one entry carrying `extra` and resolves it at
// fixedNow, returning the refusal (or nil).
func resolveWithEntry(t *testing.T, extra map[string]any) error {
	t.Helper()
	st := openStore(t)
	reg := newRegistry(t, "fixture")
	src := reg.source()
	in, signer := marketInstaller(t, st, src)

	reg.publish("acme/menu-board", "1.0.0", versionedPack(t, signer, "1.0.0"), extra)
	reg.point("acme/menu-board", "community", "1.0.0")
	reg.reindex()

	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	return err
}

// TestStagedRolloutHoldEndsExactlyWhenItSaysItDoes drives the millisecond either
// side of the hold's expiry, and the expiry itself.
func TestStagedRolloutHoldEndsExactlyWhenItSaysItDoes(t *testing.T) {
	const hold = 24
	eligibleAfter := int64(hold) * hourMsForTest

	for _, tc := range []struct {
		name        string
		publishedAt int64
		wantHeld    bool
	}{
		{"one ms before the hold expires", fixedNow - eligibleAfter + 1, true},
		{"exactly at the hold's expiry", fixedNow - eligibleAfter, false},
		{"one ms after the hold expires", fixedNow - eligibleAfter - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveWithEntry(t, map[string]any{"hold_hours": hold, "published_at": tc.publishedAt})
			held := err != nil && strings.Contains(err.Error(), "hold")
			if held != tc.wantHeld {
				t.Errorf("held=%v at %s, want %v (err %v) — a hold that outlasts its own stated duration puts this "+
					"resolver's eligibility window out of step with the registry that published it",
					held, tc.name, tc.wantHeld, err)
			}
		})
	}
}

// TestAZeroHourHoldHoldsNothingEvenForAFutureDatedEntry pins the `HoldHours > 0`
// guard, which is the difference between "no hold declared" and "a hold of zero
// length".
//
// The distinction only becomes visible on an entry whose published_at is in the
// FUTURE: with no hold there is nothing to compare against and the entry
// resolves, while a rule that computed an eligibility instant regardless would
// hold it until its own publication date arrived. A clock-skewed or
// deliberately future-dated publisher would otherwise be unable to ship at all.
func TestAZeroHourHoldHoldsNothingEvenForAFutureDatedEntry(t *testing.T) {
	err := resolveWithEntry(t, map[string]any{"hold_hours": 0, "published_at": fixedNow + hourMsForTest})
	if err != nil && strings.Contains(err.Error(), "hold") {
		t.Errorf("an entry declaring NO hold was held because its published_at is ahead of the clock: %v", err)
	}

	// The control: the same future-dated entry WITH a hold really is held, so the
	// case above is about the zero rather than about future dating being ignored.
	err = resolveWithEntry(t, map[string]any{"hold_hours": 1, "published_at": fixedNow + hourMsForTest})
	if err == nil || !strings.Contains(err.Error(), "hold") {
		t.Errorf("a future-dated entry declaring a 1-hour hold was not held: %v", err)
	}
}
