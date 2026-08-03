// Package desiredstate implements the relay's verify/apply half of relay/1
// desired-state movement (REL-050–056): VERIFYING a received
// `state.snapshot` against the relay's persisted, enrollment-anchored
// trust anchor (REL-071, `#28`), enforcing generation monotonicity
// (REL-052), and persisting `{generation, hash, screen_programs}` as
// last-applied (REL-055, internal/relay/identity). The bytes arrive over
// the persistent connection (internal/relay/relayconn's state.pull —
// relayconn.SnapshotFromFrame hands VerifyAndApply exactly the pair it
// needs); this package owns no transport of its own.
//
// Only feeder-signed state applies: a snapshot whose `sections` doesn't
// hash to its own `hash`, or whose `signature` doesn't verify under the
// persisted `desired_state_verification_key`, is rejected outright —
// nothing is applied and last-applied is left untouched. This is the
// security-load-bearing gate the player/1 server sits behind:
// VerifyAndApply's returned Applied value is the only screen-program state
// that ever reaches a screen.
package desiredstate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Errors VerifyAndApply returns for each of relay/1's typed rejection
// reasons. All are checked with errors.Is, and each leaves the persisted
// last-applied generation untouched — no section is EVER applied on any of
// these paths.
var (
	// ErrNoTrustAnchor is returned when the store holds no persisted
	// desired_state_verification_key yet (the relay has not enrolled) —
	// there is nothing to verify a snapshot's signature against.
	ErrNoTrustAnchor = errors.New("desiredstate: no desired_state_verification_key persisted — relay must enroll before applying desired state")

	// ErrSnapshotHashMismatch is returned when a pulled snapshot's `hash`
	// does not equal sha256 over its own `sections` (REL-053) — the
	// snapshot is internally inconsistent (sections tampered with, or
	// corrupted in transit) and is rejected outright, before signature
	// verification is even attempted.
	ErrSnapshotHashMismatch = errors.New("desiredstate: snapshot hash does not match its sections")

	// ErrSnapshotSignatureInvalid is returned when a pulled snapshot's
	// `signature` does not verify under the persisted
	// desired_state_verification_key trust anchor (REL-071) — relay/1's
	// SNAPSHOT_SIGNATURE_INVALID (REL-072). This is the security property:
	// only feeder-signed state (signed by the exact key learned at
	// enrollment) ever applies.
	ErrSnapshotSignatureInvalid = errors.New("desiredstate: snapshot signature did not verify against the persisted trust anchor (SNAPSHOT_SIGNATURE_INVALID)")

	// ErrGenerationRegressed is returned when a pulled snapshot's
	// `generation` is lower than the relay's persisted last-applied
	// generation (REL-052) — desired-state generations are monotonically
	// non-decreasing; a lower one is rejected outright, never applied.
	ErrGenerationRegressed = errors.New("desiredstate: snapshot generation is lower than the persisted last-applied generation")

	// ErrSectionsIncomplete is returned when a pulled snapshot's `sections`
	// object omits any one of the seven REL-060 keys — every snapshot MUST
	// carry all seven, present even when empty. This is a structural gate on
	// the raw wire bytes that fires BEFORE hash/signature verification (a
	// Go-decoded Sections struct always materializes all seven fields, so an
	// omission is only observable in the original JSON), and like every
	// rejection here it leaves the persisted last-applied generation
	// untouched — nothing is applied.
	ErrSectionsIncomplete = errors.New("desiredstate: snapshot sections is missing a required REL-060 key")
)

