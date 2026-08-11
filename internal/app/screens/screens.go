// Package screens is the app peer's LIVE SCREEN STATUS read model: what the
// site's relays report each screen has actually been observed doing (parity row
// 5.8, the legacy stack's screens-page cards).
//
// It is the screen-shaped sibling of internal/app/devices, and is deliberately
// built the same way — one view per reporting relay, merged on write, read
// lock-free by the api layer — because it solves the same problem: several
// relays each authoritative for their own part of the fleet, each report a
// full-set replace of that relay's part alone.
//
// # What this model is, and is not
//
// It is a CACHE of observations, not desired state and not an authored row.
// Nothing here is persisted: on an app restart it is empty until the relays
// report again, which takes one report interval, and that is the correct
// behaviour — a status read model that survived a restart would serve
// observations nobody had made since, dated from before the process existed.
//
// The authored screen rows live in the store and are listed by /api/v1/screens;
// this model answers a different question about the same screens and is joined
// to them by screen_id at the API layer, never merged into them.
//
// # Honesty about staleness is the whole feature
//
// The one thing this surface must not do is say "offline". It cannot tell a
// screen that is switched off from one whose network is down, from one whose
// player crashed, from one that was never paired — and every one of those sends
// an operator somewhere different. So it reports two things it can actually
// know: how long since the relay last heard from the screen, and how long since
// the app last heard from the relay. Reachability (below) applies ONE threshold
// to the first, and names it after what it is measuring.
//
// # Why the age arithmetic is done here, at read time
//
// A relay reports RELATIVE ages measured on its own clock (wire/screenstatus.go
// explains why: an absolute instant crossing between two machines is wrong by
// their skew). This model stamps each report with the APP's clock at arrival,
// and at read time adds the report's own age to each reported age:
//
//	total staleness = (relay's age at report) + (now − report arrival)
//
// Both terms are elapsed-time measurements taken on a single machine each, so
// neither depends on the two clocks agreeing. And the second term is what makes
// a DISCONNECTED relay honest: no new reports arrive, the report ages, and every
// screen behind it grows stale together — which is exactly true, since the app
// has genuinely stopped learning anything about them.
package screens

