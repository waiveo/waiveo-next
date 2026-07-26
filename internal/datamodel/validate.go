package datamodel

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
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
//   - row-shape MUSTs (DAT-061 validity range, DAT-091 non-empty commands);
//   - within-schedule daypart partition (DAT-073, DAYPART_OVERLAP via
//     ValidateNoOverlap): a schedule's dayparts MUST NOT overlap;
//   - referential integrity: a daypart/validity-window scope_node equals its
//     owning schedule's scope_node (DAT-007), and every playlist/fallback/
//     preset/schedule id reference resolves to a present row (DAT-050/070/075/080).
//
// It validates a COMPLETE set (every referenced row present), matching the
// relay/1 snapshot / full-tenant model; a dangling reference is a genuine error.
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
		rs.Playlists = append(rs.Playlists, v)
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
		rs.PresetBatches = append(rs.PresetBatches, v)
	}

	errs = append(errs, validateReferences(rs)...)
	if e := ValidateNoOverlap(rs.Dayparts); e != nil {
		errs = append(errs, *e)
	}
	return rs, errs
}

// CheckRowID enforces DAT-005a: a row's own identity field — id for every kind
// but preset-batch, whose identity field is preset_id (DAT-005's byte-exact
// exception) — MUST be a syntactically valid canonical ULID. It is the one
// helper shared by every ValidateRows decode loop below, by BuildScopeTree's
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
