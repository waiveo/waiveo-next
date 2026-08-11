package datamodel

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// identityrows.go is the reference implementation of the two IDENTITY rows
// data-model/1 DAT-004 names but does not itself schema: a screen's own
// identity row and a device row.
//
// # What these rows are, and what they are not
//
// DAT-004a fixes the question the rest of the platform kept answering by
// coincidence: a `screen_id` names a SCREEN IDENTITY ROW and a `device_id`
// names a DEVICE ROW — neither is a scope node's id. The `screen`-kind scope
// node (DAT-001) is a placement classification and nothing else; a deployment
// may place a screen row under an `org`, `site`, `group`, or `screen`-kind node
// alike (DAT-004), so reading a `screen_id` as "the id of the screen-kind node
// it happens to sit under" is not merely imprecise, it is unrepresentable for
// every deployment that puts two screens under one node.
//
// DAT-005b then binds both rows to the SAME resource-row baseline every other
// row carries — a canonical-ULID id (DAT-005a), a revision, timestamps, a
// scope_node placement (DAT-006), and the optional external_id/labels — which is
// what makes them addressable through api/1's ordinary conventions rather than
// through a bespoke surface.
//
// # Whose fields are whose
//
// data-model/1 deliberately does NOT own either row's domain fields, and neither
// does this file invent any. A device row's `driver`, `native_id`,
// `poll_cadence_seconds` and per-entity policy are the five members relay/1
// REL-063's `device_inventory` entry is defined as, carried here in the same
// field names so the desired-state projection is a selection rather than a
// translation. A screen row's optional `device_id` is player/1 PLY-124's
// "reference to the device_id of an adopted device representing the same
// physical display" — optional there, optional here, and validated only for
// resolution, never required.
//
// # Ownership of the underlying facts
//
// A device row is an ADOPTION RECORD, not a copy of what a relay sees. The
// relay is authoritative for what physically exists on its LAN and reports
// discovered-but-unadopted observations upward as candidates (relay/1
// REL-110/111); the app peer is authoritative for which of those are adopted and
// under what policy, and ships that decision DOWN as `device_inventory`
// (REL-063). REL-153 settles which side owns identity: a device's `device_id` is
// scoped to `(site, driver, native_id)` and "the app peer's own records reflect
// only which relay most recently reported it" — so `(driver, native_id)` is the
// row's natural key and the reporting relay is not part of it. That is why
// ValidateIdentityRows enforces uniqueness over `(driver, native_id)` and knows
// nothing about a relay id at all: re-homing an adopted device to a second relay
// serving the same site MUST resolve to the same row, and a row keyed by the
// reporting relay could not do that.

// Screen is a screen's own identity row (DAT-004/DAT-004a): the resource-row
// baseline (DAT-005b) plus a name and player/1 PLY-124's optional adopted-device
// reference.
//
// DeviceID is a pointer so an ABSENT link is distinguishable from an explicit
// empty string: PLY-124 makes the reference optional, and "no device is bound to
// this screen" is a different statement from "a device is bound and its id is
// blank" — only the first is representable, and the second is rejected.
// Override is a pointer for the same reason: an ABSENT override is the ordinary
// state of a screen (it resolves through the scheduling core), and a present one
// is a deliberate pin (DAT-004c).
type Screen struct {
	ID         string            `json:"id"`
	ScopeNode  string            `json:"scope_node"`
	Name       string            `json:"name"`
	DeviceID   *string           `json:"device_id"`
	Override   *ScreenOverride   `json:"override,omitempty"`
	ExternalID string            `json:"external_id,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Revision   int               `json:"revision"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
}

