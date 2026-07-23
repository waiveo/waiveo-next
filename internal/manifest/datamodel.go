package manifest

import (
	"encoding/json"
	"strconv"
)

// universalEnvelopeFields is the set of host-managed field names every
// declared collection's rows carry in addition to their declared fields
// (MAN-051). A manifest MUST NOT redeclare any of them as its own field.
var universalEnvelopeFields = map[string]bool{
	"entity_id":       true,
	"revision":        true,
	"lifecycle_state": true,
	"scope_node":      true,
	"labels":          true,
	"template_ref":    true,
	"params":          true,
}

// reservedRowFields is the api/1 resource-row baseline every pack-declared
// collection's rows ALSO carry as host-managed columns, alongside the MAN-051
// universal entity envelope: the client-assignable `external_id` grouping key
// (api/1 API-100–104) and the store's `created_at`/`updated_at` timestamps
// (data-model/1 DAT-008). They are not part of the MAN-051 envelope itself, but
// the pack-data write gate intercepts a body key by any of these names as a
// host/envelope field BEFORE consulting the declared fields — so a collection
// declaring one would have that field's client value silently dropped (or, for
// external_id, silently repurposed as the uniqueness key) and overwritten on
// read. A manifest MUST NOT redeclare any of them as its own field either, for
// the same reason it MUST NOT redeclare a universalEnvelopeFields name.
var reservedRowFields = map[string]bool{
	"external_id": true,
	"created_at":  true,
	"updated_at":  true,
}

// validateDataModel enforces MAN-050-055: a positive, non-regressing
// dataModel.version; pack-unique collection names whose fields never redeclare
// a universal-entity-envelope field and carry at most one role:title
// annotation per collection; a retention descriptor keyed only by declared
// collection names; and connections resolving against the host's connection
// registry.
func validateDataModel(m PackManifest, host HostRegistries) []Error {
	var errs []Error

	// MAN-050: dataModel.version MUST be a positive integer.
	if m.DataModel.Version <= 0 {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "dataModel.version",
			"dataModel.version MUST be a positive integer (MAN-050)"})
	}

	// MAN-053: an update's dataModel.version MUST NOT regress below the
	// currently installed version. Zero means no prior install (fresh
	// install), so the check is skipped.
	if host.InstalledDataModelVersion > 0 && m.DataModel.Version < host.InstalledDataModelVersion {
		errs = append(errs, Error{"DATAMODEL_VERSION_REGRESSION", "dataModel.version",
			"dataModel.version " + strconv.Itoa(m.DataModel.Version) +
				" is lower than the currently installed version " +
				strconv.Itoa(host.InstalledDataModelVersion) + " (MAN-053)"})
	}

	declared := map[string]bool{}
	seenNames := map[string]bool{}
	for ci, c := range m.DataModel.Collections {
		at := "dataModel.collections[" + strconv.Itoa(ci) + "]"

		// MAN-051: collection name is required and unique within the pack.
		if c.Name == "" {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				"dataModel.collections[].name is required (MAN-051)"})
		} else if seenNames[c.Name] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".name",
				`collection name "` + c.Name + `" is declared more than once (MAN-051)`})
		}
		seenNames[c.Name] = true
		declared[c.Name] = true

		titleCount := 0
		for fi, f := range c.Fields {
			fat := at + ".fields[" + strconv.Itoa(fi) + "]"

			// MAN-051: a declared field MUST NOT redeclare a
			// universal-entity-envelope field name — every row already
			// carries it in addition to declared fields.
			if universalEnvelopeFields[f.Name] {
				errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", fat + ".name",
					`field name "` + f.Name + `" collides with the universal entity envelope, ` +
						"which every row carries in addition to declared fields (MAN-051)"})
			} else if reservedRowFields[f.Name] {
				// external_id/created_at/updated_at ride every row as host-managed
				// api/1-baseline columns beside the MAN-051 envelope; a declared
				// field by any of these names is silently swallowed by the pack-data
				// write gate, so it is refused at install just like an envelope
				// collision (MAN-051).
				errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", fat + ".name",
					`field name "` + f.Name + `" collides with a host-managed row field ` +
						"(external_id, created_at, updated_at) every row carries in addition to declared fields (MAN-051)"})
			}

			// MAN-052: role, if present, MUST be "title" or "summary".
			switch f.Role {
			case "", "title", "summary":
			default:
				errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", fat + ".role",
					`field role "` + f.Role + `" MUST be "title" or "summary" (MAN-052)`})
			}
			if f.Role == "title" {
				titleCount++
			}

			// MAN-052: lifecycle, if present, MUST be "draft-publish".
			switch f.Lifecycle {
			case "", "draft-publish":
			default:
				errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", fat + ".lifecycle",
					`field lifecycle "` + f.Lifecycle + `" MUST be "draft-publish" (MAN-052)`})
			}
		}
		// MAN-052: at most one field per collection may declare role:title.
		if titleCount > 1 {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at,
				"at most one field per collection may declare role \"title\", got " +
					strconv.Itoa(titleCount) + " (MAN-052)"})
		}
	}

	// MAN-054: retention is keyed by collection name; a key naming an
	// undeclared collection is refused. Each value MUST be "unbounded" or a
	// bounded {maxAge}/{maxRows} descriptor.
	for name, raw := range m.Retention {
		at := "retention." + name
		if !declared[name] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at,
				`retention key "` + name + `" does not name a declared dataModel.collections entry (MAN-054)`})
			continue
		}
		if msg := retentionEntryProblem(raw); msg != "" {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at, msg})
		}
	}

	// MAN-055: each connection's provider/authType pair MUST be registered
	// with the host.
	for i, c := range m.Connections {
		at := "connections[" + strconv.Itoa(i) + "]"
		key := c.Provider + "/" + c.AuthType
		if !host.Providers[key] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at,
				`connections provider/authType "` + key + `" is not in the host connection registry (MAN-055)`})
		}
	}

	return errs
}

// retentionEntryProblem returns a human message describing why raw is not a
// valid MAN-054 retention descriptor ("unbounded" or an object naming exactly
// one of maxAge/maxRows, each a positive integer), or "" if it is valid.
func retentionEntryProblem(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "unbounded" {
			return ""
		}
		return `retention value "` + s + `" MUST be "unbounded" or a {maxAge}/{maxRows} descriptor (MAN-054)`
	}

	var obj struct {
		MaxAge  *int `json:"maxAge"`
		MaxRows *int `json:"maxRows"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return `retention value MUST be "unbounded" or a {maxAge}/{maxRows} descriptor (MAN-054)`
	}
	switch {
	case obj.MaxAge != nil && obj.MaxRows == nil:
		if *obj.MaxAge <= 0 {
			return "retention maxAge MUST be a positive integer (MAN-054)"
		}
		return ""
	case obj.MaxRows != nil && obj.MaxAge == nil:
		if *obj.MaxRows <= 0 {
			return "retention maxRows MUST be a positive integer (MAN-054)"
		}
		return ""
	default:
		return "retention descriptor MUST declare exactly one of maxAge or maxRows (MAN-054)"
	}
}
