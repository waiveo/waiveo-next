package playerserver

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestEvaluateRecoverySuppressesBlankDisplay wires the PLY-155 corpus case
// (PLY-155-valid-power-schedule-interaction) directly: the screen's currently
// active lease is display:blank (an intentional off-air state), the device is
// genuinely powered on and at its home baseline (so BOTH PLY-151 and PLY-152
// recovery gates pass), yet recovery MUST be suppressed purely because of the
// blank display (PLY-155) — the expected block, field for field.
func TestEvaluateRecoverySuppressesBlankDisplay(t *testing.T) {
	got := EvaluateRecovery(LivenessSignal{
		ActiveDisplay:      DisplayBlank,
		DeviceStatus:       DeviceStatus{State: "on", AppType: "home"},
		PowerScheduleState: "blank_window",
	})

	if !got.WouldPassStateCheck {
		t.Errorf("WouldPassStateCheck = false, want true (state on is in the media-player on group, PLY-151)")
	}
	if !got.WouldPassAppTypeCheck {
		t.Errorf("WouldPassAppTypeCheck = false, want true (app_type home passes PLY-152)")
	}
	if !got.SuppressedDueToBlankDisplay {
		t.Errorf("SuppressedDueToBlankDisplay = false, want true (active lease display:blank, PLY-155)")
	}
	if got.SuppressedDueToScheduledOff {
		t.Errorf("SuppressedDueToScheduledOff = true, want false (blank_window is not a scheduled power-off, PLY-156)")
	}
	if got.RecoveryAttempted {
		t.Errorf("RecoveryAttempted = true, want false (an intentionally blanked screen is not a recovery target, PLY-155/157)")
	}
}

// TestAdoptPreemptDisplayHonorsBlank wires the second half of PLY-155's
// expected block: a preempt-priority lease carrying display:content arrives at
// this same blank screen. PLY-104 requires the player accept and persist the
// new lease (PLY-091) but continue rendering blank — preemption changes what
// would render next, it does not override an intentional blank already in force.
func TestAdoptPreemptDisplayHonorsBlank(t *testing.T) {
	incoming := wire.Lease{
		LeaseID:  "01J8Z3K4N5P6Q7R8S9T0V1W2ZL",
		Priority: "preempt",
		Display:  DisplayContent,
		Content: []wire.LeaseContent{{
			Type:     "image",
			AssetRef: "sha256:dddd000000000000000000000000000000000000000000000000000000dd",
			URL:      "https://198.51.100.20/cas/dddd0000",
		}},
	}
	got := AdoptPreemptDisplay(DisplayBlank, incoming)

	if !got.PreemptLeaseAccepted {
		t.Errorf("PreemptLeaseAccepted = false, want true (a preempt lease is still accepted+persisted, PLY-091/104)")
	}
	if got.DisplayRemains != DisplayBlank {
		t.Errorf("DisplayRemains = %q, want %q (intentional blank stays in force, PLY-104)", got.DisplayRemains, DisplayBlank)
	}
	if got.PreemptForcesVisible {
		t.Errorf("PreemptForcesVisible = true, want false (preemption does not override an intentional blank, PLY-104)")
	}
}

// TestEvaluateRecoveryContentGapRecovers exercises the recover-eligible path:
// a display:content lease whose screen has fallen back to its powered-on home
// baseline (a liveness gap) — both gates pass, no suppression — so recovery is
// attempted (PLY-150/151/152/157). This is the Home-only recovery lesson: the
// screen fell back to an idle baseline, exactly what recovery targets.
func TestEvaluateRecoveryContentGapRecovers(t *testing.T) {
	got := EvaluateRecovery(LivenessSignal{
		ActiveDisplay: DisplayContent,
		DeviceStatus:  DeviceStatus{State: "on", AppType: "home"},
	})
	if !got.RecoveryAttempted {
		t.Errorf("RecoveryAttempted = false, want true (content lease, powered-on home baseline, both gates pass — PLY-157)")
	}
	if got.SuppressedDueToBlankDisplay || got.SuppressedDueToScheduledOff {
		t.Errorf("recovery unexpectedly suppressed: %+v", got)
	}
}

// TestEvaluateRecoveryLeavesStandbyAlone is the never-wake lesson (PLY-151): a
// device reporting a state outside the on semantic group (standby) still
// answers device-plane queries, but a recovery command would force an
// unintended wake — so it MUST be left alone even for a content lease.
func TestEvaluateRecoveryLeavesStandbyAlone(t *testing.T) {
	got := EvaluateRecovery(LivenessSignal{
		ActiveDisplay: DisplayContent,
		DeviceStatus:  DeviceStatus{State: "standby", AppType: "home"},
	})
	if got.WouldPassStateCheck {
		t.Errorf("WouldPassStateCheck = true, want false (standby is in the off group, PLY-151)")
	}
	if got.RecoveryAttempted {
		t.Errorf("RecoveryAttempted = true, want false (never wake an ECP-alive standby, PLY-151)")
	}
}

// TestEvaluateRecoveryLeavesForegroundAppAlone is PLY-152: a genuine
// foreground application (app_type app) is legitimately in use — recovery
// MUST NOT hijack it, even though the device is powered on.
func TestEvaluateRecoveryLeavesForegroundAppAlone(t *testing.T) {
	got := EvaluateRecovery(LivenessSignal{
		ActiveDisplay: DisplayContent,
		DeviceStatus:  DeviceStatus{State: "on", AppType: "app"},
	})
	if got.WouldPassAppTypeCheck {
		t.Errorf("WouldPassAppTypeCheck = true, want false (app_type app is a real foreground app, PLY-152)")
	}
	if got.RecoveryAttempted {
		t.Errorf("RecoveryAttempted = true, want false (a genuine foreground app is left alone, PLY-152)")
	}
}

// TestEvaluateRecoverySuppressesScheduledOff is PLY-156: an intentionally
// powered-off screen (the display-power schedule's most recent directive is
// off) is not a recovery target, exactly as PLY-155 holds for a blank one —
// even though the active lease is content and both device gates pass.
func TestEvaluateRecoverySuppressesScheduledOff(t *testing.T) {
	got := EvaluateRecovery(LivenessSignal{
		ActiveDisplay:      DisplayContent,
		DeviceStatus:       DeviceStatus{State: "on", AppType: "home"},
		PowerScheduleState: PowerScheduleOff,
	})
	if !got.SuppressedDueToScheduledOff {
		t.Errorf("SuppressedDueToScheduledOff = false, want true (schedule commanded off, PLY-156)")
	}
	if got.RecoveryAttempted {
		t.Errorf("RecoveryAttempted = true, want false (an intentionally powered-off screen is not a recovery target, PLY-156)")
	}
}

// TestEvaluateRecoveryDefersWhileSettling is PLY-154: when the device's state
// only just entered the on group, a bounded settle delay applies before any
// recovery command — the command must not race the device's own boot/foreground
// sequence and pend without effect (the power-on delay lesson).
func TestEvaluateRecoveryDefersWhileSettling(t *testing.T) {
	got := EvaluateRecovery(LivenessSignal{
		ActiveDisplay:      DisplayContent,
		DeviceStatus:       DeviceStatus{State: "on", AppType: "home"},
		JustEnteredOnGroup: true,
	})
	if !got.SettleDelayDeferred {
		t.Errorf("SettleDelayDeferred = false, want true (state just entered the on group, PLY-154)")
	}
	if got.RecoveryAttempted {
		t.Errorf("RecoveryAttempted = true, want false (recovery waits out the settle delay, PLY-154)")
	}
}
