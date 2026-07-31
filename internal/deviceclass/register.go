package deviceclass

// register.go is device-class-registry/1's registration-time collision policy:
// REG-012 (a class identifier that already resolves) and REG-031 (a group name
// that already exists for its class). Both are "MUST be rejected outright",
// and both say the existing entry MUST NOT be shadowed, overridden, or merged
// with the rejected attempt's content.
//
// # Why this exists here rather than in the caller
//
// The pack→host registration VERB is ctx/1's and is deferred. The accept/reject
// DECISION is not: it is a pure function of the registry as it stands and the
// candidate being offered, and it is what that verb will call. Keeping the
// decision here means the verb, whenever it lands, cannot get the policy subtly
// different — and means the platform can produce these refusals at all.
//
// Until then the collision policy was computed inside the conformance driver
// and by nothing else, so the suite was green on a refusal the platform itself
// could not raise. That inverts what a driver is for: it observes the
// implementation, it does not supply the behaviour it then reports as present.
//
// # The no-shadow half is structural, not a rule to obey
//
// Both functions return a NEW Registry on success and the receiver UNCHANGED on
// refusal. A rejected attempt therefore cannot shadow, override or merge into
// the existing entry — there is no code path that mutates one in place, so
// "MUST NOT be shadowed" is a property of the shape rather than a discipline
// every future caller has to keep.
//
// # Registration validates, and that is not scope creep
//
// New states the registry's central invariant: "a Registry is never built, even
// partially, from a document that fails its own structural grammar." A
// registration path that skipped validation would be a second way to build one
// that does, which would quietly retire that invariant. So a candidate is
// validated with the same Validate every other construction path uses, and a
// structurally invalid candidate is refused with the code Validate itself
// assigns — not with a collision code, because it is not a collision.

// RegisterClass offers a new extension-registered class to the registry
// (REG-012).
//
// On success it returns a registry containing the new class; the receiver is
// unchanged, so a caller holding the old value still sees the old content. On
// refusal it returns the receiver and the violations.
//
// The rejection is deliberately blind to the EXISTING entry's origin. REG-012
// spells that out — "whether the existing entry is built-in or itself
// extension-registered" — because the tempting rule (extensions may not shadow
// built-ins, but may replace each other) is the one that lets two packs fight
// over an identifier and leaves resolution order deciding which device
// vocabulary a site actually gets.
func (r Registry) RegisterClass(id string, e ClassEntry) (Registry, []Error) {
	if _, exists := r.classes[id]; exists {
		return r, []Error{{
			Code:    "CLASS_IDENTIFIER_COLLISION",
			Path:    "device_classes." + id,
			Message: "a device class with identifier " + id + " already exists in the registry; a registration attempt MUST be rejected outright rather than shadow, override or merge with it (REG-012)",
		}}
	}
	// Structurally validated as a one-class document, so a candidate is held to
	// the identical grammar a registry document is. Validating the CANDIDATE
	// alone rather than the merged result keeps the diagnosis pointed at what the
	// caller offered: a violation already present in the existing registry is not
	// this registration's fault and must not be reported against it.
	if errs := Validate(RawRegistry{DeviceClasses: map[string]ClassEntry{id: e}}); len(errs) > 0 {
		return r, errs
	}

	next := make(map[string]ClassEntry, len(r.classes)+1)
	for k, v := range r.classes {
		next[k] = v
	}
	next[id] = e
	return Registry{classes: next}, nil
}

// RegisterGroup offers a new semantic group to an existing class (REG-031).
//
// A group name is unique for its class regardless of origin, so this too is
// blind to whether the existing group is built-in or extension-registered. The
// contract states the remedy in the same breath: an author who needs a
// related-but-different set of states registers it under a new, distinct name.
//
// Registering a group onto a class this registry does not carry is refused with
// DEVICE_CLASS_UNKNOWN — the taxonomy's own code for a device_class reference
// that does not resolve — rather than silently ignored. The driver's earlier
// version treated an unknown class as "not a collision" and returned accepted,
// which is the wrong answer to a different question: nothing was registered, and
// reporting that as acceptance would tell a pack its vocabulary was live.
func (r Registry) RegisterGroup(deviceClass, groupName string, members []string) (Registry, []Error) {
	entry, ok := r.classes[deviceClass]
	if !ok {
		return r, []Error{{
			Code:    "DEVICE_CLASS_UNKNOWN",
			Path:    "device_classes." + deviceClass,
			Message: "cannot register a semantic group onto device class " + deviceClass + ": no such class exists in this registry",
		}}
	}
	if _, exists := entry.SemanticGroups[groupName]; exists {
		return r, []Error{{
			Code:    "GROUP_NAME_COLLISION",
			Path:    "device_classes." + deviceClass + ".semantic_groups." + groupName,
			Message: "semantic group " + groupName + " already exists for device class " + deviceClass + "; a registration attempt MUST be rejected outright rather than redefine or merge into its membership — register a distinctly named group instead (REG-031)",
		}}
	}

	// Built by copying the class entry and its group map, then validating the
	// resulting class. REG-030's membership rule (every member state present in
	// States) is enforced by that validation rather than restated here, so a group
	// naming a state its class does not declare is refused for the reason the
	// grammar already gives.
	groups := make(map[string][]string, len(entry.SemanticGroups)+1)
	for k, v := range entry.SemanticGroups {
		groups[k] = v
	}
	groups[groupName] = members
	candidate := entry
	candidate.SemanticGroups = groups

	if errs := Validate(RawRegistry{DeviceClasses: map[string]ClassEntry{deviceClass: candidate}}); len(errs) > 0 {
		return r, errs
	}

	next := make(map[string]ClassEntry, len(r.classes))
	for k, v := range r.classes {
		next[k] = v
	}
	next[deviceClass] = candidate
	return Registry{classes: next}, nil
}
