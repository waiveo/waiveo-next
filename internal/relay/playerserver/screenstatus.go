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
	"unicode/utf8"

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

	// unackedPulls counts program pulls that handed over CONTENT and have not
	// been acknowledged since — incremented in noteProgramPullLocked for a
	// non-empty Lease only, zeroed in noteLeaseAck.
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
	//
	// # Why an EMPTY Lease does not count, which it used to
	//
	// Every unacknowledged pull this holds has to be one the screen still owes
	// an ack for, because the read model reads a surplus as failure. A Lease
	// with no content carries nothing to confirm and gets no ack: the shipped
	// player returns from `wvDoProgram` before `wvAckLease` when the content
	// array is empty (player-v3 Program.brs), exactly as it does when a fetch
	// fails, and there is no other producer of an ack. So while these counted,
	// the count climbed on a screen with nothing outstanding.
	//
	// Which is not a corner: terminalDefault() — the blank Lease served to any
	// screen this relay holds no program for (DAT-118) — is what a freshly
	// paired screen pulls until an operator assigns it something, and a
	// scheduled `blank` program is the same empty array every night. Two of
	// those pulls (about six seconds at the player's 2 s/4 s backoff) put the
	// count at the tolerance, so the FIRST real program a box ever serves read
	// `stale` while it was genuinely downloading, and a one-screen site graded
	// `down` (internal/app/api/diagnostics.go) while working perfectly.
	//
	// The capability-filtered-to-empty case rides along on the same rule: a
	// player served nothing it can draw has nothing to fetch either.
	unackedPulls int

	// lastProgramRevision / lastPriority / lastDisplay are what this server most
	// recently HANDED that screen — read off the Lease at issuance rather than
	// off the served-program map at snapshot time, deliberately: the question a
	// console is asking is "what is that screen showing", and the answer is the
	// program the screen was last given, not the one it would be given if it
	// asked right now. Those differ for exactly as long as a screen is failing to
	// poll, which is precisely when the distinction matters.
	//
	// They are INTENT, and nothing more. A screen that was handed a program and
	// refused it has these fields describing the program it refused — which is
	// what a console reported for a whole session in 2026-08 while the wall drew
	// something else entirely. What the screen ACCEPTED is the acked* set below,
	// and the two are separate fields rather than one because they answer two
	// different operator questions ("what did I send" / "what did it take").
	lastProgramRevision string
	lastPriority        string
	lastDisplay         string

	// lastLeaseID is the lease_id of that most recently handed Lease. It is what
	// lets an arriving acknowledgement be attributed to a specific Lease rather
	// than to whatever this server happens to be holding when it lands: an ack
	// naming this id confirms exactly the facts above, and an ack naming an older
	// one confirms an older program that is no longer described here.
	lastLeaseID string

	// ackedProgramRevision / ackedDisplay / ackedContentCount are the same three
	// facts about the Lease this screen most recently ACCEPTED (PLY-091
	// `accepted: true`) — copied from the handed set at the moment the ack for
	// that exact lease_id arrives.
	//
	// This is the set a console must render as "what is on that wall", because it
	// is the only one the screen has confirmed. PLY-091's ack means the player
	// received, parsed and accepted the Lease; the shipped player additionally
	// materialises every asset BEFORE acknowledging (Program.brs, and see
	// unackedPulls above), so an accepted Lease is one whose content the screen
	// holds. Never-wipe then keeps it on the wall until another is accepted.
	//
	// They are deliberately NOT reset when a new Lease is handed over. A screen
	// that is refusing its new program is still showing the last one it took, and
	// blanking these on issuance would replace a true statement about the wall
	// with an empty one at exactly the moment an operator needs it.
	ackedProgramRevision string
	ackedDisplay         string
	ackedContentCount    int

	// rejected / rejectedProgramRevision / rejectReason record a Lease this
	// screen REFUSED — PLY-091's `accepted: false` and the `reason` that clause
	// requires with it — and that it has not superseded by accepting anything
	// since.
	//
	// A refusal is not an acknowledgement and is not counted as one: it does not
	// stamp lastAckMs and does not clear unackedPulls, because the screen has
	// confirmed nothing. It is kept until the screen accepts something, which is
	// the one event that makes it no longer true.
	//
	// A flag rather than an instant, because the fact is a STANDING one and not a
	// moment: "has this screen refused what it was given and not taken anything
	// since". How long ago it happened is already answerable from lastAckMs (how
	// long since it accepted anything at all), and a second age would be a second
	// thing to keep in step.
	rejected                bool
	rejectedProgramRevision string
	rejectReason            string

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

	// ProgramRevision/Priority/Display/ContentCount describe the Lease this
	// screen was last HANDED — the platform's intent for it, not its state.
	ProgramRevision string
	Priority        string
	Display         string
	ContentCount    int

	// AckedProgramRevision/AckedDisplay/AckedContentCount describe the Lease this
	// screen last ACCEPTED (PLY-091). Empty strings and a zero count for a screen
	// that has never accepted one, which LastAckAgeMs's own never sentinel is the
	// unambiguous marker of.
	AckedProgramRevision string
	AckedDisplay         string
	AckedContentCount    int

	// Rejected reports that this screen refused a Lease (`accepted: false`) and
	// has accepted nothing since. RejectedProgramRevision and RejectReason name
	// what it refused and why — PLY-091 requires the reason accompany the
	// refusal, and it is the whole operator-actionable content of this state.
	Rejected                bool
	RejectedProgramRevision string
	RejectReason            string

	RenderAssetRef string
}

