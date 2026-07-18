package eval

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
)

// TestBoundedHoldSuppressesFlap replays the RUL-024-for-hold-suppresses-flap
// corpus case exactly (trigger to:["on"], for:30 → 30000ms of monotonic time)
// against a FakeClock: a brief bounce into the target that reverts before the
// hold elapses never fires; a later occurrence that holds continuously for the
// full duration fires once (RUL-024/310).
func TestBoundedHoldSuppressesFlap(t *testing.T) {
	clk := clock.NewFakeClock()
	h := NewHold(BoundedHold, 30) // for: 30 seconds

	// seq 1, t=0: off->on. The "on" level begins holding; the hold arms.
	clk.SetWall(0)
	if fired := h.Step(clk, true); fired {
		t.Fatalf("seq1 (arm at t=0): fired=true, want false")
	}

	// seq 2, t=5000: on->off. The level stops holding at 5000ms elapsed, short
	// of the 30000ms requirement; the pending fire is discarded (RUL-024).
	clk.Advance(5000)
	if fired := h.Step(clk, false); fired {
		t.Fatalf("seq2 (level drops at t=5000): fired=true, want false")
	}

	// seq 3, t=40000: off->on. The level holds again; the hold restarts from
	// zero at t=40000.
	clk.Advance(35000) // 5000 -> 40000
	if fired := h.Step(clk, true); fired {
		t.Fatalf("seq3 (re-arm at t=40000): fired=true, want false")
	}

	// A tick 1ms short of the full duration must NOT fire.
	clk.Advance(29999) // 40000 -> 69999
	if fired := h.Step(clk, true); fired {
		t.Fatalf("t=69999 (29999ms held): fired=true, want false (1ms short)")
	}

	// seq 4, t=70000: on has held continuously since t=40000 (30000ms of
	// monotonic time), satisfying for:30 → fires exactly once.
	clk.Advance(1) // 69999 -> 70000
	if fired := h.Step(clk, true); !fired {
		t.Fatalf("seq4 (30000ms held at t=70000): fired=false, want true")
	}

	// A further tick while the level still holds must NOT re-fire (one fire
	// per continuous hold).
	clk.Advance(10000)
	if fired := h.Step(clk, true); fired {
		t.Fatalf("t=80000 (still held after firing): fired=true, want false (no re-fire)")
	}
}

// TestBoundedHoldSuspendFlapDebounceSemanticExpansion replays the
// RUL-321-suspend-flap-for-debounce corpus case (trigger to:"on", for:20 →
// 20000ms) at the Hold layer: the rawMatched booleans below are the
// semantic-group expansion outcomes the corpus's per-seq "matched" fields
// assert (group expansion itself is covered by RUL-321-scalar-expands-array-exact
// in the matcher tests). Two brief bounces into an on-group member are each
// discarded; only the third occurrence, which holds continuously for the full
// 20000ms, dispatches.
func TestBoundedHoldSuspendFlapDebounceSemanticExpansion(t *testing.T) {
	clk := clock.NewFakeClock()
	h := NewHold(BoundedHold, 20)

	// seq 1, t=0: off->suspend, matched=true (suspend in on-group). Arm.
	if fired := h.Step(clk, true); fired {
		t.Fatalf("seq1: fired=true, want false")
	}
	// seq 2, t=3000: suspend->off, matched=false. Hold broke at 3000ms.
	clk.Advance(3000)
	if fired := h.Step(clk, false); fired {
		t.Fatalf("seq2 (broke at 3000ms): fired=true, want false")
	}
	// seq 3, t=4000: off->suspend, matched=true. Re-arm from zero.
	clk.Advance(1000)
	if fired := h.Step(clk, true); fired {
		t.Fatalf("seq3 (re-arm at t=4000): fired=true, want false")
	}
	// seq 4, t=6500: suspend->off, matched=false. Broke at 2500ms.
	clk.Advance(2500)
	if fired := h.Step(clk, false); fired {
		t.Fatalf("seq4 (broke at 2500ms): fired=true, want false")
	}
	// seq 5, t=7000: off->on, matched=true (literal). Re-arm from zero.
	clk.Advance(500)
	if fired := h.Step(clk, true); fired {
		t.Fatalf("seq5 (re-arm at t=7000): fired=true, want false")
	}
	// seq 6, t=27000: on held continuously since t=7000 (20000ms) → fires,
	// the only dispatch across the whole sequence.
	clk.Advance(20000)
	if fired := h.Step(clk, true); !fired {
		t.Fatalf("seq6 (20000ms held at t=27000): fired=false, want true")
	}
}

