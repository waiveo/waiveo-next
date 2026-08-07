package manifest

import (
	"strconv"
	"strings"
)

// Validate checks a PackManifest against the manifest/1 grammar, given the
// host's install-time registries, and returns EVERY violation it finds — never
// just the first — each Code one of the contract's Error taxonomy codes.
//
// This pass covers identity + compatibility (MAN-001/002/003/010/011/012/013),
// capabilities & consent (MAN-020/021), the egress allowlist (MAN-030/031),
// resource limits (MAN-040/042), the data model — collections, the universal
// entity envelope, retention, and connections (MAN-050-055) — UI page/slot/
// surface declarations (MAN-060-063), device contributions (MAN-070-072),
// playable contributions (MAN-080/081), automation contributions
// (MAN-090-092), server-side actions (MAN-100/102/103), and locale catalogs
// (MAN-110/111).
func Validate(m PackManifest, host HostRegistries) []Error {
	var errs []Error
	errs = append(errs, validateIdentity(m)...)
	errs = append(errs, validateIcon(m)...)
	errs = append(errs, validateCompat(m, host)...)
	errs = append(errs, validateCapabilities(m, host)...)
	errs = append(errs, validateEgress(m)...)
	errs = append(errs, validateResources(m, host)...)
	errs = append(errs, validateDataModel(m, host)...)
	errs = append(errs, validateUI(m, host)...)
	errs = append(errs, validateDevices(m, host)...)
	errs = append(errs, validatePlayable(m)...)
	errs = append(errs, validateAutomation(m, host)...)
	errs = append(errs, validateActions(m)...)
	errs = append(errs, validateLocale(host)...)
	return errs
}

// validateIdentity enforces MAN-001/002/003: the pack id, version string, and
// displayName grammars.
func validateIdentity(m PackManifest) []Error {
	var errs []Error
	if !IsPublisherNameID(m.ID) {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "id",
			"id MUST be <publisher>/<name>, each segment ^[a-z][a-z0-9-]{1,38}$ (MAN-001)"})
	}
	if !IsThreeComponentVersion(m.Version) {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "version",
			"version MUST be a three-component digits-only dotted string with no leading zero in a component (MAN-002)"})
	}
	if !IsMsgRef(m.DisplayName) {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "displayName",
			"displayName MUST be a msg: locale-catalog reference (MAN-003)"})
	}
	return errs
}

// validateIcon checks the OPTIONAL display icon: when declared, it MUST be a
// well-formed lucide glyph name (a lowercase kebab-case identifier), so a name
// can never carry markup, a path, or whitespace into the console's icon lookup.
// The host-allowed glyph SET is resolved at render, not here — an
// unrecognized-but-well-formed name degrades to the default extension glyph
// rather than failing install, so only a MALFORMED name is refused.
func validateIcon(m PackManifest) []Error {
	if m.Icon == "" {
		return nil
	}
	if !IsIconName(m.Icon) {
		return []Error{{"MANIFEST_SCHEMA_INVALID", "icon",
			`icon, when declared, MUST be a lowercase kebab-case lucide glyph name, got "` + m.Icon + `"`}}
	}
	return nil
}

// validateCompat enforces MAN-010/011/012/013: compat.ctx present and a valid
// version range, compat.renderer present with every page-type in the host
// registry, every compat.features flag recognized, and compat.relay present iff
// a devices block exists.
func validateCompat(m PackManifest, host HostRegistries) []Error {
	var errs []Error

	// MAN-010/013: ctx is required and MUST be a version-range string.
	if strings.TrimSpace(m.Compat.Ctx) == "" {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "compat.ctx",
			"compat.ctx is required and MUST be a version-range string (MAN-010)"})
	} else if _, err := ParseVersionRange(m.Compat.Ctx); err != nil {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "compat.ctx",
			"compat.ctx is not a valid version range (MAN-013): " + err.Error()})
	}

	// MAN-010: renderer is required (an empty array is valid; a missing key is
	// not). Every declared page type MUST be a host page-type registry member.
	if m.Compat.Renderer == nil {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "compat.renderer",
			"compat.renderer is required (MAN-010)"})
	}
	for i, r := range m.Compat.Renderer {
		if !host.PageTypes[r] {
			errs = append(errs, Error{"UNKNOWN_PAGE_TYPE",
				"compat.renderer[" + strconv.Itoa(i) + "]",
				"renderer page-type \"" + r + "\" is not in the host page-type registry (MAN-010)"})
		}
	}

	// MAN-012: every feature flag MUST be recognized by the host.
	for i, f := range m.Compat.Features {
		if !host.Features[f] {
			errs = append(errs, Error{"UNKNOWN_FEATURE_FLAG",
				"compat.features[" + strconv.Itoa(i) + "]",
				"feature flag \"" + f + "\" is not recognized by the host (MAN-012)"})
		}
	}

	// MAN-011/072: compat.relay present iff a non-empty devices block exists.
	// The devices-side of the cross-check (MAN-072) is finalized alongside the
	// devices section; the MAN-011 direction — no devices MUST omit relay — is
	// enforced here.
	hasDevices := len(m.Devices) > 0
	hasRelay := strings.TrimSpace(m.Compat.Relay) != ""
	switch {
	case hasDevices && !hasRelay:
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "compat.relay",
			"a pack with a non-empty devices array MUST declare compat.relay (MAN-072)"})
	case !hasDevices && hasRelay:
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "compat.relay",
			"a pack with no devices block MUST omit compat.relay (MAN-011)"})
	case hasRelay:
		// A declared relay range MUST itself be a valid version range (MAN-013).
		if _, err := ParseVersionRange(m.Compat.Relay); err != nil {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "compat.relay",
				"compat.relay is not a valid version range (MAN-013): " + err.Error()})
		}
	}

	return errs
}
