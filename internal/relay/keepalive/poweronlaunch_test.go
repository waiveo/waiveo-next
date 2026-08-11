package keepalive

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
)

// poweronlaunch_test.go covers rule 1b — POWER-ON AUTO-LAUNCH (parity row 5.6,
// the legacy stack's power-on auto-launch automation) — as a capability
// DISTINCT from the home-confirmation recovery it shares a state machine with.
//
// Every test here is written so that it FAILS if rule 1b is removed and, where
// the distinction matters, so that it CANNOT pass by rule 2 doing the work
// instead: the screen sits on a foreign app throughout, which rule 2 refuses
// outright (TestForeignAppNeverFires pins that in the other direction). The one
// exception is TestPowerOnLaunchOffLeavesTheHomeRecoveryIntact, whose whole
// point is that rule 2 still works with rule 1b switched off.

// TestPowerOnLaunchFiresAfterTheSettleDelayWhateverTheScreenResumedInto is the
// capability's central case, and the one that separates it from rule 2: a
// screen powers on straight back into the foreign app it was last watching —
// never home, never menu — and the channel is nonetheless foregrounded, once,
// after the settle delay.
//
// This is the field scenario legacy's auto-launch existed for. A Roku resumes
// its last app, so a signage screen switched off inside somebody's streaming
// session comes back up inside that session. Rule 2 will never touch it (the
// screen is not at Home) and, without this rule, the wall shows the wrong thing
// until a human walks past.
func TestPowerOnLaunchFiresAfterTheSettleDelayWhateverTheScreenResumedInto(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{
		Adopted:       adoptEverything,
		LaunchDelay:   1500 * time.Millisecond,
		Channel:       "waiveo",
		Controller:    fc,
		PowerOnLaunch: true,
	})
	t0 := time.Unix(1700000000, 0)

	// Off first, so the poll below is a genuine off -> PowerOn EDGE rather than
	// this screen's first-ever sighting.
	k.recordObservation(obsFor("scr1", "Ready", ""), t0)

	// The power-on itself, and two more polls inside the settle window. Nothing
	// may be dispatched yet: a launch issued into a still-booting Roku pends
	// without rendering, which is the whole reason the delay exists.
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(100*time.Millisecond))
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(600*time.Millisecond))
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(1100*time.Millisecond))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls inside the settle window = %d, want 0 (%+v)", got, fc.calls)
	}

	// Past the delay: fires on the first qualifying poll, with no Home streak
	// anywhere in this sequence.
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(1700*time.Millisecond))
	calls := fc.callsFor("scr1")
	if len(calls) != 1 {
		t.Fatalf("launch calls after the settle delay = %d, want exactly 1 (%+v)", len(calls), calls)
	}
	if calls[0].command != launchCommand || calls[0].channel != "waiveo" {
		t.Errorf("dispatched call = %+v, want command=%q channel=waiveo", calls[0], launchCommand)
	}

	// The edge is consumed: every further poll of the SAME power-on is silent,
	// however long the screen stays on the foreign app.
	for _, d := range []time.Duration{2 * time.Second, 5 * time.Second, time.Minute} {
		k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(d))
	}
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls after the edge was consumed = %d, want still exactly 1 (%+v)", got, fc.calls)
	}
}

// TestPowerOnLaunchIsOffByDefaultAtTheLibraryBoundary pins Config.PowerOnLaunch's
// documented zero value. It is what lets every pre-existing test in this package
// keep asserting rule 2's behaviour unchanged, and it is why production has to
// opt in explicitly (cmd/waiveo-relay does, by default — see config.powerOnLaunchOn).
func TestPowerOnLaunchIsOffByDefaultAtTheLibraryBoundary(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{Adopted: adoptEverything, LaunchDelay: time.Millisecond, Controller: fc})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "Ready", ""), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(2*time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls with PowerOnLaunch unset = %d, want 0", got)
	}
}

// TestPowerOnLaunchOffLeavesTheHomeRecoveryIntact is the other half of the
// switch: disabling rule 1b must not disable rule 2. An operator who turns the
// power-on launch off because people share these TVs still gets a screen left
// stranded at Home recovered.
func TestPowerOnLaunchOffLeavesTheHomeRecoveryIntact(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{Adopted: adoptEverything, LaunchDelay: time.Millisecond, Controller: fc, PowerOnLaunch: false})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("home-recovery launches with rule 1b off = %d, want exactly 1", got)
	}
}