// TestBoundedHoldEngineRestartResetsHold replays the
// RUL-360-engine-restart-resets-hold corpus case (to:["unreachable"], for:300):
// an engine restart mid-hold zeroes the in-progress hold (no elapsed carried
// over, RUL-360/361), and a post-restart observation that merely reconfirms the
// durable state (not a fresh transition, RUL-025/300) does NOT restart the hold.
func TestBoundedHoldEngineRestartResetsHold(t *testing.T) {
	clk := clock.NewFakeClock()
	h := NewHold(BoundedHold, 300) // 300000ms

	// seq 1, t=0: on->unreachable. Hold arms, counting toward 300000ms.
	if fired := h.Step(clk, true); fired {
		t.Fatalf("seq1 (arm at t=0): fired=true, want false")
	}

	// seq 2, t=120000: engine restarts with 120000ms already elapsed. Reset()
	// zeroes the in-progress hold; no elapsed time is carried over.
	clk.Advance(120000)
	h.Reset()
	// The engine re-seeds the hold's held-level knowledge from the durable
	// state (still "unreachable" → the target level is currently satisfied) so
	// that a bare reconfirmation is not mistaken for a fresh entry.
	h.Seed(true)

	// seq 3, t=121000: the post-restart observation reconfirms "unreachable".
	// Per RUL-025/300 this is not a fresh transition, so the hold does not
	// begin counting again here.
	clk.Advance(1000)
	if fired := h.Step(clk, true); fired {
		t.Fatalf("seq3 (reconfirmation at t=121000): fired=true, want false (hold_restarted=false)")
	}

	// Even far past the original deadline, the reconfirmed-but-not-re-entered
	// level never fires: it will begin only on a genuine fresh transition.
	clk.Advance(400000) // t=521000, well past 121000+300000
	if fired := h.Step(clk, true); fired {
		t.Fatalf("t=521000 (still only reconfirmed): fired=true, want false")
	}

	// A genuine fresh transition after restart (level leaves the target and
	// re-enters) arms a fresh hold that counts from zero, NOT seeded by any
	// pre-restart elapsed time (RUL-360).
	clk.Advance(1000) // t=522000: level leaves the target
	h.Step(clk, false)
	clk.Advance(1000) // t=523000: fresh transition back into the target
	if fired := h.Step(clk, true); fired {
		t.Fatalf("fresh transition at t=523000: fired=true, want false (just armed)")
	}
	clk.Advance(299999) // t=822999: 1ms short of 300000
	if fired := h.Step(clk, true); fired {
		t.Fatalf("t=822999 (299999ms held): fired=true, want false")
	}
	clk.Advance(1) // t=823000: full 300000ms since the fresh transition
	if fired := h.Step(clk, true); !fired {
		t.Fatalf("t=823000 (300000ms held since fresh transition): fired=false, want true")
	}
}

// TestResetDiscardsInProgressHold checks Reset() zeroes an armed-but-not-yet-
// fired hold: after a reset, the already-held level (fed as still-matched) is
// not treated as a fresh entry, and only a genuine re-entry re-arms from zero
// (RUL-360).
func TestResetDiscardsInProgressHold(t *testing.T) {
	clk := clock.NewFakeClock()
	h := NewHold(BoundedHold, 10) // 10000ms

	h.Step(clk, true) // arm at t=0
	clk.Advance(9000)
	h.Reset()
	h.Seed(true) // durable level still satisfied

	// A reconfirmation right after reset must not fire even once the original
	// 10000ms window would have elapsed.
	clk.Advance(2000) // t=11000, past the original 10000ms deadline
	if fired := h.Step(clk, true); fired {
		t.Fatalf("post-reset reconfirmation: fired=true, want false")
	}
}