import (
	"fmt"
	"sort"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// maxScreensPerReport bounds the work one report can cause, on the same
// principle internal/app/devices' intake caps do: a relay is untrusted input,
// and a site with more than this many screens behind one relay is a relay
// misreporting rather than a deployment this platform has.
const maxScreensPerReport = 4096

// maxScreenFieldBytes bounds each free-form string a report carries
// (screen_id, program_revision, asset_ref). Generous against every real value —
// a ULID is 26 bytes and a sha256 hex digest 64 — and small enough that a
// maximal report cannot be used to grow this process without bound.
const maxScreenFieldBytes = 256

// Reachability is the ONE derived judgement this model makes, and it is named
// for what it measures rather than for what an operator might conclude.
//
//   - Live: the relay heard from this screen recently enough that a poll cannot
//     have been missed by much.
//   - Fetching: the relay handed this screen a Lease, the screen has not
//     acknowledged it, and the screen is still PLAUSIBLY WORKING ON IT —
//     recently enough that the shipped player would still be transferring, and
//     without having abandoned Lease after Lease in the meantime. See
//     reachabilityOf for why that is a state of its own and not either of the
//     neighbours, and for why both bounds are needed.
//   - Rejected: the relay is being contacted by this screen and the screen is
//     NOT taking the program it is being handed — it either said so (PLY-091's
//     `accepted: false`) or it has re-pulled past the progress bound without
//     confirming anything. See isRejecting for why that is a state of its own
//     and, in particular, why it is not `live`.
//   - Stale: contact has been made at some point, but not recently. NOT
//     "offline" — see the package doc.
//   - NeverSeen: no relay has ever observed this screen pull a program. Usually
//     a screen that is authored and paired but has not been switched on yet, or
//     one whose player has never started.
type Reachability string

const (
	ReachabilityLive      Reachability = "live"
	ReachabilityFetching  Reachability = "fetching"
	ReachabilityRejected  Reachability = "rejected"
	ReachabilityStale     Reachability = "stale"
	ReachabilityNeverSeen Reachability = "never_seen"
)

// LiveWindowMs is the staleness threshold separating Live from everything else.
//
// It is not a number chosen here. It is wire.ScreenLiveWindowMs, COMPUTED from
// the player's own three loop timings — its poll wait, its program-request
// timeout and its lease-ack timeout, all mirrored against the shipped
// BrightScript under test — and declared next to the relay's report interval, so
// the threshold and the rates it has to exceed cannot drift apart. See
// internal/shared/wire/screencadence.go for the derivation and the pins that
// hold it.
//
// This used to be a hand-written 45_000, justified against PLY-082's nominal
// 10-second poll: a number in one file and a justification in another, with
// nothing able to tell anyone when they stopped matching. Deriving it is what
// fixes the class, and the derivation is deliberately a BOUND on what a healthy
// screen can do rather than a measurement of one — read that file's two
// corrections before changing any of it. The first attempt derived the window
// from a measurement that turned out to be a broken screen's retry backoff and
// widened it to 180 000 ms; the second omitted a whole synchronous round trip
// from the loop it claimed to bound.
//
// Exported so the API layer can publish the threshold alongside the judgement:
// a consumer that disagrees with this line can draw its own from the raw age,
// which every response also carries.
const LiveWindowMs int64 = wire.ScreenLiveWindowMs

// ContentTransferWindowMs is the second threshold: how far past LiveWindowMs a
// screen with an UNACKNOWLEDGED Lease is still called Fetching rather than
// Stale. Also computed — the live window plus one whole content-fetch timeout,
// which is the player's own statement of how long a single transfer may take.
//
// Exported for the same reason as LiveWindowMs, and additionally because a
// consumer that wants to treat Fetching as Stale needs to know which line it is
// disagreeing with.
const ContentTransferWindowMs int64 = wire.ScreenContentTransferWindowMs

// MaxFetchingUnackedPulls is the THIRD threshold, and the one without which the
// other two do not add up to a health signal.
//
// A screen may be Fetching while it has at most this many program pulls
// outstanding. One is a transfer in progress; the tolerance on top absorbs a
// single failed iteration, the same allowance the live window makes. Past it,
// the screen is not slow at fetching — it is asking again and again and
// confirming nothing, which is a broken screen and must be allowed to read
// Stale.
//
// It is not a number chosen here either: wire.ScreenFetchingMaxUnackedPulls,
// derived from the shape of the player's own loop. Read that doc before touching
// this — it is where the 2026-08 case (a screen that read `fetching` forever
// while showing nothing, and kept a whole dark site out of the `down` grade) is
// written down.
//
// Exported alongside the two windows, and published on the row for the same
// reason they are: a judgement whose inputs a consumer cannot see is a number
// nobody can check.
const MaxFetchingUnackedPulls int64 = wire.ScreenFetchingMaxUnackedPulls

// Status is one screen's merged live status as this app peer currently knows it.
// Every *AgeMs is milliseconds before the read, INCLUDING the report's own age
// (see the package doc), or NeverObserved for a contact never made.
type Status struct {
	ScreenID string
	RelayID  string

	Reachability Reachability
	Paired       bool

	LastPullAgeMs        int64
	LastAckAgeMs         int64
	LastRenderStartAgeMs int64

	// UnackedPulls is how many program pulls the relay has served this screen
	// since the last acknowledgement it saw. Not an age and not merged with the
	// report's own: it is a count of events at the relay, and it does not go
	// stale — a relay that stops reporting leaves this frozen at whatever it
	// last observed, which ReportAgeMs is the field for saying.
	UnackedPulls int

	// ReportAgeMs is how long ago the REPORT this status came from arrived. It
	// is published rather than folded away because it is the one number that
	// distinguishes "this screen stopped talking to its relay" from "this relay
	// stopped talking to us" — two failures with completely different remedies
	// that every other field here renders identically.
	ReportAgeMs int64

	// ProgramRevision/Priority/Display/ContentCount are what the relay last
	// HANDED this screen: the platform's intent for it, and never on their own
	// evidence of what it is doing.
	ProgramRevision string
	Priority        string
	Display         string
	ContentCount    int

	// AckedProgramRevision/AckedDisplay/AckedContentCount are what the screen
	// last ACCEPTED (PLY-091). These are the fields a console renders as "what is
	// on that wall": the ones above describe a program a screen may be refusing.
	//
	// Empty and zero for a screen that has never accepted a Lease, which
	// LastAckAgeMs's never sentinel is the unambiguous marker of — an accepted
	// BLANK program is legitimately empty here too.
	AckedProgramRevision string
	AckedDisplay         string
	AckedContentCount    int

	// Rejected/RejectedProgramRevision/RejectReason describe a Lease the screen
	// REFUSED and has not superseded by accepting anything since. False for a
	// screen with no outstanding refusal — and false, too, for one behind a relay
	// too old to report it, which is why it is a flag and not an age (wire's
	// ScreenStatusEntry says what an absent age would have claimed).
	Rejected                bool
	RejectedProgramRevision string
	RejectReason            string

	RenderAssetRef string
}

// NeverObserved is the *AgeMs value for a contact never made. It matches the
// relay-side sentinel it is carried from (playerserver's own), rather than being
// re-derived, so no layer has to translate between two spellings of "never".
const NeverObserved int64 = -1

// relayReport is one relay's whole last report plus the app-clock instant it
// arrived at. The entries are held exactly as reported — untranslated — and the
// age arithmetic happens at read time against takenAtMs, so a report grows
// staler on its own with nothing rewriting it.
type relayReport struct {
	takenAtMs int64
	screens   []wire.ScreenStatusEntry
}

// Registry holds every reporting relay's last screen-status report and merges
// them on read.
//
// One view per relay, keyed by the AUTHENTICATED relay identity, exactly as
// internal/app/devices does — because a report is a full-set replace of THAT
// relay's screens and must not touch another's. Merging on READ rather than on
// write (which is where devices differs) because the merge here is
// age-dependent: every field a reader wants is a function of the current
// instant, so a merged snapshot cached at write time would be wrong by however
// long ago it was written, which is precisely the quantity this model exists to
// report.
type Registry struct {
	mu      sync.RWMutex
	reports map[string]relayReport

	// nowMs is the app peer's clock, injected rather than read from the wall so
	// a test can drive a report's ageing instead of sleeping through it. It is
	// the SAME clock the connection layer stamps arrivals with; a Registry whose
	// read clock and write clock disagreed would report negative ages.
	nowMs func() int64
}

// NewRegistry builds an empty registry reading time from nowMs, which is
// required: every value this model produces is a duration, so a registry with no
// clock could only ever answer zero.
func NewRegistry(nowMs func() int64) (*Registry, error) {
	if nowMs == nil {
		return nil, fmt.Errorf("screens: NewRegistry requires a clock")
	}
	return &Registry{reports: map[string]relayReport{}, nowMs: nowMs}, nil
}

// ApplyScreenStatus replaces the app peer's whole view of relayID with the
// screens in one `screen.status` report.
//
// relayID MUST be the connection's own AUTHENTICATED relay identity (REL-041/150),
// never a value the frame asserts — see relayconn.ScreenStatusSink's doc for the
// stakes. takenAtMs is this app peer's own clock at arrival, supplied by the
// caller so the whole report shares one anchor.
//
// The report is applied WHOLE or not at all, and a single bad entry refuses it
// rather than being skipped — the same discipline internal/app/devices' intake
// applies to a candidate report, and for its reason: a full-set replace with the
// bad half dropped installs a view that is not the relay's actual view, silently
// blanking exactly the screens whose entries were malformed.
func (r *Registry) ApplyScreenStatus(relayID string, takenAtMs int64, entries []wire.ScreenStatusEntry) error {
	if relayID == "" {
		return fmt.Errorf("screens: a status report carries no authenticated relay identity")
	}
	if len(entries) > maxScreensPerReport {
		return fmt.Errorf("screens: report carries %d screens, more than the %d cap", len(entries), maxScreensPerReport)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.ScreenID == "" {
			return fmt.Errorf("screens: report carries an entry with no screen_id")
		}
		// A duplicate screen_id makes the report self-contradictory: two entries
		// claim different things about one screen and nothing says which is
		// current. Refused rather than last-wins, because last-wins would depend
		// on the relay's array order for the answer to "is this screen live".
		if seen[e.ScreenID] {
			return fmt.Errorf("screens: report names screen %q twice", e.ScreenID)
		}
		seen[e.ScreenID] = true
		// Every free-form string the entry carries, including the ones a PLAYER
		// authored (`reject_reason` is the player's own words, forwarded). The
		// relay bounds that one before reporting it (playerserver's
		// maxRejectReasonBytes), which is what keeps a verbose player from
		// tripping this cap and having its whole relay's report refused — the cap
		// here is a check on a misbehaving relay, not a fuse a screen can blow.
		for _, f := range [...]string{
			e.ScreenID, e.ProgramRevision, e.RenderAssetRef, e.Priority, e.Display,
			e.AckedProgramRevision, e.AckedDisplay, e.RejectedProgramRevision, e.RejectReason,
		} {
			if len(f) > maxScreenFieldBytes {
				return fmt.Errorf("screens: report for screen %q carries a field longer than %d bytes", e.ScreenID, maxScreenFieldBytes)
			}
		}
	}

	// Copied rather than retained: the caller's slice comes off a decoded frame
	// it may reuse, and this view outlives the request that delivered it.
	held := make([]wire.ScreenStatusEntry, len(entries))
	copy(held, entries)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports[relayID] = relayReport{takenAtMs: takenAtMs, screens: held}
	return nil
}

// ForgetScreens drops everything relayID reported. Called when that relay's
// enrollment is REVOKED — not when its connection merely drops; see
// relayconn.ScreenStatusSink for that distinction.
func (r *Registry) ForgetScreens(relayID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reports, relayID)
}