// maxRejectReasonBytes bounds the PLY-091 `reason` this server retains and
// reports upstream.
//
// A player writes that string, so its length is not this server's to assume, and
// the app peer's own intake refuses a report carrying an over-long field
// (internal/app/screens) — a refusal that would discard the WHOLE report, every
// screen in it, because one player was verbose. Truncating at the producer is
// what keeps that unreachable: the field is bounded before it is ever reported,
// so the intake cap is a check on a malformed relay rather than a fuse a
// well-behaved player can blow.
const maxRejectReasonBytes = 200

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
			ScreenID:                id,
			Paired:                  s.hasLiveSessionLocked(id, now),
			LastPullAgeMs:           ageOf(now, l.lastPullMs),
			LastAckAgeMs:            ageOf(now, l.lastAckMs),
			LastRenderStartAgeMs:    ageOf(now, l.lastRenderStartMs),
			UnackedPulls:            l.unackedPulls,
			ProgramRevision:         l.lastProgramRevision,
			Priority:                l.lastPriority,
			Display:                 l.lastDisplay,
			ContentCount:            l.lastContentCount,
			AckedProgramRevision:    l.ackedProgramRevision,
			AckedDisplay:            l.ackedDisplay,
			AckedContentCount:       l.ackedContentCount,
			Rejected:                l.rejected,
			RejectedProgramRevision: l.rejectedProgramRevision,
			RejectReason:            l.rejectReason,
			RenderAssetRef:          l.lastRenderAssetRef,
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
	// One more pull the screen has not confirmed — but only if it was handed
	// something to confirm. Counted here rather than derived at snapshot time
	// because the fact is a COUNT of events and only the place the events arrive
	// can count them: the ages in a snapshot cannot tell one outstanding pull
	// from twenty, which is the whole distinction.
	//
	// The emptiness test is `len(lease.Content)` — the Lease as HANDED OVER,
	// which is the same fact the next line records, and therefore covers both
	// ways a screen ends up with nothing to fetch: a program that carries no
	// content (terminalDefault, a scheduled blank) and one the capability filter
	// emptied. See screenLiveness.unackedPulls for why an empty Lease that
	// counted made a first-ever transfer read `stale`.
	//
	// It does not RESET on an empty Lease either. That would be an ack the screen
	// never sent: pulls already outstanding are still outstanding, and a screen
	// that failed two real transfers has not proved anything by being handed a
	// blank one.
	if len(lease.Content) > 0 {
		l.unackedPulls++
	}
	l.lastProgramRevision = lease.ProgramRevision
	l.lastPriority = lease.Priority
	l.lastDisplay = lease.Display
	l.lastContentCount = len(lease.Content)
	// The id this Lease's acknowledgement will name. Recorded so an arriving ack
	// confirms the facts of the Lease it names rather than whatever was handed
	// over most recently — which is the same Lease for the shipped player, and is
	// not for one that acknowledges late.
	l.lastLeaseID = lease.LeaseID
	// The acked* set is deliberately untouched here. Handing a screen a new
	// program does not change what it is showing; only its acceptance does.
	s.liveness[screenID] = l
}

