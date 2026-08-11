package datamodel

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Error is a data-model/1 validation failure: the contract's own error code (the
// Error taxonomy where it names one; a descriptive reference-impl code for a MUST
// the taxonomy leaves unassigned, e.g. DAT-007's scope_node match) plus the
// offending field and a human message. It doubles as a Go error so later tasks
// (ValidateNoOverlap, Resolve) can return *Error where an error is expected.
type Error struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// RawRows is a bundle of scheduling-core rows as raw JSON, grouped by kind, as
// api/1 CRUD or a relay/1 desired-state snapshot (REL-065) presents them. Raw
// JSON is retained so ValidateRows can detect a row-level pack-identifying field
// (DAT-101) that no typed struct would otherwise surface.
type RawRows struct {
	Playlists       []json.RawMessage
	Casts           []json.RawMessage
	Schedules       []json.RawMessage
	ValidityWindows []json.RawMessage
	Dayparts        []json.RawMessage
	Fallbacks       []json.RawMessage
	PresetBatches   []json.RawMessage
}

// RowSet is the typed, parsed form of a RawRows bundle — the input the scope
// tree and resolution engine consume. ValidateRows returns it alongside any
// validation errors; a row that fails to parse or is pack-owned is reported and
// omitted from the set.
type RowSet struct {
	Playlists       []Playlist
	Casts           []Cast
	Schedules       []Schedule
	ValidityWindows []ValidityWindow
	Dayparts        []Daypart
	Fallbacks       []Fallback
	PresetBatches   []PresetBatch
}

// packOwnershipFields are the row-level pack-identifying field names DAT-101 bars
// from every scheduling-core row. A pack_id nested inside a playlist item
// (DAT-041) is a different, legitimate field and is never a top-level key.
var packOwnershipFields = []string{"pack_id", "owner_pack"}

var validDisplayPower = map[string]bool{"on": true, "off": true, "blank": true}

// validMisfire is the closed misfire vocabulary, byte-exact with rules/1 RUL-350
// (DAT-120).
var validMisfire = map[string]bool{"catch_up_once": true, "skip": true, "fire_each": true}

// ValidateRows parses and validates a complete scheduling-core row bundle,
// returning the typed RowSet and every validation error found. It enforces:
//   - resource-row baseline (DAT-005a): every row's own identity field (id, or
//     a preset-batch's preset_id) is a syntactically valid canonical ULID
//     (ROW_ID_INVALID);
//   - platform ownership (DAT-100/101): no row carries a row-level pack field;
//   - closed vocabularies (DAT-074 display_power, DAT-120 misfire);
//   - row-shape MUSTs (DAT-061 validity range, DAT-071 daypart time-of-day
//     format and ranges via DAYPART_TIME_INVALID and days_of_week grammar via
//     DAYPART_DAYS_INVALID, DAT-091 non-empty commands);
//   - within-schedule daypart partition (DAT-073, DAYPART_OVERLAP via
//     ValidateNoOverlap): a schedule's dayparts MUST NOT overlap;
//   - referential integrity: a daypart/validity-window scope_node equals its
//     owning schedule's scope_node (DAT-007), and every playlist/fallback/
//     preset/schedule id reference resolves to a present row (DAT-050/070/075/080).
//
// It validates a COMPLETE set (every referenced row present), matching the
// relay/1 snapshot / full-tenant model; a dangling reference is a genuine error.
// checkRowPlacement enforces DAT-006's PRESENCE half for a scheduling-core row:
// the row must carry the id of the scope node it is placed under.
//
// DAT-006 says "every row THIS CONTRACT defines other than a scope node itself",
// and DAT-005 enumerates them — playlist, schedule, validity window, daypart,
// fallback, preset batch — so all six require a placement. The identity rows
// (screen, device) require one too and already enforce it via
// checkPlacementAndName; kinds this contract does NOT define (an automation, a
// webhook endpoint, a pack row) are governed by their own contracts and are
// deliberately not covered here.
//
// This is the PRESENCE half, and it is a different fault from the resolution half
// (a scope_node naming a node that does not exist, refused by the store). An
// unplaced row does not dangle — it points nowhere at all — and it fails
// differently: its ancestor chain resolves to nil, so it falls through to the
// workspace-root path and no scope selector finds it where an operator would look.
func checkRowPlacement(scopeNode, kind string) *Error {
	if strings.TrimSpace(scopeNode) != "" {
		return nil
	}
	return &Error{
		Field:   "scope_node",
		Code:    "ROW_SCOPE_NODE_MISSING",
		Message: "a " + kind + " row MUST carry the id of the scope node it is placed under (DAT-005/DAT-006)",
	}
}