// Statuses returns every screen every reporting relay currently describes, in
// screen_id order, with all ages resolved against one clock reading.
//
// One reading for the whole call, so two screens' ages are comparable — the same
// reason the relay takes one reading per report.
//
// A screen reported by TWO relays keeps the FRESHEST report's entry. That state
// should not happen (a screen pairs with one relay) but it is reachable during a
// migration between relays, and the freshest observation is the only defensible
// answer: the alternative, an arbitrary map-order winner, would have a console
// flickering between two relays' views of one screen.
func (r *Registry) Statuses() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := r.nowMs()
	best := map[string]Status{}
	for relayID, rep := range r.reports {
		reportAge := elapsed(now, rep.takenAtMs)
		for _, e := range rep.screens {
			st := Status{
				ScreenID:                e.ScreenID,
				RelayID:                 relayID,
				Paired:                  e.Paired,
				LastPullAgeMs:           agePlus(e.LastPullAgeMs, reportAge),
				LastAckAgeMs:            agePlus(e.LastAckAgeMs, reportAge),
				LastRenderStartAgeMs:    agePlus(e.LastRenderStartAgeMs, reportAge),
				UnackedPulls:            e.UnackedPulls,
				ReportAgeMs:             reportAge,
				ProgramRevision:         e.ProgramRevision,
				Priority:                e.Priority,
				Display:                 e.Display,
				ContentCount:            e.ContentCount,
				AckedProgramRevision:    e.AckedProgramRevision,
				AckedDisplay:            e.AckedDisplay,
				AckedContentCount:       e.AckedContentCount,
				Rejected:                e.Rejected,
				RejectedProgramRevision: e.RejectedProgramRevision,
				RejectReason:            e.RejectReason,
				RenderAssetRef:          e.RenderAssetRef,
			}
			st.Reachability = reachabilityOf(st.LastPullAgeMs, st.LastAckAgeMs, st.UnackedPulls, st.Rejected)
			if prev, dup := best[e.ScreenID]; dup && prev.ReportAgeMs <= reportAge {
				continue
			}
			best[e.ScreenID] = st
		}
	}

	out := make([]Status, 0, len(best))
	for _, st := range best {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScreenID < out[j].ScreenID })
	return out
}

