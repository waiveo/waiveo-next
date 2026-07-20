package manifest

import (
	"sort"
	"strconv"
	"strings"
)

// validateCapabilities enforces MAN-020/021: every requested capability grant is
// an object with a host-recognized capability name, a scope, and a msg-ref
// reason. An unrecognized capability is refused with a typed UNKNOWN_CAPABILITY
// error naming it — never a silent no-op grant. An empty capabilities array is
// valid (a pack that needs no grants).
func validateCapabilities(m PackManifest, host HostRegistries) []Error {
	var errs []Error
	for i, c := range m.Capabilities {
		at := "capabilities[" + strconv.Itoa(i) + "]"
		if !host.Capabilities[c.Capability] {
			errs = append(errs, Error{"UNKNOWN_CAPABILITY", at + ".capability",
				`capability "` + c.Capability + `" is not in the host capability registry`})
		}
		if strings.TrimSpace(c.Scope) == "" {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".scope",
				"capabilities[].scope is required (MAN-020)"})
		}
		if !IsMsgRef(c.Reason) {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".reason",
				"capabilities[].reason MUST be a msg: locale-catalog reference (MAN-020)"})
		}
	}
	return errs
}

// ConsentChange reports which of the four consent-relevant manifest subsets
// changed between a previously granted manifest and a candidate one (MAN-022/
// 023): capabilities, egress, resources, or connections. A change in any of them
// requires re-consent on that subset; a change anywhere else in the manifest
// (e.g. a locale-catalog edit) leaves every field false.
type ConsentChange struct {
	Capabilities bool
	Egress       bool
	Resources    bool
	Connections  bool
}

// RequiresReconsent reports whether any consent-relevant subset changed and so
// the update MUST re-prompt for consent (or park pending owner acknowledgment).
func (c ConsentChange) RequiresReconsent() bool {
	return c.Capabilities || c.Egress || c.Resources || c.Connections
}

// ConsentDiff computes the semantic diff MAN-023 mandates: it compares ONLY the
// capabilities, egress, resources, and connections fields between the previously
// granted manifest prev and the candidate next — never a raw whole-manifest hash
// — so a change outside that subset (a locale-catalog edit, a UI tweak, a new
// action) never triggers re-consent. The comparison is order-independent: a mere
// reordering of the same grants is not a change.
func ConsentDiff(prev, next PackManifest) ConsentChange {
	return ConsentChange{
		Capabilities: capabilitiesKey(prev.Capabilities) != capabilitiesKey(next.Capabilities),
		Egress:       stringSetKey(prev.Egress) != stringSetKey(next.Egress),
		Resources:    prev.Resources != next.Resources,
		Connections:  connectionsKey(prev.Connections) != connectionsKey(next.Connections),
	}
}

// capabilitiesKey is an order-independent canonical string for a capabilities
// array: each grant's (capability, scope, reason) tuple, sorted.
func capabilitiesKey(caps []Capability) string {
	parts := make([]string, len(caps))
	for i, c := range caps {
		parts[i] = c.Capability + "\x00" + c.Scope + "\x00" + c.Reason
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// connectionsKey is an order-independent canonical string for a connections
// array: each (provider, authType, sorted-scopes) tuple, sorted.
func connectionsKey(conns []Connection) string {
	parts := make([]string, len(conns))
	for i, c := range conns {
		scopes := append([]string(nil), c.Scopes...)
		sort.Strings(scopes)
		parts[i] = c.Provider + "\x00" + c.AuthType + "\x00" + strings.Join(scopes, "\x1f")
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// stringSetKey is an order-independent canonical string for a string array.
func stringSetKey(ss []string) string {
	parts := append([]string(nil), ss...)
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}
