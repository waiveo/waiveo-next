package hello

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrProtocolVersionUnsupported is returned by Negotiate when the app peer
// implements no minor of the relay's declared protocol_version major
// (REL-033) — a major mismatch. The caller MUST refuse the connection with
// a typed error (PROTOCOL_VERSION_UNSUPPORTED, Error taxonomy) in place of
// hello-ack.
var ErrProtocolVersionUnsupported = errors.New("hello: protocol_version unsupported (PROTOCOL_VERSION_UNSUPPORTED)")

// Negotiate computes hello-ack's negotiated_version (REL-033/034): given the
// relay's declared protocol_version (hello.body.protocol_version, a
// major.minor string) and the app peer's own implementedMinors — the
// major.minor strings the app peer itself implements — it returns the
// highest minor within the relay's declared major that the app peer
// implements and that is <= the relay's declared minor.
//
// If the app peer implements no minor of the relay's declared major at all,
// this is a major mismatch (REL-033): Negotiate returns
// ErrProtocolVersionUnsupported, and the caller MUST refuse the connection
// (PROTOCOL_VERSION_UNSUPPORTED) rather than send hello-ack.
//
// An entry in implementedMinors that does not parse as major.minor is
// ignored rather than failing the whole negotiation — relay_version itself
// is validated strictly, since it is this call's own subject, but
// implementedMinors is the caller's own declared capability set and a
// malformed entry there is the caller's bug, not reason to refuse an
// otherwise-satisfiable relay.
//
// REL-034 (an app peer implementing minor M MUST also implement M-1 of the
// same major) is a constraint on how implementedMinors is built, not
// something Negotiate itself can enforce from the outside; see
// AppPeerImplementedMinors for a constructor that upholds it structurally.
func Negotiate(relayVersion string, implementedMinors []string) (string, error) {
	relayMajor, relayMinor, err := parseVersion(relayVersion)
	if err != nil {
		return "", fmt.Errorf("hello: Negotiate: relay protocol_version: %w", err)
	}

	bestMinor := -1
	for _, v := range implementedMinors {
		major, minor, err := parseVersion(v)
		if err != nil {
			continue
		}
		if major != relayMajor {
			continue
		}
		if minor <= relayMinor && minor > bestMinor {
			bestMinor = minor
		}
	}

	if bestMinor < 0 {
		return "", ErrProtocolVersionUnsupported
	}
	return fmt.Sprintf("%d.%d", relayMajor, bestMinor), nil
}

// AppPeerImplementedMinors returns the major.minor protocol_version strings
// an app peer currently at minor currentMinor implements, for exactly
// Negotiate's REL-033 matching — structurally upholding REL-034 (an app
// peer implementing minor M MUST also implement M-1 of the same major) so
// a relay declaring one minor behind the app peer's current always finds a
// satisfying negotiated_version under REL-033, never a major-mismatch
// refusal solely for lagging by one minor. currentMinor of 0 implements
// only minor 0 (there is no earlier minor of the same major to also
// implement).
func AppPeerImplementedMinors(major, currentMinor int) []string {
	minors := []string{fmt.Sprintf("%d.%d", major, currentMinor)}
	if currentMinor > 0 {
		minors = append(minors, fmt.Sprintf("%d.%d", major, currentMinor-1))
	}
	return minors
}

// IntersectFeatures computes hello-ack's features (REL-035): the subset of
// the relay's declared features that the app peer also recognizes,
// preserving the relay's own declared order. A relay-declared flag the app
// peer does not recognize is simply excluded from the result — never a
// reason to refuse `hello`.
func IntersectFeatures(relayFeatures, appPeerRecognized []string) []string {
	recognized := make(map[string]bool, len(appPeerRecognized))
	for _, f := range appPeerRecognized {
		recognized[f] = true
	}
	out := make([]string, 0, len(relayFeatures))
	for _, f := range relayFeatures {
		if recognized[f] {
			out = append(out, f)
		}
	}
	return out
}

// parseVersion splits a major.minor string into its integer components,
// failing on anything else (a non-numeric component, a missing/extra
// dot-separated part).
func parseVersion(v string) (major, minor int, err error) {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%q is not a major.minor string", v)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("%q major component: %w", v, err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("%q minor component: %w", v, err)
	}
	return major, minor, nil
}