// TestPowerOnLaunchRespectsTheAdoptionGate: reachability is not permission, and
// rule 1b is evaluated after the same fail-closed gate every other rule is. A
// screen this deployment has not adopted — the legacy stack's, during
// coexistence — must not have a channel foregrounded onto it when somebody
// switches it on.
func TestPowerOnLaunchRespectsTheAdoptionGate(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{
		Adopted:       func(id string) bool { return id == "mine" },
		LaunchDelay:   time.Millisecond,
		Controller:    fc,
		PowerOnLaunch: true,
	})
	t0 := time.Unix(1700000000, 0)

	for _, id := range []string{"mine", "theirs"} {
		k.recordObservation(obsFor(id, "Ready", ""), t0)
		k.recordObservation(obsFor(id, "PowerOn", "app"), t0.Add(time.Second))
		k.recordObservation(obsFor(id, "PowerOn", "app"), t0.Add(2*time.Second))
	}
	if got := len(fc.callsFor("mine")); got != 1 {
		t.Errorf("launch calls for the ADOPTED screen = %d, want 1", got)
	}
	if got := len(fc.callsFor("theirs")); got != 0 {
		t.Errorf("launch calls for the UN-ADOPTED screen = %d, want 0 (%+v)", got, fc.calls)
	}
}

// TestPowerOnLaunchNeverWakesAStandbyScreen: rule 3 dominates rule 1b exactly as
// it dominates rule 2. A screen reporting anything other than PowerOn — a
// recognized low-power value, an unrecognized one, or an absent attribute from
// an unreachable device — is never commanded, so the auto-launch can never
// itself become the thing that wakes a TV somebody switched off.
func TestPowerOnLaunchNeverWakesAStandbyScreen(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{Adopted: adoptEverything, LaunchDelay: time.Millisecond, Controller: fc, PowerOnLaunch: true})
	t0 := time.Unix(1700000000, 0)

	for i, mode := range []string{"Ready", "DisplayOff", "Headless", "", "Ready"} {
		k.recordObservation(obsFor("scr1", mode, "app"), t0.Add(time.Duration(i)*time.Second))
	}
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls while never PowerOn = %d, want 0 (%+v)", got, fc.calls)
	}
}

// TestPowerOnLaunchIsSuppressedByABlankActiveLeaseAndConsumesTheEdge pins both
// halves of rule 1b's PLY-155 interaction.
//
// Suppression: a screen the schedule has deliberately blanked (a closed-hours
// daypart, DAT-116) must not have a channel foregrounded onto it just because
// the TV was switched on.
//
// Consumption: once the blank lifts, the SUPPRESSED edge must not fire
// retroactively. Firing then would foreground over whatever the screen is
// showing at an arbitrary later instant — hours after the power-on that
// nominally justified it — which is the foreground theft rule 2's home gate
// exists to avoid. Only a NEW power-on gets a new auto-launch, which the last
// leg of this test proves by driving one.
func TestPowerOnLaunchIsSuppressedByABlankActiveLeaseAndConsumesTheEdge(t *testing.T) {
	fc := &fakeController{}
	var blank int32 = 1 // atomic bool: 1 = the active Lease is blank
	k := New(Config{
		Adopted:       adoptEverything,
		LaunchDelay:   time.Millisecond,
		Controller:    fc,
		PowerOnLaunch: true,
		ActiveDisplay: func(string) string {
			if atomic.LoadInt32(&blank) == 1 {
				return playerserver.DisplayBlank
			}
			return playerserver.DisplayContent
		},
	})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "Ready", ""), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(2*time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls while the active Lease is blank = %d, want 0 (PLY-155)", got)
	}

	// The blank lifts. The already-consumed edge must stay consumed.
	atomic.StoreInt32(&blank, 0)
	for _, d := range []time.Duration{3 * time.Second, 4 * time.Second, time.Hour} {
		k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(d))
	}
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls after the suppression lifted = %d, want 0 (a suppressed power-on edge is consumed, not deferred)", got)
	}

	// A genuine new power-on cycle DOES get its own auto-launch — the edge is
	// consumed per power-on, not per screen for the process's lifetime.
	k.recordObservation(obsFor("scr1", "Ready", ""), t0.Add(2*time.Hour))
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(2*time.Hour+time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(2*time.Hour+2*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls after a SECOND power-on cycle = %d, want exactly 1", got)
	}
}

