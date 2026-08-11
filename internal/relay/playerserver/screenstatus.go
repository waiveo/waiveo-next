// screenstatus.go is the relay's answer to "what is that screen actually
// doing?" — the per-screen liveness observations this server accumulates as a
// side effect of serving player/1, and the snapshot of them a reporter carries
// to the app peer (parity row 5.8, the legacy stack's screens-page cards).
//
// # It observes, it does not probe
//
// Nothing here contacts a screen. Every fact recorded below is one this server
// already had, because a paired player HAS to come here to work: it pulls its
// program (PLY-080), it acknowledges the Lease it was handed (PLY-091), and it
// reports the render it began (PLY-110). Those three arrivals are the strongest
// possible evidence a screen is alive — they are the screen doing its job — and
// they cost nothing to record because the requests are already being served.
//
// A separate reachability probe (an ECP poll, a ping) would answer a DIFFERENT
// and weaker question: whether the device responds, not whether the player is
// running and being served. A Roku that answers ECP while its channel has
// crashed is exactly the failure an operator needs to see, and a probe-based
// status would report it healthy.
//
// # Why every age is RELATIVE, and no absolute timestamp crosses the wire
//
// The one thing an operator wants is "when did I last hear from this screen",
// and the one thing that reliably corrupts that answer is clock skew. The relay
// stamps its observations from its own clock; the app renders them against the
// browser's. An absolute epoch-ms instant travelling between them is silently
// wrong by whatever the two clocks differ by — and a relay on an appliance with
// no RTC that boots before NTP settles is not a hypothetical.
//
// So a Snapshot carries AGES, each computed against the same relay clock reading
// that produced them (Snapshot's own `TakenAgeMs` is always 0 by construction —
// it is the anchor the app adds its own elapsed time to). Nothing downstream has
// to trust that two machines agree about what time it is; it only has to trust
// that each machine can measure elapsed time on its own, which every clock can.
//
// # What a Snapshot deliberately does not say
//
// It never says "offline". This server cannot distinguish a screen that is
// switched off from one whose network is down, from one whose player has
// crashed, from one it has simply never been paired with — and an operator told
// "offline" will go and check the wrong thing. It reports how long it has been
// since each kind of contact and lets the reader draw the line; the app's own
// read model is where a threshold is applied, and it says why (internal/app/screens).
package playerserver