// TestBoundedHoldForZeroFiresOnTransition checks that for:0 (RUL-024: behaves
// as if for were absent) fires immediately on the matching transition, with no
// hold.
func TestBoundedHoldForZeroFiresOnTransition(t *testing.T) {
	clk := clock.NewFakeClock()
	h := NewHold(BoundedHold, 0)

	if fired := h.Step(clk, true); !fired {
		t.Fatalf("for:0 rising edge: fired=false, want true (fires immediately)")
	}
	// The same held level must not re-fire without a fresh transition.
	clk.Advance(5000)
	if fired := h.Step(clk, true); fired {
		t.Fatalf("for:0 still held: fired=true, want false (no re-fire)")
	}
	// A fresh transition fires again.
	h.Step(clk, false)
	if fired := h.Step(clk, true); !fired {
		t.Fatalf("for:0 fresh transition: fired=false, want true")
	}
}

// TestDebounceHoldReArmsOnEachChange checks the unscoped-state-trigger debounce
// semantics (RUL-026): once a qualifying change occurs the trigger fires only
// if no further qualifying change arrives for the full monotonic span; each new
// change restarts the window from that change.
func TestDebounceHoldReArmsOnEachChange(t *testing.T) {
	clk := clock.NewFakeClock()
	h := NewHold(DebounceHold, 5) // 5000ms settle window

	// A change starts the window; a quiet tick before it elapses does not fire.
	if fired := h.Step(clk, true); fired { // t=0: change, arm
		t.Fatalf("t=0 change: fired=true, want false")
	}
	clk.Advance(3000)
	if fired := h.Step(clk, false); fired { // t=3000: quiet, 3000<5000
		t.Fatalf("t=3000 quiet (3000<5000): fired=true, want false")
	}

	// A further change before the window elapses re-arms from that change.
	clk.Advance(1000)
	if fired := h.Step(clk, true); fired { // t=4000: change, re-arm
		t.Fatalf("t=4000 change (re-arm): fired=true, want false")
	}
	// 4999ms after the re-arm: still short.
	clk.Advance(4999)
	if fired := h.Step(clk, false); fired { // t=8999
		t.Fatalf("t=8999 quiet (4999<5000 since re-arm): fired=true, want false")
	}
	// Full 5000ms of quiet since the last change → fires once.
	clk.Advance(1)
	if fired := h.Step(clk, false); !fired { // t=9000
		t.Fatalf("t=9000 quiet (5000ms since re-arm): fired=false, want true")
	}
	// It does not fire again while quiet continues (settled).
	clk.Advance(10000)
	if fired := h.Step(clk, false); fired {
		t.Fatalf("t=19000 still quiet after firing: fired=true, want false")
	}
}

// TestDebounceHoldForZeroFiresImmediately checks that for:0 on an unscoped
// trigger fires immediately on the qualifying change (RUL-024: for:0 as if
// absent), with no settle window.
func TestDebounceHoldForZeroFiresImmediately(t *testing.T) {
	clk := clock.NewFakeClock()
	h := NewHold(DebounceHold, 0)
	if fired := h.Step(clk, true); !fired {
		t.Fatalf("debounce for:0 change: fired=false, want true")
	}
	clk.Advance(1000)
	if fired := h.Step(clk, false); fired {
		t.Fatalf("debounce for:0 quiet: fired=true, want false")
	}
}

// TestHoldMonotonicOnly checks a hold reads only clock.Mono(): a wall-clock
// jump (SetWall) mid-hold neither shortens nor extends it (RUL-024/361).
func TestHoldMonotonicOnly(t *testing.T) {
	clk := clock.NewFakeClock()
	h := NewHold(BoundedHold, 10) // 10000ms

	h.Step(clk, true) // arm at mono=0
	// A large wall-clock jump forward must not satisfy the monotonic hold.
	clk.SetWall(1_000_000)
	clk.Advance(9999) // mono=9999, 1ms short
	if fired := h.Step(clk, true); fired {
		t.Fatalf("wall jumped but mono=9999: fired=true, want false")
	}
	// A wall-clock jump backward must not extend it either; only mono matters.
	clk.SetWall(0)
	clk.Advance(1) // mono=10000
	if fired := h.Step(clk, true); !fired {
		t.Fatalf("mono=10000 reached: fired=false, want true (wall-clock irrelevant)")
	}
}
