package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screenprograms.go derives the relay/1 `screen_programs` section (REL-061) from
// authored store rows: ONE entry per SCREEN IDENTITY ROW, each resolved through
// the data-model/1 scheduling core at the placement that screen actually sits at.
//
// # Why the screen row, and not the scope node
//
// The three things that used to be called "a screen" are distinct rows: the
// `screen`-kind scope node is a placement CLASSIFICATION (DAT-001), the relay's
// paired screen id is an opaque player/1 credential, and a `screen_id` on the
// wire names a SCREEN IDENTITY ROW (DAT-004a). Only the identity row joins a
// delivered program to a place in the tree, and it is the one this file iterates:
// a screen row may hang off a node of ANY kind (org, site, group, or screen), and
// two screen rows may share one node, so deriving per scope node would both lose
// screens and invent them. The `screen`-kind nodes are not consulted here at all.
//
// # What decides a screen's program
//
// Nothing in this file decides scheduling. The whole per-instant algorithm —
// the DAT-051 applicability cascade up the parent_id chain, the DAT-052 in-force
// test, the DAT-053 strict precedence order, DAT-111 per-instant daypart
// layering, the DAT-117 fallback layer, and the DAT-118 terminal default — lives
// in internal/datamodel (resolve.go) and is called, never re-derived here
// (data-model/1's own "derive, don't re-implement" discipline). This file's job
// is exactly the wiring the section was missing: take a real screen's real
// placement, hand it to that resolver at a stated instant, and project the
// answer onto REL-061's `{screen_id, program_revision, priority, display,
// content}`.
//
// That is also why WHICH schedule applies is never a question this file answers:
// it walks no lists and matches no ids, it passes screen.ScopeNode to
// datamodel.Resolve and the cascade decides. A site-wide schedule therefore
// governs every screen beneath it, and a nearer or higher-priority schedule wins
// over it, with no rule about either restated here.
//
// # Relationship to the relay's own resolver
//
// internal/relay/schedulehost performs the SAME projection continuously, on the
// relay, from the `schedule` section this snapshot also carries — that is what
// re-resolves a daypart boundary without a re-pull. The section derived here is
// therefore the app-authored BASELINE for a generation: correct at the instant
// the generation was built, and superseded per-instant by the relay's own
// resolver for any screen a carried schedule governs. Both sides read the same
// rows through the same engine, so they agree at any shared instant;
// TestDerivedContentMatchesRelaySideProjection pins that.

// leasePriorityScheduled is the REL-061 `priority` every schedule-derived program
// carries. The emergency `preempt` class is a separate, deliberately-invoked
// takeover path (REL-061, player/1 PLY-108) and is never what ordinary schedule
// resolution produces — a screen whose schedule simply says "show this now" is
// `scheduled`, exactly as the relay's own projection classifies it.
const leasePriorityScheduled = "scheduled"

// DeriveScreenPrograms resolves ONE REL-061 screen-program per screen identity
// row in rows.Screens, at absolute instant nowMs (Unix ms), with content URLs
// based at contentBaseURL. Entries come back in screen-row id order (the order
// the store reads them in), so the section is a deterministic function of
// (rows, nowMs, contentBaseURL) and therefore so is the snapshot `hash`
// (REL-053).
//
// nowMs is a PARAMETER and never a wall-clock read: scheduling resolution is
// time-dependent by construction (DAT-111 layers per instant), so the instant a
// generation was resolved at is an input to it, and a caller that wants a
// reproducible generation passes a fixed one.
//
// The returned []datamodel.Error carries per-screen DEGRADES, not a failure of
// the whole section — the same convention internal/relay/schedulehost.BuildStore
// uses for the same kind of input. Today there is exactly one: a screen whose
// effective tz cannot be resolved (no site-kind ancestor with declared geo,
// DAT-034 EFFECTIVE_GEO_UNRESOLVED — reachable by placing a screen row under an
// org node, which carries no geo by rule). Such a screen is OMITTED from the
// section entirely rather than resolved against a substituted zone: DAT-034
// forbids the platform ever standing in box-local state for an unresolved one,
// and a wrong-timezone program is worse than an absent one. The caller logs what
// came back; a well-formed store returns none.
//
// A store with no screen rows yields a non-nil EMPTY section, which REL-060
// explicitly provides for ("an empty array … where a site currently has nothing
// to populate a section with") — never a nil that would marshal as `null`.
func DeriveScreenPrograms(rows store.DesiredStateResult, contentBaseURL string, nowMs int64) ([]wire.ScreenProgram, []datamodel.Error) {
	programs := make([]wire.ScreenProgram, 0, len(rows.Screens))
	if len(rows.Screens) == 0 {
		return programs, nil
	}

	rowStore, errs := resolutionStore(rows)

	for _, screen := range rows.Screens {
		state, err := datamodel.Resolve(rowStore, screen.ScopeNode, nowMs)
		if err != nil {
			// Resolve fails on exactly one thing: an unresolvable effective tz
			// (DAT-033/034). Degrade by omission — see this function's own doc.
			if e, ok := err.(*datamodel.Error); ok {
				errs = append(errs, *e)
			} else {
				errs = append(errs, datamodel.Error{
					Field:   "scope_node",
					Code:    "EFFECTIVE_GEO_UNRESOLVED",
					Message: "screen " + screen.ID + ": " + err.Error(),
				})
			}
			continue
		}
		programs = append(programs, programFor(screen, state, rowStore, contentBaseURL))
	}

	return programs, errs
}