// checkDaysOfWeek enforces DAT-071's days_of_week grammar: a non-empty array
// of unique integers 0–6. One error names the first violation found — the
// remedy is the same (fix the array) whichever member is at fault.
func checkDaysOfWeek(days []int) *Error {
	if len(days) == 0 {
		return &Error{Field: "days_of_week", Code: "DAYPART_DAYS_INVALID",
			Message: "days_of_week must be a non-empty array of unique integers 0-6 (0 = Sunday); an empty array stores a daypart that covers nothing (DAT-071)"}
	}
	seen := [7]bool{}
	for _, d := range days {
		if d < 0 || d > 6 {
			return &Error{Field: "days_of_week", Code: "DAYPART_DAYS_INVALID",
				Message: fmt.Sprintf("days_of_week member %d is outside 0-6 (0 = Sunday) (DAT-071)", d)}
		}
		if seen[d] {
			return &Error{Field: "days_of_week", Code: "DAYPART_DAYS_INVALID",
				Message: fmt.Sprintf("days_of_week member %d appears more than once; members must be unique (DAT-071)", d)}
		}
		seen[d] = true
	}
	return nil
}

func ValidateRows(raw RawRows) (RowSet, []Error) {
	var rs RowSet
	var errs []Error

	for _, r := range raw.Playlists {
		if e := checkPackOwnership(r); e != nil {
			errs = append(errs, *e)
			continue
		}
		var v Playlist
		if !decode(r, &v, &errs) {
			continue
		}
		if e := CheckRowID(v.ID, "id"); e != nil {
			errs = append(errs, *e)
		}
		if e := checkRowPlacement(v.ScopeNode, "playlist"); e != nil {
			errs = append(errs, *e)
		}
		errs = append(errs, checkPlaylistItems(v.Items)...)
		rs.Playlists = append(rs.Playlists, v)
	}
	for _, r := range raw.Casts {
		if e := checkPackOwnership(r); e != nil {
			errs = append(errs, *e)
			continue
		}
		var v Cast
		if !decode(r, &v, &errs) {
			continue
		}
		if e := CheckRowID(v.ID, "id"); e != nil {
			errs = append(errs, *e)
		}
		if e := checkRowPlacement(v.ScopeNode, "cast"); e != nil {
			errs = append(errs, *e)
		}
		// A cast is the row an operator PICKS from a list, so an unnamed one is
		// unfindable in the only interface that reaches it. The api layer's
		// declared schema states the same rule for a request body; this is what
		// makes it true of every writer, including a seed and a restore, which
		// never pass through that schema.
		if strings.TrimSpace(v.Name) == "" {
			errs = append(errs, Error{
				Field:   "name",
				Code:    "CAST_NAME_MISSING",
				Message: "a cast row MUST carry a non-empty name (DAT-043)",
			})
		}
		// A cast-wide default is the same rule as a slide's own dwell time, one
		// level up: omitting it says "no default", and a non-positive value is a
		// duration nothing can honour. It is checked beside the name rather than
		// inside checkCastSlides because it is a property of the CAST, not of
		// any slide — reporting it against `slides[0]` would send an operator to
		// the wrong control.
		if v.DefaultDurationMS < 0 {
			errs = append(errs, Error{
				Field:   "default_duration_ms",
				Code:    "CAST_DEFAULT_DURATION_INVALID",
				Message: "a cast's default_duration_ms, when stated, MUST be positive; omit it for no cast-wide default (DAT-043)",
			})
		}
		errs = append(errs, checkCastSlides(v.Slides)...)
		rs.Casts = append(rs.Casts, v)
	}
	for _, r := range raw.Schedules {
		if e := checkPackOwnership(r); e != nil {
			errs = append(errs, *e)
			continue
		}
		var v Schedule
		if !decode(r, &v, &errs) {
			continue
		}
		if e := CheckRowID(v.ID, "id"); e != nil {
			errs = append(errs, *e)
		}
		if v.Misfire != "" && !validMisfire[v.Misfire] {
			errs = append(errs, Error{Field: "misfire", Code: "MISFIRE_INVALID", Message: "misfire must be one of catch_up_once, skip, fire_each (DAT-120)"})
		}
		if e := checkRowPlacement(v.ScopeNode, "schedule"); e != nil {
			errs = append(errs, *e)
		}
		rs.Schedules = append(rs.Schedules, v)
	}
	for _, r := range raw.ValidityWindows {
		if e := checkPackOwnership(r); e != nil {
			errs = append(errs, *e)
			continue
		}
		var v ValidityWindow
		if !decode(r, &v, &errs) {
			continue
		}
		if e := CheckRowID(v.ID, "id"); e != nil {
			errs = append(errs, *e)
		}
		if v.StartsAt != nil && v.EndsAt != nil && *v.EndsAt <= *v.StartsAt {
			errs = append(errs, Error{Field: "ends_at", Code: "VALIDITY_WINDOW_RANGE_INVALID", Message: "ends_at must be strictly greater than starts_at when both are non-null (DAT-061)"})
		}
		if e := checkRowPlacement(v.ScopeNode, "validity window"); e != nil {
			errs = append(errs, *e)
		}
		rs.ValidityWindows = append(rs.ValidityWindows, v)
	}
	for _, r := range raw.Dayparts {
		if e := checkPackOwnership(r); e != nil {
			errs = append(errs, *e)
			continue
		}
		var v Daypart
		if !decode(r, &v, &errs) {
			continue
		}
		if e := CheckRowID(v.ID, "id"); e != nil {
			errs = append(errs, *e)
		}
		if !validDisplayPower[v.DisplayPower] {
			errs = append(errs, Error{Field: "display_power", Code: "DISPLAY_POWER_INVALID", Message: "display_power must be one of on, off, blank (DAT-074)"})
		}
		if v.Misfire != "" && !validMisfire[v.Misfire] {
			errs = append(errs, Error{Field: "misfire", Code: "MISFIRE_INVALID", Message: "misfire must be one of catch_up_once, skip, fire_each (DAT-120)"})
		}
		// DAT-071's write-time refusal: a malformed time must never be STORED,
		// because everything downstream of a stored row assumes it expands — the
		// authoring gate is where a row that cannot expand becomes impossible,
		// rather than a live row whose meaning depends on a runtime refusal
		// firing everywhere it is read.
		if _, err := parseTimeOfDay(v.StartTime); err != nil {
			errs = append(errs, Error{Field: "start_time", Code: "DAYPART_TIME_INVALID", Message: err.Error() + "; start_time must be HH:MM:SS with HH 00-23 and MM/SS 00-59 (DAT-071)"})
		}
		if _, err := parseTimeOfDay(v.EndTime); err != nil {
			errs = append(errs, Error{Field: "end_time", Code: "DAYPART_TIME_INVALID", Message: err.Error() + "; end_time must be HH:MM:SS with HH 00-23 and MM/SS 00-59 (DAT-071)"})
		}
		// DAT-071's days_of_week grammar, refused at write time with the failure
		// directions in mind: an out-of-range weekday fails OPEN (the wrap
		// expansion's (day+1) mod 7 tail lands real coverage on a real weekday
		// the author never named), and an empty array fails SILENT (a daypart
		// covering nothing, on a schedule that looks configured).
		if e := checkDaysOfWeek(v.DaysOfWeek); e != nil {
			errs = append(errs, *e)
		}
		if e := checkRowPlacement(v.ScopeNode, "daypart"); e != nil {
			errs = append(errs, *e)
		}
		rs.Dayparts = append(rs.Dayparts, v)
	}
	for _, r := range raw.Fallbacks {
		if e := checkPackOwnership(r); e != nil {
			errs = append(errs, *e)
			continue
		}
		var v Fallback
		if !decode(r, &v, &errs) {
			continue
		}
		if e := CheckRowID(v.ID, "id"); e != nil {
			errs = append(errs, *e)
		}
		if !validDisplayPower[v.DisplayPower] {
			errs = append(errs, Error{Field: "display_power", Code: "DISPLAY_POWER_INVALID", Message: "display_power must be one of on, off, blank (DAT-074)"})
		}
		if e := checkRowPlacement(v.ScopeNode, "fallback"); e != nil {
			errs = append(errs, *e)
		}
		rs.Fallbacks = append(rs.Fallbacks, v)
	}
	for _, r := range raw.PresetBatches {
		if e := checkPackOwnership(r); e != nil {
			errs = append(errs, *e)
			continue
		}
		var v PresetBatch
		if !decode(r, &v, &errs) {
			continue
		}
		if e := CheckRowID(v.PresetID, "preset_id"); e != nil {
			errs = append(errs, *e)
		}
		if len(v.Commands) == 0 {
			errs = append(errs, Error{Field: "commands", Code: "PRESET_BATCH_COMMANDS_EMPTY", Message: "a preset-batch row's commands array MUST be non-empty (DAT-091)"})
		}
		if e := checkRowPlacement(v.ScopeNode, "preset batch"); e != nil {
			errs = append(errs, *e)
		}
		rs.PresetBatches = append(rs.PresetBatches, v)
	}

	errs = append(errs, validateReferences(rs)...)
	if e := ValidateNoOverlap(rs.Dayparts); e != nil {
		errs = append(errs, *e)
	}
	return rs, errs
}

