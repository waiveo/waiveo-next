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
// digits-only components (MAJOR.MINOR.PATCH), no leading zero in a component
// except `0` itself. The leading-zero refusal is MAN-002's injectivity rule —
// exactly one spelling per version — so the string that identifies an artifact
// and the triple that orders it (marketplace/1 MKT-050a) can never disagree;
// "1.0.05" would be a second spelling of "1.0.5".
var versionRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// iconNameRe is the grammar the optional display icon MUST match: a lowercase
// kebab-case lucide glyph name — a letter-led segment of lowercase letters and
// digits, joined by single hyphens (no leading/trailing/double hyphen). It is
// intentionally strict so a name can never carry markup, a path, whitespace, or
// case into the console's icon lookup. Membership in the host-allowed glyph set
// is NOT enforced here: an unrecognized-but-well-formed name degrades to the
// default extension glyph at render, never a broken icon or a refused install.
var iconNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

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
// three digits-only dot-separated components, no leading zeros.
func IsThreeComponentVersion(s string) bool {
	return versionRe.MatchString(s)
}

// IsIconName reports whether s is a well-formed lucide glyph name (the optional
// display-icon grammar): a lowercase kebab-case identifier. Shape only — it does
// not check membership in the host-allowed glyph set (that resolves at render).
func IsIconName(s string) bool {
	return iconNameRe.MatchString(s)
}
