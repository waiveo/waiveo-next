// discoveryenginestate.go carries `discovery.engine_state` — the relay's
// unsolicited upward report of what its discovery ENGINE is currently set up to
// watch for, as opposed to what a scan is doing right now
// (discoveryscanstatus.go).
//
// # Why this is a separate frame from scan_status
//
// The two answer different questions and change at different moments. A scan is
// an activity: it starts, it finishes, and `discovery.scan_status` is reported at
// those two transitions. The watch set is a CONFIGURATION: it changes when a
// signed desired-state generation applies — a pack installed, a pack removed —
// which is a moment no scan is involved in. Riding this on `scan_status` would
// make these numbers stale exactly when a pack was installed or removed, the one
// moment they matter most, so they travel on their own frame reported at their
// own transition.
//
// # What it is for
//
// A pack declares how its devices look (`pack_match_patterns`, REL-064) and the
// relay derives lane watches from those declarations. Several things can go
// wrong between "a pack declared it" and "a relay is watching for it", and every
// one of them is silent:
//
//   - the mDNS lane is off in this deployment, so an mDNS pattern is DECLARED
//     and undeliverable — binding multicast is the deployment's call, not a
//     pack's;
//   - the match form is `macOui`, which no lane implements yet;
//   - the pattern is malformed, which means a producer bug in a section that was
//     signed and hash-checked.
//
// The relay already distinguishes all three. Until this frame existed it wrote
// them only to its own journal, so the operator could tell "the pack is wrong"
// from "the relay cannot deliver it here" ONLY over SSH. A console showing a
// discovered-device list has no way to say "nothing is watching for anything",
// which is the state a deployment with no packs installed is actually in.
package wire

// Frame type discriminator for the upward engine-state report. A relay that
// never sends it is not an error — an app peer receiving an unknown frame
// ignores it under REL-004 additive tolerance, and one that receives none simply
// has no engine state to show, which is distinct from a relay reporting zeroes.
const FrameTypeDiscoveryEngineState = "discovery.engine_state"

// DiscoveryEngineStateBody is `discovery.engine_state`'s body: which passive
// lanes this relay is running, how many watches each is holding after the
// current generation applied, and what was declared but not delivered.
//
// # Every count is present even at zero
//
// None of these carry `omitempty`, deliberately. Zero is the single most
// important value here: `ssdp_watches: 0, mdns_watches: 0, pack_patterns: 0`
// says "this relay is watching for nothing", which is precisely the condition an
// operator cannot otherwise see and the reason this frame exists. Omitting a
// zero would make it indistinguishable from a relay that declined to report the
// member at all, collapsing "watching for nothing" into "did not say" — the same
// absent-vs-empty confusion `open_ports` had to be rescued from.
//
// The lane booleans are what keep a zero readable. `mdns_watches: 0` with
// `mdns_lane: true` means the generation declared no mDNS patterns; the same
// zero with `mdns_lane: false` means this deployment never bound multicast at
// all. Those are different problems with different owners, and a count alone
// cannot tell them apart.
type DiscoveryEngineStateBody struct {
	// SSDPLane / MDNSLane report whether each passive lane is RUNNING on this
	// relay, independent of how many watches it holds.
	SSDPLane bool `json:"ssdp_lane"`
	MDNSLane bool `json:"mdns_lane"`

	// SSDPWatches / MDNSWatches are the live watch counts each lane accepted for
	// the current generation — builtin plus pack-derived, after the merge that
	// folds duplicate declarations into one watch. A lane that is not running
	// reports zero, which the matching lane boolean explains.
	SSDPWatches int `json:"ssdp_watches"`
	MDNSWatches int `json:"mdns_watches"`

	// PackPatterns is how many `pack_match_patterns` entries the applied
	// generation carried (REL-064) — what packs DECLARED, before any question of
	// whether a lane could take them. Read against the two watch counts it says
	// whether declarations are reaching the lanes at all.
	PackPatterns int `json:"pack_patterns"`

	// MDNSUndeliverable is how many declared mDNS patterns this relay could not
	// install because the mDNS lane is not running here. Not an error and not the
	// pack's fault: it is the deployment's own lane switch, reported so the
	// operator attributes it correctly.
	MDNSUndeliverable int `json:"mdns_undeliverable"`

	// MacOUIUnimplemented is how many declared patterns used the `macOui` match
	// form, which is a valid MAN-071 form that no lane implements yet. Counted
	// rather than dropped so a pack author is told their declaration is
	// well-formed and inert, instead of watching it vanish.
	MacOUIUnimplemented int `json:"mac_oui_unimplemented"`

	// Malformed is how many pattern entries failed to parse. A signed,
	// hash-checked section carrying a malformed pattern means a PRODUCER bug, not
	// a transport fault, which is why it is surfaced rather than skipped.
	Malformed int `json:"malformed"`
}

// WatchingNothing reports whether this relay has no live watch in any lane.
//
// It is the one derived judgement this package makes about engine state, and it
// exists here rather than in each consumer because it is the question the frame
// was added to answer: a relay watching nothing discovers nothing passively, no
// matter how healthy every other signal looks. A console, an operator CLI and a
// health check should all agree on what that means.
func (b DiscoveryEngineStateBody) WatchingNothing() bool {
	return b.SSDPWatches == 0 && b.MDNSWatches == 0
}
