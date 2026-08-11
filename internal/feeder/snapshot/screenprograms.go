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
		programs = append(programs, programFor(screen, state, rowStore, contentBaseURL, rows.ScreenOverrides[screen.ID]))
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
//
// UNLESS this screen carries an active push-now override, in which case
// overrideProgram replaces all of the above (see its own doc). The resolution
// still runs: it costs nothing an override changes and it keeps the per-screen
// DAT-034 degrade uniform — a screen whose effective tz will not resolve is
// omitted from the section whether it is overridden or not, because the reason
// it is omitted (nothing on this side may substitute box-local state) has
// nothing to do with what an operator pushed.
func programFor(screen datamodel.Screen, state datamodel.EffectiveState, rowStore datamodel.RowStore, contentBaseURL string, override store.ScreenOverride) wire.ScreenProgram {
	if override.ScreenID != "" {
		return overrideProgram(screen, rowStore, contentBaseURL, override)
	}
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

// leasePriorityPreempt is the REL-061/PLY-108 `priority` a PUSH-NOW override
// carries: the deliberately-invoked takeover class, as opposed to the
// `scheduled` class ordinary resolution produces.
//
// It is the correct classification rather than a convenient marker, and the
// distinction does real work at two places downstream. A player treats a
// preempt Lease as an immediate interrupt rather than waiting for the current
// item to finish (PLY-100/101), which is what makes "now" mean now on the
// screen and not at the end of a five-minute slide. And the relay refuses to let
// a same-generation schedule resolution overwrite a preempt program
// (playerserver.SetProgram's own priority fence) — without which the relay's
// 30-second re-resolve tick would quietly put the schedule back within half a
// minute of every push.
const leasePriorityPreempt = "preempt"

// overrideProgram projects a screen's active push-now override onto its REL-061
// entry: `display: content`, `priority: preempt`, and the override's cast or
// playlist projected through the SAME two functions ordinary resolution uses
// (castContent / playlistContent), so a cast shown by a push is byte-identical
// to the same cast shown by a schedule.
//
// display is unconditionally `content`, and that is the point of the operation:
// an operator pushing a cast to a screen is saying "show this", which is not a
// statement a daypart's display_power can be allowed to override — a screen the
// schedule had blanked is precisely the screen someone needs to put an emergency
// notice on. (What the SCREEN then does with a preempt Lease arriving over an
// active blank is player/1's own PLY-104 question, decided there, not here.)
//
// An override whose target row has vanished projects to an EMPTY content array
// rather than to the schedule. That is deliberate and is the honest reading of
// the two available answers: the store refuses to write an override naming a
// missing row and refuses to delete a cast a playlist names, so reaching this
// state means the referenced row went away underneath a live override — and
// silently falling back to the schedule would present an operator with a screen
// that looks like their push was never made, while the override is still very
// much in force and will keep winning. An empty preempt program is visibly
// wrong, which is what it is.
//
// It takes no duration override: the item-level `duration_seconds` a playlist
// item can carry (DAT-042) belongs to the playlist item, and a push names a cast
// directly with no item to carry one — so each slide's own `duration_ms` governs,
// falling back to the player's default (slideDurationMS with a zero item
// duration).
func overrideProgram(screen datamodel.Screen, rowStore datamodel.RowStore, contentBaseURL string, override store.ScreenOverride) wire.ScreenProgram {
	var content []wire.ContentRef
	if override.CastID != "" {
		content = castContent(rowStore, override.CastID, 0, contentBaseURL)
	} else {
		content = playlistContent(rowStore, override.PlaylistID, contentBaseURL)
	}
	prog := wire.ScreenProgram{
		ScreenID: screen.ID,
		Priority: leasePriorityPreempt,
		Display:  displayContent,
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
// projectable item, IN AUTHORED ORDER (DAT-041). An unknown or empty playlist
// id, and a playlist with no projectable items, both yield an empty array.
//
// An `asset` item projects to a content-addressed reference; a `slide` item
// (native slide rendering, parity milestone 2) projects to a `content_type:
// "slide"` reference carrying the authored layer stack; a `cast` item
// (data-model/1 DAT-043) projects to ONE such reference PER SLIDE of the
// referenced cast, in authored order (castContent) — the one source under which
// an item is not one-to-one with a played item. A `playable` (pack) item names
// content this contract has no direct reference form for and is skipped, not
// faked — and a slide whose layers do not validate (wire.ValidateSlideLayers,
// applied in resolveLayers) is likewise skipped rather than emitted malformed,
// whether it came from an inline item or a cast.
//
// An item's own `duration_seconds` override (DAT-042) rides onto `duration_ms`
// as seconds*1000 when present and non-zero (REL-061a); an item with no override
// carries no `duration_ms` key at all, per that field's `omitempty`.
//
// For an `asset` item `content_type` is the item's OWN authored value
// (datamodel.PlaylistItem.ContentType), carried through unchanged — which is
// what makes an uploaded video playable rather than a still frame. An item that
// states none carries none, and REL-061a defines an absent content_type as
// meaning `image` (this codebase's own historical implicit value, applied by
// internal/relay/playerserver.SetServedProgram), so every playlist authored
// before that field existed projects byte-identically to before. A `slide`
// item, by contrast, is a distinct item KIND, so it states `content_type:
// "slide"` explicitly — a fact its authored `source` field does contain.
//
// The `url` grammar (`<base>/content/<hex>`) and the empty-origin degrade are the
// same ones every content-serving path in this codebase uses (REL-061/140): with
// no content origin there is no URL to state, and none is fabricated — an image
// layer whose URL cannot be stated fails validation, so a slide referencing
// unfetchable content is dropped rather than shipped with a dead image.
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
			var durationMS int64
			if item.DurationSeconds != nil && *item.DurationSeconds != 0 {
				durationMS = int64(*item.DurationSeconds) * 1000
			}
			if item.Source == datamodel.PlaylistSourceCast {
				content = append(content, castContent(rowStore, item.CastID, durationMS, contentBaseURL)...)
				continue
			}
			if item.Source == sourceSlide {
				layers, ok := resolveSlideLayers(item.Slide, contentBaseURL)
				if !ok {
					// A slide whose layers do not validate (wire.ValidateSlideLayers)
					// is SKIPPED, not emitted malformed: a player has no defined
					// behavior for a bad layer, so a slide that would not draw cleanly
					// never reaches the wire — the same discipline the relay applies
					// when it re-validates on the way to a Lease (playerserver).
					continue
				}
				content = append(content, wire.ContentRef{
					ContentType: contentTypeSlide,
					Layers:      layers,
					ExpiresAt:   contentURLExpiresAt,
					DurationMS:  durationMS,
				})
				continue
			}
			if item.AssetRef == "" {
				continue // a pack `playable` has no direct content reference.
			}
			content = append(content, wire.ContentRef{
				AssetRef: item.AssetRef,
				URL:      contentURL(contentBaseURL, item.AssetRef),
				// The item's own authored `content_type` (DAT-041), carried
				// VERBATIM — including the empty string an item that states none
				// leaves, which REL-061a defines as `image`. Carrying it rather
				// than normalizing keeps this projection a pure function of the
				// authored row: every playlist written before the field existed
				// still marshals with no `content_type` key and therefore still
				// hashes to the same snapshot, while a `video` item reaches the
				// relay as a video and the player plays it instead of trying to
				// draw an MP4 as a Poster.
				ContentType: item.ContentType,
				ExpiresAt:   contentURLExpiresAt,
				DurationMS:  durationMS,
			})
		}
		return content
	}
	return content
}

