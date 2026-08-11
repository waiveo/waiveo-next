// Package datamodel is the reference Go implementation of data-model/1's
// scheduling core (contracts/data-model-1.md): the six platform-owned row kinds
// (playlist, schedule, validity-window, daypart, fallback, preset-batch), their
// validation and platform-ownership guard (DAT-005–008/040/050/060/070–101), and
// — in later files of this package — the scope-node tree, DST-correct daypart
// holding, and the per-instant cross-schedule resolution engine that projects an
// effective daypart to a player/1 Lease and fires preset batches on rising edges.
//
// This file defines the row structs with exact Wire-shape `json` tags. The
// scheduling-core rows are STATES (a daypart is continuous membership, not a
// one-shot event); the resolution engine treats them accordingly.
package datamodel

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Playlist is a playlist row (DAT-040): the resource-row baseline (DAT-005–008)
// plus name and an ordered items array.
type Playlist struct {
	ID         string            `json:"id"`
	ScopeNode  string            `json:"scope_node"`
	Name       string            `json:"name"`
	Items      []PlaylistItem    `json:"items"`
	ExternalID string            `json:"external_id,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Revision   int               `json:"revision"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
}

// PlaylistItem is one entry of a playlist's items array (DAT-041). `source` is
// `asset` (asset_ref present), `playable` (pack_id + content_id present),
// `slide` (a `slide` object carrying one authored layer stack — native slide
// rendering, parity milestone 2), or `cast` (a `cast_id` naming an authored
// multi-slide Cast row, DAT-043). A nested pack_id here names the item's content
// pack and is NOT a row-level pack-ownership field (DAT-101 bars only the latter).
//
// Slide is set ONLY for a `source: "slide"` item and is `omitempty`, so every
// pre-existing `asset`/`playable` item — which never populates it — marshals
// with no `slide` key at all, byte-identical to before this field existed. A
// slide item carries no asset_ref of its own: its content is the layer stack,
// and an image layer inside it names content-addressed bytes exactly as a plain
// asset item's asset_ref does (DAT-041 discipline), resolved to a fetch URL at
// projection time (internal/feeder/snapshot and internal/relay/schedulehost).
//
// CastID is set ONLY for a `source: "cast"` item, likewise `omitempty`. It is a
// REFERENCE where Slide is an inline literal, and that is the whole difference
// between the two: a cast is the unit an operator authors once and schedules in
// several places, so editing it must change every screen playing it, which an
// inlined copy per playlist could never do. One cast item expands, at projection
// time, into ONE CONTENT ITEM PER SLIDE in authored order — a playlist item and
// a played item are not one-to-one for this source.
//
// ContentType is what an `asset` item's bytes ARE — `image` or `video` — and it
// is the field that makes a video schedulable at all.
//
// # Why a field, and why on the item
//
// Before it existed, an asset item projected to a content reference with no
// content_type, which every consumer resolves as `image` (REL-061a's stated
// default, applied by internal/relay/playerserver.SetServedProgram). So an
// operator could upload an MP4, reference it from a playlist, get a 201, and
// watch a screen try to draw it as a still Poster: a surface that accepted the
// work and never performed it. Nothing anywhere in the authored rows carried
// the answer, so no projection could have derived one.
//
// The alternative — SNIFFING the stored bytes at projection time — was
// rejected. The content origin is content-addressed and deliberately holds no
// metadata (no filename, no MIME type: internal/feeder/origin), the relay's own
// re-resolution never has the bytes at all (it holds only the schedule section,
// internal/relay/schedulehost), and the two projections MUST agree item for
// item. A field the operator states is the only answer both sides can read, and
// it is also the honest one: whether an asset should PLAY as a video is an
// authoring decision, not a property recoverable from a container header.
//
// It is `omitempty` and only meaningful for `source: "asset"`. An item that
// states none marshals with no `content_type` key at all — byte-identical to
// every playlist authored before this field existed — and is projected as
// `image`, this codebase's own historical implicit value. A `slide` or `cast`
// item's content type is decided by its SOURCE, not by this field, so stating
// one on those is a validation error rather than a silently ignored member (see
// checkPlaylistItems).
type PlaylistItem struct {
	Source          string `json:"source"`
	AssetRef        string `json:"asset_ref,omitempty"`
	ContentType     string `json:"content_type,omitempty"`
	PackID          string `json:"pack_id,omitempty"`
	ContentID       string `json:"content_id,omitempty"`
	DurationSeconds *int   `json:"duration_seconds,omitempty"`
	Slide           *Slide `json:"slide,omitempty"`
	CastID          string `json:"cast_id,omitempty"`
}