// ScreenOverride is a screen row's program pin (DAT-004c): the one place this
// model admits a per-screen content assignment, superseding whatever the
// scheduling core resolves for that screen while it applies.
//
// It is deliberately NOT a second scheduling mechanism — no cascade, no
// priority, no layering, no recurrence. It is "show this here, now, until it is
// cleared or lapses": the shape an operator's push-now gesture and an
// automation's signage action (rules/1 RUL-234/235) both need, and the shape a
// schedule cannot express because a schedule is authored at a scope node and
// governs every screen beneath it.
//
// ExpiresAt is what makes an alert retire itself. DAT-004d makes a lapsed
// override behave as absent at RESOLUTION time, with no write required — so a
// relay that loses its app peer still stops showing an alert whose window
// closed, and nothing has to be running to clear it.
type ScreenOverride struct {
	Mode      string `json:"mode"`
	CastID    string `json:"cast_id,omitempty"`
	Message   string `json:"message,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	SetAt     int64  `json:"set_at,omitempty"`
}

// The two DAT-004c override modes. `play` is the daily assignment (legacy's
// play_cast); `alert` is the takeover, which a delivered program carries as
// player/1's `preempt` priority so a screen interrupts what it is showing
// rather than waiting for a natural item boundary (DAT-004d).
const (
	ScreenOverrideModePlay  = "play"
	ScreenOverrideModeAlert = "alert"
)

// maxOverrideMessageLen bounds an alert's literal `message` at the same 200
// characters a row's name is bounded at (DAT-004c), counted in code points for
// the same reason — it is drawn on a screen for a human to read.
const maxOverrideMessageLen = 200

// Applies reports whether this override governs a screen's program at tMs
// (DAT-004d): it applies when it is set and either states no expiry or expires
// strictly after tMs. A nil receiver is the absent case and never applies, so a
// caller can ask the question without first testing for nil.
//
// Expiry is evaluated at RESOLUTION time rather than retired by a writer, which
// is what makes an alert self-limiting on a relay with no app peer: nothing has
// to be running for the window to close.
func (o *ScreenOverride) Applies(tMs int64) bool {
	if o == nil {
		return false
	}
	return o.ExpiresAt == 0 || o.ExpiresAt > tMs
}

// Device is a device row (DAT-004/DAT-004a) — the adopted-device record whose
// `id` IS the `device_id` a relay/1 `device_inventory` entry carries (REL-063).
// Driver, NativeID, PollCadenceSeconds and Entities are that entry's own members,
// in its own field names; Name, and the baseline around them, are DAT-005b's.
//
// PollCadenceSeconds is a pointer because REL-063 states no default: an absent
// cadence means "this deployment has not stated one", which a zero would silently
// misreport as "poll continuously".
type Device struct {
	ID                 string            `json:"id"`
	ScopeNode          string            `json:"scope_node"`
	Name               string            `json:"name"`
	Driver             string            `json:"driver"`
	NativeID           string            `json:"native_id"`
	PollCadenceSeconds *int              `json:"poll_cadence_seconds"`
	Entities           []DeviceEntity    `json:"entities"`
	ExternalID         string            `json:"external_id,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	Revision           int               `json:"revision"`
	CreatedAt          int64             `json:"created_at"`
	UpdatedAt          int64             `json:"updated_at"`
}

// DeviceEntity is one entry of a device row's `entities` array — relay/1
// REL-063's `{entity_id, device_class, enabled, hidden, display_name, category}`,
// field for field.
//
// Every member here is authored POLICY rather than a discovered fact: whether an
// adopted entity is enabled, whether it is hidden from an operator's ordinary
// view, what it is called, and whether it is a primary or a diagnostic surface
// are decisions no discovery sweep can make. That is the concrete reason a device
// row is an authored resource and not a projection — there is nowhere else for
// these four values to live.
type DeviceEntity struct {
	EntityID    string `json:"entity_id"`
	DeviceClass string `json:"device_class"`
	Enabled     bool   `json:"enabled"`
	Hidden      bool   `json:"hidden"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category"`
}

// RawIdentityRows is a bundle of identity rows as raw JSON, grouped by kind — the
// same shape RawRows takes for the scheduling core, and for the same reason: the
// two tables are validated TOGETHER because a screen's `device_id` reference
// (PLY-124) can only be resolved against the device table.
type RawIdentityRows struct {
	Screens []json.RawMessage
	Devices []json.RawMessage
}

// IdentitySet is the typed, parsed form of a RawIdentityRows bundle. A row that
// fails to parse is reported and omitted from the set.
type IdentitySet struct {
	Screens []Screen
	Devices []Device
}

// validEntityCategory is REL-063's closed `category` vocabulary.
var validEntityCategory = map[string]bool{"primary": true, "diagnostic": true}

// maxIdentityRowNameLen bounds a screen's or device's `name` at the same 200
// characters DAT-001a bounds a scope node's at, counted in Unicode code points
// rather than bytes for the same reason: a name is a human label, and a
// multi-byte character is one character to the human who typed it.
const maxIdentityRowNameLen = 200