// Applied is the relay's locally-applied Wave-1 first-photon desired-state
// result: the one screen-program's one image content item a later player/1
// server (Task 9, and Task 10's program delivery) serves to the screen,
// plus the generation it came from. The zero value (Applied{}) is what
// VerifyAndApply returns alongside every rejection error above — never a
// partially-populated value.
//
// Priority and Display carry the applied screen-program's own
// `priority`/`display` fields (REL-061) UNMODIFIED — a later player/1
// Lease's own same-named fields MUST reflect these exactly (`player/1`
// PLY-108/109), which is the mechanism by which a preempt/blank assignment
// reaches a screen through this relay's own offline-cached last-applied
// snapshot, without requiring a live app-peer connection at the moment a
// screen needs it.
//
// PairingGrants carries the verified snapshot's sections.pairing_grants
// (REL-067) unmodified — the pairing server (internal/relay/playerserver)
// resolves a PairingRequest's grant_selector against these, exactly as
// REL-121/REL-126 require. Because this whole struct is only ever produced
// by an already-hash-and-signature-verified snapshot, a caller holding an
// Applied value can trust its PairingGrants exactly as much as it trusts
// the rest of the struct — no separate verification step applies here.
// EdgeRules carries the verified snapshot's sections.edge_rules.rules
// (REL-062) unmodified — the raw rules/1 authored-rule JSON objects the
// feeder signed, which internal/relay/automationhost compiles + loads into
// the edge engine (Task 2). Because this whole struct is only ever produced
// by an already-hash-and-signature-verified snapshot, EdgeRules rides the
// SAME trust as everything else here — there is no separate verification
// step for it, exactly as REL-062's signed-section discipline requires.
type Applied struct {
	Generation int64
	// Hash is the snapshot's own `hash` (REL-053), verified against a recompute
	// over its sections before anything here was extracted.
	//
	// It is carried because REL-070 states the idempotent-apply no-op in terms of
	// HASH equality and says so holds "regardless of whether `generation` itself
	// advanced". A caller fencing on generation alone cannot implement that rule —
	// the value it is stated in terms of never reaches the decision. That was the
	// gap: the hash was verified here and persisted by ApplyGeneration, then
	// dropped on the way out, so the one consumer that decides whether to re-run
	// apply-time side effects had only the generation to go on.
	Hash            string
	ScreenID        string
	ProgramRevision string
	Priority        string
	Display         string
	Image           wire.ContentRef
	PairingGrants   []wire.PairingGrant
	EdgeRules       []json.RawMessage

	// ScreenPrograms is the verified snapshot's full sections.screen_programs
	// array (REL-061), carried unmodified — every entry's priority/display/
	// content preserved opaquely for a later player/1 program delivery
	// (Task 2), which serves this persisted copy offline without a live
	// app-peer connection. The convenience fields above (ScreenID, Priority,
	// …) mirror ScreenPrograms[0] for the Wave-1 single-program case; this
	// array is the complete source of truth.
	ScreenPrograms []wire.ScreenProgram

	// Revoked is the verified snapshot's
	// sections.revocation_and_site.revoked (REL-066), carried unmodified —
	// the opaque identifier strings the relay's own player-credential
	// authority MUST treat as revoked. REL-123 makes this the relay's LAST-
	// SYNCED copy: it is enforced against every credential decision the relay
	// makes regardless of whether its app peer is reachable at that moment,
	// which is only possible because it rides the persisted, verified snapshot
	// like every other section rather than being asked for per decision.
	//
	// Carried as the snapshot decoded it, including nil. A nil slice and an
	// empty one mean the same thing to every consumer here — this generation
	// revokes nothing — and normalizing would be inventing a distinction the
	// section does not draw. What consumers MUST NOT do is treat an empty list
	// as "no information": REL-066 requires the key on every snapshot, so an
	// empty one is the app peer positively stating that nothing is revoked,
	// and a screen that was revoked in an earlier generation and is absent
	// here is thereby UN-revoked (playerserver.Server.SetRevokedScreens).
	Revoked []string

	// SiteEffective is the verified snapshot's
	// sections.revocation_and_site.site_effective (REL-066), carried
	// unmodified — the site's persisted {tz, lat, long} a relay's dayparting
	// and sun/time evaluation apply verified wall-clock time against, so they
	// stay correct across a restart from the persisted snapshot alone without
	// first completing a fresh hello.
	SiteEffective wire.SiteEffective

	// ContentOrigin is the verified snapshot's
	// sections.revocation_and_site.content_origin (REL-061/066), carried
	// unmodified — the base URL a screen fetches this site's content from,
	// which a later relay-side schedule resolver (internal/relay/schedulehost)
	// threads into each schedule-resolved content item's `url`
	// (`ContentOrigin + "/content/" + hex(asset_ref)`), the SAME URL grammar
	// snapshot.Build already uses for the app-authored path (REL-061). An
	// empty ContentOrigin means the feeder carried no content origin — a
	// resolver leaves resolved content items without a url rather than
	// fabricating a relay-local one (REL-140: relay never in the content path).
	ContentOrigin string

	// ContentURLKey is the verified snapshot's
	// sections.revocation_and_site.content_url_key (REL-066a), decoded from
	// base64 — the key this relay MINTS signed content URLs with, for every
	// item it serves alike (REL-066d).
	//
	// Empty means the app peer delivered no key, and the relay serves unsigned
	// URLs: the behaviour every deployment had before the field existed. It is
	// credential material (REL-066b) — never logged, never in telemetry, never
	// served to a screen, which receives minted URLs and never what mints them.
	ContentURLKey []byte

	// Schedule is the verified snapshot's sections.schedule (REL-065),
	// carried unmodified — the scheduling-core rows + scope nodes the feeder
	// signed, carried opaquely (raw JSON per row) for a later relay-side
	// resolver (internal/relay/schedulehost) to unmarshal into data-model/1's
	// own row types and derive a dayparting timeline from. Like every field
	// here it is only ever produced by an already-hash-and-signature-verified
	// snapshot, so it rides the SAME trust as everything else — there is no
	// separate verification step for it, exactly as REL-065's signed-section
	// discipline requires. An empty-but-typed schedule (today's first-photon
	// state) carries all seven arrays present and empty.
	Schedule wire.ScheduleSection
}

