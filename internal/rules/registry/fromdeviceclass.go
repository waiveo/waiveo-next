package registry

import "github.com/maaxton/waiveo-next/internal/deviceclass"

// FromDeviceClass adapts a canonical device-class-registry/1 registry
// (internal/deviceclass) to the rules/1 registry.Registry surface, so the
// media-player vocabulary a state match, typed comparison, change-emission
// classification, and command-vocabulary check resolve against is defined ONCE
// — in the contract-validated canonical registry — rather than duplicated in a
// hand-maintained fixture.
//
// The adapter delegates semantic-group membership (REG-032/033) and command
// existence (REG-052) straight to dc, and maps dc's raw attribute type vocabulary
// (device-class-registry/1 REG-041: "string"|"number"|"boolean"|"enum") onto
// rules/1's ValueType — "string"/"enum"->String (an enum compares as its string
// value, RUL-260/263), "number"->Number, "boolean"->Boolean, absent->Unknown.
// That mapping lives HERE, in the consumer adapter, never in the leaf
// deviceclass package (which exposes only the raw contract kind, AttributeKind).
//
// overlay carries the small set of corpus-only typed attributes a rules fixture
// references but device-class-registry/1 does not pin as REG-064 built-in
// content. It is merged into AttributeType and ChangeEmission ONLY — never into
// commands or semantic groups — and a base class's own REG-064 attribute ALWAYS
// wins over an overlay entry of the same name: the overlay can only ADD an
// attribute the canonical class lacks, never shadow one it declares (REG-064).
func FromDeviceClass(dc deviceclass.Registry, overlay Overlay) Registry {
	return fromDeviceClass{dc: dc, overlay: overlay}
}

// fromDeviceClass is the registry.Registry implementation FromDeviceClass returns:
// a canonical deviceclass.Registry plus a documented corpus-only Overlay.
type fromDeviceClass struct {
	dc      deviceclass.Registry
	overlay Overlay
}

var _ Registry = fromDeviceClass{}

// SemanticGroups delegates straight to the canonical registry's live
// group consultation (REG-032/033): built-in groups first, evaluated at call
// time, nil for an unknown class or an ungrouped state. The overlay never
// contributes groups.
func (a fromDeviceClass) SemanticGroups(deviceClass, state string) []string {
	return a.dc.SemanticGroups(deviceClass, state)
}

// AttributeType resolves attr's rules/1 ValueType. The canonical class wins: a
// REG-064-declared attribute's kind (AttributeKind != "") maps to its ValueType
// and the overlay is not consulted. Only when the canonical class declares no
// such attribute does the class-scoped overlay ADD one; absent from both, the
// type is Unknown (RUL-262 — an Unknown-typed comparison does not match).
func (a fromDeviceClass) AttributeType(deviceClass, attr string) ValueType {
	if kind := a.dc.AttributeKind(deviceClass, attr); kind != "" {
		return valueTypeForKind(kind)
	}
	if ov, ok := a.overlay.attribute(deviceClass, attr); ok {
		return ov.Type
	}
	return Unknown
}

// ChangeEmission returns attr's change-emission class (REG-044). The canonical
// class wins: a declared attribute's non-empty emission is returned and the
// overlay is not consulted (a REG-064 attribute always carries "significant" or
// "cosmetic", so "" unambiguously means the canonical class does not declare
// attr). Only then does the class-scoped overlay supply one; absent from both,
// "".
func (a fromDeviceClass) ChangeEmission(deviceClass, attr string) string {
	if ce := a.dc.ChangeEmission(deviceClass, attr); ce != "" {
		return ce
	}
	if ov, ok := a.overlay.attribute(deviceClass, attr); ok {
		return ov.ChangeEmission
	}
	return ""
}

// CommandExists delegates straight to the canonical command vocabulary
// (REG-052); the overlay never contributes commands.
func (a fromDeviceClass) CommandExists(deviceClass, command string) bool {
	return a.dc.CommandExists(deviceClass, command)
}

// valueTypeForKind maps device-class-registry/1's raw attribute/param type
// vocabulary (REG-041) onto rules/1's ValueType. An enum is compared as its
// plain string value (RUL-263), so it maps to String; an unrecognized or empty
// kind maps to Unknown.
func valueTypeForKind(kind string) ValueType {
	switch kind {
	case "string", "enum":
		return String
	case "number":
		return Number
	case "boolean":
		return Boolean
	default:
		return Unknown
	}
}

// OverlayAttr is one overlay attribute's already-mapped rules/1 typing: its
// ValueType and its REG-044 change-emission class ("significant" or "cosmetic").
type OverlayAttr struct {
	Type           ValueType
	ChangeEmission string
}

// Overlay carries corpus-only typed attributes an adapter merges ON TOP of a
// canonical registry, keyed by device class then attribute name. These are
// attributes a rules fixture references that device-class-registry/1 does not
// pin as REG-064 built-in content, so they live here as a documented overlay and
// are never mixed into the canonical deviceclass package. The zero value is a
// usable empty overlay (contributes nothing).
type Overlay struct {
	attrs map[string]map[string]OverlayAttr
}

// NewOverlay builds an Overlay from a per-device-class map of extra typed
// attributes, copying the input so the returned Overlay is not aliased to the
// caller's maps.
func NewOverlay(attrs map[string]map[string]OverlayAttr) Overlay {
	if len(attrs) == 0 {
		return Overlay{}
	}
	cp := make(map[string]map[string]OverlayAttr, len(attrs))
	for class, byName := range attrs {
		inner := make(map[string]OverlayAttr, len(byName))
		for name, a := range byName {
			inner[name] = a
		}
		cp[class] = inner
	}
	return Overlay{attrs: cp}
}

// attribute looks up an overlay entry by device class and attribute name. The
// nil-map lookups make the zero-value Overlay safe: it resolves nothing.
func (o Overlay) attribute(deviceClass, attr string) (OverlayAttr, bool) {
	a, ok := o.attrs[deviceClass][attr]
	return a, ok
}
