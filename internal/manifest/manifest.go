// Package manifest is the canonical, contract-validated home of manifest/1: the
// declarative document every pack ships at its root — identity, host
// compatibility, requested capabilities and consent, network egress, resource
// limits, the pack's own data model (the universal entity envelope), UI page/
// slot/surface declarations, device/playable/automation contributions, actions,
// and locale catalogs. The host reads and validates this document to render
// consent, provision the sandbox, register collections, and wire UI/automation
// — entirely before any pack code runs.
//
// This package is a dependency leaf: it imports the standard library only (plus
// internal/shared/ulid for ULID-typed fields). It MUST NOT import
// internal/deviceclass, internal/events, or internal/rules — the host's
// install-time inputs (the capability, page-type, provider, and device-class
// registries, and the memory floor) arrive as plain Go sets/values in
// HostRegistries, so this package cites those contracts without depending on
// their code.
package manifest

import "encoding/json"

// PackManifest mirrors the manifest/1 wire shape: every declared section as a
// typed field. Absent optional sections unmarshal to their zero value and are
// tolerated; Validate interprets the document, it does not require every
// section to be present.
type PackManifest struct {
	ID           string                     `json:"id"`
	Version      string                     `json:"version"`
	DisplayName  string                     `json:"displayName"`
	Description  string                     `json:"description,omitempty"`
	Icon         string                     `json:"icon,omitempty"`
	Compat       Compat                     `json:"compat"`
	Capabilities []Capability               `json:"capabilities"`
	Egress       []string                   `json:"egress"`
	Resources    Resources                  `json:"resources"`
	DataModel    DataModel                  `json:"dataModel"`
	Retention    map[string]json.RawMessage `json:"retention"`
	Connections  []Connection               `json:"connections,omitempty"`
	UI           UI                         `json:"ui"`
	Devices      []Device                   `json:"devices,omitempty"`
	Contributes  Contributes                `json:"contributes,omitempty"`
	Actions      []Action                   `json:"actions"`
	Drivers      json.RawMessage            `json:"drivers,omitempty"`
	Sources      json.RawMessage            `json:"sources,omitempty"`
	Diagnostics  json.RawMessage            `json:"diagnostics,omitempty"`
}

// Compat is the compatibility block (MAN-010-013): the ctx version range the
// pack's logic supports, an optional relay version range required by a devices
// contribution (MAN-011), the page-type names the pack's UI uses (each MUST be
// a host page-type), and optional feature flags. Renderer is nil when the key
// is absent and a non-nil empty slice when declared empty, so MAN-010 presence
// is distinguishable from an empty renderer list.
type Compat struct {
	Ctx      string   `json:"ctx"`
	Relay    string   `json:"relay,omitempty"`
	Renderer []string `json:"renderer"`
	Features []string `json:"features,omitempty"`
}

// Capability is one requested capability grant (MAN-020): the capability name
// (a host registry entry), its capability-specific scope, and a msg-ref reason.
type Capability struct {
	Capability string `json:"capability"`
	Scope      string `json:"scope"`
	Reason     string `json:"reason"`
}

// Resources is the resource-limit block (MAN-040): the pack process's memory
// ceiling (MiB), relative CPU scheduling weight, storage quota (MiB), and the
// concurrent-schedule-timer ceiling.
type Resources struct {
	Memory             int `json:"memory"`
	CPUWeight          int `json:"cpuWeight"`
	StorageQuota       int `json:"storageQuota"`
	MaxScheduledTimers int `json:"maxScheduledTimers"`
}

// DataModel is the pack's declared data model (MAN-050-053): a positive version,
// its collections, and optional ordered migration steps.
type DataModel struct {
	Version     int          `json:"version"`
	Collections []Collection `json:"collections"`
	Migrations  []Migration  `json:"migrations,omitempty"`
}

// Collection is one declared collection (MAN-051): a pack-unique name and its
// declared fields. Every row also carries the host-managed universal entity
// envelope in addition to these declared fields.
type Collection struct {
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
	// Singleton marks a collection that holds at most one row (MAN-056) — the
	// shape a settings-form page edits. Written without omitempty nowhere on
	// the wire: this rides the manifest the publisher authored, and a pack that
	// omits it declares an ordinary unbounded collection.
	Singleton bool `json:"singleton,omitempty"`
}

// Field is one declared collection field (MAN-051/052): its name and type, an
// optional role (title|summary), searchable flag, and lifecycle annotation.
type Field struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Role       string `json:"role,omitempty"`
	Searchable bool   `json:"searchable,omitempty"`
	Lifecycle  string `json:"lifecycle,omitempty"`
}

// Migration is one ordered data-model migration step (MAN-053).
type Migration struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// Connection is one external-service connection (MAN-055): a provider/authType
// pair (which MUST be host-registered) and its requested scopes.
type Connection struct {
	Provider string   `json:"provider"`
	AuthType string   `json:"authType"`
	Scopes   []string `json:"scopes"`
}

// UI is the UI-declaration block (MAN-060-063): the pack's pages, the slots it
// exposes for other packs' fragments, and the surfaces it mounts.
type UI struct {
	Pages    []Page    `json:"pages"`
	Slots    []Slot    `json:"slots,omitempty"`
	Surfaces []Surface `json:"surfaces,omitempty"`
}