// PlaylistSourceAsset is the DAT-041 playlist-item `source` value whose content
// is a single content-addressed asset — the only source PlaylistItem.ContentType
// applies to.
//
// PlaylistContentTypeImage and PlaylistContentTypeVideo are that field's CLOSED
// vocabulary, and they are byte-identical to player/1's own content-type floor
// (PLY-014: every conformant player declares at least `image` and `video`) —
// the value rides unchanged from this row, through both content projections,
// onto the Lease content item's `type` a player switches its renderer on. There
// is deliberately no translation step anywhere along that path, so these three
// constants are the whole vocabulary.
//
// They are exported here, on the contract's reference implementation, for the
// same reason PlaylistSourceCast is: three independent components must agree on
// these strings — the row validator and BOTH projections — and the consequence
// of one disagreeing is not a compile error but a screen quietly showing a
// frozen first frame instead of a playing video.
const (
	PlaylistSourceAsset      = "asset"
	PlaylistContentTypeImage = "image"
	PlaylistContentTypeVideo = "video"
)

// PlaylistSourceCast is the DAT-041 playlist-item `source` value whose content
// is an authored Cast row referenced by `cast_id`.
//
// It is exported and lives HERE, on the contract's own reference implementation,
// rather than being spelled as a private constant in each place that recognizes
// it. Three independent components have to agree on this one string — the row
// validator that resolves the reference, and BOTH content projections that
// expand it (internal/feeder/snapshot and internal/relay/schedulehost) — and the
// consequence of one of them disagreeing is not a compile error: the projection
// simply stops recognizing cast items and a screen silently plays fewer slides
// than its playlist says. The pre-existing `slide` source predates this and is
// still spelled locally in each projection; a cast is not going to repeat that.
const PlaylistSourceCast = "cast"