// reachabilityOf judges a screen from the two contacts a working player makes
// every cycle: the program pull and the Lease acknowledgement that follows it.
//
// # Live is judged on the freshest CONTACT, not on the pull alone
//
// The pull is the signal that matters because it is the one a player makes
// unconditionally, forever; the ack only ever happens after a pull, so counting
// it can make a screen look fresher but never staler, and it IS a real round
// trip from that screen. A render start is deliberately still excluded: it only
// happens when there is content to render, so a screen correctly showing a blank
// program would look dead if judged by it.
//
// # Why Fetching exists
//
// The gap between a pull and its ack is, in the shipped player, exactly the
// content-materialisation phase: `wvEnsureContent` fetches every asset the Lease
// names that this screen does not already hold — up to 120 000 ms EACH, then a
// whole-file SHA-256 — and only then does `wvAckLease` fire. All of it is
// serialised into the poll loop, so all of it lands inside `last_pull_age_ms`.
//
// A 60 MB video on building wifi therefore takes a perfectly healthy screen past
// LiveWindowMs while never-wipe keeps the OUTGOING program on the wall the whole
// time. Reporting that screen `stale` is the wasted investigation this column
// exists to avoid. Reporting it `live` would be a claim nothing supports — no
// contact has been made. So it gets its own word, and the word says what was
// measured: the relay handed it a Lease and is waiting to hear back.
//
// # Why Fetching expires, and why ONE expiry was not enough
//
// A screen that lost power between its pull and its ack produces the identical
// observation, and this model cannot tell the two apart. An unbounded Fetching
// would therefore hide a dead screen forever — the withdrawn 180 000 ms window
// wearing a friendlier label. So the state expires. It takes TWO bounds to do
// that, and for one round it had only the first:
//
//   - AGE. The pull may be at most ContentTransferWindowMs old: one whole
//     content-fetch timeout past the live window, the player's own statement of
//     how long a single transfer may take. This expires a screen that went
//     SILENT.
//   - PROGRESS. At most MaxFetchingUnackedPulls pulls may be outstanding. This
//     expires a screen that is failing LOUDLY, and nothing else can.
//
// The second was missing, and its absence made this state permanently capture
// the exact screen the whole cadence file was corrected for. That wall answered
// every program pull with a 200 (so the relay re-stamped its pull, and the age
// bound reset), failed every content fetch on a 403, returned before the ack
// (Program.brs:337, ahead of wvAckLease at :365) and retried at the 60 000 ms
// backoff cap forever. Its age topped out at 78 000 ms — inside the 172 000 ms
// window — and its last pull was permanently unacknowledged, so BOTH clauses of
// the old rule held on every sample and it read `fetching` for the rest of its
// life. An age bound cannot expire a signal whose age keeps resetting.
//
// The cost was not the word on the card. The console told an operator
// "Collecting content" — and, in the phrasing of the day, that the screen was
// still showing its last program — about a screen showing nothing, and — worse
// — the fleet roll-up grades `down` on `Live == 0 &&
// Fetching == 0` (internal/app/api/diagnostics.go), so a whole site in this
// state was permanently `degraded` and never `down`. The dark-fleet alarm was
// off for precisely the failure it exists for.
//
// # The dependency this rests on, named so it cannot break quietly
//
// "Unacknowledged means transferring" is true only while the player acks AFTER
// the fetch. player/1's PLY-088 says a player MUST be able to acknowledge a
// Lease whose assets it cannot yet fetch — which this player does not do today,
// and a future one might. wire's TestTheAckFollowsTheContentFetch reads the
// shipped BrightScript and fails if that order ever inverts, because when it
// does this clause stops firing and a screen downloading a video goes back to
// reading Stale with nothing to say so.
//
// A second, weaker dependency, stated because it is easy to over-read the field
// names: the relay does not correlate an ack with the Lease it acknowledges
// (playerserver's noteLeaseAck). "Unacknowledged" therefore means "the last ack
// predates the last pull", and unackedPulls means "CONTENT-BEARING pulls served
// since the last ack of any kind". Both are exact for the shipped player and
// would be loosened by one that acknowledged out of order.
//
// The "content-bearing" qualifier is load-bearing in the opposite direction to
// everything above, and it is the correction this clause needed most. A Lease
// with no content gets no ack either — the shipped player refuses an empty
// content array before wvAckLease, exactly as it does a failed fetch — so while
// blank Leases counted, the two ordinary sources of them (a screen with no
// program assigned, and any screen scheduled blank overnight) drove the count
// past the tolerance with NOTHING outstanding. A box roughly six seconds past
// pairing was already at the bound, and the first program an operator ever
// assigned read `stale` while it downloaded perfectly normally. The counter is
// the thing that has to be right here: this clause reads a surplus as failure,
// so anything counted that cannot be acknowledged is a false failure.
// # Why Rejected exists, and why it has to be tested BEFORE Live
//
// Everything above judges CONTACT. None of it judges whether the contact
// achieved anything, and that gap is the whole of the 2026-08-11 defect: a wall
// that pulled every 10 seconds, refused every program it was handed, and drew an
// hour-old slide throughout was reported `live`, with the program it was
// refusing named as what it was showing. Both statements were derived correctly
// and both were false about the thing an operator was looking at.
//
// `live` is reached on the freshest contact alone, and a screen that pulls on
// cadence has a fresh contact no matter what it does with the answer. So a
// refusal cannot be a case ordered after `live`; it has to be asked first, or the
// state can never fire for the screens it exists for. Only a screen that would
// otherwise have read `live` (or `stale`, at the top of a backoff sawtooth) can
// reach it — a screen that is genuinely mid-transfer takes the Fetching branch
// below, because the two are decided by the SAME progress bound in opposite
// directions and cannot both hold.
func reachabilityOf(lastPullAgeMs, lastAckAgeMs int64, unackedPulls int, rejected bool) Reachability {
	if lastPullAgeMs == NeverObserved {
		return ReachabilityNeverSeen
	}
	if isRejecting(lastPullAgeMs, unackedPulls, rejected) {
		return ReachabilityRejected
	}
	if freshestContact(lastPullAgeMs, lastAckAgeMs) <= LiveWindowMs {
		return ReachabilityLive
	}
	if isFetching(lastPullAgeMs, lastAckAgeMs, unackedPulls) {
		return ReachabilityFetching
	}
	return ReachabilityStale
}

