package wire

import (
	"encoding/json"
	"fmt"
)

// SectionKeys is the exact set of REL-060 `sections` keys, in the contract's
// declared order: every `state.snapshot` MUST carry all seven, present even
// when empty (an empty array or explicit empty placeholder, never an omitted
// key). Both this package's own field-shape tests and the relay's
// completeness gate (ValidateSectionsComplete, internal/relay/desiredstate)
// derive their key set from this one list so they cannot drift apart.
var SectionKeys = []string{
	"screen_programs",
	"edge_rules",
	"device_inventory",
	"schedule",
	"revocation_and_site",
	"pairing_grants",
	"workflow_generation",
}

// ValidateSectionsComplete is REL-060's structural completeness gate: it
// reports an error unless rawSections is a JSON object carrying every one of
// the seven SectionKeys. A key present with a JSON `null` value (as
// `workflow_generation` carries in this version, REL-068) counts as present —
// only an OMITTED key fails. This is a purely structural check on the raw
// wire bytes, distinct from and prior to hash/signature verification: a
// Go-decoded Sections struct always materializes all seven fields (missing
// ones as zero values), so completeness can only be checked against the
// original JSON, before it is decoded into the typed struct. Callers wrap the
// returned error in their own typed rejection reason.
func ValidateSectionsComplete(rawSections json.RawMessage) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawSections, &m); err != nil {
		return fmt.Errorf("wire: sections is not a JSON object: %w", err)
	}
	for _, k := range SectionKeys {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("wire: sections is missing REL-060 key %q", k)
		}
	}
	return nil
}

// StateSnapshotBody is the relay/1 `state.snapshot` message body
// (relay/1 REL-051): `{generation, hash, signature, sections}`.
// `signed_with_key` is diagnostic-only (REL-075) and omitted when unset.
type StateSnapshotBody struct {
	Generation    int64    `json:"generation"`
	Hash          string   `json:"hash"`
	Signature     string   `json:"signature"`
	SignedWithKey string   `json:"signed_with_key,omitempty"`
	Sections      Sections `json:"sections"`
}

// Sections is the relay/1 `state.snapshot` `sections` object (REL-060):
// exactly these 7 keys, all present in every snapshot (an empty array or
// an explicit empty placeholder where a site has nothing to populate a
// section with yet — never an omitted key).
type Sections struct {
	ScreenPrograms     []ScreenProgram   `json:"screen_programs"`
	EdgeRules          EdgeRules         `json:"edge_rules"`
	DeviceInventory    DeviceInventory   `json:"device_inventory"`
	Schedule           ScheduleSection   `json:"schedule"`
	RevocationAndSite  RevocationAndSite `json:"revocation_and_site"`
	PairingGrants      []PairingGrant    `json:"pairing_grants"`
	WorkflowGeneration any               `json:"workflow_generation"`
}

// ScheduleSection is the relay/1 `schedule` section (REL-065): the
// scheduling-core rows + scope nodes the feeder carries OPAQUELY for the
// relay to derive a dayparting timeline from (data-model/1). relay/1 does not
// own these row shapes — data-model/1 does — so every array element is left
// as raw JSON here, exactly as `edge_rules` carries rules/1's own compiled
// entries (REL-062). The relay-side parser (internal/relay/schedulehost)
// unmarshals these arrays into data-model/1's own row types and resolves them
// through internal/datamodel; this package never interprets them.
//
// Every one of the seven arrays is present and non-null in a well-formed
// snapshot (REL-060): call Normalized before hashing/signing so each empty
// array marshals as `[]`, never `null`. Field declaration order IS the
// canonical marshal order (encoding/json marshals struct fields in Go
// declaration order), so byte-identical content always hashes identically —
// the REL-053 byte-identical-marshaling → hash invariant.
type ScheduleSection struct {
	ScopeNodes      []json.RawMessage `json:"scope_nodes"`
	Playlists       []json.RawMessage `json:"playlists"`
	Schedules       []json.RawMessage `json:"schedules"`
	ValidityWindows []json.RawMessage `json:"validity_windows"`
	Dayparts        []json.RawMessage `json:"dayparts"`
	Fallbacks       []json.RawMessage `json:"fallbacks"`
	PresetBatches   []json.RawMessage `json:"preset_batches"`
}

// Normalized returns a copy of sec with every nil row array replaced by a
// non-nil empty slice, so each of the seven scheduling-core arrays marshals
// as `[]` rather than `null` (REL-060: sections carry an empty array, never
// an omitted key or a null placeholder). The feeder calls this before
// hashing/signing so the schedule section satisfies the REL-060 no-null
// invariant exactly as every other section does, and rides the same
// hash/signature. Normalization does not touch element bytes, so it preserves
// the byte-identical-marshaling → hash invariant.
func (sec ScheduleSection) Normalized() ScheduleSection {
	norm := func(s []json.RawMessage) []json.RawMessage {
		if s == nil {
			return []json.RawMessage{}
		}
		return s
	}
	return ScheduleSection{
		ScopeNodes:      norm(sec.ScopeNodes),
		Playlists:       norm(sec.Playlists),
		Schedules:       norm(sec.Schedules),
		ValidityWindows: norm(sec.ValidityWindows),
		Dayparts:        norm(sec.Dayparts),
		Fallbacks:       norm(sec.Fallbacks),
		PresetBatches:   norm(sec.PresetBatches),
	}
}