// Cast is a cast row (DAT-043) — a "slidecast": the ordered set of authored
// slides an operator builds once and schedules onto screens, and the unit the
// legacy system's operators worked in. It is the resource-row baseline
// (DAT-005–008) plus `name` and a non-empty ordered `slides` array.
//
// A cast is deliberately its OWN row rather than a shape inlined into a playlist
// item. It is referenced by id from a playlist item (`source: "cast"`), so one
// authored cast plays on every screen whose schedule reaches a playlist naming
// it, and one edit changes all of them. An inlined copy would make "change the
// lunch menu" an edit of every playlist that shows it — which is the operator
// experience the rewrite exists to replace, not reproduce.
//
// The slides are the row's whole content: unlike a playlist, a cast references
// no other row, so a cast is complete on its own and a screen playing one needs
// nothing but the row and the content origin its image layers resolve against.
//
// DefaultDurationMS is the cast's OWN default dwell time in milliseconds,
// `omitempty`. A slidecast's slides overwhelmingly share one timing, so without
// it "hold every slide for eight seconds" is an edit of every slide, and every
// slide added later silently gets whatever the player defaults to rather than
// what the rest of the deck does. It sits third in the per-slide resolution
// (DAT-042): the slide's own `duration_ms`, then the referencing playlist item's
// `duration_seconds`, then this, then the player's default. Below the item
// override deliberately — `duration_seconds` is a statement about one PLACEMENT
// and this is a statement about slides that said nothing, so a cast-wide default
// that outranked the override would make the override unreachable for casts.
//
// Template marks the cast a starting point rather than a document a screen
// plays, `omitempty` so an ordinary cast marshals byte-identically to before
// this field existed. A template is a FLAVOR of cast rather than a resource of
// its own because it is exactly a cast nothing has scheduled yet: a separate
// kind would duplicate the slide shape, the layer gate, the asset-reference
// projection (the retention sweep must not reclaim a template's images either)
// and the Studio itself, and the two copies would drift. validateReferences
// refuses a playlist item that names one — see CAST_TEMPLATE_NOT_SCHEDULABLE.
type Cast struct {
	ID                string            `json:"id"`
	ScopeNode         string            `json:"scope_node"`
	Name              string            `json:"name"`
	Slides            []CastSlide       `json:"slides"`
	DefaultDurationMS int64             `json:"default_duration_ms,omitempty"`
	Template          bool              `json:"template,omitempty"`
	ExternalID        string            `json:"external_id,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Revision          int               `json:"revision"`
	CreatedAt         int64             `json:"created_at"`
	UpdatedAt         int64             `json:"updated_at"`
}

// CastSlide is one slide of a cast (DAT-043): an id unique within its own cast,
// an optional per-slide dwell time, and the ordered stack of positioned native
// layers a player draws — the SAME wire.Layer shape a `source: "slide"` playlist
// item carries and the player ultimately renders, so an authored slide and a
// served slide are one type end to end rather than two that must be kept equal.
//
// ID is scoped to the cast, not to the workspace, and is deliberately NOT a
// ULID: it names a position in one authored document (what an editor's undo
// stack and a reorder operate on), never a row anything else can reference. What
// it must be is stable and unique within the cast, which is what the validator
// enforces — a duplicate would make "the slide the operator moved" ambiguous.
//
// DurationMS is the slide's own dwell time in milliseconds, `omitempty`. When it
// is absent the projected content item falls back to the referencing playlist
// item's `duration_seconds` override (DAT-042), and when neither is stated the
// item carries no duration at all and the player applies its own default —
// the same three-step resolution on both projections.
// # Slide GROUPS are deliberately absent — a recorded decision, not an oversight
//
// The legacy authoring tool let a slide belong to a named group (a "sub-deck")
// so a NAV element on one slide could jump the deck to another group. Adding
// `group string` here would be trivial and would fit this shape perfectly, and
// that is exactly the trap: nothing in this system would read it.
//
// A group is only meaningful once something can SELECT by it, and both of the
// consumers that would are absent:
//
//   - the nav layer kind (parity row 1.5) — there is no wire.Layer kind that
//     jumps, and no player behavior for one; and
//   - a way to schedule one group — a playlist item names a whole cast
//     (PlaylistItem.CastID) and BOTH projections expand every slide of it.
//
// So a `group` field today would be a member an operator could set, that would
// round-trip perfectly, and that would change nothing on any screen — the
// precise defect shape this rebuild keeps producing. The honest state is: not
// modelled.
//
// When it IS built, the smallest coherent design is a pair, not a single field:
// `CastSlide.Group` (an optional name) plus `PlaylistItem.CastGroup` (an
// optional filter), with datamodel refusing a CastGroup naming no slide of the
// referenced cast, and both castContent projections expanding only the matching
// slides. That gives groups a real consumer WITHOUT waiting on a nav layer —
// "schedule just the Lunch group of the Menu cast" — and the nav kind can then
// jump between them later as a second increment.
type CastSlide struct {
	ID         string       `json:"id"`
	DurationMS int64        `json:"duration_ms,omitempty"`
	Layers     []wire.Layer `json:"layers"`
}

// Slide is a `source: "slide"` playlist item's authored content (DAT-041; native
// slide rendering, parity milestone 2): the ordered stack of positioned native
// layers a player draws directly, its shape single-sourced from wire.Layer (the
// player/1 Lease `layers` shape) so an authored slide and the served slide are
// the SAME layer type end-to-end, never a re-encoding. Layers are back-to-front
// (array index is z-order).
//
// The stack IS validated at authoring time, by the same gate a cast slide's is:
// checkPlaylistItems runs wire.ValidateAuthoredSlideLayers over it. It once was
// not, and the gap was not a smaller rule but the absence of the rule — a stack
// a cast refused with a 422 was accepted inline with a 201, and every consumer
// downstream inherited a shape the authoring surface says cannot exist. The
// AUTHORED form of the gate is the right one here for the reason it is right
// for a cast: an image layer's fetch URL is derived from the content origin at
// projection, so only the content-addressed asset_ref is authored. The serve-time
// form (wire.ValidateSlideLayers, url required) is still applied by the producer
// that projects it (feeder snapshot / relay schedulehost), which drops a slide
// whose layers do not validate rather than serving it malformed.
type Slide struct {
	Layers []wire.Layer `json:"layers"`
}

// Schedule is a schedule row (DAT-050): baseline plus name, an optional
// fallback_id, an optional priority (default 0; higher wins, DAT-053), and an
// optional misfire (DAT-120).
type Schedule struct {
	ID         string            `json:"id"`
	ScopeNode  string            `json:"scope_node"`
	Name       string            `json:"name"`
	FallbackID string            `json:"fallback_id,omitempty"`
	Priority   *int              `json:"priority,omitempty"`
	Misfire    string            `json:"misfire,omitempty"`
	ExternalID string            `json:"external_id,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Revision   int               `json:"revision"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
}