// isRejecting reports whether a screen is in contact and NOT taking what it is
// being handed. Two independent pieces of evidence, either of which is enough,
// and one age gate over both.
//
// # The evidence
//
//   - It SAID SO. PLY-091's acknowledgement carries `accepted`, and a player that
//     answers `false` has reported a refusal in as many words, with the `reason`
//     that clause requires. The relay holds that refusal until the screen accepts
//     something (playerserver's noteLeaseAck), so an outstanding one is current
//     by construction and needs no tolerance: nothing is being guessed.
//
//   - It keeps ASKING and never confirms. unackedPulls past
//     MaxFetchingUnackedPulls is not a second opinion about the same thing — it
//     is the SAME bound `fetching` is drawn at, read on the other side. A screen
//     within the bound is plausibly materialising content and reads `fetching`;
//     one past it has abandoned Lease after Lease, which is what the shipped
//     player does when it refuses a program (it returns before `wvAckLease` and
//     retries on a backoff), and is why the bound exists at all. Deriving both
//     states from one line is deliberate: two bounds would be two places to
//     disagree about where "still trying" ends.
//
// The second is what actually catches today's player, which has no `accepted:
// false` path at all (Program.brs acknowledges only success). The first is what
// catches a conformant one the moment it grows one, and is why this does not wait
// three pulls to believe a screen that has already told us.
//
// # The age gate, which is the mirror this clause could most easily become
//
// A screen that failed a few pulls and then LOST POWER presents an unacknowledged
// count that never decreases, forever. Reporting that `rejected` would be the
// same defect this state was built to remove, pointed the other way: a dead
// screen wearing a word that says it is talking to us. So the pull must be recent
// enough that the screen is still plausibly there — the same
// ContentTransferWindowMs `fetching` expires on, for the same reason and out of
// the same constant. Past it, contact itself is the thing in doubt, and `stale`
// is the honest answer whatever the counters say.
//
// # What is deliberately NOT re-checked here
//
// pullIsUnacknowledged. It is implied by the count clause (a pull served since
// the last ack is by definition after it), and asking it again would let any
// arriving ack-shaped signal — an interaction press stamps the same instant —
// clear a refusal the screen genuinely made. The refusal record is cleared by
// exactly one event, at the relay: the screen accepting something.
func isRejecting(lastPullAgeMs int64, unackedPulls int, rejected bool) bool {
	if lastPullAgeMs == NeverObserved || lastPullAgeMs > ContentTransferWindowMs {
		return false
	}
	if rejected {
		return true
	}
	return int64(unackedPulls) > MaxFetchingUnackedPulls
}