// castContent expands ONE `source: "cast"` playlist item (DAT-041) into the
// REL-061 content references its cast's slides project to: one reference per
// slide, in authored order, each a `content_type: "slide"` entry carrying that
// slide's own layer stack. A playlist item and a played item are one-to-one for
// every other source; for a cast they are not, and that fan-out is the whole
// point — an operator schedules one cast and a screen plays all of its slides.
//
// itemDurationMS is the referencing playlist item's own `duration_seconds`
// override already converted to ms (0 when it stated none). What each slide's
// dwell time resolves to from there — slide `duration_ms`, then that override,
// then the CAST's own `default_duration_ms`, then nothing at all (the field's
// omitempty, and the player's own default) — is datamodel.SlideDwellMS's rule,
// called rather than restated. This side and internal/relay/schedulehost used to
// hold a private copy each, whose doc admitted they were byte-for-byte equal; a
// screen must see the same dwell times whether it is playing the app-signed
// baseline or the relay's re-resolution of a daypart boundary, and one function
// is the only way that stays true.
//
// An unknown cast id contributes nothing rather than a placeholder. The store
// refuses a playlist naming a cast that does not exist and refuses the deletion
// of one that is named (datamodel.ValidateRows), so this is a degraded-store
// path, and on a degraded store the honest projection of "content that is not
// there" is no content — never an item a screen would stall on.
//
// A slide whose layers do not validate is skipped for the same reason a
// standalone `slide` item is (see resolveSlideLayers): a player has no defined
// behavior for a malformed layer, so a slide that would not draw cleanly never
// reaches the wire. One bad slide costs its own slot and not the whole cast.
func castContent(rowStore datamodel.RowStore, castID string, itemDurationMS int64, contentBaseURL string) []wire.ContentRef {
	out := []wire.ContentRef{}
	if castID == "" {
		return out
	}
	for i := range rowStore.Rows.Casts {
		c := rowStore.Rows.Casts[i]
		if c.ID != castID {
			continue
		}
		for _, slide := range c.Slides {
			layers, ok := resolveLayers(slide.Layers, contentBaseURL)
			if !ok {
				continue
			}
			out = append(out, wire.ContentRef{
				ContentType: contentTypeSlide,
				Layers:      layers,
				ExpiresAt:   contentURLExpiresAt,
				DurationMS:  datamodel.SlideDwellMS(slide, c, itemDurationMS),
			})
		}
		return out
	}
	return out
}