// EffectivePriority is the schedule's precedence primary key (DAT-050): its own
// priority, or 0 when it declares none.
func (s Schedule) EffectivePriority() int {
	if s.Priority == nil {
		return 0
	}
	return *s.Priority
}

// ValidityWindow is a validity-window row (DAT-060): schedule_id references its
// owning schedule, scope_node matches that schedule's (DAT-007). starts_at and
// ends_at are nullable timestamps — null means, respectively, no lower or no
// upper bound (DAT-061); both are always emitted (null, not omitted).
type ValidityWindow struct {
	ID         string `json:"id"`
	ScheduleID string `json:"schedule_id"`
	ScopeNode  string `json:"scope_node"`
	StartsAt   *int64 `json:"starts_at"`
	EndsAt     *int64 `json:"ends_at"`
	Revision   int    `json:"revision"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// Daypart is a daypart row (DAT-070). days_of_week/start_time/end_time declare
// its local coverage (DAT-071–072); display_power (DAT-074) is the display state
// it commands while it holds; playlist_id/preset_batch_id are optional refs
// (DAT-075); misfire is optional (DAT-076).
type Daypart struct {
	ID            string `json:"id"`
	ScheduleID    string `json:"schedule_id"`
	ScopeNode     string `json:"scope_node"`
	DaysOfWeek    []int  `json:"days_of_week"`
	StartTime     string `json:"start_time"`
	EndTime       string `json:"end_time"`
	DisplayPower  string `json:"display_power"`
	PlaylistID    string `json:"playlist_id,omitempty"`
	PresetBatchID string `json:"preset_batch_id,omitempty"`
	Misfire       string `json:"misfire,omitempty"`
	Name          string `json:"name,omitempty"`
	Revision      int    `json:"revision"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// EffectiveMisfire resolves a daypart's misfire (DAT-076): its own, else its
// owning schedule's, else catch_up_once (the recurring-state default, DAT-121).
func (d Daypart) EffectiveMisfire(owning Schedule) string {
	if d.Misfire != "" {
		return d.Misfire
	}
	if owning.Misfire != "" {
		return owning.Misfire
	}
	return "catch_up_once"
}

// Fallback is a fallback row (DAT-080): baseline plus name, display_power, and an
// optional playlist_id — the resolution-order default beneath a schedule's
// dayparts (DAT-081).
type Fallback struct {
	ID           string            `json:"id"`
	ScopeNode    string            `json:"scope_node"`
	Name         string            `json:"name"`
	DisplayPower string            `json:"display_power"`
	PlaylistID   string            `json:"playlist_id,omitempty"`
	ExternalID   string            `json:"external_id,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Revision     int               `json:"revision"`
	CreatedAt    int64             `json:"created_at"`
	UpdatedAt    int64             `json:"updated_at"`
}

// PresetBatch is a preset-batch row (DAT-090). Its identity field is preset_id,
// not id — the sole baseline exception (DAT-005), kept byte-exact with the field
// a rules/1 preset_batch action resolves against (RUL-170). commands is a
// non-empty device-command list (DAT-091); last_outcome records the most recent
// invocation (DAT-092/093).
type PresetBatch struct {
	PresetID    string              `json:"preset_id"`
	ScopeNode   string              `json:"scope_node"`
	Name        string              `json:"name"`
	Commands    []PresetCommand     `json:"commands"`
	LastOutcome *PresetBatchOutcome `json:"last_outcome,omitempty"`
	ExternalID  string              `json:"external_id,omitempty"`
	Labels      map[string]string   `json:"labels,omitempty"`
	Revision    int                 `json:"revision"`
	CreatedAt   int64               `json:"created_at"`
	UpdatedAt   int64               `json:"updated_at"`
}

// PresetCommand is one device-class command in a preset batch (DAT-091): the
// target entity, a command name from that entity's device class, and its typed
// params (resolved against the device-class registry, REG-050–052).
type PresetCommand struct {
	EntityID string          `json:"entity_id"`
	Command  string          `json:"command"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// PresetBatchOutcome is a preset-batch row's recorded invocation outcome
// (DAT-092): the byte-exact {outcome, results} shape rules/1 RUL-172 fixes
// (mirrored from internal/rules/eval so a preset row can carry it), plus this
// contract's own evaluated_at. outcome is one of complete, partial, or failed.
type PresetBatchOutcome struct {
	Outcome     string          `json:"outcome"`
	Results     []CommandResult `json:"results"`
	EvaluatedAt int64           `json:"evaluated_at"`
}

// CommandResult is one command's pass/fail entry within a PresetBatchOutcome —
// the identical shape rules/1's PresetBatchOutcome uses (DAT-092, RUL-172).
type CommandResult struct {
	Target  string `json:"target"`
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// SlideDwellMS resolves ONE cast slide's dwell time in milliseconds, and is the
// single implementation of DAT-042's four-step order:
//
//	slide.duration_ms → item duration_seconds → cast.default_duration_ms → 0
//
// itemDurationMS is the referencing playlist item's own `duration_seconds`
// override ALREADY converted to milliseconds (0 when the item stated none). A
// returned 0 means "state no duration at all" — the projected content item omits
// the key (its `omitempty`) and the player applies its own default.
//
// The item override sits ABOVE the cast default deliberately (DAT-042 says so in
// the same words): `duration_seconds` is a statement about one PLACEMENT of the
// cast, `default_duration_ms` a statement about slides that said nothing, so a
// cast-wide default outranking the override would make the override unreachable
// for every cast item that exists.
//
// It is EXPORTED and lives here, beside the Cast and CastSlide shapes, because
// two independent projections have to agree on it — the app-signed baseline
// (internal/feeder/snapshot) and the relay's daypart re-resolver
// (internal/relay/schedulehost) — and a screen must see the same dwell times
// whether it is playing one or the other. Both previously carried a private copy
// whose own doc admitted it was "byte-for-byte" the other's and that "a change
// to it is visibly a change that must be made twice"; adding the cast default
// was exactly that change, so the copies became one function instead. This is
// the same single-source discipline wire.ValidateSlideLayers documents for the
// layer rules the two projections also share.
func SlideDwellMS(slide CastSlide, cast Cast, itemDurationMS int64) int64 {
	if slide.DurationMS > 0 {
		return slide.DurationMS
	}
	if itemDurationMS > 0 {
		return itemDurationMS
	}
	if cast.DefaultDurationMS > 0 {
		return cast.DefaultDurationMS
	}
	return 0
}
