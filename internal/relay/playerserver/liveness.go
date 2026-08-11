// This file implements player/1's relay-side screen-liveness recovery
// decision (PLY-150-157) plus the preempt-into-blank interaction (PLY-104).
//
// Screen liveness and recovery (PLY-150) lets a relay recover a screen whose
// player has stopped presenting its assigned program in the foreground — but
// only through a narrow set of gates, each ruling out a distinct false-recovery
// case. This decision is a PURE evaluation (no device-plane I/O of its own —
// the actual launch command rides relay/1 REL-112 / device-class-registry
// REG-066, whose own corpora cover delivery, PLY §Notes): given a screen's
// currently active lease display, its device's most recently reported status,
// and the display-power schedule's most recent directive, it decides whether
// foreground recovery MAY be attempted, and — critically — SUPPRESSES it for
// an intentionally off-air screen (a display:blank lease, PLY-155, or a
// scheduled power-off, PLY-156), which is not a failure to recover from.
package playerserver

import "github.com/maaxton/waiveo-next/internal/shared/wire"

// display values a Lease's `display` field carries (PLY-093): exactly one of
// content or blank. blank is the intentional on-but-showing-nothing off-air
// state a compiled program can assign.
const (
	DisplayContent = "content"
	DisplayBlank   = "blank"
)

// priority values a Lease's `priority` field carries (PLY-108): exactly one of
// scheduled (ordinary schedule resolution) or preempt (a deliberately-invoked
// takeover — an operator's push-now override, snapshot.overrideProgram).
//
// PLY-100/101 give the two an ORDERING at the player: a preempt Lease interrupts
// the item currently on screen rather than waiting for it to end. SetProgram's
// own priority fence honours the same ordering on the relay, refusing to let a
// same-generation `scheduled` write replace a `preempt` program — so the two
// halves of "a takeover outranks the schedule" read off one pair of names.
const (
	PriorityScheduled = "scheduled"
	PriorityPreempt   = "preempt"
)

// PowerScheduleOff is the display-power schedule directive that most recently
// commanded the target device off (PLY-156). The schedule's own row shape is
// out of player/1's scope (Scope); only the fact that a relay honors its most
// recent power-off directive matters here — any other value (including an empty
// one or a blank-window marker) is not a power-off and does not suppress on
// this ground.
const PowerScheduleOff = "off"

// onSemanticGroup is the media-player device class's `on` semantic group
// (device-class-registry REG-063): the canonical states that count as
// genuinely powered on. A device whose most recently reported state is outside
// this group MUST be left alone (PLY-151) — a recovery attempt against it would
// itself force an unintended wake.
var onSemanticGroup = map[string]bool{
	"on":        true,
	"playing":   true,
	"idle":      true,
	"paused":    true,
	"buffering": true,
}

// recoverableAppTypes are the app_type attribute values (device-class-registry
// REG-064) against which foreground recovery MAY be attempted (PLY-152): a
// screen that has fallen back to an idle baseline (home) or a menu surface.
// PLY-152 also permits an absent/unknown app_type — handled in
// recoverableAppType, not via this table.
var recoverableAppTypes = map[string]bool{
	"home": true,
	"menu": true,
}

// recoverableAppType reports whether appType permits recovery under PLY-152:
// home or menu, or absent/unknown (the empty string, or a value the driver did
// not classify). A genuine foreground app or a screensaver is excluded.
func recoverableAppType(appType string) bool {
	if appType == "" || appType == "unknown" {
		return true
	}
	return recoverableAppTypes[appType]
}

// DeviceStatus is a screen's most recently reported device-plane status
// relevant to recovery gating (device-class-registry REG-063/REG-064): the
// canonical power State and the foreground surface's AppType.
type DeviceStatus struct {
	State   string // canonical state, classified into a semantic group (REG-063)
	AppType string // foreground surface category (REG-064): app, screensaver, home, menu, or absent
}

// LivenessSignal is the full input to a screen-liveness recovery evaluation
// (PLY-150-157): the screen's currently active lease display (PLY-093), the
// device's most recently reported status (REG-063/064), the display-power
// schedule's most recent directive (PLY-156), and whether the device's state
// has only just entered the on semantic group (PLY-154's settle window).
type LivenessSignal struct {
	ActiveDisplay      string       // the currently active Lease's display (PLY-093): content or blank
	DeviceStatus       DeviceStatus // most recently reported device status
	PowerScheduleState string       // display-power schedule's most recent directive; PowerScheduleOff suppresses (PLY-156)
	JustEnteredOnGroup bool         // true within the PLY-154 settle window after the state entered the on group
}