import (
	"sort"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screenLiveness is one screen's accumulated observation record. All three
// instants are this server's own clock (nowMs), stored absolute internally and
// converted to ages at the moment a Snapshot is taken, so a snapshot taken later
// reports a larger age from the same stored value with no re-stamping anywhere.
//
// A zero instant means "never observed", which is a distinct and important state
// from "observed a long time ago": a screen that has pulled and then gone quiet
// is a screen that broke, while one that has never pulled at all was probably
// never paired. ScreenStatus keeps them distinct all the way to the console.
type screenLiveness struct {
	lastPullMs        int64
	lastAckMs         int64
	lastRenderStartMs int64

	// unackedPulls counts program pulls served since the last acknowledgement
	// this server saw — incremented in noteProgramPullLocked, zeroed in
	// noteLeaseAck.
	//
	// It exists because the two ages above cannot answer the question the app
	// peer's read model actually has to ask. "Is this screen transferring
	// content, or is it broken?" looks identical in the ages: both are a pull
	// with no ack after it. The difference is whether the screen is making
	// PROGRESS, and the observable form of that is whether it keeps producing
	// new unacknowledged pulls. A transferring screen has exactly one
	// outstanding (the fetch is serialised inside the player's poll loop); a
	// screen whose content fetch fails abandons the Lease and pulls again on a
	// backoff, forever.
	//
	// A count rather than a derived age also survives the case that made this
	// necessary: the failing screen's pull age RESETS on every retry, so no
	// bound on it can ever expire. See wire.ScreenFetchingMaxUnackedPulls.
	unackedPulls int

	// lastProgramRevision / lastPriority / lastDisplay are what this server most
	// recently HANDED that screen — read off the Lease at issuance rather than
	// off the served-program map at snapshot time, deliberately: the question a
	// console is asking is "what is that screen showing", and the answer is the
	// program the screen was last given, not the one it would be given if it
	// asked right now. Those differ for exactly as long as a screen is failing to
	// poll, which is precisely when the distinction matters.
	lastProgramRevision string
	lastPriority        string
	lastDisplay         string

	// lastContentCount and lastRenderAssetRef are the two facts that make
	// "playing" concrete rather than abstract: how many content items the last
	// Lease carried, and the asset the player last told us it had actually put on
	// screen (PLY-110). The second is the only one that is EVIDENCE of playback
	// rather than of intent.
	lastContentCount   int
	lastRenderAssetRef string
}

// ScreenStatus is one screen's observed status as this relay knows it — the
// per-screen entry of a Snapshot, and the shape that rides to the app peer.
//
// Every *AgeMs is milliseconds BEFORE the snapshot instant, and -1 means "never"
// (see screenLiveness). -1 rather than 0 because 0 is a real, common answer — a
// screen that pulled in the same millisecond the snapshot was taken — and a
// sentinel that collides with a real value is a sentinel that will eventually be
// read as one.
type ScreenStatus struct {
	ScreenID string

	// Paired reports whether this relay currently holds a live (non-terminated,
	// unexpired) channel-token session for the screen. It is what separates "this
	// screen has never checked in" from "this screen is not set up at all", which
	// an operator staring at a blank card needs to be able to tell apart.
	Paired bool

	LastPullAgeMs        int64
	LastAckAgeMs         int64
	LastRenderStartAgeMs int64

	// UnackedPulls is program pulls served since the last ack seen — see
	// screenLiveness. Unlike the ages it is not a duration and has no "never"
	// sentinel: zero is the ordinary healthy answer.
	UnackedPulls int

	ProgramRevision string
	Priority        string
	Display         string
	ContentCount    int
	RenderAssetRef  string
}

// neverObserved is the *AgeMs value for a contact this relay has never seen.
const neverObserved int64 = -1

// ScreenStatuses returns one ScreenStatus per screen this relay knows anything
// about — every screen it has served a program to, holds a program for, or holds
// a live session for — with every age measured against ONE clock reading taken
// here.
//
// One reading for the whole snapshot, not one per screen: two screens' ages must
// be comparable ("this one is 4s stale and that one is 90s stale"), and reading
// the clock per entry makes the comparison off by however long the walk took. It
// is the same reason handleProgram reads its clock once for the expiry check and
// the Lease stamps both.
//
// The union of three sources rather than just the liveness map, because each
// covers a case the others cannot. A screen with a served program that has never
// pulled is the "configured but never checked in" card an operator most needs to
// see, and it appears in `programs` alone. A screen that paired and has not yet
// pulled appears in the session index alone. Reporting only screens that have
// pulled would make the surface silent about exactly the screens that are broken.
func (s *Server) ScreenStatuses() []ScreenStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowMs()

	ids := map[string]bool{}
	for id := range s.liveness {
		ids[id] = true
	}
	for id := range s.programs {
		ids[id] = true
	}
	for _, sess := range s.tokens {
		if sess.ScreenID != "" {
			ids[sess.ScreenID] = true
		}
	}

	out := make([]ScreenStatus, 0, len(ids))
	for id := range ids {
		l := s.liveness[id]
		prog := s.programForLocked(id)
		st := ScreenStatus{
			ScreenID:             id,
			Paired:               s.hasLiveSessionLocked(id, now),
			LastPullAgeMs:        ageOf(now, l.lastPullMs),
			LastAckAgeMs:         ageOf(now, l.lastAckMs),
			LastRenderStartAgeMs: ageOf(now, l.lastRenderStartMs),
			UnackedPulls:         l.unackedPulls,
			ProgramRevision:      l.lastProgramRevision,
			Priority:             l.lastPriority,
			Display:              l.lastDisplay,
			ContentCount:         l.lastContentCount,
			RenderAssetRef:       l.lastRenderAssetRef,
		}
		// A screen that has never pulled has been handed nothing, so it has no
		// "last handed" program to report — but it DOES have a currently served
		// one, and reporting that (rather than three empty strings) is what makes
		// a never-checked-in card say "configured to show X, never collected it"
		// instead of saying nothing at all.
		if l.lastPullMs == 0 {
			st.ProgramRevision = prog.ProgramRevision
			st.Priority = prog.Priority
			st.Display = prog.Display
			st.ContentCount = len(prog.Content)
		}
		out = append(out, st)
	}
	// Sorted by screen_id, so a console rendering the list gets a stable order
	// across reports rather than Go's map-iteration shuffle — a table whose rows
	// reorder every ten seconds is unreadable, and a diff between two reports is
	// meaningless without one.
	sort.Slice(out, func(i, j int) bool { return out[i].ScreenID < out[j].ScreenID })
	return out
}