// resolutionStore assembles the datamodel.RowStore the resolution engine reads
// from a desired-state result: the validated scope-node tree plus the typed
// scheduling-core rows. Both inputs have already passed these exact validators at
// write time (internal/app/store's validateAfterWrite), so any error returned
// here means the persisted rows no longer satisfy what accepting them asserted —
// reported, never silently repaired, and the resolution still proceeds over
// whatever validated: a single malformed row must not blank every screen.
func resolutionStore(rows store.DesiredStateResult) (datamodel.RowStore, []datamodel.Error) {
	tree, treeErrs := datamodel.BuildScopeTree(rows.ScopeNodes)
	rowSet, rowErrs := datamodel.ValidateRows(rows.Rows)
	return datamodel.RowStore{Tree: tree, Rows: rowSet}, append(treeErrs, rowErrs...)
}

// programFor projects one screen's already-resolved EffectiveState onto its
// REL-061 entry.
//
//   - display is Resolve's own already-projected Lease `display` (DAT-113–115:
//     on→content, blank→blank, off→blank, and the DAT-118 terminal default's
//     blank). This function does not re-derive that mapping.
//   - content is the effective daypart's or fallback's playlist, projected item
//     by item and in order (playlistContent). A `blank` display carries an empty
//     content array — a screen told to show nothing has nothing to fetch, and
//     REL-060's no-null rule means that array is empty, not absent.
//   - priority is always `scheduled` (leasePriorityScheduled).
//   - program_revision is derived from the projected program itself
//     (programRevisionFor).
func programFor(screen datamodel.Screen, state datamodel.EffectiveState, rowStore datamodel.RowStore, contentBaseURL string) wire.ScreenProgram {
	content := []wire.ContentRef{}
	if state.Display == displayContent {
		content = playlistContent(rowStore, state.PlaylistID, contentBaseURL)
	}
	prog := wire.ScreenProgram{
		ScreenID: screen.ID,
		Priority: leasePriorityScheduled,
		Display:  state.Display,
		Content:  content,
	}
	prog.ProgramRevision = programRevisionFor(prog)
	return prog
}

// displayContent is the REL-061/PLY-093 `display` value naming a powered screen
// showing its playlist — the one value that makes a program carry content. The
// other values (`blank` today) carry none, so this file needs no name for them.
const displayContent = "content"

