// screenstatus.go carries the relay→app-peer `screen.status` report: what each
// screen behind a relay has actually been observed doing (parity row 5.8).
//
// Like the rest of this package it is a data shape only; the producing side is
// internal/relay/playerserver (which accumulates the observations) plus
// cmd/waiveo-relay's reporter loop, and the consuming side is
// internal/feeder/relayconn handing the report to internal/app/screens.
//
// # Where this sits in relay/1
//
// It is an ADDITIVE relay/1 frame in the same shape and posture as
// `device.candidates` (REL-110/111), and modelled on it deliberately rather
// than invented fresh: a relay-initiated, fire-and-forget, FULL-SET report that
// the app peer takes as REPLACING its prior view of that relay. relay/1 does not
// define it — screen liveness reporting has no contract clause yet — so it is
// named, shaped and documented exactly like the frame it is a sibling of, and
// the moment the contract does define one this is the shape to reconcile.
//
// Full-set replace, not a delta, for the same reason candidates are: a delta
// stream needs the two sides to agree about a starting point they cannot
// establish across a reconnect, and a screen that has DISAPPEARED from a relay's
// view (its session dropped, its program removed) has to be able to leave the
// app's view too — which only a replace expresses.
//
// # Fire-and-forget, unlike pairing.redeemed
//
// No acknowledgement frame, and that is a real distinction from
// `pairing.redeemed` (REL-124d), which is acknowledged because losing one loses
// a fact nobody can recompute. A lost status report loses nothing: the next one
// is along in seconds and carries the whole truth again. Making it acknowledged
// would add a correlation table and a retry ledger to protect data with a
// ten-second shelf life.
//
// # Ages, not timestamps
//
// Every duration here is milliseconds BEFORE the report was taken, measured
// entirely on the relay's own clock. See internal/relay/playerserver's
// screenstatus.go for the full argument; the short version is that an absolute
// instant crossing between two machines is wrong by their skew, and an appliance
// that boots before NTP settles makes that skew arbitrarily large.
package wire

// FrameTypeScreenStatus is the relay→app-peer screen-liveness report's `type`.
const FrameTypeScreenStatus = "screen.status"

// ScreenStatusBody is `screen.status`'s body: the relay's FULL current view of
// every screen it knows anything about.
//
// Screens is never nil on the wire when the relay has nothing to report — an
// EMPTY array is the meaningful value ("this relay knows of no screens"), and it
// is the value that clears a stale view, so a producer that dropped it to null
// would leave the app peer showing screens the relay has forgotten.
type ScreenStatusBody struct {
	Screens []ScreenStatusEntry `json:"screens"`
}

// ScreenStatusEntry is one screen's observed status.
//
// Each `*_age_ms` is -1 for "never observed", which is a distinct state from a
// large age: a screen that pulled once and stopped is broken, one that has never
// pulled was probably never paired, and an operator needs to be shown different
// things. -1 rather than 0 because 0 is a real answer.
//
// This is the APP PEER's decode shape as much as the relay's encode shape, and —
// unlike DeviceCandidate, which is deliberately kept separate from the relay's
// own producing type — this one IS shared, because both sides of it are this
// repository's own code compiled from one module and neither side is untrusted
// input to the other in the sense that matters there: a relay cannot use this
// frame to name another relay's screens (the app peer keys the report by the
// AUTHENTICATED connection identity, never by anything in the body), so there is
// no field here whose meaning depends on trusting the sender.
type ScreenStatusEntry struct {
	ScreenID string `json:"screen_id"`

	// Paired reports whether the relay currently holds a live channel-token
	// session for this screen — "set up" as distinct from "checking in".
	Paired bool `json:"paired"`

	LastPullAgeMs        int64 `json:"last_pull_age_ms"`
	LastAckAgeMs         int64 `json:"last_ack_age_ms"`
	LastRenderStartAgeMs int64 `json:"last_render_start_age_ms"`

	// UnackedPulls is how many program pulls this relay has served the screen
	// since the last acknowledgement it saw from it — 0 for a screen that is up
	// to date, 1 for one materialising the Lease it was just handed, and
	// climbing for one that keeps asking and never confirms.
	//
	// It is a COUNT and not a duration because the thing it has to distinguish
	// cannot be told apart by duration. A screen downloading a 60 MB video and a
	// screen failing every content fetch on a 403 both present an
	// unacknowledged pull of the same age; only the second one keeps making new
	// ones. See wire.ScreenFetchingMaxUnackedPulls for the whole argument and
	// for what an ack does and does not correlate with.
	UnackedPulls int `json:"unacked_pulls"`

	// ProgramRevision/Priority/Display/ContentCount describe the program the
	// screen was last HANDED (or, for a screen that has never pulled, the one
	// currently waiting for it). Priority is PLY-108's own enumeration, so a
	// console can tell a scheduled program from an operator's push-now takeover.
	ProgramRevision string `json:"program_revision,omitempty"`
	Priority        string `json:"priority,omitempty"`
	Display         string `json:"display,omitempty"`
	ContentCount    int    `json:"content_count"`

	// RenderAssetRef is the asset the player last reported actually putting on
	// screen (PLY-110) — the only field here that is evidence of playback rather
	// than of intent.
	RenderAssetRef string `json:"render_asset_ref,omitempty"`
}