// checkPlaylistItems enforces the DAT-041 rules that govern one playlist item's
// own members: its `content_type` — the field that decides whether an asset item
// PLAYS as a video or is drawn as a still image (PlaylistItem.ContentType) — and
// the AUTHORED LAYER STACK a `source: "slide"` item carries inline.
//
// It reports EVERY failing item rather than the first, for the same reason
// checkCastSlides does: a playlist is a document an operator edits as a whole,
// and an editor forced to re-submit once per bad item to discover the next one
// is API-013's multi-field answer thrown away.
//
// Three rules, and every one of them exists because the alternative is silence:
//
//   - a stated content_type is one of the closed vocabulary (image/video). An
//     unrecognised value would ride, unaltered, all the way onto the Lease
//     content item's `type`, where the relay's content-type filter (PLY-013,
//     playerserver.filterContentTypes) would drop the item because no player
//     declares that type — a screen showing nothing, with the only evidence
//     buried in a Lease no operator reads. Refusing it at the write is the only
//     place the operator is still holding the thing that is wrong.
//   - content_type is stated only on an `asset` item. A `slide` or `cast`
//     item's type is decided by its SOURCE (both project to `slide` items), and
//     a `playable` has no direct content reference at all, so a content_type on
//     any of those cannot change what the screen plays. Accepting it would
//     store an operator's stated intent that nothing will ever honour — the
//     accepts-work-it-never-performs shape — so it is refused instead.
//   - a `source: "slide"` item's INLINE layer stack passes
//     wire.ValidateAuthoredSlideLayers — the SAME gate checkCastSlides applies
//     to a cast slide's stack, from the same function, never a second copy.
//
// That third rule was missing, and its absence was not a smaller version of the
// cast rule — it was the whole rule. The two shapes are identical downstream:
// store.RowLayerStacks enumerates both, both content projections run both
// through the identical resolveLayers, the retention sweep holds both shapes'
// asset references, and GET /derive/pending queues both. Only the AUTHORING gate
// read one of them. So a layer stack a cast refused with a 422 was accepted with
// a 201 inline, and every consumer downstream then had to cope with a shape the
// authoring surface promises cannot exist: a zero-layer slide, an unknown layer
// kind, geometry off the canvas, a `derive` layer with NO SPEC AT ALL. The last
// one crashed the off-appliance renderer outright (a nil spec dereferenced mid
// pass), taking every other layer's completed work in that pass with it — the
// exact "built the enforcement half, skipped the authoring half" shape this
// package's own comments keep naming.
//
// The reported field is `items[i].slide.layers`, the same JSON path
// store.RowLayerStacks and store.RowAssetReferences build their references
// under, so an operator is pointed at the layer rather than at "the playlist".
//
// Nothing here re-validates `source` itself or the source/field pairing: those
// belong to DAT-041's own rules and are not this function's business. In
// particular a `slide` object hung on a NON-slide item is that rule's business,
// not this one's — but its layers are still checked, because the alternative is
// storing an unvalidatable stack on the strength of a mislabelled source.
func checkPlaylistItems(items []PlaylistItem) []Error {
	var errs []Error
	for i, item := range items {
		if item.Slide != nil {
			if err := wire.ValidateAuthoredSlideLayers(item.Slide.Layers); err != nil {
				errs = append(errs, Error{
					Field:   fmt.Sprintf("items[%d].slide.layers", i),
					Code:    "PLAYLIST_ITEM_SLIDE_LAYERS_INVALID",
					Message: err.Error() + " (DAT-041)",
				})
			}
		}
		if item.ContentType == "" {
			continue
		}
		if item.ContentType != PlaylistContentTypeImage && item.ContentType != PlaylistContentTypeVideo {
			errs = append(errs, Error{
				Field: fmt.Sprintf("items[%d].content_type", i),
				Code:  "PLAYLIST_ITEM_CONTENT_TYPE_INVALID",
				Message: fmt.Sprintf(
					"content_type %q is not one of %s/%s; a player switches its renderer on this value and would be served nothing for an unknown one (DAT-041)",
					item.ContentType, PlaylistContentTypeImage, PlaylistContentTypeVideo),
			})
			continue
		}
		if item.Source != PlaylistSourceAsset {
			errs = append(errs, Error{
				Field: fmt.Sprintf("items[%d].content_type", i),
				Code:  "PLAYLIST_ITEM_CONTENT_TYPE_INVALID",
				Message: fmt.Sprintf(
					"content_type is only meaningful on a %q item; a %q item's content type is decided by its source (DAT-041)",
					PlaylistSourceAsset, item.Source),
			})
		}
	}
	return errs
}

