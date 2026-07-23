package deviceclass

import "sort"

// Registry is a validated, indexed device-class-registry/1 document — the
// resolution and consultation API rules/1's compiler and matching function,
// and the relay device plane's command vocabulary, resolve a device_class,
// command, attribute, or raw-value classification against (REG-001: data,
// never code). New builds a Registry only from a RawRegistry that already
// validates clean; Builtin wraps the platform's own canonical media-player
// content (REG-060).
type Registry struct {
	classes map[string]ClassEntry
}

// New validates reg against device-class-registry/1's structural grammar
// (Validate) and, when Validate reports zero violations, indexes it into a
// queryable Registry. When Validate reports any violation, New returns them
// and the returned Registry is the zero value — a caller MUST check len(errs)
// before querying it; a Registry is never built, even partially, from a
// document that fails its own structural grammar.
func New(reg RawRegistry) (Registry, []Error) {
	if errs := Validate(reg); len(errs) > 0 {
		return Registry{}, errs
	}
	classes := make(map[string]ClassEntry, len(reg.DeviceClasses))
	for id, e := range reg.DeviceClasses {
		classes[id] = e
	}
	return Registry{classes: classes}, nil
}

// Builtin returns the platform's canonical registry: the single built-in
// media-player class device-class-registry/1 requires (REG-060), byte-exact
// per REG-061-066 (TestBuiltinMediaPlayerMatchesCorpus). It is the one source
// every consumer — the rules/1 registry adapter and the relay device plane's
// command vocabulary — resolves device commands and states against.
func Builtin() Registry {
	reg, errs := New(builtinRawRegistry)
	if len(errs) != 0 {
		// The built-in registry's own content is contract-pinned and covered by
		// TestBuiltinRegistryValidatesClean; reaching this would mean the
		// built-in literal itself regressed, not a caller error.
		panic("deviceclass: built-in registry failed its own structural validation: " + errs[0].Error())
	}
	return reg
}

// ClassNames returns every device-class identifier the registry indexes, sorted
// ascending — the recognized-device-class set a host hands manifest/1 as its
// MAN-070 device-class registry (a manifest's devices[].deviceClass MUST name a
// member). Derived from the indexed registry rather than a hand-maintained
// duplicate, so it tracks the built-in content (media-player today) exactly.
func (r Registry) ClassNames() []string {
	names := make([]string, 0, len(r.classes))
	for name := range r.classes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Class resolves deviceClass to its ClassEntry, reporting false when no entry
// by that identifier exists in the registry (DEVICE_CLASS_UNKNOWN) — Registry
// never fabricates a class entry for an unresolved identifier.
func (r Registry) Class(deviceClass string) (ClassEntry, bool) {
	e, ok := r.classes[deviceClass]
	return e, ok
}

// CommandExists reports whether command names a declared command of
// deviceClass (REG-052). An unknown deviceClass reports false
// (DEVICE_CLASS_UNKNOWN); an undeclared command reports false
// (COMMAND_UNKNOWN). Neither case panics — both degrade to the same bool.
func (r Registry) CommandExists(deviceClass, command string) bool {
	_, ok := r.ResolveCommand(deviceClass, command)
	return ok
}

// ResolveCommand resolves command against deviceClass's declared commands
// (REG-052), returning the full Command declaration — name and typed params —
// so a caller can validate supplied arguments against it. An unknown
// deviceClass (DEVICE_CLASS_UNKNOWN) or an unresolved command name
// (COMMAND_UNKNOWN) both report the zero Command and false.
func (r Registry) ResolveCommand(deviceClass, command string) (Command, bool) {
	e, ok := r.classes[deviceClass]
	if !ok {
		return Command{}, false
	}
	for _, c := range e.Commands {
		if c.Name == command {
			return c, true
		}
	}
	return Command{}, false
}

// Classify implements State vocabulary's whitelist raw-value classification
// (REG-021): a rawValue that names one of deviceClass's own states IS that
// state; every other raw value — including one this driver has never seen —
// resolves to the class's own unknown_state_fallback (REG-022), never to a
// blacklisted-away permissive default. For an unknown deviceClass there is no
// class to consult and so no fallback to degrade to (DEVICE_CLASS_UNKNOWN);
// Classify returns "" — a caller distinguishing "unknown class" from
// "recognized fallback" MUST check Class first.
func (r Registry) Classify(deviceClass, rawValue string) string {
	e, ok := r.classes[deviceClass]
	if !ok {
		return ""
	}
	for _, s := range e.States {
		if s == rawValue {
			return s
		}
	}
	return e.UnknownStateFallback
}

// SemanticGroups returns the names of every semantic group deviceClass
// declares whose membership includes state (REG-030), read from the class's
// live group data on every call rather than a value snapshotted at Registry
// construction (REG-033) — a group's membership mutated after New returns is
// visible to this method's very next call, because it is the ClassEntry's own
// map read directly, never cached elsewhere. Built-in groups are consulted
// before extension-registered ones (REG-032); this package constructs only
// built-in groups today (an extension registers a class/group at runtime
// through ctx/1's pack↔host verb, out of scope here), so that ordering is
// realized as a stable, deterministic (sorted-by-name) scan over the single
// origin this task can produce. Reports nil for an unknown deviceClass or a
// state ungrouped by any of the class's semantic groups.
func (r Registry) SemanticGroups(deviceClass, state string) []string {
	e, ok := r.classes[deviceClass]
	if !ok {
		return nil
	}
	names := make([]string, 0, len(e.SemanticGroups))
	for name := range e.SemanticGroups {
		names = append(names, name)
	}
	sort.Strings(names)
	var groups []string
	for _, name := range names {
		for _, member := range e.SemanticGroups[name] {
			if member == state {
				groups = append(groups, name)
				break
			}
		}
	}
	return groups
}

// AttributeKind returns deviceClass's attr's declared type — one of "string",
// "number", "boolean", "enum" (REG-041) — or "" when deviceClass or attr is
// unknown. This is the contract's own raw type vocabulary; mapping it onto
// rules/1's ValueType (string/enum->String, number->Number, boolean->Boolean,
// absent->Unknown) is the rules/registry adapter's concern, not this
// package's.
func (r Registry) AttributeKind(deviceClass, attr string) string {
	a, ok := r.attribute(deviceClass, attr)
	if !ok {
		return ""
	}
	return a.Type
}

// ChangeEmission returns deviceClass's attr's declared change-emission class —
// "significant" or "cosmetic" (REG-044) — or "" when deviceClass or attr is
// unknown.
func (r Registry) ChangeEmission(deviceClass, attr string) string {
	a, ok := r.attribute(deviceClass, attr)
	if !ok {
		return ""
	}
	return a.ChangeEmission
}

// attribute finds deviceClass's typed attribute declaration named attr.
func (r Registry) attribute(deviceClass, attr string) (Attribute, bool) {
	e, ok := r.classes[deviceClass]
	if !ok {
		return Attribute{}, false
	}
	for _, a := range e.Attributes {
		if a.Name == attr {
			return a, true
		}
	}
	return Attribute{}, false
}