// ValidateIdentityRows parses and validates a complete identity-row bundle,
// returning the typed IdentitySet and every validation error found. Like
// ValidateRows, it judges a COMPLETE set — every referenced row present — which
// is what lets a dangling screen→device reference be a genuine error rather than
// an artifact of partial input.
//
// It enforces:
//   - the resource-row baseline (DAT-005a/DAT-005b): every row's `id` is a
//     canonical ULID (ROW_ID_INVALID) and carries a `scope_node` (DAT-006);
//   - a stated, bounded `name` on both kinds;
//   - relay/1 REL-063's device shape: a non-empty `driver` and `native_id`, a
//     positive `poll_cadence_seconds` when stated, and per entity a canonical-ULID
//     `entity_id`, a non-empty `device_class`, and a `category` from the closed
//     vocabulary;
//   - relay/1 REL-153's device identity: `(driver, native_id)` is unique across
//     device rows, since re-adopting one physical device MUST resolve to one
//     `device_id`;
//   - player/1 PLY-124's optional screen→device link: a stated `device_id`
//     resolves to a device row (REFERENCE_INVALID).
func ValidateIdentityRows(raw RawIdentityRows) (IdentitySet, []Error) {
	var set IdentitySet
	var errs []Error

	deviceIDs := map[string]bool{}
	identityTuples := map[string]string{}

	for _, r := range raw.Devices {
		var d Device
		if !decode(r, &d, &errs) {
			continue
		}
		if e := CheckRowID(d.ID, "id"); e != nil {
			errs = append(errs, *e)
		}
		errs = append(errs, checkPlacementAndName(d.ScopeNode, d.Name, "device")...)
		errs = append(errs, checkDeviceEntry(d, identityTuples)...)
		deviceIDs[d.ID] = true
		set.Devices = append(set.Devices, d)
	}

	for _, r := range raw.Screens {
		var s Screen
		if !decode(r, &s, &errs) {
			continue
		}
		if e := CheckRowID(s.ID, "id"); e != nil {
			errs = append(errs, *e)
		}
		errs = append(errs, checkPlacementAndName(s.ScopeNode, s.Name, "screen")...)
		// PLY-124's link is OPTIONAL, so absence is silent. A PRESENT one is
		// held to the same standard every other cross-row reference in this
		// package is (DAT-050/070/075/080's REFERENCE_INVALID): it names a row
		// that exists, or it is refused. A blank string is not "no link" — the
		// absent pointer already spells that — so it is refused too.
		errs = append(errs, checkScreenOverride(s.Override)...)
		if s.DeviceID != nil && !deviceIDs[*s.DeviceID] {
			errs = append(errs, Error{
				Field: "device_id",
				Code:  "REFERENCE_INVALID",
				Message: "a screen's device_id does not reference an existing device row; " +
					"the reference is optional, but a stated one MUST resolve (player/1 PLY-124)",
			})
		}
		set.Screens = append(set.Screens, s)
	}

	return set, errs
}

// checkScreenOverride enforces DAT-004c's SHAPE on a screen row's program
// override (SCREEN_OVERRIDE_INVALID). An absent override is the ordinary state
// of a screen and is silent.
//
// What it deliberately does NOT check is whether `cast_id` resolves to a cast
// row. This validator judges the identity-row bundle (screens + devices); casts
// live in the scheduling-core bundle, and the two are validated by different
// passes over different inputs — so a check here would either be permanently
// blind or would force every cast write to re-validate every screen. DAT-004c
// therefore places that check on the surface IMPOSING the override, which holds
// both families at once, and makes the projection contribute no content for an
// unresolvable cast (the same degrade an unresolvable playlist cast reference
// already has). Stating this here rather than leaving it as an omission is the
// point: a reader looking for the reference check must find out where it lives,
// not conclude there isn't one.
func checkScreenOverride(o *ScreenOverride) []Error {
	if o == nil {
		return nil
	}
	invalid := func(msg string) []Error {
		return []Error{{Field: "override", Code: "SCREEN_OVERRIDE_INVALID", Message: msg}}
	}
	switch o.Mode {
	case ScreenOverrideModePlay, ScreenOverrideModeAlert:
	default:
		return invalid("a screen override's mode MUST be one of play, alert (DAT-004c)")
	}
	// Exactly one of cast_id/message: neither names nothing to show, both names
	// two things with no rule ranking them.
	if (strings.TrimSpace(o.CastID) == "") == (strings.TrimSpace(o.Message) == "") {
		return invalid("a screen override MUST declare exactly one of cast_id, message (DAT-004c)")
	}
	if o.Message != "" {
		if o.Mode != ScreenOverrideModeAlert {
			return invalid("a screen override's literal message is admissible only under mode \"alert\"; a play override names a cast (DAT-004c)")
		}
		if utf8.RuneCountInString(o.Message) > maxOverrideMessageLen {
			return invalid("a screen override's message MUST be at most 200 characters (DAT-004c)")
		}
	}
	if o.ExpiresAt < 0 {
		return invalid("a screen override's expires_at MUST be a non-negative Timestamp-ms; omit it for no expiry (DAT-004c)")
	}
	return nil
}