// playlistContent projects the playlist named by playlistID — the effective
// daypart's or fallback's own `playlist_id`, already resolved onto
// state.PlaylistID by the engine — into REL-061 content references, one per
// asset item, IN AUTHORED ORDER (DAT-041). An unknown or empty playlist id, and
// a playlist with no asset items, both yield an empty array.
//
// Only an `asset` item projects: a `playable` (pack) item names content this
// contract has no direct reference form for and is skipped, not faked.
//
// An item's own `duration_seconds` override (DAT-042) rides onto `duration_ms`
// as seconds*1000 when present and non-zero (REL-061a); an item with no override
// carries no `duration_ms` key at all, per that field's `omitempty`.
//
// `content_type` is deliberately left UNSET. REL-061a defines an absent
// content_type as meaning `image` — this codebase's own historical implicit
// value, applied by internal/relay/playerserver.SetServedProgram — and a
// data-model/1 asset playlist item carries no content-type column to source a
// better answer from, so stating one here would be inventing a fact the authored
// row does not contain.
//
// The `url` grammar (`<base>/content/<hex>`) and the empty-origin degrade are the
// same ones every content-serving path in this codebase uses (REL-061/140): with
// no content origin there is no URL to state, and none is fabricated.
func playlistContent(rowStore datamodel.RowStore, playlistID string, contentBaseURL string) []wire.ContentRef {
	content := []wire.ContentRef{}
	if playlistID == "" {
		return content
	}
	for i := range rowStore.Rows.Playlists {
		p := rowStore.Rows.Playlists[i]
		if p.ID != playlistID {
			continue
		}
		for _, item := range p.Items {
			if item.AssetRef == "" {
				continue // a pack `playable` has no direct content reference.
			}
			var durationMS int64
			if item.DurationSeconds != nil && *item.DurationSeconds != 0 {
				durationMS = int64(*item.DurationSeconds) * 1000
			}
			content = append(content, wire.ContentRef{
				AssetRef:   item.AssetRef,
				URL:        contentURL(contentBaseURL, item.AssetRef),
				ExpiresAt:  contentURLExpiresAt,
				DurationMS: durationMS,
			})
		}
		return content
	}
	return content
}

// contentURL builds a derived content item's fetch URL from the content-origin
// base and a content-addressed asset_ref (`sha256:<hex>`) — byte-identical to
// the form contentRefFor stamps on the fixture path and to the one
// internal/relay/schedulehost derives for the same asset, so there is exactly one
// content-URL grammar on the wire (REL-061). An empty base yields an empty url:
// a screen surfaces that as unresolvable content, which is the honest degrade,
// and no relay-local or app-local origin is ever substituted (REL-066/140).
func contentURL(contentBaseURL, assetRef string) string {
	if contentBaseURL == "" {
		return ""
	}
	return contentBaseURL + "/content/" + strings.TrimPrefix(assetRef, "sha256:")
}

// programRevisionFor derives a screen program's `program_revision` (REL-061)
// from the PROGRAM ITSELF: the hex sha256 of the canonical marshaling of the
// three fields that determine what a screen actually plays — `display`,
// `priority`, and the full ordered `content` array.
//
// The revision is the value a player compares to decide whether to swap what it
// is showing (player/1 PLY-090/108), so it has to satisfy both halves of one
// property: it must NOT change while the delivered program is unchanged, and it
// MUST change whenever the delivered program changes. A digest of the program
// gives both by construction, and gives them for every input that reaches the
// program — an operator adding an item to a playlist, reordering it, retiming an
// item, pointing a daypart at a different playlist, or blanking a screen all
// change these bytes, and a rebuild that changes none of them reproduces the same
// revision exactly.
//
// It deliberately does NOT digest the identity of the winning schedule/daypart
// layer (which is what the relay-side projection names its revisions by): two
// dayparts that deliver a byte-identical program deliver the same program, and
// churning the revision across that boundary would make a player re-swap to what
// it is already showing. It equally deliberately excludes `screen_id`: the
// revision names a program, not a screen, so two screens showing the same thing
// agree — which is a true statement about them, and one a consumer may rely on.
//
// The value is opaque on the wire (REL-061 constrains its form not at all), and
// the whole digest is carried rather than a prefix so no truncation-collision
// question arises.
func programRevisionFor(prog wire.ScreenProgram) string {
	canon, err := json.Marshal(struct {
		Display  string            `json:"display"`
		Priority string            `json:"priority"`
		Content  []wire.ContentRef `json:"content"`
	}{Display: prog.Display, Priority: prog.Priority, Content: prog.Content})
	if err != nil {
		// Unreachable: every field is a string, an int64, or a slice of those.
		// A revision that cannot be derived must not silently collapse to a
		// constant every program would share, so it names its own failure.
		return "unresolved"
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// contentURLExpiresAt is the `expires_at` every content reference this package
// stamps carries. No content-URL TTL policy is defined yet — the content origin
// serves by content address without expiry — so it is 0 on both the derived and
// the fixture path rather than an invented deadline.
const contentURLExpiresAt = 0