// RecoveryEvaluation is the relay's screen-liveness recovery decision for a
// single evaluation (PLY-150-157). The three would-suppress/pass flags expose
// WHY the net decision came out as it did — each isolates one gate — and
// RecoveryAttempted is PLY-157's complete conjunction: recovery is attempted
// only when the device is genuinely powered on (PLY-151) and not in genuine
// foreground use (PLY-152), settled past its own power-on transition (PLY-154),
// and not subject to an intentional blank (PLY-155) or scheduled-off (PLY-156).
type RecoveryEvaluation struct {
	WouldPassStateCheck         bool // PLY-151: device state is in the on semantic group
	WouldPassAppTypeCheck       bool // PLY-152: app_type is home/menu/absent
	SuppressedDueToBlankDisplay bool // PLY-155: active lease display is blank (intentional off-air)
	SuppressedDueToScheduledOff bool // PLY-156: schedule most recently commanded the device off
	SettleDelayDeferred         bool // PLY-154: state just entered the on group; wait out the settle delay
	RecoveryAttempted           bool // PLY-157: the net decision — every gate satisfied, no suppression
}

// EvaluateRecovery is the pure screen-liveness recovery decision (PLY-150-157):
// it independently evaluates each recovery gate, then attempts recovery ONLY
// when all pass and nothing suppresses. PLY-153 requires PLY-151 and PLY-152
// both hold (neither substitutes for the other), and PLY-155/156 suppress
// entirely regardless of the device-gate outcomes — an intentionally off-air
// screen is not a recovery target. The three field-paid keep-alive lessons live
// here at the decision level: never wake a device outside the on group
// (PLY-151), recover only a home/menu baseline never a foreground app
// (PLY-152), and defer through a bounded settle delay after a power-on
// transition (PLY-154).
func EvaluateRecovery(sig LivenessSignal) RecoveryEvaluation {
	ev := RecoveryEvaluation{
		WouldPassStateCheck:         onSemanticGroup[sig.DeviceStatus.State],
		WouldPassAppTypeCheck:       recoverableAppType(sig.DeviceStatus.AppType),
		SuppressedDueToBlankDisplay: sig.ActiveDisplay == DisplayBlank,
		SuppressedDueToScheduledOff: sig.PowerScheduleState == PowerScheduleOff,
		SettleDelayDeferred:         sig.JustEnteredOnGroup,
	}
	ev.RecoveryAttempted = ev.WouldPassStateCheck &&
		ev.WouldPassAppTypeCheck &&
		!ev.SuppressedDueToBlankDisplay &&
		!ev.SuppressedDueToScheduledOff &&
		!ev.SettleDelayDeferred
	return ev
}

// BlankPreemptOutcome is the outcome of a preempt-priority Lease arriving at a
// screen whose currently active lease display is in force (PLY-104): the lease
// is always accepted and persisted (PLY-091), DisplayRemains is what the screen
// keeps rendering, and PreemptForcesVisible reports whether the preemption
// itself drove the screen to visible content.
type BlankPreemptOutcome struct {
	PreemptLeaseAccepted bool   // PLY-091: a preempt lease is accepted+persisted regardless
	DisplayRemains       string // PLY-104: what the screen continues rendering (content or blank)
	PreemptForcesVisible bool   // PLY-104: whether preemption forced the screen out of blank
}

// AdoptPreemptDisplay applies PLY-104: a preempt-priority incoming Lease
// assigned to a screen whose currently active display is blank MUST NOT force
// the display out of blank — the player accepts and persists the new Lease
// (PLY-091) but continues rendering blank until a subsequent Lease's own
// display says otherwise. When the active display is already content, the
// incoming preempt lease's own display governs (its content becomes visible).
// activeDisplay is the screen's currently active Lease display (PLY-093).
func AdoptPreemptDisplay(activeDisplay string, incoming wire.Lease) BlankPreemptOutcome {
	out := BlankPreemptOutcome{PreemptLeaseAccepted: true}
	if activeDisplay == DisplayBlank {
		// PLY-104: an intentional blank is in force; preemption changes what
		// would render next, not the blank already assigned.
		out.DisplayRemains = DisplayBlank
		out.PreemptForcesVisible = false
		return out
	}
	out.DisplayRemains = incoming.Display
	out.PreemptForcesVisible = incoming.Display == DisplayContent
	return out
}
