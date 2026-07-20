package manifest

import (
	"net"
	"regexp"
	"strconv"
	"strings"
)

// egressLabelRe is one DNS label: an alphanumeric-bounded run of up to 63
// characters. A hostname is one or more of these joined by dots.
var egressLabelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// validateEgress enforces MAN-030: every egress allowlist entry MUST be a bare
// hostname, a hostname with a wildcard leftmost label (`*.example.com`), or an
// IP-literal. A bare wildcard (`*`) or a CIDR block wider than a single host is
// refused with a typed EGRESS_ENTRY_INVALID error naming the offending entry.
// An empty array declares no egress and is valid. https is the implied scheme
// (MAN-031); no per-entry scheme or port is carried.
func validateEgress(m PackManifest) []Error {
	var errs []Error
	for i, e := range m.Egress {
		if msg := egressEntryProblem(e); msg != "" {
			errs = append(errs, Error{"EGRESS_ENTRY_INVALID", "egress[" + strconv.Itoa(i) + "]", msg})
		}
	}
	return errs
}

// egressEntryProblem returns a human message describing why entry is not a valid
// MAN-030 egress allowlist entry, or "" if it is valid.
func egressEntryProblem(entry string) string {
	switch {
	case entry == "":
		return `"" is an empty egress entry`
	case entry == "*":
		return `"*" is a bare wildcard, which is not an allowed egress entry`
	case strings.Contains(entry, "/"):
		return `"` + entry + `" is a CIDR block, which is wider than a single host and not an allowed egress entry`
	}

	// A wildcard is permitted only as the leftmost label, and only once.
	host := entry
	if strings.HasPrefix(entry, "*.") {
		host = entry[len("*."):]
	}
	if strings.Contains(host, "*") {
		return `"` + entry + `" is not a bare hostname, wildcard-leftmost hostname, or IP-literal`
	}

	// A bare (non-wildcard) entry may be an IP-literal.
	if entry == host && net.ParseIP(entry) != nil {
		return ""
	}
	// Otherwise the (post-wildcard) remainder MUST be a well-formed hostname.
	if isEgressHostname(host) {
		return ""
	}
	return `"` + entry + `" is not a bare hostname, wildcard-leftmost hostname, or IP-literal`
}

// isEgressHostname reports whether h is a dotted sequence of valid DNS labels.
func isEgressHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !egressLabelRe.MatchString(label) {
			return false
		}
	}
	return true
}