// isFetching reports whether a screen past the live window is materialising
// content rather than failing — all three conditions, spelled out separately
// because each one expires a different failure and dropping any of them makes
// this state hide one.
func isFetching(lastPullAgeMs, lastAckAgeMs int64, unackedPulls int) bool {
	if !pullIsUnacknowledged(lastPullAgeMs, lastAckAgeMs) {
		return false
	}
	// Silent too long: a screen that lost power between its pull and its ack.
	if lastPullAgeMs > ContentTransferWindowMs {
		return false
	}
	// Not making progress: a screen that keeps pulling and never confirms. The
	// count is the relay's, not derived here, because only the place the pulls
	// arrive can count them — see the doc above for why no function of the two
	// ages can answer this.
	return int64(unackedPulls) <= MaxFetchingUnackedPulls
}

// freshestContact is the smaller (more recent) of the two ages, treating the
// never-observed sentinel as no contact at all rather than as an enormous one.
func freshestContact(lastPullAgeMs, lastAckAgeMs int64) int64 {
	if lastAckAgeMs != NeverObserved && lastAckAgeMs < lastPullAgeMs {
		return lastAckAgeMs
	}
	return lastPullAgeMs
}

// pullIsUnacknowledged reports whether the screen's most recent pull is still
// waiting for its ack — an ack OLDER than the pull answered an earlier one.
//
// Both ages have had the same report age added to them (agePlus), so comparing
// them is comparing two instants measured on one clock, which is the only
// comparison this model is allowed to make.
//
// What it does NOT establish, spelled out because the name overstates it: the
// relay stamps its ack instant on ANY arriving acknowledgement and reads no
// `lease_id` off it (playerserver's noteLeaseAck). So this really answers "does
// the last ack this relay saw predate the last pull it served", which coincides
// with "the outstanding Lease is unconfirmed" only because the shipped player
// pulls and acks one-for-one, in order, on one thread.
func pullIsUnacknowledged(lastPullAgeMs, lastAckAgeMs int64) bool {
	return lastAckAgeMs == NeverObserved || lastAckAgeMs > lastPullAgeMs
}

// agePlus adds the report's own age to an age measured inside it, preserving the
// never-observed sentinel: a contact that had never happened when the report was
// taken has still never happened now, and adding a duration to -1 would turn
// "never" into a plausible-looking number.
func agePlus(reported, reportAge int64) int64 {
	if reported == NeverObserved {
		return NeverObserved
	}
	if reported < 0 {
		// Any other negative value is a relay reporting something this model has
		// no reading for. Treated as never-observed rather than trusted or
		// arithmetically propagated: a negative age is not a measurement, and
		// carrying it forward would surface as a screen that pulled in the
		// future.
		return NeverObserved
	}
	return reported + reportAge
}

// elapsed is now−then, clamped at 0. A report stamped in the future (the app's
// own clock stepped backwards between arrival and read) reads as brand new
// rather than as a negative age that every comparison below would mis-order.
func elapsed(now, then int64) int64 {
	if then > now {
		return 0
	}
	return now - then
}