// noteLeaseAck records that screenID acknowledged a Lease. Acknowledgement is a
// stronger signal than the pull that preceded it: the pull only proves the
// screen asked, while the ack proves it received, parsed and accepted what it
// was given (PLY-091).
//
// # The TIMING signals still correlate nothing, and readers depend on that
//
// lastAckMs and unackedPulls are stamped and cleared by ANY accepted ack,
// whatever Lease it names. So `lastAckMs` means "the last time this screen
// accepted anything", and the derived judgements built on it mean less than
// their names suggest:
//
//   - "this pull is unacknowledged" (internal/app/screens' pullIsUnacknowledged)
//     is really "the last accepted ack this relay saw predates the last pull it
//     served";
//   - unackedPulls going back to zero means "some ack arrived", not "the
//     outstanding Lease was confirmed".
//
// On the shipped player the two are the same thing — one pull, one ack, in
// order, on one thread, wvAckLease passing the leaseId it was just handed — and
// wire's TestTheAckFollowsTheContentFetch is what keeps that ordering from
// changing quietly. A player that retried an old ack, or acknowledged out of
// order, would reset both signals without having confirmed anything. The
// PROGRAM facts below do not have that weakness: they are promoted only for an
// ack naming the Lease they describe, which is the correlation this file's
// earlier revision noted as available on the wire and not yet used.
//
// # An acknowledgement carries an ANSWER, and this is where it is read
//
// PLY-091's body is `{lease_id, accepted, reason?}`, and `accepted` is the only
// field in the whole player/1 surface that reports what the screen DID with what
// it was given. It used to be discarded here: every arriving ack — including a
// refusal — stamped the ack instant, cleared the outstanding-pull count, and
// left the console reporting the refused program as the screen's state. A
// refusal is therefore now recorded as a refusal (rejected*), and only
// `accepted: true` promotes the handed facts to the acked* set.
func (s *Server) noteLeaseAck(screenID, leaseID string, accepted bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.liveness[screenID]
	if !accepted {
		// Not an acknowledgement of anything: lastAckMs and unackedPulls are
		// both "the screen confirmed something", and it did the opposite. The
		// refused Lease is named from the handed set only when the refusal names
		// the Lease that set describes — a late refusal of an older Lease is
		// still a refusal, but it did not refuse THIS program.
		l.rejected = true
		l.rejectReason = truncate(reason, maxRejectReasonBytes)
		l.rejectedProgramRevision = ""
		if leaseID != "" && leaseID == l.lastLeaseID {
			l.rejectedProgramRevision = l.lastProgramRevision
		}
		s.liveness[screenID] = l
		return
	}
	l.lastAckMs = s.nowMs()
	l.unackedPulls = 0
	// Accepting something is what makes an earlier refusal no longer true.
	l.rejected = false
	l.rejectedProgramRevision = ""
	l.rejectReason = ""
	// Promote the handed facts to the confirmed set only for an ack that names
	// the Lease those facts describe. An ack for an older lease_id is genuine
	// liveness (recorded above) but confirms an older program, and copying the
	// current one under it would attribute a brand-new program to a screen that
	// has never seen it.
	if leaseID != "" && leaseID == l.lastLeaseID {
		l.ackedProgramRevision = l.lastProgramRevision
		l.ackedDisplay = l.lastDisplay
		l.ackedContentCount = l.lastContentCount
	}
	s.liveness[screenID] = l
}

// noteScreenContact records a non-Lease arrival from screenID that is
// nevertheless proof the screen is alive and being served — today, a viewer
// interaction (PLY's interactive press). It stamps the same liveness instant an
// acknowledgement does.
//
// It deliberately does NOT touch the acked*/rejected* sets: a press says a human
// touched the wall, not that the screen accepted the program it was last handed,
// and a press by someone standing in front of a screen that is refusing its new
// program must not read as that screen having taken it.
//
// What it still shares with an accepted ack is the unackedPulls reset, which is
// a known and separately-booked conflation: a press is liveness evidence but not
// content-transfer evidence, and clearing a CONTENT-transfer failure count on
// one is a different fact standing in for the fact the counter measures. It is
// left exactly as it was here rather than changed under an unrelated fix; the
// rejected* set above is not cleared by it, so a refusal a screen actually made
// survives any number of presses.
func (s *Server) noteScreenContact(screenID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.liveness[screenID]
	l.lastAckMs = s.nowMs()
	l.unackedPulls = 0
	s.liveness[screenID] = l
}

// truncate caps s at max bytes, cutting on a rune boundary so the result is
// still valid UTF-8 (the wire is UTF-8 JSON, and a report carrying a severed
// rune is a report the app peer's decoder may reject in full).
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
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
