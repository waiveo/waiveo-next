package manifest

// validateLocale enforces MAN-110/111. The reserved drivers/sources/
// diagnostics sections are tolerated absent or empty — this contract performs
// no check against their content (reserved for a future manifest/1 minor), so
// their absence is never itself a validation failure. The pack's bundle MUST
// include at least messages/en.json, the default-locale catalog every msg:
// reference this contract's grammars require ultimately resolves against.
func validateLocale(host HostRegistries) []Error {
	if !host.BundleFiles["messages/en.json"] {
		return []Error{{"MANIFEST_SCHEMA_INVALID", "messages",
			`the pack bundle MUST include "messages/en.json" (MAN-111)`}}
	}
	return nil
}
