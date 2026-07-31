// Package contentgc reclaims content bytes the workspace no longer uses.
//
// The content origin (internal/feeder/origin) is append-only: an upload writes a
// file named by its own hash and nothing has ever removed one. That is fine for a
// demo and wrong for a box that runs for years — every replaced slide, every
// abandoned upload, every asset dropped from a playlist stays on disk forever,
// and the appliance eventually dies of a full disk with no diagnosis pointing at
// content. This package is the sweep that ends that, and it is written under one
// rule that overrides every other consideration in it:
//
//	Reclaiming content that is still in use is far worse than never reclaiming.
//
// A screen that goes blank in a shop window is a visible, customer-facing failure
// that the platform cannot undo — the bytes are gone. Unbounded growth is a slow,
// observable, recoverable problem. So every ambiguous case here resolves to
// KEEPING the asset, and each guard below says which ambiguity it is resolving.
//
// # What "still in use" means, exactly
//
// Content reaches a screen by exactly one path: a playlist item's `asset_ref`
// (data-model/1 DAT-041), resolved per screen into that generation's screen
// program (internal/feeder/snapshot), signed, pulled by a relay, and handed to a
// screen as a lease whose `url` the screen fetches from this origin DIRECTLY
// (relay/1 REL-061/140 — the relay is never in the data path and holds no asset
// store of its own, REL-141/142). So the reference set is "every asset_ref every
// playlist row names", and the app store is the only place it can be read from.
//
// The hard part is not the current generation. It is that a relay may be serving
// an OLDER one. The store keeps no generation history — a snapshot is rebuilt
// from current rows and cached by generation, one at a time — so the asset set of
// generation N-1 is not recoverable once N exists. An asset dropped from a
// playlist is unreferenced at N and still named by every screen program a relay
// on N-1 is serving. Deleting it blanks those screens.
//
// Since older generations cannot be enumerated, this sweep does not try. It
// requires instead that there BE no older generation in play: content is reclaimed
// only when every relay that could still be serving a program has demonstrably
// applied the very generation whose reference set was just read. Under that
// condition "unreferenced at the current generation" and "unreferenced everywhere"
// are the same statement, and it is a statement about a fact the fleet reported
// rather than about a window somebody guessed.
//
// # The four guards
//
// Every one of them fails toward retention, and none of them subsumes another:
//
//  1. FLEET CONVERGENCE — the fleet floor must be KNOWN and equal to the
//     generation the reference set was read at. Unknown (a relay is enrolled but
//     not connected, or connected but has not acknowledged a generation) reclaims
//     nothing at all: an offline relay's screens keep fetching from this origin,
//     and this process cannot say what program they are on.
//  2. MINIMUM ASSET AGE — bytes younger than MinAssetAgeMs are never reclaimed.
//     An upload is not a schedule: a client uploads an asset and authors the
//     playlist that names it moments — or days — later, and between the two the
//     asset is unreferenced and indistinguishable from garbage. This guard is what
//     makes "upload, then author" safe, and it is measured from the bytes' arrival
//     (which survives a restart as the file's mtime) rather than from anything
//     this process remembers.
//  3. MINIMUM UNREFERENCED AGE — an asset must have been observed unreferenced by
//     an EARLIER sweep, at least MinUnreferencedAgeMs ago. One observation never
//     deletes. This is the drain window for anything already in flight when the
//     reference disappeared — a screen mid-rotation, a lease already handed out —
//     and it is the reason a reference removed at 09:00 is not bytes deleted at
//     09:00.
//  4. PER-RUN CAP — at most MaxReclaimPerSweep assets go in one sweep. It bounds
//     the blast radius of any bug in the three guards above (a sweep that decided
//     wrongly deletes 64 assets, not 4000, and the next sweep is an hour away for
//     someone to notice in), and it bounds how long the app store's write lock is
//     held, since every api write on the box queues behind this sweep.
//
// # Atomicity
//
// The reference read and the deletions happen inside ONE hold of the app store's
// write lock (store.WithContentReferences). Without that, a playlist naming an
// asset can commit in the window between "nothing references this" and the unlink,
// and the platform hands a client a 201 for a playlist that resolves to a 404 on
// every screen. See that method's own doc for the interleaving, and
// api.playlistAssetGuard for the other half — the in-transaction check that turns
// the losing race into an honest 422 instead of a stored dangling reference.
package contentgc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
)

