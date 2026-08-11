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
type PlaylistItem struct {
	Source          string `json:"source"`
	AssetRef        string `json:"asset_ref,omitempty"`
	PackID          string `json:"pack_id,omitempty"`
	ContentID       string `json:"content_id,omitempty"`
	DurationSeconds *int   `json:"duration_seconds,omitempty"`
	Slide           *Slide `json:"slide,omitempty"`
	CastID          string `json:"cast_id,omitempty"`
}

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
type Cast struct {
	ID         string            `json:"id"`
	ScopeNode  string            `json:"scope_node"`
	Name       string            `json:"name"`
	Slides     []CastSlide       `json:"slides"`
	ExternalID string            `json:"external_id,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Revision   int               `json:"revision"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
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
// (array index is z-order). The stack is not validated at authoring time — an
// image layer's fetch URL is derived from the content origin at projection, so
// wire.ValidateSlideLayers is applied by the producer that projects it (feeder
// snapshot / relay schedulehost), which drops a slide whose layers do not
// validate rather than serving it malformed.
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
