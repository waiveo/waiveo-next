package deviceclass

import (
	"reflect"
	"testing"
)

// newClass is a structurally conformant extension-registered class, the shape a
// registration attempt carries. Built fresh per call so a test that mutates it
// cannot leak into another.
func newClass() ClassEntry {
	return ClassEntry{
		Origin:               "extension-registered",
		States:               []string{"idle", "busy"},
		UnknownStateFallback: "idle",
		SemanticGroups:       map[string][]string{"active": {"busy"}},
	}
}

// TestRegisterClassRefusesACollidingIdentifier is REG-012's core: an identifier
// that already resolves is rejected outright.
//
// Asserted against the BUILT-IN media-player, because REG-012 is explicit that
// the existing entry's origin is irrelevant — "whether the existing entry is
// built-in or itself extension-registered".
func TestRegisterClassRefusesACollidingIdentifier(t *testing.T) {
	r := Builtin()
	next, errs := r.RegisterClass("media-player", newClass())
	if !hasCode(errs, "CLASS_IDENTIFIER_COLLISION") {
		t.Fatalf("registering onto an existing class identifier was not refused with CLASS_IDENTIFIER_COLLISION, got %+v", errs)
	}
	// The no-shadow half. A refused attempt must leave the existing entry exactly
	// as it was — not merged with the candidate's states, not replaced by it.
	got, ok := next.Class("media-player")
	if !ok {
		t.Fatal("a refused registration removed the existing class from the registry")
	}
	want, _ := r.Class("media-player")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a refused registration altered the existing entry.\n got: %+v\nwant: %+v", got, want)
	}
}

// TestRegisterClassRefusesACollisionWithAnExtensionRegisteredEntry is the other
// half of "regardless of origin". A rule that only protected built-ins would
// pass the test above and let two packs fight over one identifier, with
// resolution order silently deciding which device vocabulary a site gets.
func TestRegisterClassRefusesACollisionWithAnExtensionRegisteredEntry(t *testing.T) {
	first, errs := Builtin().RegisterClass("acme-blender", newClass())
	if len(errs) != 0 {
		t.Fatalf("registering a fresh extension class: %+v", errs)
	}
	if _, errs := first.RegisterClass("acme-blender", newClass()); !hasCode(errs, "CLASS_IDENTIFIER_COLLISION") {
		t.Fatalf("a second registration onto an EXTENSION-registered identifier was not refused, got %+v", errs)
	}
}

// TestRegisterClassAcceptsAFreshIdentifier is the control. Without it, a
// registration that refuses everything satisfies every refusal test here.
func TestRegisterClassAcceptsAFreshIdentifier(t *testing.T) {
	before := Builtin()
	after, errs := before.RegisterClass("acme-blender", newClass())
	if len(errs) != 0 {
		t.Fatalf("a fresh, structurally clean identifier was refused: %+v", errs)
	}
	if _, ok := after.Class("acme-blender"); !ok {
		t.Error("an accepted registration did not appear in the returned registry")
	}
	// The receiver is a distinct value, so a caller holding the pre-registration
	// registry still sees the pre-registration content.
	if _, ok := before.Class("acme-blender"); ok {
		t.Error("registration mutated the receiver in place — a caller holding the old registry sees the new class")
	}
}

// TestRegisterClassValidatesTheCandidate: a structurally invalid candidate is
// refused with the grammar's own code, not with a collision code.
//
// New's invariant is that "a Registry is never built, even partially, from a
// document that fails its own structural grammar". A registration path that
// skipped validation would be a second way to build one that does.
func TestRegisterClassValidatesTheCandidate(t *testing.T) {
	bad := newClass()
	bad.Origin = "bogus" // ORIGIN_INVALID (REG-011)
	after, errs := Builtin().RegisterClass("acme-blender", bad)
	if !hasCode(errs, "ORIGIN_INVALID") {
		t.Fatalf("a structurally invalid candidate was not refused by the grammar, got %+v", errs)
	}
	if hasCode(errs, "CLASS_IDENTIFIER_COLLISION") {
		t.Error("an invalid candidate was reported as a collision; it is not one, and the caller would look for the wrong fault")
	}
	if _, ok := after.Class("acme-blender"); ok {
		t.Error("a candidate that failed validation was admitted to the registry anyway")
	}
}