// checkPlacementAndName enforces the two baseline members an identity row shares
// with every other row: DAT-006's `scope_node` placement, and a stated, bounded
// `name`. kind names the row kind in the message so a caller reading a 422 knows
// which table refused it.
func checkPlacementAndName(scopeNode, name, kind string) []Error {
	var errs []Error
	if strings.TrimSpace(scopeNode) == "" {
		errs = append(errs, Error{
			Field:   "scope_node",
			Code:    "ROW_SCOPE_NODE_MISSING",
			Message: "a " + kind + " row MUST carry the id of the scope node it is placed under (DAT-004/DAT-006)",
		})
	}
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > maxIdentityRowNameLen {
		errs = append(errs, Error{
			Field:   "name",
			Code:    "ROW_NAME_INVALID",
			Message: "a " + kind + " row's name MUST be non-empty, not whitespace-only, and at most 200 characters",
		})
	}
	return errs
}

// checkDeviceEntry enforces the relay/1-owned half of a device row: REL-063's
// entry members and closed `category` vocabulary, and REL-153's
// `(driver, native_id)` identity.
//
// identityTuples accumulates across the whole bundle and maps a seen
// `driver\x00native_id` to the row id that claimed it, so the SECOND claimant is
// the one reported — the first is, by construction, the row that was already
// there.
func checkDeviceEntry(d Device, identityTuples map[string]string) []Error {
	var errs []Error
	if strings.TrimSpace(d.Driver) == "" {
		errs = append(errs, Error{
			Field:   "driver",
			Code:    "DEVICE_IDENTITY_INCOMPLETE",
			Message: "a device row MUST carry a driver: it is half of the (site, driver, native_id) tuple a device_id's identity is scoped to (relay/1 REL-063)",
		})
	}
	if strings.TrimSpace(d.NativeID) == "" {
		errs = append(errs, Error{
			Field:   "native_id",
			Code:    "DEVICE_IDENTITY_INCOMPLETE",
			Message: "a device row MUST carry a native_id: it is half of the (site, driver, native_id) tuple a device_id's identity is scoped to (relay/1 REL-063)",
		})
	}
	if d.Driver != "" && d.NativeID != "" {
		key := d.Driver + "\x00" + d.NativeID
		if prior, seen := identityTuples[key]; seen {
			errs = append(errs, Error{
				Field: "native_id",
				Code:  "DEVICE_IDENTITY_DUPLICATE",
				Message: "another device row (" + prior + ") already claims this (driver, native_id); " +
					"re-adopting one physical device MUST resolve to one device_id (relay/1 REL-063/REL-153)",
			})
		} else {
			identityTuples[key] = d.ID
		}
	}
	if d.PollCadenceSeconds != nil && *d.PollCadenceSeconds <= 0 {
		errs = append(errs, Error{
			Field:   "poll_cadence_seconds",
			Code:    "POLL_CADENCE_INVALID",
			Message: "a stated poll_cadence_seconds MUST be a positive number of seconds (relay/1 REL-063)",
		})
	}

	seenEntity := map[string]bool{}
	for i, ent := range d.Entities {
		field := "entities[" + strconv.Itoa(i) + "]"
		if strings.TrimSpace(ent.EntityID) == "" {
			errs = append(errs, Error{
				Field:   field + ".entity_id",
				Code:    "ENTITY_ID_MISSING",
				Message: "a device row's entity MUST carry an entity_id — it is the unit a device command is addressed to (relay/1 REL-063/REL-112)",
			})
		} else if !ulid.Valid(ent.EntityID) {
			// An entity_id is a platform-minted identifier like any other: the
			// device/entity read model refuses a non-ULID one (internal/app/devices),
			// events/1 types the ids in a durable payload as ULIDs, and the
			// api/1 surface declares this member as one. Accepting a
			// free-form string HERE would let an authored row name an entity
			// that no other plane could ever address.
			errs = append(errs, Error{
				Field:   field + ".entity_id",
				Code:    "ENTITY_ID_INVALID",
				Message: "a device row's entity_id MUST be a syntactically valid canonical ULID",
			})
		} else if seenEntity[ent.EntityID] {
			errs = append(errs, Error{
				Field:   field + ".entity_id",
				Code:    "ENTITY_ID_DUPLICATE",
				Message: "a device row MUST NOT list the same entity_id twice; a command addressed to it would have two definitions of its policy",
			})
		} else {
			seenEntity[ent.EntityID] = true
		}
		if strings.TrimSpace(ent.DeviceClass) == "" {
			errs = append(errs, Error{
				Field:   field + ".device_class",
				Code:    "DEVICE_CLASS_MISSING",
				Message: "a device row's entity MUST carry a device_class — it is the vocabulary a command to this entity resolves against (device-class-registry/1 REG-052)",
			})
		}
		if !validEntityCategory[ent.Category] {
			errs = append(errs, Error{
				Field:   field + ".category",
				Code:    "ENTITY_CATEGORY_INVALID",
				Message: "a device row's entity category MUST be one of primary, diagnostic (relay/1 REL-063)",
			})
		}
	}
	return errs
}
