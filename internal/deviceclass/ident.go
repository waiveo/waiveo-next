package deviceclass

import "regexp"

// snakeIdentRe is the Snake identifier grammar (device-class-registry/1
// Definitions): lowercase ASCII letters, digits, and underscores, starting with
// a letter. Every state, semantic-group, attribute, command, and command-param
// name this contract defines MUST match it.
var snakeIdentRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// classIdentRe is the Class identifier grammar (device-class-registry/1
// Definitions): lowercase ASCII letters, digits, and hyphens, starting with a
// letter — the same grammar manifest/1 uses for a pack's own id (MAN-001) and
// the form rules/1's device_class filter carries on the wire (RUL-010).
var classIdentRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// IsSnakeIdent reports whether s is a valid Snake identifier (^[a-z][a-z0-9_]*$).
func IsSnakeIdent(s string) bool { return snakeIdentRe.MatchString(s) }

// IsClassIdent reports whether s is a valid Class identifier (^[a-z][a-z0-9-]*$).
func IsClassIdent(s string) bool { return classIdentRe.MatchString(s) }