// checkCastSlides enforces DAT-043's slide rules over one cast's slides, and
// reports EVERY failing slide rather than the first — a cast is a document an
// operator edits as a whole, so an editor that had to re-submit once per bad
// slide to discover the next one is the API-013 multi-error answer thrown away.
//
// The rules:
//   - the slides array is non-empty. A cast with no slides plays nothing, and a
//     playlist item referencing it would contribute no content at all: a screen
//     silently showing less than its playlist says, which is the failure mode
//     hardest to notice from the outside.
//   - every slide carries an id, unique within its own cast (never across
//     casts — see CastSlide's own doc for why it is a document-local name).
//   - a stated duration_ms is positive. A zero is the absent value (omitempty
//     writes no key for it) and a negative is a dwell time nothing can honour.
//   - every slide's layers pass wire.ValidateAuthoredSlideLayers — the SHARED
//     slide-layer gate, in its authoring form: identical rules to the one a
//     relay applies before serving, minus the derived image `url` that does not
//     exist yet at authoring time. Reusing it is what makes "a cast the store
//     accepted" and "a slide a player will actually be served" the same set;
//     a private copy of the geometry rules here would drift into accepting
//     slides that are dropped, unexplained, on the way to a screen.
func checkCastSlides(slides []CastSlide) []Error {
	if len(slides) == 0 {
		return []Error{{
			Field:   "slides",
			Code:    "CAST_SLIDES_EMPTY",
			Message: "a cast row's slides array MUST be non-empty; a cast with no slides plays nothing (DAT-043)",
		}}
	}
	var errs []Error
	seen := make(map[string]bool, len(slides))
	for i, s := range slides {
		switch {
		case strings.TrimSpace(s.ID) == "":
			errs = append(errs, Error{
				Field:   fmt.Sprintf("slides[%d].id", i),
				Code:    "CAST_SLIDE_ID_INVALID",
				Message: "every slide MUST carry a non-empty id, unique within its cast (DAT-043)",
			})
		case seen[s.ID]:
			errs = append(errs, Error{
				Field:   fmt.Sprintf("slides[%d].id", i),
				Code:    "CAST_SLIDE_ID_INVALID",
				Message: fmt.Sprintf("slide id %q appears more than once; a slide id MUST be unique within its cast (DAT-043)", s.ID),
			})
		default:
			seen[s.ID] = true
		}
		if s.DurationMS < 0 {
			errs = append(errs, Error{
				Field:   fmt.Sprintf("slides[%d].duration_ms", i),
				Code:    "CAST_SLIDE_DURATION_INVALID",
				Message: "a slide's duration_ms, when stated, MUST be positive; omit it to inherit the playlist item's own duration (DAT-043)",
			})
		}
		if err := wire.ValidateAuthoredSlideLayers(s.Layers); err != nil {
			errs = append(errs, Error{
				Field:   fmt.Sprintf("slides[%d].layers", i),
				Code:    "CAST_SLIDE_LAYERS_INVALID",
				Message: err.Error() + " (DAT-043)",
			})
		}
	}
	return errs
}

