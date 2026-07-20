package registry

import "github.com/maaxton/waiveo-next/internal/deviceclass"

// FixtureRegistry is the small, fixed built-in registry the rules-1 corpus
// documents beside itself (rules-1 Conformance notes: "corpus cases use a
// small fixed fixture registry documented beside the corpus, the same
// convention manifest/1's corpus notes establish"). It carries exactly one
// device class, media-player, whose states, semantic groups, typed attributes,
// and commands are NO LONGER hand-maintained here: they are sourced from the
// canonical, contract-validated device-class-registry/1 built-in
// (deviceclass.Builtin(), REG-060-066) through the FromDeviceClass adapter, so
// the media-player vocabulary is defined in exactly one place. FixtureRegistry
// is now a thin value whose methods delegate to that single adapter instance;
// callers keep constructing it as registry.FixtureRegistry{}.
//
// On top of the canonical built-in it carries a small, documented overlay of
// three corpus-only typed attributes the rules-1 corpus references but
// device-class-registry/1 does NOT pin as REG-064 built-in content:
//
//	volume       Number   significant  (corpus-only; RUL-023-attribute-only-vs-state-change)
//	media_title  String   cosmetic     (corpus-only; RUL-023-attribute-only-vs-state-change)
//	powered_on   Boolean  significant  (corpus-only; RUL-101-corroborating-and-cross-entity-conditions)
//
// These live ONLY as this overlay, never mixed into the canonical deviceclass
// content, and can only ADD to media-player — a REG-064 attribute of the same
// name always wins (FromDeviceClass never lets the overlay shadow it). The
// canonical built-in supplies the six REG-064 attributes (active_app,
// active_app_id, app_type[enum->String, RUL-263], power_mode, is_screensaver,
// app_version), the on/off semantic groups (REG-063), and the four commands
// launch/home/keypress/power (REG-066).
//
// FixtureRegistry has no extension-registered content of its own: every
// SemanticGroups call resolves only against the canonical built-in groups.
type FixtureRegistry struct{}

var _ Registry = FixtureRegistry{}

// corpusOverlay is the documented set of corpus-only typed attributes layered on
// top of the canonical media-player built-in (see FixtureRegistry). It ADDS
// only; it never shadows a REG-064 attribute.
var corpusOverlay = NewOverlay(map[string]map[string]OverlayAttr{
	"media-player": {
		"volume":      {Type: Number, ChangeEmission: "significant"},
		"media_title": {Type: String, ChangeEmission: "cosmetic"},
		"powered_on":  {Type: Boolean, ChangeEmission: "significant"},
	},
})

// fixtureAdapter is the single canonical-registry-backed Registry every
// FixtureRegistry value delegates to: the platform's built-in media-player class
// plus the corpus-only overlay. Built once at package initialization.
var fixtureAdapter = FromDeviceClass(deviceclass.Builtin(), corpusOverlay)

// SemanticGroups implements Registry.
func (FixtureRegistry) SemanticGroups(deviceClass, state string) []string {
	return fixtureAdapter.SemanticGroups(deviceClass, state)
}

// AttributeType implements Registry.
func (FixtureRegistry) AttributeType(deviceClass, attr string) ValueType {
	return fixtureAdapter.AttributeType(deviceClass, attr)
}

// ChangeEmission implements Registry.
func (FixtureRegistry) ChangeEmission(deviceClass, attr string) string {
	return fixtureAdapter.ChangeEmission(deviceClass, attr)
}

// CommandExists implements Registry.
func (FixtureRegistry) CommandExists(deviceClass, command string) bool {
	return fixtureAdapter.CommandExists(deviceClass, command)
}
