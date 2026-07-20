package deviceclass

import (
	"sort"
	"strconv"
)

// Error is a device-class-registry/1 validation failure: one of the contract's
// Error taxonomy codes, a dotted path to the offending field, and a human
// message. It doubles as a Go error so later tasks may return it where an error
// is expected.
type Error struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e Error) Error() string { return e.Code + " at " + e.Path + ": " + e.Message }

// validType is the closed type vocabulary REG-041 pins for a typed attribute and
// (REG-051) a command param.
var validType = map[string]bool{"string": true, "number": true, "boolean": true, "enum": true}

// Validate checks a RawRegistry against device-class-registry/1's structural
// grammar and returns EVERY violation it finds — never just the first — each
// Code one of the contract's Error taxonomy codes. It interprets the document's
// own shape only (REG-002/010/011/020/022/030/040/041/042/043/044/050/051): it
// does not resolve a device_class or command reference, nor evaluate a
// registration-time class/group collision (those are the resolution and
// registration entry points, not this structural pass). The registry is data,
// never code (REG-001).
//
// Class keys and semantic-group names are visited in sorted order so the
// returned slice is deterministic regardless of Go map iteration order; the
// ordered slices (states, attributes, commands, params) are visited in their
// declared order.
func Validate(reg RawRegistry) []Error {
	var errs []Error
	ids := make([]string, 0, len(reg.DeviceClasses))
	for id := range reg.DeviceClasses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		errs = append(errs, validateClass(id, reg.DeviceClasses[id])...)
	}
	return errs
}

func validateClass(id string, e ClassEntry) []Error {
	base := "device_classes." + id
	var errs []Error

	// REG-010: the class's key MUST be a Class identifier.
	if !IsClassIdent(id) {
		errs = append(errs, Error{"CLASS_IDENTIFIER_INVALID", base, "class key is not a valid Class identifier ^[a-z][a-z0-9-]*$ (REG-010)"})
	}

	// REG-011: origin MUST be exactly one of the two literals.
	if e.Origin != "built-in" && e.Origin != "extension-registered" {
		errs = append(errs, Error{"ORIGIN_INVALID", base + ".origin", "origin MUST be exactly built-in or extension-registered (REG-011)"})
	}

	// REG-020: states MUST be a non-empty array of Snake identifiers.
	if len(e.States) == 0 {
		errs = append(errs, Error{"STATE_LIST_EMPTY", base + ".states", "a ClassEntry MUST declare a non-empty states array (REG-020)"})
	}
	stateSet := make(map[string]bool, len(e.States))
	for i, s := range e.States {
		if !IsSnakeIdent(s) {
			errs = append(errs, Error{"SNAKE_IDENTIFIER_INVALID", base + ".states[" + strconv.Itoa(i) + "]", "a state name MUST be a Snake identifier ^[a-z][a-z0-9_]*$ (REG-020)"})
		}
		stateSet[s] = true
	}

	// REG-022: unknown_state_fallback MUST be a member of the class's own states.
	if !stateSet[e.UnknownStateFallback] {
		errs = append(errs, Error{"UNKNOWN_STATE_FALLBACK_INVALID", base + ".unknown_state_fallback", "unknown_state_fallback MUST be a member of the class's own states (REG-022)"})
	}

	// REG-030: semantic groups — Snake-identifier name, members in the vocabulary.
	names := make([]string, 0, len(e.SemanticGroups))
	for name := range e.SemanticGroups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		gpath := base + ".semantic_groups." + name
		if !IsSnakeIdent(name) {
			errs = append(errs, Error{"SNAKE_IDENTIFIER_INVALID", gpath, "a semantic-group name MUST be a Snake identifier (REG-030)"})
		}
		for i, m := range e.SemanticGroups[name] {
			if !stateSet[m] {
				errs = append(errs, Error{"GROUP_MEMBER_NOT_IN_VOCABULARY", gpath + "[" + strconv.Itoa(i) + "]", "a semantic-group member MUST be present in the class's own states (REG-030)"})
			}
		}
	}

	errs = append(errs, validateAttributes(base, e.Attributes)...)
	errs = append(errs, validateCommands(base, e.Commands)...)
	return errs
}

