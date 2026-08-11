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
//   - Stale: contact has been made at some point, but not recently. NOT
//     "offline" — see the package doc.
//   - NeverSeen: no relay has ever observed this screen pull a program. Usually
//     a screen that is authored and paired but has not been switched on yet, or
//     one whose player has never started.
type Reachability string

const (
	ReachabilityLive      Reachability = "live"
	ReachabilityStale     Reachability = "stale"
	ReachabilityNeverSeen Reachability = "never_seen"
)

// LiveWindowMs is the staleness threshold separating Live from Stale, and the
// number is derived rather than picked: a player polls its program about every
// 10 seconds (player/1 PLY-082), so 45s is four missed polls plus slack for a
// slow request and the reporting interval's own granularity.
//
// It is deliberately generous. The cost of calling a healthy screen stale is an
// operator chasing a screen that is fine, repeatedly, until they stop trusting
// the column at all — which destroys the surface. The cost of calling a
// just-failed screen live is that they learn about it 45 seconds later than
// they might have, on a wall nobody was watching anyway. Those are not
// symmetric.
//
// Exported so the API layer can publish the threshold alongside the judgement:
// a consumer that disagrees with this line can draw its own from the raw age,
// which every response also carries.
const LiveWindowMs int64 = 45_000

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

	// ReportAgeMs is how long ago the REPORT this status came from arrived. It
	// is published rather than folded away because it is the one number that
	// distinguishes "this screen stopped talking to its relay" from "this relay
	// stopped talking to us" — two failures with completely different remedies
	// that every other field here renders identically.
	ReportAgeMs int64

	ProgramRevision string
	Priority        string
	Display         string
	ContentCount    int
	RenderAssetRef  string
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
		for _, f := range [...]string{e.ScreenID, e.ProgramRevision, e.RenderAssetRef, e.Priority, e.Display} {
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
				ScreenID:             e.ScreenID,
				RelayID:              relayID,
				Paired:               e.Paired,
				LastPullAgeMs:        agePlus(e.LastPullAgeMs, reportAge),
				LastAckAgeMs:         agePlus(e.LastAckAgeMs, reportAge),
				LastRenderStartAgeMs: agePlus(e.LastRenderStartAgeMs, reportAge),
				ReportAgeMs:          reportAge,
				ProgramRevision:      e.ProgramRevision,
				Priority:             e.Priority,
				Display:              e.Display,
				ContentCount:         e.ContentCount,
				RenderAssetRef:       e.RenderAssetRef,
			}
			st.Reachability = reachabilityOf(st.LastPullAgeMs)
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

// reachabilityOf applies the ONE threshold this model draws (LiveWindowMs), off
// the PULL age specifically rather than off whichever contact was most recent.
//
// The pull is the right signal because it is the one a working player makes
// unconditionally, every cycle, forever. An ack only follows a pull; a render
// start only happens when there is content to render, so a screen correctly
// showing a blank program would look dead if judged by it.
func reachabilityOf(lastPullAgeMs int64) Reachability {
	switch {
	case lastPullAgeMs == NeverObserved:
		return ReachabilityNeverSeen
	case lastPullAgeMs <= LiveWindowMs:
		return ReachabilityLive
	default:
		return ReachabilityStale
	}
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