// TestPowerOnLaunchDoesNotDisarmTheHomeRecovery is the "the two rules must not
// fight" assertion, stated as the failure that matters: a power-on launch the
// device PENDED (the settle delay reduces that risk, it does not eliminate it)
// leaves the screen sitting at Home, and the home-confirmation recovery must
// still fire for it.
//
// It fails if rule 1b borrows rule 2's per-streak `launched` cooldown — the
// tempting way to suppress the duplicate launch, which would silently switch
// self-healing off for the whole streak that follows a power-on.
func TestPowerOnLaunchDoesNotDisarmTheHomeRecovery(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{Adopted: adoptEverything, LaunchDelay: time.Millisecond, Controller: fc, PowerOnLaunch: true})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "Ready", ""), t0)
	// The power-on edge itself — inside the settle window, so nothing fires.
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	// Past the settle delay: rule 1b fires its one launch.
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls at the power-on edge = %d, want 1", got)
	}
	// ...which the device pends: the screen is still sitting at Home two polls
	// later, so rule 2's confirmation must reach its threshold and recover it.
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(3*time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(4*time.Second))
	if got := fc.callCount(); got != 2 {
		t.Fatalf("launch calls after the pended launch left the screen at Home = %d, want 2 (rule 1b's, then rule 2's recovery)", got)
	}
	// And rule 2's own cooldown still holds afterwards — no machine-gunning.
	for _, d := range []time.Duration{5 * time.Second, 6 * time.Second, 7 * time.Second} {
		k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(d))
	}
	if got := fc.callCount(); got != 2 {
		t.Fatalf("launch calls once both rules had fired = %d, want still 2 (%+v)", got, fc.calls)
	}
}

// TestEachRuleNamesItselfInItsDispatch pins the two rules' distinct launch
// reasons, which is what makes them separable in a relay's log — and, more to
// the point here, what makes it possible to assert WHICH rule fired rather than
// only that something did.
func TestEachRuleNamesItselfInItsDispatch(t *testing.T) {
	t0 := time.Unix(1700000000, 0)

	// Rule 1b: powered on into a foreign app, so rule 2 can never be the one
	// that fired.
	k1 := New(Config{Adopted: adoptEverything, LaunchDelay: time.Millisecond, PowerOnLaunch: true})
	k1.recordObservation(obsFor("scr1", "Ready", ""), t0)
	k1.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(time.Second))
	if reason, launch := k1.evaluate("scr1", "PowerOn", "app", t0.Add(2*time.Second)); !launch || reason != launchReasonPowerOn {
		t.Errorf("power-on edge evaluate = (%q, %v), want (%q, true)", reason, launch, launchReasonPowerOn)
	}

	// Rule 2: rule 1b switched off, so only the home streak can fire.
	k2 := New(Config{Adopted: adoptEverything, LaunchDelay: time.Millisecond})
	k2.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)
	k2.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	if reason, launch := k2.evaluate("scr1", "PowerOn", "home", t0.Add(2*time.Second)); !launch || reason != launchReasonHomeRecovery {
		t.Errorf("home-streak evaluate = (%q, %v), want (%q, true)", reason, launch, launchReasonHomeRecovery)
	}
}

// TestPowerOnLaunchFiresPerScreenIndependently: the edge bookkeeping is
// per-screen state, so one screen powering on neither fires nor consumes
// another's.
func TestPowerOnLaunchFiresPerScreenIndependently(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{Adopted: adoptEverything, LaunchDelay: time.Millisecond, Controller: fc, PowerOnLaunch: true})
	t0 := time.Unix(1700000000, 0)

	// Both screens off; only scr1 comes on.
	k.recordObservation(obsFor("scr1", "Ready", ""), t0)
	k.recordObservation(obsFor("scr2", "Ready", ""), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(2*time.Second))
	if got := len(fc.callsFor("scr1")); got != 1 {
		t.Errorf("scr1 launches = %d, want 1", got)
	}
	if got := len(fc.callsFor("scr2")); got != 0 {
		t.Errorf("scr2 launches while it is still off = %d, want 0", got)
	}

	// Now scr2 comes on: it gets its OWN launch, unaffected by scr1's consumed edge.
	k.recordObservation(obsFor("scr2", "PowerOn", "app"), t0.Add(3*time.Second))
	k.recordObservation(obsFor("scr2", "PowerOn", "app"), t0.Add(4*time.Second))
	if got := len(fc.callsFor("scr2")); got != 1 {
		t.Errorf("scr2 launches after its own power-on = %d, want 1", got)
	}
	if got := len(fc.callsFor("scr1")); got != 1 {
		t.Errorf("scr1 launches after scr2's power-on = %d, want still 1", got)
	}
}