// ServedProgram returns the relay's persisted last-applied screen_programs
// (REL-061) from store, decoded — the relay's OFFLINE serve path. Its sole
// input is the durable operational store: it performs no network I/O and
// contacts no app peer, so a relay disconnected from its app peer continues
// serving a screen's program purely from its own last-applied, durably
// persisted desired-state snapshot (REL-055/061). A relay that has never
// applied a generation returns an empty slice (the REL-060 empty
// placeholder), not an error.
//
// The returned entries carry priority/display/content EXACTLY as the verified
// snapshot applied them — an emergency-takeover `preempt`/`content`
// assignment reaches a screen through the relay's own offline continuity
// without requiring a live app-peer connection at the moment the screen needs
// it (opaque carriage, REL-061/062/065).
func ServedProgram(store *identity.Store) ([]wire.ScreenProgram, error) {
	if store == nil {
		return nil, fmt.Errorf("desiredstate: ServedProgram: store must not be nil")
	}
	raw, err := store.LastAppliedScreenPrograms()
	if err != nil {
		return nil, fmt.Errorf("desiredstate: ServedProgram: read persisted screen_programs: %w", err)
	}
	var programs []wire.ScreenProgram
	if err := json.Unmarshal(raw, &programs); err != nil {
		return nil, fmt.Errorf("desiredstate: ServedProgram: decode persisted screen_programs: %w", err)
	}
	return programs, nil
}