// CheckRowID enforces DAT-005a: a row's own identity field — id for every kind
// but preset-batch, whose identity field is preset_id (DAT-005's byte-exact
// exception) — MUST be a syntactically valid canonical ULID. It is the one
// helper shared by every ValidateRows decode loop in this file, by BuildScopeTree's
// per-node loop (scopetree.go), and by the store package's own post-write
// check for automations (internal/app/store/scheduling.go) — automations sit
// outside RawRows/RowSet entirely (they are gated by the rules compiler, not
// ValidateRows), so DAT-005a is enforced identically for all EIGHT resource
// kinds from this one exported implementation, never a second one reimplemented
// at the store layer.
func CheckRowID(id, field string) *Error {
	if !ulid.Valid(id) {
		return &Error{Field: field, Code: "ROW_ID_INVALID", Message: "a row's id (a preset-batch's preset_id) MUST be a syntactically valid canonical ULID (DAT-005a)"}
	}
	return nil
}

// checkPackOwnership rejects a scheduling-core row (any of the six kinds) that
// carries a row-level pack-identifying field (DAT-100/101). Only the row's own
// TOP-LEVEL JSON keys are inspected — a pack_id nested inside a playlist item
// (DAT-041) is never seen here.
func checkPackOwnership(raw []byte) *Error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil // a malformed row is a separate concern surfaced by decode()
	}
	for _, f := range packOwnershipFields {
		if _, ok := obj[f]; ok {
			return &Error{Field: f, Code: "SCHEDULER_ROW_PACK_OWNED", Message: "a scheduling-core row MUST NOT carry a row-level pack-identifying field; a pack's sole route to schedulable content is a playlist item's source: playable reference (DAT-101)"}
		}
	}
	return nil
}

