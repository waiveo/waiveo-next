package eval

import "github.com/maaxton/waiveo-next/internal/rules/registry"

// MatchState is rules/1's one shared state-matching algorithm (RUL-320),
// reused identically by every call site that compares a scalar or array
// expected value against an entity's canonical state string — a state
// trigger's from/to (RUL-021), a state condition's state (RUL-110), and any
// future call site this contract adds. It MUST NOT be reimplemented per call
// site (RUL-320).
//
// expected is either a scalar string or an array of strings (RUL-021): a
// []string or a []any of strings, the shape encoding/json produces when an
// array-typed JSON field is decoded into `any`. Per RUL-321:
//
//   - A scalar expected value first attempts an exact string match against
//     actual; failing that, it attempts membership in actual's device
//     class's semantic groups (reg.SemanticGroups(deviceClass, actual)),
//     built-in groups first and then any extension-registered group, in
//     that order — reg.SemanticGroups already returns groups in that order,
//     so this function need only check membership in its result.
//   - An array expected value matches only via exact literal membership;
//     semantic-group expansion never applies to an array, even for a member
//     state that would otherwise group-match the same string as a scalar.
//
// Any other expected shape (not a string, []string, or []any of strings)
// evaluates false rather than erroring or coercing, consistent with this
// contract's fail-closed posture (RUL-262/271/284).
func MatchState(reg registry.Registry, deviceClass, actual string, expected any) bool {
	switch v := expected.(type) {
	case string:
		return matchScalar(reg, deviceClass, actual, v)
	case []string:
		return matchArrayExact(actual, v)
	case []any:
		return matchArrayExactAny(actual, v)
	default:
		return false
	}
}

// matchScalar implements RUL-321's scalar branch: exact string match first,
// then semantic-group membership.
func matchScalar(reg registry.Registry, deviceClass, actual, expected string) bool {
	if actual == expected {
		return true
	}
	for _, group := range reg.SemanticGroups(deviceClass, actual) {
		if group == expected {
			return true
		}
	}
	return false
}

// matchArrayExact implements RUL-321's array branch: exact literal
// membership only, never group expansion.
func matchArrayExact(actual string, expected []string) bool {
	for _, s := range expected {
		if s == actual {
			return true
		}
	}
	return false
}

// matchArrayExactAny is matchArrayExact for the []any shape encoding/json
// produces for an array field decoded into `any`; a non-string element never
// matches (no coercion, RUL-260) and is simply skipped.
func matchArrayExactAny(actual string, expected []any) bool {
	for _, e := range expected {
		if s, ok := e.(string); ok && s == actual {
			return true
		}
	}
	return false
}