// ServedRevocation returns the relay's persisted last-applied
// `revocation_and_site.revoked` set (REL-066) from store, decoded — the
// revocation counterpart of ServedProgram, and read on the same OFFLINE boot
// path for the same reason. Its sole input is the durable operational store: it
// performs no network I/O and contacts no app peer, which is what makes REL-123
// enforceable "regardless of connectivity" across a restart, not merely across
// a disconnection the process survived. A relay that has never applied a
// generation returns an empty slice (the REL-060 empty placeholder), not an
// error.
//
// The two reads are deliberately symmetric: a boot that installs its persisted
// screen_programs without also installing the revocation set that was applied
// in the SAME generation serves those programs to credentials the app peer has
// already voided — the relay's own durable state disagreeing with itself about
// one generation.
func ServedRevocation(store *identity.Store) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("desiredstate: ServedRevocation: store must not be nil")
	}
	raw, err := store.LastAppliedRevokedScreens()
	if err != nil {
		return nil, fmt.Errorf("desiredstate: ServedRevocation: read persisted revoked: %w", err)
	}
	var revoked []string
	if err := json.Unmarshal(raw, &revoked); err != nil {
		return nil, fmt.Errorf("desiredstate: ServedRevocation: decode persisted revoked: %w", err)
	}
	return revoked, nil
}

// extractApplied builds VerifyAndApply's returned Applied from a verified
// snapshot's sections. ScreenPrograms carries the whole section; the flat
// convenience fields (ScreenID, ProgramRevision, Priority, Display, Image)
// mirror entry [0] for the single-screen callers that predate the section being
// per-screen.
//
// It refuses NOTHING structural about screen_programs, and deliberately so.
// Both shapes it used to refuse are now states the app peer legitimately
// derives and REL-060/061 legitimately describe:
//
//   - an EMPTY screen_programs array is REL-060's own stated "empty array where
//     a site currently has nothing to populate a section with" — a deployment
//     with no screen rows yet. The flat fields stay zero and the relay serves
//     nothing to nobody, which is the correct answer, not a malformed snapshot.
//   - a screen-program with EMPTY content is what a `display: blank` assignment
//     IS: a screen the schedule says to blank (data-model/1 DAT-114/115) or one
//     the terminal default resolves for (DAT-118) is powered on and showing
//     nothing, so it has nothing to fetch. Refusing that would have made a
//     relay reject every generation built during an overnight blank daypart —
//     an outage exactly when the schedule was working correctly.
//
// A genuinely malformed snapshot is still caught: hash and signature
// verification run before this, and every typed rejection reason relay/1 owns
// is raised there.
func extractApplied(generation int64, hash string, sections wire.Sections) (Applied, error) {
	var prog wire.ScreenProgram
	if len(sections.ScreenPrograms) > 0 {
		prog = sections.ScreenPrograms[0]
	}
	var image wire.ContentRef
	if len(prog.Content) > 0 {
		image = prog.Content[0]
	}

	return Applied{
		Generation:      generation,
		Hash:            hash,
		ScreenID:        prog.ScreenID,
		ProgramRevision: prog.ProgramRevision,
		Priority:        prog.Priority,
		Display:         prog.Display,
		Image:           image,
		PairingGrants:   sections.PairingGrants,
		EdgeRules:       sections.EdgeRules.Rules,
		ScreenPrograms:  sections.ScreenPrograms,
		Revoked:         sections.RevocationAndSite.Revoked,
		SiteEffective:   sections.RevocationAndSite.SiteEffective,
		ContentOrigin:   sections.RevocationAndSite.ContentOrigin,
		ContentURLKey:   decodeContentURLKey(sections.RevocationAndSite.ContentURLKey),
		Schedule:        sections.Schedule,
	}, nil
}

// decodeContentURLKey decodes the base64 content-URL key a snapshot carries
// (REL-066a), yielding nil for an absent or unusable value.
//
// A malformed key is treated as ABSENT rather than as an error that refuses the
// snapshot. Refusing would let one bad field take down a relay's entire desired
// state — schedules, revocations, pairing grants and all — over a field whose
// own absence is explicitly conformant. Degrading instead means content URLs go
// unsigned, which the origin then refuses, which surfaces as unfetchable content
// on a screen rather than as a relay that will not apply anything.
func decodeContentURLKey(b64 string) []byte {
	if b64 == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	return key
}