// decode unmarshals one raw row into v, recording a ROW_MALFORMED error and
// returning false on failure.
func decode(raw []byte, v any, errs *[]Error) bool {
	if err := json.Unmarshal(raw, v); err != nil {
		*errs = append(*errs, Error{Field: "", Code: "ROW_MALFORMED", Message: err.Error()})
		return false
	}
	return true
}

// validateReferences enforces DAT-007 (a daypart/validity-window scope_node
// equals its owning schedule's) and id-reference existence (DAT-050/070/075/080)
// across the parsed set.
func validateReferences(rs RowSet) []Error {
	var errs []Error

	scheduleByID := map[string]Schedule{}
	for _, s := range rs.Schedules {
		scheduleByID[s.ID] = s
	}
	playlistIDs := map[string]bool{}
	for _, p := range rs.Playlists {
		playlistIDs[p.ID] = true
	}
	fallbackIDs := map[string]bool{}
	for _, f := range rs.Fallbacks {
		fallbackIDs[f.ID] = true
	}
	presetIDs := map[string]bool{}
	for _, p := range rs.PresetBatches {
		presetIDs[p.PresetID] = true
	}
	// Two maps rather than one, because "the cast is not there" and "the cast is
	// a template" are different refusals with different remedies (DAT-043).
	castIDs := map[string]bool{}
	templateCastIDs := map[string]bool{}
	for _, c := range rs.Casts {
		castIDs[c.ID] = true
		if c.Template {
			templateCastIDs[c.ID] = true
		}
	}

	// A playlist item that names a cast (DAT-041 `source: "cast"`) MUST name one
	// that is present. This is the same referential rule the daypart→playlist and
	// fallback→playlist links already carry, and it earns its place for the same
	// reason from the other direction: because ValidateRows runs over the RESULTING
	// full row-set on every write, it is also what refuses the DELETE of a cast a
	// playlist still plays — otherwise the playlist would keep the reference and
	// the screen would quietly lose those slides from its rotation.
	for _, p := range rs.Playlists {
		for i, item := range p.Items {
			if item.Source != PlaylistSourceCast {
				continue
			}
			if item.CastID == "" || !castIDs[item.CastID] {
				errs = append(errs, Error{
					Field: fmt.Sprintf("items[%d].cast_id", i),
					Code:  "REFERENCE_INVALID",
					Message: fmt.Sprintf(
						"playlist %s item %d declares source %q but its cast_id does not reference an existing cast row (DAT-041/DAT-043)",
						p.ID, i, PlaylistSourceCast),
				})
				continue
			}
			// A TEMPLATE resolves perfectly well — the row is right there — so
			// this is not a REFERENCE_INVALID and must not be reported as one:
			// telling an operator the cast "does not exist" while it sits in
			// their template gallery sends them looking for the wrong problem.
			// The rule itself is DAT-043's: a template exists to be edited as
			// the SOURCE of future casts, so a screen playing one would change
			// every time somebody improved the starting point.
			//
			// Enforced HERE, in the whole-row-set validator, rather than as a
			// create-time guard on the playlist family, for the reason every
			// referential rule in this file is: ValidateRows re-runs over the
			// row-set a write would LEAVE BEHIND, so this equally refuses
			// flipping an already-scheduled cast to `template: true` — which a
			// one-way check at playlist-write time would let straight through.
			if templateCastIDs[item.CastID] {
				errs = append(errs, Error{
					Field: fmt.Sprintf("items[%d].cast_id", i),
					Code:  "CAST_TEMPLATE_NOT_SCHEDULABLE",
					Message: fmt.Sprintf(
						"playlist %s item %d references cast %s, which is marked template: true; create a cast from the template and schedule that (DAT-043)",
						p.ID, i, item.CastID),
				})
			}
		}
	}

	for _, s := range rs.Schedules {
		if s.FallbackID != "" && !fallbackIDs[s.FallbackID] {
			errs = append(errs, Error{Field: "fallback_id", Code: "REFERENCE_INVALID", Message: "schedule fallback_id does not reference an existing fallback row (DAT-050)"})
		}
	}
	for _, f := range rs.Fallbacks {
		if f.PlaylistID != "" && !playlistIDs[f.PlaylistID] {
			errs = append(errs, Error{Field: "playlist_id", Code: "REFERENCE_INVALID", Message: "fallback playlist_id does not reference an existing playlist row (DAT-080)"})
		}
	}
	for _, d := range rs.Dayparts {
		owning, ok := scheduleByID[d.ScheduleID]
		if !ok {
			errs = append(errs, Error{Field: "schedule_id", Code: "REFERENCE_INVALID", Message: "daypart schedule_id does not reference an existing schedule row (DAT-070)"})
		} else if d.ScopeNode != owning.ScopeNode {
			errs = append(errs, Error{Field: "scope_node", Code: "SCOPE_NODE_MISMATCH", Message: "a daypart's scope_node MUST equal its owning schedule's scope_node (DAT-007)"})
		}
		if d.PlaylistID != "" && !playlistIDs[d.PlaylistID] {
			errs = append(errs, Error{Field: "playlist_id", Code: "REFERENCE_INVALID", Message: "daypart playlist_id does not reference an existing playlist row (DAT-075)"})
		}
		if d.PresetBatchID != "" && !presetIDs[d.PresetBatchID] {
			errs = append(errs, Error{Field: "preset_batch_id", Code: "REFERENCE_INVALID", Message: "daypart preset_batch_id does not reference an existing preset-batch row (DAT-075)"})
		}
	}
	for _, w := range rs.ValidityWindows {
		owning, ok := scheduleByID[w.ScheduleID]
		if !ok {
			errs = append(errs, Error{Field: "schedule_id", Code: "REFERENCE_INVALID", Message: "validity-window schedule_id does not reference an existing schedule row (DAT-060)"})
		} else if w.ScopeNode != owning.ScopeNode {
			errs = append(errs, Error{Field: "scope_node", Code: "SCOPE_NODE_MISMATCH", Message: "a validity-window's scope_node MUST equal its owning schedule's scope_node (DAT-007)"})
		}
	}
	return errs
}