// TestRegisterGroupRefusesACollidingName is REG-031: a group name already in
// use for that class is rejected outright, never merged and never redefined.
//
// media-player's built-in groups come from the contract-pinned content, so this
// exercises the built-in-existing direction the requirement calls out.
func TestRegisterGroupRefusesACollidingName(t *testing.T) {
	r := Builtin()
	entry, _ := r.Class("media-player")
	var existing string
	for name := range entry.SemanticGroups {
		existing = name
		break
	}
	if existing == "" {
		t.Fatal("the built-in media-player class declares no semantic groups, so this test cannot exercise REG-031")
	}
	wantMembers := append([]string(nil), entry.SemanticGroups[existing]...)

	next, errs := r.RegisterGroup("media-player", existing, []string{"playing"})
	if !hasCode(errs, "GROUP_NAME_COLLISION") {
		t.Fatalf("registering an existing group name was not refused with GROUP_NAME_COLLISION, got %+v", errs)
	}
	after, _ := next.Class("media-player")
	if !reflect.DeepEqual(after.SemanticGroups[existing], wantMembers) {
		t.Errorf("a refused group registration changed the existing group's membership.\n got: %v\nwant: %v",
			after.SemanticGroups[existing], wantMembers)
	}
}

// TestRegisterGroupAcceptsADistinctName is the control, and is also the remedy
// REG-031 itself prescribes: an author who needs a related-but-different set of
// states registers it under a new, distinctly named group.
func TestRegisterGroupAcceptsADistinctName(t *testing.T) {
	before := Builtin()
	entry, _ := before.Class("media-player")
	member := entry.States[0]

	after, errs := before.RegisterGroup("media-player", "acme_watchable", []string{member})
	if len(errs) != 0 {
		t.Fatalf("a distinctly named group over a declared state was refused: %+v", errs)
	}
	got, _ := after.Class("media-player")
	if !reflect.DeepEqual(got.SemanticGroups["acme_watchable"], []string{member}) {
		t.Errorf("an accepted group did not appear with its declared members, got %v", got.SemanticGroups["acme_watchable"])
	}
	stale, _ := before.Class("media-player")
	if _, ok := stale.SemanticGroups["acme_watchable"]; ok {
		t.Error("group registration mutated the receiver's class entry in place")
	}
}

// TestRegisterGroupRefusesAnUnknownClass: registering onto a class this registry
// does not carry is a refusal, not an acceptance.
//
// The pre-production version of this decision — computed inside the conformance
// driver — returned "not rejected" here, which reads as success. Nothing was
// registered either way, so reporting acceptance would tell a pack its
// vocabulary was live when no class had ever received it.
func TestRegisterGroupRefusesAnUnknownClass(t *testing.T) {
	_, errs := Builtin().RegisterGroup("no-such-class", "acme_group", []string{"idle"})
	if !hasCode(errs, "DEVICE_CLASS_UNKNOWN") {
		t.Fatalf("registering a group onto an unknown class was not refused with DEVICE_CLASS_UNKNOWN, got %+v", errs)
	}
}

// TestRegisterGroupValidatesMembership: REG-030 requires every member state to
// be one the class declares. Enforced by the same Validate the rest of the
// package uses rather than restated in the registration path, so a group naming
// an undeclared state is refused for the reason the grammar already gives.
func TestRegisterGroupValidatesMembership(t *testing.T) {
	after, errs := Builtin().RegisterGroup("media-player", "acme_impossible", []string{"not_a_declared_state"})
	if !hasCode(errs, "GROUP_MEMBER_NOT_IN_VOCABULARY") {
		t.Fatalf("a group naming an undeclared state was not refused, got %+v", errs)
	}
	got, _ := after.Class("media-player")
	if _, ok := got.SemanticGroups["acme_impossible"]; ok {
		t.Error("a group that failed validation was admitted to the class anyway")
	}
}
