// Package registry is the rules/1 consumer-side surface of the
// device-class-registry/1 contract: the interface every state comparison
// (RUL-320/321), typed-value comparison (RUL-260/261), attribute
// change-emission classification (RUL-330, device-class-registry/1 REG-044),
// and command-vocabulary check (RUL-160) resolves a `device_class` reference
// against. This package never authors registry content itself — a real
// deployment's registry is host/companion-document configuration, out of
// this contract's own scope (device-class-registry/1's scope note) — it only
// fixes the shape every consumer reads through, plus (fixture.go) the small,
// fixed fixture registry the rules-1 corpus documents beside itself.
package registry

// ValueType is a comparable value's declared type (RUL-260): an entity
// attribute's own declared type from the device-class registry, a trigger or
// condition's typed field, or Unknown when the registry carries no typed
// declaration for the attribute in question (an untyped/dynamic attribute) —
// a comparison against an Unknown-typed value MUST evaluate as not-matching
// rather than raising an error or coercing (RUL-262). An `enum`-typed
// attribute (device-class-registry/1 REG-041) compares as String: its values
// are plain strings, exact and case-sensitive (RUL-263); only the closed set
// of accepted values is enum-specific, and that is a compile-time concern
// (REG-041/RUL-261) this package does not encode.
type ValueType int

const (
	// Unknown means the registry has no typed declaration for the value in
	// question.
	Unknown ValueType = iota
	String
	Number
	Boolean
)

// String renders the ValueType's name for diagnostics.
func (t ValueType) String() string {
	switch t {
	case String:
		return "string"
	case Number:
		return "number"
	case Boolean:
		return "boolean"
	default:
		return "unknown"
	}
}

// Registry is the device-class-registry/1 data every state match, typed
// comparison, change-emission classification, and command-vocabulary check
// resolves against. Implementations MUST evaluate semantic-group membership
// live at call time, never snapshotted at compile time or engine start
// (RUL-322, device-class-registry/1 REG-033) — a group registered or
// extended after an engine restart MUST be visible to the very next call.
type Registry interface {
	// SemanticGroups returns every semantic group of deviceClass that state
	// is a member of, built-in groups first and then extension-registered
	// groups in that order (RUL-321, device-class-registry/1 REG-032); an
	// extension-registered group MUST NOT be able to shadow a built-in
	// group's membership. Returns nil for an unknown deviceClass or a state
	// that belongs to no group.
	SemanticGroups(deviceClass, state string) []string

	// AttributeType returns attr's declared value type for deviceClass
	// (RUL-260/261), or Unknown if deviceClass or attr has no typed
	// declaration.
	AttributeType(deviceClass, attr string) ValueType

	// ChangeEmission returns "significant" or "cosmetic" for a declared
	// attribute (device-class-registry/1 REG-044), or "" if deviceClass or
	// attr is not declared. Callers computing the RUL-330 aggregate
	// any-attribute-changed signal MUST treat "" the same as an explicit
	// "cosmetic": an undeclared attribute does not participate in that
	// signal either way.
	ChangeEmission(deviceClass, attr string) string

	// CommandExists reports whether command is declared in deviceClass's
	// command vocabulary (RUL-160, device-class-registry/1 REG-050/052).
	CommandExists(deviceClass, command string) bool
}