// sourceSlide is the data-model/1 playlist-item `source` value (DAT-041) whose
// content is an authored layer stack rather than a content-addressed asset —
// the native-slide item kind (native slide rendering, parity milestone 2).
const sourceSlide = "slide"

// contentTypeSlide is the REL-061a `content_type` a slide screen-program entry
// carries, matching the player/1 content `type` (PLY-083) the relay stamps onto
// the Lease item it converts this reference into (internal/relay/playerserver's
// own leaseContentTypeSlide). A relay routes an entry through wire.
// ValidateSlideLayers, and onto a slide Lease item, off exactly this value.
const contentTypeSlide = "slide"

// resolveSlideLayers projects an authored slide item's layer stack into the
// wire.Layer slice a REL-061 slide reference carries, or reports that it is not
// projectable. It is the ONE place the feeder side turns authored slide data
// into servable layers, so both halves of that job live together: derive each
// image layer's fetch URL from the content origin (the authored row stores only
// the content-addressed asset_ref, DAT-041, exactly as a plain asset item does),
// then admit the stack ONLY if wire.ValidateSlideLayers accepts it — the single,
// shared gate a relay re-applies (playerserver.SetServedProgram) and the
// schedulehost re-resolver applies too, so no drifting second copy of the rules
// exists. A nil slide, or one whose layers do not validate, is not projectable
// (ok=false) and the caller drops the item.
func resolveSlideLayers(slide *datamodel.Slide, contentBaseURL string) ([]wire.Layer, bool) {
	if slide == nil {
		return nil, false
	}
	return resolveLayers(slide.Layers, contentBaseURL)
}

// resolveLayers is the layer-level half of that job, shared with the cast
// expansion (castContent): a cast's slides carry the same wire.Layer stack an
// inline `slide` item does, and they must reach the wire through the identical
// URL derivation and the identical validation gate — two paths that agreed today
// and diverged later would show an operator a cast slide that renders and an
// inline slide that does not, from the same authored layers.
func resolveLayers(authored []wire.Layer, contentBaseURL string) ([]wire.Layer, bool) {
	layers := make([]wire.Layer, len(authored))
	for i, l := range authored {
		if wire.LayerFetchesContent(l.Kind) {
			// A content-bearing layer's URL — an image's or a video's,
			// wire.LayerFetchesContent naming the pair once — is derived from the
			// content origin, never authored: the same content-URL grammar and
			// empty-origin degrade a plain asset item uses (contentURL). An empty
			// origin leaves the URL empty, which ValidateSlideLayers then rejects,
			// so a slide that could not fetch its bytes is dropped rather than
			// served with a dead URL.
			//
			// Asking the shared predicate rather than testing for `image` inline
			// is what makes adding a content-bearing kind a one-line change in
			// wire instead of a change that has to be remembered here AND in
			// internal/relay/schedulehost — the two projections a screen must
			// never see disagree.
			l.URL = contentURL(contentBaseURL, l.AssetRef)
		}
		layers[i] = l
	}
	if err := wire.ValidateSlideLayers(layers); err != nil {
		return nil, false
	}
	return layers, true
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
