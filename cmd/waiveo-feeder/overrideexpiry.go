package main

import (
	"context"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// overrideexpiry.go publishes a screen override's LAPSE.
//
// A screen program override may carry a TTL (data-model/1 DAT-004c's
// `expires_at`), and DAT-004d says a lapsed one is treated as absent "with no
// write required to retire it". The retirement half of that is real and lives in
// the derivation: ScreenOverride.Applies is evaluated every time a screen's
// program is projected, and the row is never rewritten.
//
// The DELIVERY half was missing, and its absence was the whole defect. A relay
// pulls the desired state only when nudged, and the nudge — and the
// `state.unchanged` answer behind it (REL-050/051/057) — is keyed on the
// GENERATION, which advances only on a write. So the projection was correct and
// nobody ever asked it again: a sixty-second fire-drill alert was still
// `priority=preempt pinned=true` an hour later, because nothing else in the
// store had been edited in the meantime. Mechanism built, seam unwired.
//
// This file is the seam. It waits until the earliest pending `expires_at` and
// advances the generation, which is the ONE thing every existing delivery
// mechanism already reacts to: the post-commit hook nudges every live relay
// (REL-057), each re-pulls, the app peer re-derives (its snapshot cache is
// expiry-aware for the same reason — desiredStateSource.current), and the screen
// gets a schedule-resolved program at a strictly newer generation. Newer
// generation is also what unblocks the two anti-revert guards, both of which are
// deliberately scoped to a single generation: playerserver's priority fence
// admits any newer generation's write, and soleServedScreenID is recomputed on
// the new apply, where the screen is no longer pinned and its schedule resolver
// resumes.
//
// # Why a generation bump is not the "write" DAT-004d forbids
//
// DAT-004d forbids requiring a write TO RETIRE the override — i.e. an operator
// or a sweeper having to null out the row's `override` member before the screen
// stops showing it. Nothing here writes a row, and the override member stays
// exactly where the operator left it; what stops it applying is still the clock,
// evaluated at resolution time. The generation is the announcement, not the
// retirement.
//
// # What this does not cover, stated rather than implied
//
// If the FEEDER is down when an alert lapses, the relay keeps serving the alert
// until the feeder returns (at which point the boot derive is already past the
// expiry and the first pull corrects it). Closing that would mean carrying
// `expires_at` onto the wire so the relay lapses the entry itself — a real
// option, and the other half of DAT-004d's ambition, but it needs the relay to
// have a fallback program to swap to at that instant, which today only the
// schedule resolver it is currently fenced out of can supply. Not done here, and
// not silently: an override outliving a dead app peer is a known residue, not a
// covered case.

// overrideExpiryPoll bounds how long the loop will sleep before it re-reads the
// store even with nothing pending.
//
// It is a safety net, not the mechanism. The mechanism is exact — sleep until
// the earliest `expires_at`, re-arm on every commit — and this only bounds the
// damage from a re-arm that was somehow missed (a hook cleared, a commit racing
// the arm). A minute is short enough that the worst case is an alert one minute
// late rather than indefinitely late, and long enough that an idle deployment is
// doing one trivial read a minute.
const overrideExpiryPoll = time.Minute

// overrideExpiryLoop advances the desired-state generation each time a screen
// override's TTL lapses, for the life of ctx.
//
// wake is signalled (non-blockingly, by armOverrideExpiry) whenever the store
// commits, so a freshly-imposed sixty-second alert re-arms the wait immediately
// instead of being noticed at the next poll.
func overrideExpiryLoop(ctx context.Context, st *store.Store, nowMs func() int64, wake <-chan struct{}) {
	timer := time.NewTimer(overrideExpiryPoll)
	defer timer.Stop()
	for {
		wait := overrideExpiryPoll
		if next, err := nextPendingOverrideExpiry(ctx, st, nowMs()); err != nil {
			log.Printf("waiveo-feeder: override expiry: read desired state: %v", err)
		} else if next > 0 {
			// +1ms so the wake lands strictly AFTER the expiry instant: Applies
			// is `ExpiresAt > tMs`, so re-deriving exactly at it would still
			// find the override applying and the loop would arm the same
			// deadline again.
			if d := time.Duration(next-nowMs()+1) * time.Millisecond; d < wait {
				wait = max(d, 0)
			}
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return
		case <-wake:
			continue // a commit changed the rows; recompute the deadline
		case <-timer.C:
		}

		// Re-read rather than trusting the deadline we armed on: between arming
		// and firing an operator may have cleared the override by hand, in which
		// case the clearing write already advanced the generation and a second
		// bump here would nudge every relay in the site for nothing.
		next, err := nextPendingOverrideExpiry(ctx, st, nowMs())
		if err != nil {
			log.Printf("waiveo-feeder: override expiry: read desired state: %v", err)
			continue
		}
		if next > 0 {
			continue // woke early (the poll bound); nothing has lapsed yet
		}
		if !anyOverrideSet(ctx, st) {
			continue // nothing with a TTL is set at all; nothing to publish
		}
		if err := st.AdvanceGeneration(ctx); err != nil {
			log.Printf("waiveo-feeder: override expiry: advance generation: %v", err)
			continue
		}
		log.Printf("waiveo-feeder: a screen override's TTL lapsed; desired-state generation advanced so the fleet re-resolves")
	}
}

// nextPendingOverrideExpiry reads the screen rows and returns the earliest
// override expiry still in the future, or zero when none is.
func nextPendingOverrideExpiry(ctx context.Context, st *store.Store, nowMs int64) (int64, error) {
	ds, err := st.DesiredState(ctx)
	if err != nil {
		return 0, err
	}
	return nextOverrideExpiry(ds.Screens, nowMs), nil
}

// anyOverrideSet reports whether ANY screen row still carries an override with
// an expiry on it.
//
// It is what keeps the loop from bumping the generation forever once a TTL'd
// alert has lapsed: after the lapse, nextPendingOverrideExpiry is zero — the
// same answer it gives for a store that never had an override at all — so
// without this second question the loop would advance the generation on every
// poll for the rest of the process's life, nudging every relay in the site once
// a minute for no change.
//
// It stays true while the LAPSED override is still on the row, which is
// deliberate: one more bump when the operator finally clears it is one nudge for
// a real change, and the alternative (remembering which expiry was last
// published) is state that has to survive a restart to be correct.
func anyOverrideSet(ctx context.Context, st *store.Store) bool {
	ds, err := st.DesiredState(ctx)
	if err != nil {
		return false
	}
	for _, s := range ds.Screens {
		if s.Override != nil && s.Override.ExpiresAt > 0 {
			return true
		}
	}
	return false
}

// armOverrideExpiry returns the wake channel the loop waits on and the
// commit hook that signals it.
//
// The signal is a non-blocking send onto a buffered-by-one channel, for the
// reason the OnCommit doc gives: the hook runs on the WRITING goroutine, so a
// blocking send would couple every api write's latency to this loop's schedule.
// A collapsed burst of commits is correct rather than merely tolerable — the
// loop re-reads the store when it wakes, so one wake after ten writes computes
// the same deadline eleven wakes would.
func armOverrideExpiry() (wake chan struct{}, onCommit func()) {
	wake = make(chan struct{}, 1)
	return wake, func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
