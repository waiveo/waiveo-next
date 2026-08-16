package manifest

import "testing"

// validateRuntime (ui.go) shipped with no test pinning any of its refusals —
// the pilot extension is its first real consumer, and these are the shapes the
// pilot's packaging path can produce by mistake.

// runtimeHost is testHost with the declared entry present in the bundle, so a
// case about the EXEC grammar is not failed by the bundle-presence rule first.
func runtimeHost() HostRegistries {
	h := testHost()
	h.BundleFiles["bin/pack"] = true
	return h
}

// A declarative pack omits `runtime` entirely, and that omission is not an
// error: the tier is optional (MAN-065's closing sentence).
func TestValidateNoRuntimeBlockIsValid(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Runtime = nil
	if errs := Validate(m, testHost()); hasField(errs, "runtime.entry") || hasField(errs, "runtime.exec") {
		t.Fatalf("a pack with no runtime block must not fail runtime validation, got %+v", errs)
	}
}

// The contract's canonical code-carrying shape passes.
func TestValidateRuntimeCanonicalShapeOK(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Runtime = &Runtime{Entry: "bin/pack", Exec: []string{"$entry"}}
	if errs := Validate(m, runtimeHost()); hasField(errs, "runtime.entry") || hasField(errs, "runtime.exec") {
		t.Fatalf("the canonical runtime shape must validate, got %+v", errs)
	}
}

// An interpreter-style exec keeps the placeholder as its own token.
func TestValidateRuntimeInterpreterExecOK(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Runtime = &Runtime{Entry: "bin/pack", Exec: []string{"/usr/bin/env", "$entry", "--verbose"}}
	if errs := Validate(m, runtimeHost()); hasField(errs, "runtime.exec") {
		t.Fatalf("an exec with one $entry token among others must validate, got %+v", errs)
	}
}

// An entry naming no bundled file is a pack that installs and cannot start —
// refused while the publisher is still looking at it (MAN-065, the same rule
// ui.surfaces[].entry follows).
func TestValidateRuntimeEntryMustBeInTheBundle(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Runtime = &Runtime{Entry: "bin/not-bundled", Exec: []string{"$entry"}}
	errs := Validate(m, runtimeHost())
	if !hasCode(errs, "MANIFEST_SCHEMA_INVALID") || !hasField(errs, "runtime.entry") {
		t.Fatalf("an entry absent from the bundle MUST be rejected at runtime.entry, got %+v", errs)
	}
}

// A runtime block with an empty entry declares code and names nothing to run.
func TestValidateRuntimeEmptyEntryRejected(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Runtime = &Runtime{Entry: "", Exec: []string{"$entry"}}
	errs := Validate(m, runtimeHost())
	if !hasCode(errs, "MANIFEST_SCHEMA_INVALID") || !hasField(errs, "runtime.entry") {
		t.Fatalf("an empty runtime.entry MUST be rejected at runtime.entry, got %+v", errs)
	}
}

// MAN-065: exec MUST be non-empty. An omitted exec is a manifest that leaves
// the host to guess how to run the entry.
func TestValidateRuntimeEmptyExecRejected(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Runtime = &Runtime{Entry: "bin/pack", Exec: nil}
	errs := Validate(m, runtimeHost())
	if !hasCode(errs, "MANIFEST_SCHEMA_INVALID") || !hasField(errs, "runtime.exec") {
		t.Fatalf("an empty runtime.exec MUST be rejected at runtime.exec, got %+v", errs)
	}
}

// MAN-065: `$entry` appears exactly once. Zero placeholders is an exec that
// never runs the declared entry; two is an exec the host cannot substitute
// unambiguously. `--path=$entry` embeds it in a token the host substitutes
// whole-token-only, so it counts as zero — validating it would install a pack
// that runs with a literal `$entry` in its argv.
func TestValidateRuntimePlaceholderCountEnforced(t *testing.T) {
	for name, exec := range map[string][]string{
		"zero":     {"./bin/pack"},
		"two":      {"$entry", "$entry"},
		"embedded": {"--path=$entry"},
	} {
		t.Run(name, func(t *testing.T) {
			m := loadManifest(t, man001File)
			m.Runtime = &Runtime{Entry: "bin/pack", Exec: exec}
			errs := Validate(m, runtimeHost())
			if !hasField(errs, "runtime.exec") {
				t.Fatalf("exec %v MUST be rejected at runtime.exec, got %+v", exec, errs)
			}
		})
	}
}