func validateAttributes(base string, attrs []Attribute) []Error {
	var errs []Error
	seen := make(map[string]bool, len(attrs))
	for i, a := range attrs {
		ap := base + ".attributes[" + strconv.Itoa(i) + "]"

		// REG-040: attribute name is a Snake identifier, unique within the class.
		if !IsSnakeIdent(a.Name) {
			errs = append(errs, Error{"SNAKE_IDENTIFIER_INVALID", ap + ".name", "an attribute name MUST be a Snake identifier (REG-040)"})
		}
		if seen[a.Name] {
			errs = append(errs, Error{"ATTRIBUTE_NAME_COLLISION", ap + ".name", "two attributes in the same class share a name (REG-040)"})
		}
		seen[a.Name] = true

		// REG-041: closed type vocabulary + the enum/values coupling.
		errs = append(errs, validateTypeShape(a.Type, a.Values, ap, "ATTRIBUTE_TYPE_INVALID")...)

		// REG-042: unit MAY be declared only when type is number.
		if a.Unit != nil && a.Type != "number" {
			errs = append(errs, Error{"ATTRIBUTE_UNIT_NOT_APPLICABLE", ap + ".unit", "unit MAY be declared only when type is number (REG-042)"})
		}

		// REG-044: change_emission is exactly significant or cosmetic.
		if a.ChangeEmission != "significant" && a.ChangeEmission != "cosmetic" {
			errs = append(errs, Error{"CHANGE_EMISSION_INVALID", ap + ".change_emission", "change_emission MUST be exactly significant or cosmetic (REG-044)"})
		}
	}
	return errs
}

func validateCommands(base string, cmds []Command) []Error {
	var errs []Error
	seen := make(map[string]bool, len(cmds))
	for i, c := range cmds {
		cp := base + ".commands[" + strconv.Itoa(i) + "]"

		// REG-050: command name is a Snake identifier, unique within the class.
		if !IsSnakeIdent(c.Name) {
			errs = append(errs, Error{"SNAKE_IDENTIFIER_INVALID", cp + ".name", "a command name MUST be a Snake identifier (REG-050)"})
		}
		if seen[c.Name] {
			errs = append(errs, Error{"COMMAND_NAME_COLLISION", cp + ".name", "two commands in the same class share a name (REG-050)"})
		}
		seen[c.Name] = true

		// REG-051: each param is a Snake-named, closed-vocabulary-typed declaration.
		for j, p := range c.Params {
			pp := cp + ".params[" + strconv.Itoa(j) + "]"
			if !IsSnakeIdent(p.Name) {
				errs = append(errs, Error{"SNAKE_IDENTIFIER_INVALID", pp + ".name", "a command-param name MUST be a Snake identifier (REG-051)"})
			}
			errs = append(errs, validateTypeShape(p.Type, p.Values, pp, "COMMAND_PARAM_TYPE_INVALID")...)
		}
	}
	return errs
}

// validateTypeShape enforces REG-041's closed type vocabulary and its enum/values
// coupling — an enum MUST carry a non-empty array of unique values; any other
// type MUST omit values — emitting the caller-supplied code (ATTRIBUTE_TYPE_INVALID
// for a typed attribute, COMMAND_PARAM_TYPE_INVALID for a command param, whose
// param types share this exact vocabulary per REG-051).
func validateTypeShape(typ string, values []string, path, code string) []Error {
	var errs []Error
	if !validType[typ] {
		errs = append(errs, Error{code, path + ".type", "type MUST be one of string, number, boolean, enum (REG-041)"})
	}
	switch typ {
	case "enum":
		if len(values) == 0 {
			errs = append(errs, Error{code, path + ".values", "type enum MUST carry a non-empty values array (REG-041)"})
			break
		}
		vseen := make(map[string]bool, len(values))
		for _, v := range values {
			if vseen[v] {
				errs = append(errs, Error{code, path + ".values", "enum values MUST be unique (REG-041)"})
				break
			}
			vseen[v] = true
		}
	default:
		if len(values) > 0 {
			errs = append(errs, Error{code, path + ".values", "values MUST be absent when type is not enum (REG-041)"})
		}
	}
	return errs
}