// Default windows. They are generous on purpose: the cost of being wrong in the
// retaining direction is disk, and the cost of being wrong in the reclaiming
// direction is a dark screen nobody can restore.
const (
	// DefaultMinAssetAgeMs is how long bytes must have been in the origin before
	// they are reclaimable at all — a week. It is sized for the human workflow it
	// protects rather than for the machine: an operator uploading a season's
	// artwork on Monday and building the playlists on Thursday must not come back
	// to an empty asset list, and there is no signal anywhere in this platform
	// that distinguishes that operator from an abandoned upload.
	DefaultMinAssetAgeMs = int64(7 * 24 * time.Hour / time.Millisecond)

	// DefaultMinUnreferencedAgeMs is how long an asset must have been CONTINUOUSLY
	// observed unreferenced before it is reclaimable — a day. It is the drain
	// window: a lease already handed to a screen, a render already in flight, a
	// relay that has applied the new generation but whose screens have not yet
	// rotated off the old content. A day is far longer than any of those, which is
	// the point — this guard is not tuned, it is padded.
	DefaultMinUnreferencedAgeMs = int64(24 * time.Hour / time.Millisecond)

	// DefaultMaxReclaimPerSweep bounds one sweep's deletions. With an hourly
	// cadence this reclaims 1536 assets a day, which is far above any real
	// authoring rate and far below "the whole store, instantly, because a guard
	// was wrong".
	DefaultMaxReclaimPerSweep = 64
)

// ContentOrigin is the content store's retention surface: enumerate, and reclaim
// one asset. Deliberately narrower than *origin.Store — a sweep has no business
// reaching Add, Serve or Purge.
type ContentOrigin interface {
	Entries() []origin.Entry
	Remove(hexDigest string) error
}

// ReferenceSource hands a callback the workspace's content references while
// holding the write lock that makes acting on them safe (*store.Store).
type ReferenceSource interface {
	WithContentReferences(ctx context.Context, use func(store.ContentReferences) error) error
	// Generation reads the current desired-state generation WITHOUT holding the
	// write lock, so the fleet oracle can be asked about a specific generation
	// before that lock is taken. See Sweep for why the ordering matters.
	Generation(ctx context.Context) (int64, error)
}

// FleetConverged answers ONE question: has every relay that could still be
// serving this workspace's content applied exactly generation `target`?
//
// It is asked that rather than "what is the lowest generation applied" because a
// floor cannot express the answer. A floor is a minimum, so a fleet where one
// relay sits at the target and another has run AHEAD of it reports the target
// and reads as converged — while the relay in front is serving a program built
// from rows this sweep never read. Asking about a specific generation makes the
// out-of-range relay visible instead of averaged away.
//
// known=false whenever ANY relay that might be serving screens cannot be
// accounted for — enrolled but not connected, connected but never acknowledged a
// generation, or an acknowledgement this process cannot read. It is not an error
// and is not rare; it means this sweep reclaims nothing and the next asks again.
//
// A deployment with NO relay answers (true, true) for every target: no relay
// means no screen is being served, so no older program exists to be broken. That
// case must not be expressed as a generation — answering with a floor of 0 makes
// a relay-less box fail the equality test forever, so it never reclaims and
// never reports why.
type FleetConverged func(target int64) (converged, known bool)

// Config wires a Sweeper. Origin, References and Fleet are required; the windows
// default to the constants above when left zero.
type Config struct {
	Origin     ContentOrigin
	References ReferenceSource
	Fleet      FleetConverged
	NowMs      func() int64

	MinAssetAgeMs        int64
	MinUnreferencedAgeMs int64
	MaxReclaimPerSweep   int
}

// Reason names why an asset survived a sweep. Every asset scanned lands under
// exactly one, so a Result accounts for the whole origin and an operator reading
// a log line can tell "nothing was garbage" from "everything was blocked".
type Reason string

const (
	// ReasonReferenced — a playlist row names it. The only permanent reason.
	ReasonReferenced Reason = "referenced"
	// ReasonFirstObservation — the first sweep to see it unreferenced. One
	// observation never deletes (guard 3).
	ReasonFirstObservation Reason = "first-observation"
	// ReasonTooNew — the bytes are younger than MinAssetAgeMs (guard 2).
	ReasonTooNew Reason = "too-new"
	// ReasonMarkTooRecent — unreferenced, but not for long enough yet (guard 3).
	ReasonMarkTooRecent Reason = "mark-too-recent"
	// ReasonFleetNotConverged — a relay is, or may be, serving another
	// generation (guard 1).
	ReasonFleetNotConverged Reason = "fleet-not-converged"
	// ReasonBatchLimit — eligible, but this sweep's cap was reached (guard 4).
	ReasonBatchLimit Reason = "batch-limit"
	// ReasonRemoveFailed — eligible, and the filesystem refused. The asset is
	// kept and served; the next sweep tries again.
	ReasonRemoveFailed Reason = "remove-failed"
)

