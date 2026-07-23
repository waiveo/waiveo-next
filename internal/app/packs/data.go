package packs

// This file is the pack-data write gate: the pure, store-agnostic validation of a
// row written to one of a pack's manifest-declared collections. A row carries the
// universal entity envelope (MAN-051) beside its declared fields, and this is the
// single place a write is checked against BOTH — the envelope invariants
// (scope_node a required ULID; lifecycle_state per the collection's MAN-052
// declaration; labels an array of strings; template_ref a ULID; params an object
// present only with a template_ref) and each declared field's type. No pack code
// runs here or anywhere: a pack is data, and this validates data.
//
// Retention/quota (MAN-054/040) are DEFERRED this wave: a collection's row count
// and age are unbounded pending the retention-enforcement task. The manifest's
// retention descriptor is already validated at install (internal/manifest); its
// enforcement — pruning by maxAge/maxRows, rejecting a write past storageQuota —
// is a later, separate increment and is intentionally not applied to writes here.

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"

	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// Row lifecycle states (MAN-051): the closed set lifecycle_state may hold.
const (
	lifecycleDraft     = "draft"
	lifecyclePublished = "published"
	lifecycleArchived  = "archived"
)

// draftPublishAnnotation is the MAN-052 field annotation that opts a collection
// into the draft/publish lifecycle flow.
const draftPublishAnnotation = "draft-publish"

// hostOwnedRowFields are the envelope fields the HOST owns (MAN-051): entity_id is
// assigned once and immutable, revision is host-incremented, created_at/updated_at
// are store timestamps. A write body MAY echo them (a GET→edit→PATCH roundtrip of
// the whole row) but they are silently ignored — never client-settable — so a
// naive full-object PATCH round-trips without a spurious rejection.
var hostOwnedRowFields = map[string]bool{
	"entity_id":  true,
	"revision":   true,
	"created_at": true,
	"updated_at": true,
}

// envelopeWriteFields are the universal-envelope fields a client MAY set on a
// pack-data write (MAN-051), plus the api/1 client-assignable external_id grouping
// key (external_id is NOT part of the MAN-051 envelope, but rides the same write
// body). Any body key outside this set, the host-owned set, and the collection's
// declared fields is an undeclared field and is refused.
var envelopeWriteFields = map[string]bool{
	"lifecycle_state": true,
	"scope_node":      true,
	"labels":          true,
	"template_ref":    true,
	"params":          true,
	"external_id":     true,
}

// RowError is one pack-data write validation failure — a declared field whose
// value does not match its declared type, or an envelope-invariant violation. The
// api layer renders a slice of these as the api/1 `errors` extension (API-013):
// one {field, code, message} object per failure, under the registry-valid
// top-level VALIDATION_FAILED (API-011).
type RowError struct {
	Field   string
	Code    string
	Message string
}

// ValidatedRow is the store-ready result of a passing ValidateRowWrite: the parsed
// universal envelope (host-owned entity_id/revision assigned by the store, not
// here) plus the canonical declared-fields Body. It maps 1:1 onto a store.PackRow's
// caller-supplied fields.
type ValidatedRow struct {
	LifecycleState string
	ScopeNode      string
	Labels         []string
	TemplateRef    string
	Params         json.RawMessage
	ExternalID     string
	Body           json.RawMessage
}

// CollectionByName returns the manifest's declared collection with name, and
// whether it exists (MAN-051). An undeclared collection is the api layer's 404.
func CollectionByName(m *manifest.PackManifest, name string) (manifest.Collection, bool) {
	for _, c := range m.DataModel.Collections {
		if c.Name == name {
			return c, true
		}
	}
	return manifest.Collection{}, false
}

// collectionParticipatesInLifecycle reports whether collection c opts into the
// draft/publish lifecycle flow (MAN-052): true iff any declared field carries the
// `lifecycle: draft-publish` annotation. A collection WITHOUT it treats every row
// as `published` and rejects a write of any other lifecycle_state.
func collectionParticipatesInLifecycle(c manifest.Collection) bool {
	for _, f := range c.Fields {
		if f.Lifecycle == draftPublishAnnotation {
			return true
		}
	}
	return false
}