// ScreenProgram is one relay/1 `screen_programs` entry (REL-061).
// `Priority` is one of `scheduled` or `preempt`; `Display` is one of
// `content` or `blank`.
type ScreenProgram struct {
	ScreenID        string       `json:"screen_id"`
	ProgramRevision string       `json:"program_revision"`
	Priority        string       `json:"priority"`
	Display         string       `json:"display"`
	Content         []ContentRef `json:"content"`
}

// ContentRef is one signed content reference inside a ScreenProgram's
// `content` array (REL-061): `asset_ref` is a content-addressed `sha256:`
// URI in the same form `signhash.ContentID` produces; `url` is where a
// screen fetches the bytes directly (never through the relay, REL-140). A
// `content` array carries these in intended playback order, and each entry
// is independently asset_ref-verifiable — an array of N items is N
// independently signed-and-fetchable references, not just the first.
//
// ContentType and DurationMS are additive fields (REL-004: within major
// version 1, a new optional field MAY be introduced in a minor) beyond this
// contract's originally-published `{asset_ref, url, expires_at}` shape:
//
//   - ContentType names this item's kind, drawn from the same floor
//     `player/1`'s own `content_types` capability declares (`player/1`
//     PLY-014: at least `image`/`video`), and is carried unmodified into the
//     player/1 Lease content item's own `type` field for this entry
//     (internal/relay/playerserver.SetServedProgram). An item whose
//     ContentType is "" — an older feeder that predates this field — MUST be
//     treated as `image`, this codebase's own historical implicit value from
//     before this field existed (SetServedProgram applies that default).
//   - DurationMS is an optional per-item display-duration override, non-zero
//     when set — mirroring data-model/1's own per-item `duration_seconds`
//     override column (DAT-042). It is carried opaquely in this Wave: no
//     playback-advance driver in this codebase reads it yet, but it rides
//     `hash`/`signature` (REL-053) exactly like every other field, so a
//     later per-item-timing consumer can trust it as much as the rest of the
//     snapshot.
//
// Both fields marshal `omitempty` so a ContentRef built without them (every
// call site that predates this pair) marshals byte-identically to before
// their introduction — no new JSON keys appear, and no snapshot hash changes,
// unless a caller actually populates one.
type ContentRef struct {
	AssetRef    string `json:"asset_ref"`
	URL         string `json:"url"`
	ExpiresAt   int64  `json:"expires_at"`
	ContentType string `json:"content_type,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
}

// EdgeRules is the relay/1 `edge_rules` section (REL-062): a
// `rules_minor_version` naming the `rules/1` minor this generation was
// compiled against, plus an array of `rules/1`'s own CompiledRuleEntry
// objects, carried opaquely — relay/1 does not own that entry shape, so
// Rules elements are left as raw JSON here.
type EdgeRules struct {
	RulesMinorVersion string            `json:"rules_minor_version"`
	Rules             []json.RawMessage `json:"rules"`
}

// DeviceInventory is the relay/1 `device_inventory` section (REL-063/064).
//
// The two arrays answer two different questions and are derived from two
// different sources:
//
//   - `devices` (REL-063) is the app peer's ADOPTED set — the adoption
//     decision the app authors and ships down, never a copy of what a relay
//     discovered (REL-110/111 keep the relay authoritative for what exists).
//     DeviceEntry below is the typed shape a producer builds each element
//     from.
//   - `pack_match_patterns` (REL-064) is manifest/1's device-contribution
//     shape (MAN-070/071), sourced from every INSTALLED PACK's declared
//     `devices` contribution. REL-064 is explicit that these are "the
//     patterns a relay watches for during discovery INDEPENDENT of what is
//     already adopted", so the adopted set does not select them.
//
// Both arrays carry their elements as raw JSON, for the same reason
// `edge_rules.rules` and every `schedule` row array do: this struct is
// re-marshaled on the RELAY side to recompute `hash` from the decoded
// snapshot (wire.HashSections, internal/relay/desiredstate), and a
// json.RawMessage round-trips byte-for-byte where a typed re-marshal would
// silently re-order keys or drop members a future minor added — either of
// which would break the REL-053 hash check on a snapshot that was in fact
// intact.
//
// Call Normalized before hashing/signing so an empty section marshals as
// `{"devices":[],"pack_match_patterns":[]}` rather than carrying a `null`
// array (REL-060).
type DeviceInventory struct {
	Devices           []json.RawMessage `json:"devices"`
	PackMatchPatterns []json.RawMessage `json:"pack_match_patterns"`
}

// Normalized returns a copy of inv with each nil array replaced by a non-nil
// empty slice, so both marshal as `[]` rather than `null` (REL-060) — the
// DeviceInventory counterpart of ScheduleSection.Normalized, and normalizing
// only nil-ness, so it preserves the byte-identical-marshaling → hash
// invariant (REL-053).
func (inv DeviceInventory) Normalized() DeviceInventory {
	if inv.Devices == nil {
		inv.Devices = []json.RawMessage{}
	}
	if inv.PackMatchPatterns == nil {
		inv.PackMatchPatterns = []json.RawMessage{}
	}
	return inv
}

// DeviceEntry is one relay/1 `device_inventory.devices` entry (REL-063):
// `{device_id, driver, native_id, poll_cadence_seconds, entities}`, and
// exactly those five members. It is the PRODUCER-side type — an app peer
// marshals one of these per adopted device into the section's raw `devices`
// array — so the entry's contract shape and its field order live here, in the
// relay/1 wire package that owns them, rather than being spelled out ad hoc
// by whichever component happens to build a snapshot.
//
// The app peer's adopted-device row carries the api/1 resource baseline
// around these five (name, scope_node, labels, revision, timestamps); none of
// that is relay-facing, so building this type from a row is a SELECTION of
// the five contract members rather than a dump of the stored row.
//
// DeviceID is the adopted-device row's own id — the `device_id` REL-063
// names, whose identity is the `(site, driver, native_id)` tuple (REL-153),
// never the relay that happens to currently report the device.
//
// PollCadenceSeconds is a pointer with NO omitempty: REL-063 lists the member
// on every entry and fixes no default, so an unstated cadence marshals as an
// explicit `null` ("this deployment has not stated one") rather than as an
// absent key or as a fabricated `0` that a relay would read as "poll
// continuously".
type DeviceEntry struct {
	DeviceID           string         `json:"device_id"`
	Driver             string         `json:"driver"`
	NativeID           string         `json:"native_id"`
	PollCadenceSeconds *int           `json:"poll_cadence_seconds"`
	Entities           []DeviceEntity `json:"entities"`
}

// DeviceEntity is one entry of a DeviceEntry's `entities` array (REL-063):
// `{entity_id, device_class, enabled, hidden, display_name, category}`, with
// `category` one of `primary` or `diagnostic`.
//
// Every member but `entity_id`/`device_class` is authored POLICY rather than
// a discovered fact — whether an adopted entity is enabled, whether it is
// hidden, what it is called, and whether it is a primary or a diagnostic
// surface are decisions no discovery sweep can report. That is what makes
// this section desired state travelling DOWN to a relay rather than a
// projection of the relay's own report.
type DeviceEntity struct {
	EntityID    string `json:"entity_id"`
	DeviceClass string `json:"device_class"`
	Enabled     bool   `json:"enabled"`
	Hidden      bool   `json:"hidden"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category"`
}