// Result is one sweep's accounting.
type Result struct {
	Generation     int64
	FleetConverged bool
	FleetKnown     bool
	Scanned        int
	// PlaylistRows is how many playlist rows the reference set this pass acted on
	// was derived from. Carried out rather than consulted here: no rule inside the
	// pass can act on it (see store.ContentReferences), but a reclamation taken
	// from a zero-row reference set is worth saying out loud, and only the caller
	// has somewhere to say it.
	PlaylistRows   int
	Reclaimed      int
	ReclaimedBytes int64
	Retained       map[Reason]int
	// RemoveErrors are the per-asset failures that produced ReasonRemoveFailed.
	// They are carried rather than returned, because one unremovable asset must
	// not abort the sweep for the rest.
	RemoveErrors []error
}

// Sweeper reclaims unreferenced content on demand. Safe for concurrent use,
// though there is no reason to run two.
type Sweeper struct {
	cfg Config

	// mu guards marks, which is the whole of this type's state.
	mu sync.Mutex
	// marks is hexDigest -> the instant the first CONSECUTIVE sweep observed it
	// unreferenced, cleared the moment it is referenced again.
	//
	// It is in memory, so a restart forgets it and every unreferenced asset must
	// serve its MinUnreferencedAgeMs over again. That is the correct direction to
	// fail in — the failure mode is "retained longer" — and it is why this is not
	// the only thing standing between a fresh upload and deletion: MinAssetAgeMs
	// is measured from the bytes' own on-disk age and survives any number of
	// restarts.
	// marks records, per digest, WHEN it was first observed unreferenced and the
	// store generation that observation was made at. Both halves are needed —
	// see the mark handling in Sweep for why the timestamp alone is unsound.
	marks map[string]mark
}

// New builds a Sweeper, defaulting the windows and rejecting a wiring that could
// not be safe. A nil Fleet is refused rather than defaulted to "converged":
// defaulting it would silently turn the one guard that knows about older
// generations off, and a deployment that forgot to wire it would look identical
// to one that had.
func New(cfg Config) (*Sweeper, error) {
	if cfg.Origin == nil {
		return nil, errors.New("contentgc: no content origin")
	}
	if cfg.References == nil {
		return nil, errors.New("contentgc: no reference source")
	}
	if cfg.Fleet == nil {
		return nil, errors.New("contentgc: no fleet floor; a sweep with no way to ask what generation the fleet is serving cannot reclaim safely")
	}
	if cfg.NowMs == nil {
		cfg.NowMs = func() int64 { return time.Now().UnixMilli() }
	}
	if cfg.MinAssetAgeMs <= 0 {
		cfg.MinAssetAgeMs = DefaultMinAssetAgeMs
	}
	if cfg.MinUnreferencedAgeMs <= 0 {
		cfg.MinUnreferencedAgeMs = DefaultMinUnreferencedAgeMs
	}
	if cfg.MaxReclaimPerSweep <= 0 {
		cfg.MaxReclaimPerSweep = DefaultMaxReclaimPerSweep
	}
	return &Sweeper{cfg: cfg, marks: map[string]mark{}}, nil
}

