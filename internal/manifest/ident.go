package manifest

import (
	"regexp"
	"strings"
)

// idSegmentRe is the grammar each of a pack id's two segments MUST match
// (MAN-001): a lowercase ASCII letter followed by 1-38 lowercase letters,
// digits, or hyphens — so every segment is 2-39 characters. It is the same
// Class-identifier family device-class-registry/1 uses, extended with a length
// bound.
var idSegmentRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}$`)

// versionRe is the MAN-002 version-string grammar: three dot-separated,
// digits-only components (MAJOR.MINOR.PATCH).
var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// IsPublisherNameID reports whether id is a well-formed pack id (MAN-001):
// exactly <publisher>/<name>, each segment matching idSegmentRe.
func IsPublisherNameID(id string) bool {
	pub, name, ok := strings.Cut(id, "/")
	if !ok {
		return false
	}
	// Cut splits on the first '/', so a second slash lands inside name.
	if strings.Contains(name, "/") {
		return false
	}
	return idSegmentRe.MatchString(pub) && idSegmentRe.MatchString(name)
}

// IsMsgRef reports whether s is a locale-catalog reference (MAN-003): the msg:
// prefix followed by a non-empty key.
func IsMsgRef(s string) bool {
	const prefix = "msg:"
	return strings.HasPrefix(s, prefix) && len(s) > len(prefix)
}

// IsThreeComponentVersion reports whether s is a MAN-002 version string:
// three digits-only dot-separated components.
func IsThreeComponentVersion(s string) bool {
	return versionRe.MatchString(s)
}
