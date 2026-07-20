package deviceclass

import (
	"reflect"
	"testing"
)

// TestNewValidatesFirst: New MUST run Validate before indexing and surface
// every violation it reports — a Registry MUST NOT be built from a document
// that fails its own structural grammar.
func TestNewValidatesFirst(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.Origin = "bogus" // ORIGIN_INVALID (REG-011)
	reg.DeviceClasses["media-player"] = e
	_, errs := New(reg)
	if !hasCode(errs, "ORIGIN_INVALID") {
		t.Fatalf("expected New to surface ORIGIN_INVALID from Validate, got %+v", errs)
	}
}

// TestNewIndexesCleanRegistry: a structurally clean RawRegistry indexes into
// a queryable Registry with zero errors.
func TestNewIndexesCleanRegistry(t *testing.T) {
	reg := loadCorpusRegistry(t)
	r, errs := New(reg)
	if len(errs) != 0 {
		t.Fatalf("expected zero errors indexing the clean corpus registry, got %+v", errs)
	}
	if !r.CommandExists("media-player", "launch") {
		t.Fatal("a New-built Registry did not index media-player's own commands")
	}
}

// TestClass: Class resolves a known device class and reports false — never
// panics or fabricates an entry — for one that does not exist
// (DEVICE_CLASS_UNKNOWN).
func TestClass(t *testing.T) {
	r := Builtin()
	if _, ok := r.Class("media-player"); !ok {
		t.Fatal("expected media-player to resolve")
	}
	if _, ok := r.Class("nope"); ok {
		t.Fatal("expected an unknown class to report false (DEVICE_CLASS_UNKNOWN)")
	}
}

// TestCommandExists (REG-052): a declared command resolves true, an
// undeclared one resolves false (COMMAND_UNKNOWN), and an unknown device
// class resolves false (DEVICE_CLASS_UNKNOWN) — never a panic.
func TestCommandExists(t *testing.T) {
	r := Builtin()
	if !r.CommandExists("media-player", "launch") {
		t.Fatal("expected launch to exist on media-player")
	}
	if r.CommandExists("media-player", "fly") {
		t.Fatal("expected fly to NOT exist on media-player (COMMAND_UNKNOWN)")
	}
	if r.CommandExists("nope", "launch") {
		t.Fatal("expected an unknown class to report false (DEVICE_CLASS_UNKNOWN)")
	}
}

// TestResolveCommand: resolves the full Command declaration — name and typed
// params — so a caller can validate supplied arguments, and degrades to the
// zero Command + false for an unresolved command or an unknown class.
func TestResolveCommand(t *testing.T) {
	r := Builtin()
	cmd, ok := r.ResolveCommand("media-player", "power")
	if !ok {
		t.Fatal("expected power to resolve")
	}
	want := Command{
		Name:   "power",
		Params: []Param{{Name: "state", Type: "enum", Values: []string{"on", "off"}, Required: true}},
	}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("power command mismatch:\n got:  %+v\n want: %+v", cmd, want)
	}
	if _, ok := r.ResolveCommand("media-player", "fly"); ok {
		t.Fatal("expected fly to NOT resolve (COMMAND_UNKNOWN)")
	}
	if _, ok := r.ResolveCommand("nope", "launch"); ok {
		t.Fatal("expected an unknown class to NOT resolve (DEVICE_CLASS_UNKNOWN)")
	}
}

// TestClassify (REG-021/022): a whitelist — a recognized raw value IS its own
// state, and every unrecognized raw value resolves to unknown_state_fallback
// (off, REG-062), never the reverse (never a blacklist).
func TestClassify(t *testing.T) {
	r := Builtin()
	cases := []struct{ raw, want string }{
		{"playing", "playing"},
		{"garbage", "off"},
		{"on", "on"},
	}
	for _, tc := range cases {
		if got := r.Classify("media-player", tc.raw); got != tc.want {
			t.Fatalf("Classify(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	if got := r.Classify("nope", "on"); got != "" {
		t.Fatalf("expected an unknown class to classify to \"\" (DEVICE_CLASS_UNKNOWN), got %q", got)
	}
}

// TestSemanticGroups (REG-030/032/033): reports every group whose membership
// includes state, nil for an unknown class or an ungrouped state.
func TestSemanticGroups(t *testing.T) {
	r := Builtin()
	cases := []struct {
		state string
		want  []string
	}{
		{"playing", []string{"on"}},
		{"standby", []string{"off"}},
		{"off", []string{"off"}},
	}
	for _, tc := range cases {
		got := r.SemanticGroups("media-player", tc.state)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("SemanticGroups(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
	if got := r.SemanticGroups("nope", "on"); got != nil {
		t.Fatalf("expected an unknown class to report nil groups, got %v", got)
	}
	if got := r.SemanticGroups("media-player", "no-such-state"); got != nil {
		t.Fatalf("expected an ungrouped state to report nil groups, got %v", got)
	}
}

// TestAttributeKind: returns the contract's own raw type vocabulary
// (string|number|boolean|enum), "" for an unknown attribute or class.
func TestAttributeKind(t *testing.T) {
	r := Builtin()
	if got := r.AttributeKind("media-player", "app_type"); got != "enum" {
		t.Fatalf("AttributeKind(app_type) = %q, want enum", got)
	}
	if got := r.AttributeKind("media-player", "is_screensaver"); got != "boolean" {
		t.Fatalf("AttributeKind(is_screensaver) = %q, want boolean", got)
	}
	if got := r.AttributeKind("media-player", "no-such-attr"); got != "" {
		t.Fatalf("expected an unknown attribute to report \"\", got %q", got)
	}
	if got := r.AttributeKind("nope", "app_type"); got != "" {
		t.Fatalf("expected an unknown class to report \"\", got %q", got)
	}
}

// TestChangeEmission (REG-044): significant/cosmetic, "" for an unknown
// attribute or class.
func TestChangeEmission(t *testing.T) {
	r := Builtin()
	if got := r.ChangeEmission("media-player", "app_version"); got != "cosmetic" {
		t.Fatalf("ChangeEmission(app_version) = %q, want cosmetic", got)
	}
	if got := r.ChangeEmission("media-player", "active_app"); got != "significant" {
		t.Fatalf("ChangeEmission(active_app) = %q, want significant", got)
	}
	if got := r.ChangeEmission("media-player", "no-such-attr"); got != "" {
		t.Fatalf("expected an unknown attribute to report \"\", got %q", got)
	}
	if got := r.ChangeEmission("nope", "app_version"); got != "" {
		t.Fatalf("expected an unknown class to report \"\", got %q", got)
	}
}
