package deviceclass

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// corpusFile is the frozen oracle for the built-in media-player class's
// complete, valid registry entry.
var corpusFile = filepath.Join("..", "..", "conformance", "corpora", "device-class-registry", "roku-media-player.json")

// loadCorpusRegistry reads the roku-media-player corpus case and returns its
// input registry, freshly unmarshaled on every call so a caller may mutate its
// own copy without disturbing another test's.
func loadCorpusRegistry(t *testing.T) RawRegistry {
	t.Helper()
	b, err := os.ReadFile(corpusFile)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c struct {
		Input RawRegistry `json:"input"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal corpus: %v", err)
	}
	if _, ok := c.Input.DeviceClasses["media-player"]; !ok {
		t.Fatal("corpus input is missing the media-player class")
	}
	return c.Input
}

// hasCode reports whether errs carries at least one Error with the given code.
func hasCode(errs []Error, code string) bool {
	for _, e := range errs {
		if e.Code == code {
			return true
		}
	}
	return false
}

// TestValidateCorpusMediaPlayerClean is the happy path: the frozen corpus entry
// validates with zero errors (REG-002/010/011/020/022/030/040/041/043/044/050/051).
func TestValidateCorpusMediaPlayerClean(t *testing.T) {
	reg := loadCorpusRegistry(t)
	if errs := Validate(reg); len(errs) != 0 {
		t.Fatalf("expected zero errors on the corpus media-player entry, got %d: %+v", len(errs), errs)
	}
}

// TestValidateClassIdentifierInvalid: a class key with an underscore is not a
// Class identifier (REG-010).
func TestValidateClassIdentifierInvalid(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	delete(reg.DeviceClasses, "media-player")
	reg.DeviceClasses["media_player"] = e // underscore is not a Class identifier
	errs := Validate(reg)
	if !hasCode(errs, "CLASS_IDENTIFIER_INVALID") {
		t.Fatalf("expected CLASS_IDENTIFIER_INVALID, got %+v", errs)
	}
}

// TestValidateOriginInvalid: origin outside {built-in, extension-registered}
// (REG-011).
func TestValidateOriginInvalid(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.Origin = "third-party"
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "ORIGIN_INVALID") {
		t.Fatalf("expected ORIGIN_INVALID, got %+v", errs)
	}
}

// TestValidateStateListEmpty: zero states (REG-020).
func TestValidateStateListEmpty(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.States = nil
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "STATE_LIST_EMPTY") {
		t.Fatalf("expected STATE_LIST_EMPTY, got %+v", errs)
	}
}

// TestValidateUnknownStateFallbackInvalid: the fallback is not a member of the
// class's own states (REG-022).
func TestValidateUnknownStateFallbackInvalid(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.UnknownStateFallback = "nonesuch" // a valid snake ident, but not in states
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "UNKNOWN_STATE_FALLBACK_INVALID") {
		t.Fatalf("expected UNKNOWN_STATE_FALLBACK_INVALID, got %+v", errs)
	}
}

// TestValidateGroupMemberNotInVocabulary: a semantic-group member outside the
// class's states (REG-030).
func TestValidateGroupMemberNotInVocabulary(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.SemanticGroups["on"] = []string{"on", "flying"} // "flying" is not a state
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "GROUP_MEMBER_NOT_IN_VOCABULARY") {
		t.Fatalf("expected GROUP_MEMBER_NOT_IN_VOCABULARY, got %+v", errs)
	}
}

// TestValidateAttributeNameCollision: two attributes share a name (REG-040).
func TestValidateAttributeNameCollision(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.Attributes = append(e.Attributes, Attribute{Name: "active_app", Type: "string", Nullable: true, ChangeEmission: "significant"})
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "ATTRIBUTE_NAME_COLLISION") {
		t.Fatalf("expected ATTRIBUTE_NAME_COLLISION, got %+v", errs)
	}
}

// TestValidateEnumWithoutValues: type enum with no values (REG-041).
func TestValidateEnumWithoutValues(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.Attributes[0].Type = "enum" // active_app: now enum, but Values is nil
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "ATTRIBUTE_TYPE_INVALID") {
		t.Fatalf("expected ATTRIBUTE_TYPE_INVALID for enum-without-values, got %+v", errs)
	}
}

// TestValidateValuesWithNonEnum: values present with a non-enum type (REG-041).
func TestValidateValuesWithNonEnum(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.Attributes[0].Values = []string{"a", "b"} // active_app: string with values
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "ATTRIBUTE_TYPE_INVALID") {
		t.Fatalf("expected ATTRIBUTE_TYPE_INVALID for values-on-string, got %+v", errs)
	}
}

// TestValidateUnitNotApplicable: unit on a non-number attribute (REG-042).
func TestValidateUnitNotApplicable(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	u := "px"
	e.Attributes[0].Unit = &u // active_app is a string, not a number
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "ATTRIBUTE_UNIT_NOT_APPLICABLE") {
		t.Fatalf("expected ATTRIBUTE_UNIT_NOT_APPLICABLE, got %+v", errs)
	}
}

// TestValidateChangeEmissionInvalid: change_emission outside the closed pair
// (REG-044).
func TestValidateChangeEmissionInvalid(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.Attributes[0].ChangeEmission = "loud"
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "CHANGE_EMISSION_INVALID") {
		t.Fatalf("expected CHANGE_EMISSION_INVALID, got %+v", errs)
	}
}

// TestValidateCommandParamTypeInvalid: a command param type outside the closed
// vocabulary (REG-051).
func TestValidateCommandParamTypeInvalid(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.Commands[0].Params[0].Type = "blob" // launch/channel: not a valid type
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "COMMAND_PARAM_TYPE_INVALID") {
		t.Fatalf("expected COMMAND_PARAM_TYPE_INVALID, got %+v", errs)
	}
}

// TestValidateSnakeIdentifierInvalid: a non-snake state/group/attribute/command/
// param name (REG-020/030/040/050/051).
func TestValidateSnakeIdentifierInvalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(e *ClassEntry)
	}{
		{"state", func(e *ClassEntry) { e.States[0] = "On-Air" }},
		{"group", func(e *ClassEntry) {
			e.SemanticGroups["On-Air"] = e.SemanticGroups["on"]
			delete(e.SemanticGroups, "on")
		}},
		{"attribute", func(e *ClassEntry) { e.Attributes[0].Name = "active-app" }},
		{"command", func(e *ClassEntry) { e.Commands[0].Name = "Launch-App" }},
		{"param", func(e *ClassEntry) { e.Commands[0].Params[0].Name = "Channel-Name" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := loadCorpusRegistry(t)
			e := reg.DeviceClasses["media-player"]
			tc.mutate(&e)
			reg.DeviceClasses["media-player"] = e
			errs := Validate(reg)
			if !hasCode(errs, "SNAKE_IDENTIFIER_INVALID") {
				t.Fatalf("expected SNAKE_IDENTIFIER_INVALID for a non-snake %s name, got %+v", tc.name, errs)
			}
		})
	}
}

// TestValidateReturnsAllViolations: Validate reports EVERY violation, not just
// the first — two independent defects both surface.
func TestValidateReturnsAllViolations(t *testing.T) {
	reg := loadCorpusRegistry(t)
	e := reg.DeviceClasses["media-player"]
	e.Attributes[0].ChangeEmission = "loud" // CHANGE_EMISSION_INVALID
	e.Attributes[1].Type = "bogus"          // ATTRIBUTE_TYPE_INVALID
	reg.DeviceClasses["media-player"] = e
	errs := Validate(reg)
	if !hasCode(errs, "CHANGE_EMISSION_INVALID") || !hasCode(errs, "ATTRIBUTE_TYPE_INVALID") {
		t.Fatalf("expected both CHANGE_EMISSION_INVALID and ATTRIBUTE_TYPE_INVALID, got %+v", errs)
	}
	if len(errs) < 2 {
		t.Fatalf("expected at least 2 violations, got %d: %+v", len(errs), errs)
	}
}

// TestIdentGrammars pins the two identifier grammars directly.
func TestIdentGrammars(t *testing.T) {
	if !IsSnakeIdent("active_app") || IsSnakeIdent("active-app") || IsSnakeIdent("Active") || IsSnakeIdent("1x") || IsSnakeIdent("") {
		t.Fatal("IsSnakeIdent grammar wrong")
	}
	if !IsClassIdent("media-player") || IsClassIdent("media_player") || IsClassIdent("Media") || IsClassIdent("1x") || IsClassIdent("") {
		t.Fatal("IsClassIdent grammar wrong")
	}
}
