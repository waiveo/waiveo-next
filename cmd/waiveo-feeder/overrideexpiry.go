package main

import (
	"context"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
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
//
// # Publishing each lapse exactly once
//
// `published` is the set of lapses this loop has already announced, as
// screen_id -> the `expires_at` that lapsed. It is the whole guard against a
// nudge storm, and it is a SET rather than a high-water mark because the state
// it is guarding against is not monotonic: a lapsed override stays on its row
// until an operator clears it, so "is anything lapsed?" answers yes forever
// after the first alert and would advance the generation on every poll for the
// rest of the process's life — nudging every relay in the site, once a minute,
// for a change that already happened. (That is not hypothetical: the first cut
// of this loop asked exactly that question and did exactly that, observed on a
// running stack.)
//
// Comparing the PAIR (screen, expires_at) rather than a watermark also gets the
// two neighbouring cases right without a special case for either: an operator
// clearing a lapsed override shrinks the set and announces nothing (their own
// clearing write already advanced the generation), and a fresh TTL'd override on
// the same screen lapses at a different instant and so is a new pair, announced
// once.
//
// It is in-memory and does not survive a restart, deliberately: a feeder that
// comes back up re-announces the current lapses once, which is a single extra
// nudge and is the more conservative answer anyway — it has no way to know what
// the relays applied while it was gone.
func overrideExpiryLoop(ctx context.Context, st *store.Store, nowMs func() int64, wake <-chan struct{}) {
	published := map[string]int64{}
	timer := time.NewTimer(overrideExpiryPoll)
	defer timer.Stop()
	for {
		screens, err := overrideScreens(ctx, st)
		if err != nil {
			log.Printf("waiveo-feeder: override expiry: read desired state: %v", err)
		} else {
			now := nowMs()
			// Announce anything that has lapsed and has not been announced,
			// BEFORE arming the next wait — so a lapse that happened while this
			// loop was asleep (or before it started) is published on the first
			// pass rather than waiting for another deadline.
			lapsed := lapsedOverrides(screens, now)
			if hasUnpublishedLapse(lapsed, published) {
				if aerr := st.AdvanceGeneration(ctx); aerr != nil {
					log.Printf("waiveo-feeder: override expiry: advance generation: %v", aerr)
				} else {
					log.Printf("waiveo-feeder: %d screen override(s) lapsed; desired-state generation advanced so the fleet re-resolves", len(lapsed))
				}
			}
			published = lapsed
		}

		wait := overrideExpiryPoll
		if screens != nil {
			if next := nextOverrideExpiry(screens, nowMs()); next > 0 {
				// +1ms so the wake lands strictly AFTER the expiry instant:
				// Applies is `ExpiresAt > tMs`, so waking exactly at it would
				// find the override still applying and arm the same deadline
				// again.
				if d := time.Duration(next-nowMs()+1) * time.Millisecond; d < wait {
					wait = max(d, 0)
				}
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
		case <-timer.C:
		}
	}
}

// overrideScreens reads the screen rows the two decisions below are made over.
func overrideScreens(ctx context.Context, st *store.Store) ([]datamodel.Screen, error) {
	ds, err := st.DesiredState(ctx)
	if err != nil {
		return nil, err
	}
	if ds.Screens == nil {
		// Distinguish "read fine, no screens" from "did not read": the caller
		// arms its wait only on a successful read, and a nil slice from a
		// screen-less store must not read as a failure.
		return []datamodel.Screen{}, nil
	}
	return ds.Screens, nil
}

// lapsedOverrides returns screen_id -> `expires_at` for every override that HAS
// lapsed at nowMs: it states an expiry and that expiry is at or before now.
//
// At-or-before, matching ScreenOverride.Applies' strict `ExpiresAt > tMs`: an
// override whose expiry equals the instant being judged has already stopped
// applying, so it is a lapse to publish and not one still pending.
func lapsedOverrides(screens []datamodel.Screen, nowMs int64) map[string]int64 {
	out := map[string]int64{}
	for _, s := range screens {
		o := s.Override
		if o == nil || o.ExpiresAt <= 0 || o.ExpiresAt > nowMs {
			continue
		}
		out[s.ID] = o.ExpiresAt
	}
	return out
}

// hasUnpublishedLapse reports whether lapsed contains a (screen, expires_at)
// pair published does not — i.e. whether anything has lapsed that the fleet has
// not already been told about.
//
// It deliberately does NOT fire when lapsed is a strict SUBSET of published: an
// override disappearing from the set means an operator cleared it, and their
// clearing write advanced the generation itself. Announcing it again would be a
// second nudge for one change.
func hasUnpublishedLapse(lapsed, published map[string]int64) bool {
	for screenID, expiresAt := range lapsed {
		if published[screenID] != expiresAt {
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