// hasLiveSessionLocked reports whether any channel-token session currently
// resolves to screenID and is neither terminated nor expired at now. The caller
// holds s.mu.
//
// It walks the token index rather than keeping a per-screen counter, because the
// index is bounded by the paired-screen count and a counter would be a second
// piece of state to keep in step with every mint, drop and expiry — three places
// to forget, for a walk over a handful of entries taken once per report.
func (s *Server) hasLiveSessionLocked(screenID string, now int64) bool {
	for _, sess := range s.tokens {
		if sess.ScreenID != screenID || sess.Terminated {
			continue
		}
		if now > sess.ExpiresAt {
			continue
		}
		return true
	}
	return false
}

// ageOf converts a stored absolute instant into an age before now, mapping the
// never-observed zero onto neverObserved. A stamp in the future (a clock that
// stepped backwards between the stamp and the read) clamps to 0 rather than
// reporting a negative age that would be indistinguishable from the sentinel.
func ageOf(now, stamp int64) int64 {
	if stamp == 0 {
		return neverObserved
	}
	if stamp > now {
		return 0
	}
	return now - stamp
}

// noteProgramPullLocked records that screenID pulled a program at nowMs and was
// handed this Lease. The caller holds s.mu.
//
// The recorded facts are read off the LEASE — the thing that was actually
// handed over, after the capability filter dropped whatever the player could not
// draw — rather than off the served program. A player that declares only `image`
// and is served a slide-only program receives an EMPTY content list, and a status
// surface reading the served program would report it as showing three slides.
func (s *Server) noteProgramPullLocked(screenID string, nowMs int64, lease wire.Lease) {
	l := s.liveness[screenID]
	l.lastPullMs = nowMs
	// One more pull the screen has not confirmed. Counted here rather than
	// derived at snapshot time because the fact is a COUNT of events and only
	// the place the events arrive can count them: the ages in a snapshot cannot
	// tell one outstanding pull from twenty, which is the whole distinction.
	l.unackedPulls++
	l.lastProgramRevision = lease.ProgramRevision
	l.lastPriority = lease.Priority
	l.lastDisplay = lease.Display
	l.lastContentCount = len(lease.Content)
	s.liveness[screenID] = l
}

// noteLeaseAck records that screenID acknowledged a Lease. Acknowledgement is a
// stronger signal than the pull that preceded it: the pull only proves the
// screen asked, while the ack proves it received, parsed and accepted what it
// was given (PLY-091).
//
// # It correlates NOTHING, and every reader downstream depends on knowing that
//
// This records that AN ack arrived. It does not read the `lease_id` off it and
// does not check that the acknowledged Lease is the one most recently served.
// So `lastAckMs` means "the last time this screen acknowledged anything", and
// the derived judgements built on it mean less than their names suggest:
//
//   - "this pull is unacknowledged" (internal/app/screens' pullIsUnacknowledged)
//     is really "the last ack this relay saw predates the last pull it served";
//   - unackedPulls going back to zero means "some ack arrived", not "the
//     outstanding Lease was confirmed".
//
// On the shipped player the two are the same thing — one pull, one ack, in
// order, on one thread, wvAckLease passing the leaseId it was just handed — and
// wire's TestTheAckFollowsTheContentFetch is what keeps that ordering from
// changing quietly. A player that retried an old ack, or acknowledged out of
// order, would reset both signals without having confirmed anything. If that
// ever becomes possible, correlate here: the `lease_id` is already on the wire
// in both directions, so this is a place to compare it, not a fact to rediscover.
func (s *Server) noteLeaseAck(screenID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.liveness[screenID]
	l.lastAckMs = s.nowMs()
	l.unackedPulls = 0
	s.liveness[screenID] = l
}

// noteRenderStart records that screenID began presenting assetRef (PLY-110) —
// the only fact in this whole record that is evidence of something being ON the
// screen rather than of the screen having been told what to show.
func (s *Server) noteRenderStart(screenID, assetRef string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.liveness[screenID]
	l.lastRenderStartMs = s.nowMs()
	l.lastRenderAssetRef = assetRef
	s.liveness[screenID] = l
}