// RevocationAndSite is the relay/1 `revocation_and_site` section
// (REL-066): `revoked` an array of opaque identifier strings,
// `site_effective` a persisted copy of the site's placement data, and
// `content_origin` (REL-061) the base URL a screen fetches this site's
// content from — the app's own content origin, carried opaquely from the
// feeder so a relay-side schedule resolver (internal/relay/schedulehost) can
// derive fetchable content URLs (`content_origin + "/content/" + hex`)
// without ever guessing a relay-local origin (REL-140: relay never in the
// content path). A plain optional string, not a required REL-060 section
// key: an absent key normalizes to "" via encoding/json's ordinary zero-value
// decode — no explicit normalization step is needed.
type RevocationAndSite struct {
	Revoked       []string      `json:"revoked"`
	SiteEffective SiteEffective `json:"site_effective"`
	ContentOrigin string        `json:"content_origin"`
}

// SiteEffective mirrors `{tz, lat, long}` — relay/1's persisted copy of a
// site's placement data (REL-066). Distinct from Hello's SiteBinding
// (REL-036), which additionally carries `scope_node`.
type SiteEffective struct {
	TZ   string  `json:"tz"`
	Lat  float64 `json:"lat"`
	Long float64 `json:"long"`
}

// PairingGrant is one relay/1 `pairing_grants` entry (REL-067,
// Player-credential authority): a pending pairing-grant record minted
// against this site.
type PairingGrant struct {
	GrantID                string `json:"grant_id"`
	Purpose                string `json:"purpose"`
	ResultingPrincipalKind string `json:"resulting_principal_kind"`
	TTL                    int64  `json:"ttl"`
	RedemptionMode         string `json:"redemption_mode"`
	IssuedAt               int64  `json:"issued_at"`
}