// Page is one UI page declaration (MAN-060/061): a pack-unique path, a pageType
// (which MUST be a compat.renderer member), a msg-ref title, and optional
// fragment/sizeHint marking it contributable into another pack's slot.
type Page struct {
	Path     string `json:"path"`
	PageType string `json:"pageType"`
	TitleMsg string `json:"titleMsg"`
	Fragment string `json:"fragment,omitempty"`
	SizeHint string `json:"sizeHint,omitempty"`
}

// Slot is one named slot point (MAN-062) other packs' fragment pages bind into.
type Slot struct {
	Name    string   `json:"name"`
	Accepts []string `json:"accepts"`
}

// Surface is one surface declaration (MAN-063): a pack-unique name and the
// single bundled frontend entry point it mounts from.
type Surface struct {
	Name  string `json:"name"`
	Entry string `json:"entry"`
}

// Device is one device-class contribution (MAN-070): the device-class name
// (a host device-class registry entry), its discovery-match patterns, and the
// command/entity capabilities requested for that class.
type Device struct {
	DeviceClass  string           `json:"deviceClass"`
	Match        []map[string]any `json:"match"`
	Capabilities []string         `json:"capabilities"`
}

// Contributes carries the pack's playable and automation contributions.
type Contributes struct {
	Playable   *Playable   `json:"playable,omitempty"`
	Automation *Automation `json:"automation,omitempty"`
}

// Playable is the playable contribution (MAN-080/081): its content type,
// duration semantics, an opaque renderHints object, a pack-local content id,
// and — only when durationSemantics is fixed — a positive durationSeconds.
type Playable struct {
	ContentType       string          `json:"contentType"`
	DurationSemantics string          `json:"durationSemantics"`
	DurationSeconds   *float64        `json:"durationSeconds,omitempty"`
	RenderHints       json.RawMessage `json:"renderHints,omitempty"`
	ContentID         string          `json:"contentId"`
}

// Automation carries the pack's automation contributions (MAN-090-092).
type Automation struct {
	Events   []AutomationEvent   `json:"events,omitempty"`
	Actions  []AutomationAction  `json:"actions,omitempty"`
	Triggers []AutomationTrigger `json:"triggers,omitempty"`
}

// AutomationEvent is one durable automation-source event (MAN-090): its name
// (pack-namespaced <pack-id>.<local>) and payload schema.
type AutomationEvent struct {
	Name          string          `json:"name"`
	PayloadSchema json.RawMessage `json:"payloadSchema"`
}

// AutomationAction is the automation-facing view of a declared action (MAN-091):
// a name that MUST also appear in actions, a fields schema, and its execution
// class (relay-command|app-service).
type AutomationAction struct {
	Name         string          `json:"name"`
	FieldsSchema json.RawMessage `json:"fieldsSchema"`
	Execution    string          `json:"execution"`
}

// AutomationTrigger is one named automation trigger macro (MAN-092).
type AutomationTrigger struct {
	Name         string          `json:"name"`
	Msg          string          `json:"msg"`
	Matches      json.RawMessage `json:"matches"`
	ParamsSchema json.RawMessage `json:"paramsSchema"`
}

// Action is one declared server-side action (MAN-100/103): a pack-unique name,
// its params schema, the capability scope required to invoke it, its audit and
// idempotency classes, and whether automation may call it.
type Action struct {
	Name               string          `json:"name"`
	ParamsSchema       json.RawMessage `json:"paramsSchema"`
	CapabilityScope    string          `json:"capabilityScope"`
	AuditClass         string          `json:"auditClass"`
	IdempotencyClass   string          `json:"idempotencyClass"`
	AutomationCallable bool            `json:"automationCallable,omitempty"`
}

// HostRegistries carries the install-time inputs a manifest is validated
// against — the host's recognized capability/feature/page-type/device-class/
// provider sets and the memory floor — supplied as plain Go values so this
// package stays a leaf. The recognized sets are host configuration, not part of
// manifest/1: an unrecognized entry names the offender, never a silent grant.
type HostRegistries struct {
	// Capabilities is the host capability registry (MAN-021).
	Capabilities map[string]bool
	// Features is the host feature-flag set (MAN-012).
	Features map[string]bool
	// PageTypes is the host declarative-renderer page-type registry (MAN-010).
	PageTypes map[string]bool
	// DeviceClasses is the host device-class registry (MAN-070).
	DeviceClasses map[string]bool
	// Providers is the host connection registry, keyed "provider/authType"
	// (MAN-055).
	Providers map[string]bool
	// BundleFiles is the set of file paths present in the pack's own bundle
	// artifact — the set a ui.surfaces[].entry MUST resolve against (MAN-063).
	BundleFiles map[string]bool
	// MemoryFloorMiB is the host-configured minimum resources.memory (MAN-042).
	MemoryFloorMiB int
	// InstalledDataModelVersion is the dataModel.version of the pack's
	// currently installed manifest, if any (MAN-053). Zero means no prior
	// install exists, so the version-regression check is skipped.
	InstalledDataModelVersion int
}

// Error is a manifest/1 validation failure: one of the contract's Error
// taxonomy codes, a dotted path to the offending field, and a human message. It
// doubles as a Go error. The field-location serializes under the wire key
// "field" — the key the contract's ValidationResult shape and every corpus
// fixture use (matching the sibling datamodel/rules validators).
type Error struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e Error) Error() string { return e.Code + " at " + e.Field + ": " + e.Message }
