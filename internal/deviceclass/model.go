// Package deviceclass is the canonical, contract-validated home of
// device-class-registry/1: the registry data model, its structural grammar
// (Validate), the resolution/consultation API, and the byte-exact built-in
// class content the platform ships. The registry is DATA a consumer interprets,
// never executable code (REG-001).
//
// This package is a dependency leaf: it imports the standard library only. The
// adapters that bridge it to rules/1's registry.Registry surface and to the
// relay device plane's command vocabulary live in those packages and import
// THIS one — one direction, no cycle.
package deviceclass

// RawRegistry is the registry's top-level document shape (REG-002): an object
// carrying a device_classes key mapping each device class's Class identifier to
// that class's ClassEntry. It is the raw, as-authored form Validate interprets
// and New indexes.
type RawRegistry struct {
	DeviceClasses map[string]ClassEntry `json:"device_classes"`
}

// ClassEntry is one device class's own published data (REG-002/010): its
// origin, closed state vocabulary, unknown-state fallback, named semantic groups
// over that vocabulary, typed attribute declarations, and command vocabulary.
type ClassEntry struct {
	// Origin is exactly one of built-in or extension-registered (REG-011).
	Origin string `json:"origin"`
	// States is the class's complete, closed set of canonical states — a
	// non-empty array of unique Snake identifiers (REG-020).
	States []string `json:"states"`
	// UnknownStateFallback is the member of States a driver classification
	// resolves an unrecognized raw value to (REG-021/022).
	UnknownStateFallback string `json:"unknown_state_fallback"`
	// SemanticGroups maps each group's Snake-identifier name to a non-empty
	// array of member states, each present in States (REG-030).
	SemanticGroups map[string][]string `json:"semantic_groups"`
	// Attributes are the class's typed attribute declarations (REG-040).
	Attributes []Attribute `json:"attributes"`
	// Commands are the class's command vocabulary (REG-050).
	Commands []Command `json:"commands"`
}

// Attribute is one typed attribute declaration (REG-040): its name, value type,
// optional unit, nullability, change-emission class, and — when type is enum —
// its closed set of valid values (REG-041).
type Attribute struct {
	Name string `json:"name"`
	// Type is exactly one of string, number, boolean, enum (REG-041).
	Type string `json:"type"`
	// Unit MAY be declared only when Type is number (REG-042); nil means
	// absent or JSON null, which is the only shape any other type may carry.
	Unit *string `json:"unit,omitempty"`
	// Nullable states whether the value MAY be absent/null; default false
	// (REG-043).
	Nullable bool `json:"nullable"`
	// ChangeEmission is exactly significant or cosmetic (REG-044).
	ChangeEmission string `json:"change_emission"`
	// Values enumerates an enum attribute's valid values — a non-empty array
	// of unique strings when Type is enum, absent otherwise (REG-041).
	Values []string `json:"values,omitempty"`
}

// Command is one command declaration (REG-050): its name and typed parameter
// list. Params is never omitted — an empty array when the command takes none
// (REG-051).
type Command struct {
	Name   string  `json:"name"`
	Params []Param `json:"params"`
}

// Param is one command parameter (REG-051): its name, type (the same closed
// vocabulary as Attribute, including the enum/Values coupling), and whether it
// is required.
type Param struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Values   []string `json:"values,omitempty"`
}