// Sweep runs one reclamation pass and reports what it did.
//
// An error means the pass could not be DECIDED — the reference set could not be
// read — and in that case nothing was deleted. A per-asset removal failure is not
// an error: it is carried in the Result and the asset is kept.
func (s *Sweeper) Sweep(ctx context.Context) (Result, error) {
	res := Result{Retained: map[Reason]int{}}

	// The fleet is asked BEFORE the store's write lock is taken, and that ordering
	// is deliberate rather than incidental. The oracle is supplied by the
	// deployment and answers by consulting registries this package knows nothing
	// about; one that happened to read the app store — which is a perfectly
	// reasonable thing for an oracle to do — would deadlock against the very lock
	// this sweep is about to hold, on a code path that only runs on a real box.
	//
	// Which means the generation to ask about must also be read first, and then
	// RE-CHECKED under the lock: the answer is only usable if the reference set
	// turns out to have been read at the same generation the fleet was asked
	// about. If the store advanced in between, this sweep reclaims nothing and
	// the next one asks again. That re-check is what closes the window between
	// learning the answer and acting on it, without holding the lock across a
	// callback this package does not control.
	askedGen, genErr := s.cfg.References.Generation(ctx)
	if genErr != nil {
		return res, fmt.Errorf("contentgc: read generation: %w", genErr)
	}
	convergedAtAsked, fleetKnown := s.cfg.Fleet(askedGen)

	err := s.cfg.References.WithContentReferences(ctx, func(refs store.ContentReferences) error {
		// Everything from here to the end of this callback runs under the app
		// store's write lock: no playlist can be authored, patched or deleted
		// while this decides and acts. Keep it bounded (MaxReclaimPerSweep) and
		// call nothing that re-enters the store.
		now := s.cfg.NowMs()

		// The fleet is converged only when EVERY relay has applied EXACTLY the
		// generation this reference set was read at. Not ">=", and not a minimum:
		// a relay on a greater generation is serving a program built from rows
		// this callback never read, so an asset unreferenced in what it saw may
		// be referenced in what it did not. Either direction — ahead or behind —
		// resolves the same way: delete nothing, ask again next sweep.
		//
		// The answer above was about askedGen. It only describes THIS reference
		// set if the generation has not moved since, which is checked here under
		// the lock.
		converged := fleetKnown && convergedAtAsked && askedGen == refs.Generation

		res.Generation = refs.Generation
		res.PlaylistRows = refs.PlaylistRows
		res.FleetConverged = convergedAtAsked
		res.FleetKnown = fleetKnown

		entries := s.cfg.Origin.Entries()
		res.Scanned = len(entries)

		s.mu.Lock()
		defer s.mu.Unlock()

		live := make(map[string]bool, len(entries))
		for _, e := range entries {
			live[e.HexDigest] = true

			if refs.Digests[e.HexDigest] {
				// Referenced: forget any mark. An asset that comes back into use
				// must serve the full unreferenced window again if it later
				// leaves, never resume a clock from the last time it was idle.
				delete(s.marks, e.HexDigest)
				res.Retained[ReasonReferenced]++
				continue
			}

			m, marked := s.marks[e.HexDigest]
			// A mark is only evidence about the window it was taken in. Clearing
			// it on OBSERVING a reference is not enough: a reference created and
			// removed BETWEEN two sweeps is never observed, so the clock would
			// keep running from an observation made before the asset was in use.
			// An asset referenced sixty seconds ago would then be destroyed —
			// while a player still holds a lease naming it, and permanently, so
			// an operator who toggles an asset out of a playlist and back
			// tomorrow finds it gone and must re-upload.
			//
			// Any write that removes a reference advances the generation, so a
			// generation that has moved since the mark means the reference set
			// may have changed in ways this sweeper never saw. The mark is only
			// usable if the generation has not moved since it was taken.
			if marked && m.generation != refs.Generation {
				marked = false
			}
			if !marked {
				s.marks[e.HexDigest] = mark{at: now, generation: refs.Generation}
				res.Retained[ReasonFirstObservation]++
				continue
			}
			markedAt := m.at

			switch {
			case now-e.StoredAtMs < s.cfg.MinAssetAgeMs:
				res.Retained[ReasonTooNew]++
			case now-markedAt < s.cfg.MinUnreferencedAgeMs:
				res.Retained[ReasonMarkTooRecent]++
			case !converged:
				res.Retained[ReasonFleetNotConverged]++
			case res.Reclaimed >= s.cfg.MaxReclaimPerSweep:
				res.Retained[ReasonBatchLimit]++
			default:
				if err := s.cfg.Origin.Remove(e.HexDigest); err != nil {
					res.Retained[ReasonRemoveFailed]++
					res.RemoveErrors = append(res.RemoveErrors,
						fmt.Errorf("contentgc: reclaim %s: %w", e.HexDigest, err))
					continue
				}
				delete(s.marks, e.HexDigest)
				res.Reclaimed++
				res.ReclaimedBytes += int64(e.SizeBytes)
			}
		}

		// Drop marks for digests the origin no longer holds, so this map cannot
		// grow without bound across the life of the process — which would be a
		// small, quiet version of the very problem this package exists to fix.
		for hexDigest := range s.marks {
			if !live[hexDigest] {
				delete(s.marks, hexDigest)
			}
		}
		return nil
	})
	if err != nil {
		return Result{Retained: map[Reason]int{}}, err
	}
	return res, nil
}

// mark is one digest's unreferenced observation: when it was taken, and the
// store generation it was taken at.
//
// The generation is what makes the window mean what it says. Without it the
// mark records only that the asset was unreferenced at some past instant, and
// a reference that appeared and vanished between two sweeps leaves no trace —
// so the elapsed window would be measured from before the asset was last in
// use rather than from when it stopped being used.
type mark struct {
	at         int64
	generation int64
}
