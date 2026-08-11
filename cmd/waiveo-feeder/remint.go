package main

import (
	"context"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// remint.go PUBLISHES the content-URL re-mint.
//
// It is the delivery half of the bound desiredStateSource.current() enforces,
// and without it that bound stops at the feeder's own memory.
//
// # The gap this closes
//
// A snapshot build mints every app-authored content URL as a signed capability
// that expires (contenturl.SnapshotTTL). desiredStateSource caps its cache at
// contenturl.SnapshotRemintInterval so it can never SERVE a generation whose
// URLs are more than half dead — correct, and not sufficient, because a rebuilt
// snapshot at an unchanged generation reaches nobody:
//
//   - relayconn answers `state.unchanged`, with no snapshot body at all, when a
//     pull's `since_generation` equals the current one (REL-050/051);
//   - the relay's live pull no-ops on a generation not strictly greater than the
//     one it last applied (cmd/waiveo-relay's rePuller.tick);
//   - and that tick has no timer of its own — it fires on a `state.changed`
//     nudge or on reconnect, and the nudge comes only from the store's
//     post-commit hook, i.e. from a real write.
//
// So on a site nobody authors, the freshly minted snapshot sat in the feeder
// while every relay went on serving the URLs it pulled at T. The relay passes an
// app-authored `url` through to the Lease unmodified (playerserver's
// SetServedProgram), and its own serve-time re-mint covers only a
// single-governed-node, single-screen, unpinned site (soleServedScreenID) — a
// pinned push-now override is passthrough by design, which is the exact path
// HV-1 was found on. At T+SnapshotTTL every image and video on that screen 403s,
// and only a relay restart (whose boot pull asks `since=0`) recovers it.
//
// # Why the generation, and why that is not a lie about what changed
//
// The generation is the ONE thing every existing delivery mechanism reacts to,
// which is why store.AdvanceGeneration exists and why overrideexpiry.go already
// reaches for it: "the desired state changed and no write announced it". A
// re-mint is the same shape. The desired state's CONTENT is unchanged, but the
// bytes a relay must hold to keep serving it are not, and there is no narrower
// announcement on the wire than the generation.
//
// The apply on the relay side is therefore real work, not a no-op: REL-070's
// same-hash suppression does not swallow it, because the snapshot's hash covers
// the URLs and a re-mint changes them by construction.
//
// # The coupling that makes this affordable, which is load-bearing
//
// Nudging every relay twice a day is only acceptable because it costs no screen
// its rotation. snapshot.programRevisionFor digests the program through
// revisionContent, which strips `exp` and `sig` from every minted URL and drops
// `expires_at` — so a re-mint of the same program reproduces the SAME
// program_revision, and a player (PLY-090/108) does not treat the re-applied
// program as a new one. If that reduction were ever removed, this loop would
// restart every screen in the site every twelve hours; the two are pinned
// together by TestARemintReachesTheFleetWithoutRestartingAScreen here and by
// snapshot.TestRebuildingTheSameProgramReproducesItsRevision next door.
//
// # Why unconditional, rather than "only if nothing else advanced it"
//
// Skipping a tick when an authoring write already advanced the generation
// recently looks like a saving and is a regression: the tick is anchored to the
// LOOP, not to the write, so a write at T+11h followed by a skipped tick at T+12h
// leaves the fleet holding URLs minted at T+11h until T+24h — 13 hours of the
// TTL spent instead of 12, and the margin the sizing argument rests on gone. An
// unconditional advance keeps the invariant flat and stateless: every relay is
// nudged at least once per interval, so the URLs it holds always have at least
// SnapshotTTL - SnapshotRemintInterval of life left in them.
//
// The cost is two extra snapshot pulls per relay per day, and one row-less write
// transaction per interval on the feeder.

// remintLoop advances the desired-state generation every `every`, for the life
// of ctx, so the fleet re-pulls a freshly minted snapshot even on a site nobody
// is authoring.
//
// It is deliberately separate from overrideExpiryLoop: that loop publishes a
// change in what the desired state SAYS, on an instant the rows themselves name,
// and this one publishes a change in how long the current answer stays fetchable.
// Both call AdvanceGeneration, and each waking the other through the commit hook
// is harmless — the expiry loop re-reads and finds nothing new to publish, and
// this loop's cadence is its own.
func remintLoop(ctx context.Context, st *store.Store, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.AdvanceGeneration(ctx); err != nil {
				// Non-fatal: the next tick tries again, and the snapshot cache's
				// own window (cacheWindowEnd) still refuses to serve a stale
				// build to anything that does pull. Logged because a feeder
				// that cannot write is a feeder whose fleet is quietly ageing
				// toward a 403.
				log.Printf("waiveo-feeder: content-url re-mint: advance generation: %v", err)
				continue
			}
			log.Printf("waiveo-feeder: content-url re-mint due (every %s); desired-state generation advanced so every relay pulls freshly minted content urls", every)
		}
	}
}
