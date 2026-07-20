package deviceclass

import (
	"reflect"
	"testing"
)

// TestBuiltinMediaPlayerMatchesCorpus is the byte-exactness gate: the built-in
// media-player ClassEntry (device-class-registry/1 REG-060-066) MUST deep-equal
// the frozen corpus oracle field-for-field — the eight-member state vocabulary in
// order (REG-061), off as the unknown-state fallback (REG-062), the on/off semantic
// groups (REG-063), all six typed attributes with their types/nullability/values/
// change_emission (REG-064), and all four commands with their params (REG-066). No
// added, missing, renamed, or reordered member survives this assertion.
func TestBuiltinMediaPlayerMatchesCorpus(t *testing.T) {
	corpus := loadCorpusRegistry(t)
	want, ok := corpus.DeviceClasses["media-player"]
	if !ok {
		t.Fatal("corpus is missing the media-player class")
	}
	got, ok := builtinRawRegistry.DeviceClasses["media-player"]
	if !ok {
		t.Fatal("the built-in registry is missing the media-player class (REG-060)")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("built-in media-player ClassEntry drifted from the corpus (REG-060-066)\n built-in: %+v\n  corpus:  %+v", got, want)
	}
}

// TestBuiltinRegistryValidatesClean asserts the built-in registry passes the full
// structural grammar with zero errors — the corpus's own expected.valid == true.
func TestBuiltinRegistryValidatesClean(t *testing.T) {
	if errs := Validate(builtinRawRegistry); len(errs) != 0 {
		t.Fatalf("the built-in registry MUST validate with zero errors, got %d: %+v", len(errs), errs)
	}
}