// ValidateRowWrite validates a complete effective pack-data row body against
// collection c and, on success, returns the store-ready envelope + declared-fields
// body. The body is the whole intended row: on a create it is the request body
// (absent envelope fields take their defaults — lifecycle_state `published`, labels
// `[]`); on a patch it is the current row shallow-merged with the request patch, so
// unchanged envelope/declared values are inherited and this one path validates
// both. Every failure is collected (not short-circuited) so a client sees all of
// them at once. Host-owned fields in the body are ignored; a body key that is
// neither an envelope field, external_id, host-owned, nor a declared field is an
// undeclared field and is refused.
//
// Nothing here executes pack content — the body is parsed as data and checked
// against the manifest's declared shape.
func ValidateRowWrite(c manifest.Collection, body []byte) (ValidatedRow, []RowError) {
	raw := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			return ValidatedRow{}, []RowError{{Field: "", Code: "VALIDATION_FAILED",
				Message: "the request body must be a JSON object"}}
		}
	}

	var errs []RowError
	out := ValidatedRow{LifecycleState: lifecyclePublished, Labels: []string{}}

	// scope_node — a required scope-node ULID reference (MAN-051).
	if v, ok := raw["scope_node"]; ok && !isJSONNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			errs = append(errs, RowError{"scope_node", "INVALID_TYPE", "scope_node must be a string"})
		} else {
			out.ScopeNode = s
		}
	}
	if out.ScopeNode == "" {
		errs = append(errs, RowError{"scope_node", "REQUIRED",
			"scope_node is required (a scope-node ULID reference)"})
	} else if !ulid.Valid(out.ScopeNode) {
		errs = append(errs, RowError{"scope_node", "INVALID_ULID", "scope_node must be a valid ULID"})
	}

	// lifecycle_state — MAN-052. A participating collection accepts the closed set
	// {draft, published, archived}; a non-participating one accepts only published.
	participates := collectionParticipatesInLifecycle(c)
	if v, ok := raw["lifecycle_state"]; ok && !isJSONNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			errs = append(errs, RowError{"lifecycle_state", "INVALID_TYPE", "lifecycle_state must be a string"})
		} else if s != "" {
			out.LifecycleState = s
		}
	}
	if participates {
		switch out.LifecycleState {
		case lifecycleDraft, lifecyclePublished, lifecycleArchived:
		default:
			errs = append(errs, RowError{"lifecycle_state", "INVALID_LIFECYCLE_STATE",
				`lifecycle_state must be one of "draft", "published", "archived"`})
		}
	} else if out.LifecycleState != lifecyclePublished {
		errs = append(errs, RowError{"lifecycle_state", "LIFECYCLE_NOT_ALLOWED",
			`this collection does not declare "lifecycle: draft-publish"; every row is "published" and no other lifecycle_state may be written (MAN-052)`})
	}

	// labels — an array of strings, may be empty (MAN-051).
	if v, ok := raw["labels"]; ok && !isJSONNull(v) {
		var ls []string
		if err := json.Unmarshal(v, &ls); err != nil {
			errs = append(errs, RowError{"labels", "INVALID_TYPE", "labels must be an array of strings"})
		} else {
			out.Labels = ls
		}
	}

	// template_ref — an optional ULID; params — an optional object present ONLY
	// when template_ref is present (MAN-051).
	if v, ok := raw["template_ref"]; ok && !isJSONNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			errs = append(errs, RowError{"template_ref", "INVALID_TYPE", "template_ref must be a string"})
		} else {
			out.TemplateRef = s
		}
	}
	if out.TemplateRef != "" && !ulid.Valid(out.TemplateRef) {
		errs = append(errs, RowError{"template_ref", "INVALID_ULID", "template_ref must be a valid ULID"})
	}
	if v, ok := raw["params"]; ok && !isJSONNull(v) {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v, &obj); err != nil {
			errs = append(errs, RowError{"params", "INVALID_TYPE", "params must be an object"})
		} else {
			out.Params = append(json.RawMessage(nil), v...)
		}
		if out.TemplateRef == "" {
			errs = append(errs, RowError{"params", "PARAMS_WITHOUT_TEMPLATE",
				"params may be present only when template_ref is present (MAN-051)"})
		}
	}

	// external_id — the api/1 client-assignable grouping key (optional string).
	if v, ok := raw["external_id"]; ok && !isJSONNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			errs = append(errs, RowError{"external_id", "INVALID_TYPE", "external_id must be a string"})
		} else {
			out.ExternalID = s
		}
	}

	// Declared fields + undeclared-field detection. Each body key that is not an
	// envelope field, external_id, or host-owned MUST name a declared field, and
	// its value (when non-null) MUST match that field's declared type.
	declared := make(map[string]manifest.Field, len(c.Fields))
	for _, f := range c.Fields {
		declared[f.Name] = f
	}
	fieldBody := map[string]json.RawMessage{}
	for key, val := range raw {
		if hostOwnedRowFields[key] || envelopeWriteFields[key] {
			continue
		}
		f, ok := declared[key]
		if !ok {
			errs = append(errs, RowError{key, "UNKNOWN_FIELD",
				"field " + strconv.Quote(key) + " is not declared by this collection"})
			continue
		}
		if !isJSONNull(val) && !typeMatches(f.Type, val) {
			errs = append(errs, RowError{key, "INVALID_TYPE",
				"field " + strconv.Quote(key) + " must be of type " + strconv.Quote(f.Type)})
			continue
		}
		fieldBody[key] = append(json.RawMessage(nil), val...)
	}

	bodyJSON, err := json.Marshal(fieldBody)
	if err != nil {
		return ValidatedRow{}, []RowError{{Field: "", Code: "VALIDATION_FAILED",
			Message: "the declared fields could not be encoded"}}
	}
	out.Body = bodyJSON
	return out, errs
}

// typeMatches reports whether a declared field's JSON value matches its declared
// type. The recognized scalar types are `string`, `number`, `integer`, and
// `boolean`; an UNRECOGNIZED type is accepted permissively (any JSON) — the
// manifest engine does not constrain the declared-type vocabulary, so this gate
// enforces the types it knows and passes the rest through rather than rejecting a
// manifest the install already accepted. A JSON null is handled by the caller
// (treated as clearing the field, never type-checked).
func typeMatches(declaredType string, raw json.RawMessage) bool {
	switch declaredType {
	case "string":
		var s string
		return json.Unmarshal(raw, &s) == nil
	case "boolean":
		var b bool
		return json.Unmarshal(raw, &b) == nil
	case "number":
		var n float64
		return json.Unmarshal(raw, &n) == nil
	case "integer":
		var n float64
		if json.Unmarshal(raw, &n) != nil {
			return false
		}
		return n == math.Trunc(n)
	default:
		return true
	}
}

// isJSONNull reports whether raw is the JSON literal null.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
