package ctxproto

import (
	"fmt"
	"io"
	"sort"

	"github.com/maaxton/waiveo-next/internal/manifest"
)

// The ctx/1 hello/negotiate handshake (CTX-020–024): the first exchange on every
// connection, and the only one whose verbs may cross the wire before it finishes.

// Handshake verbs. These two are the ONLY verbs admissible before the handshake
// completes (CTX-024).
const (
	VerbHello    = "control.hello"
	VerbHelloAck = "control.hello-ack"
)

// Error codes this layer raises, spelled as the ctx/1 error taxonomy publishes
// them.
const (
	CodeIncompatibleRange = "INCOMPATIBLE_RANGE"
	CodeProtocolViolation = "PROTOCOL_VIOLATION"
)

// Hello is the pack's opening frame body (CTX-020).
type Hello struct {
	ManifestID      string
	ManifestVersion string
	CtxRange        string
	FeatureFlags    []string
}

// HelloAck is the host's response body (CTX-021).
type HelloAck struct {
	NegotiatedVersion string
	FeatureFlags      []string
	Deprecated        map[string]DeprecatedVerb
}

// DeprecatedVerb is one entry of the ack's `deprecated` map (CTX-021).
type DeprecatedVerb struct {
	DeprecatedIn string `msgpack:"deprecated_in"`
	RemovedIn    string `msgpack:"removed_in"`
	Message      string `msgpack:"message"`
}

// HostVersion is one ctx/1 major.minor a host implements.
type HostVersion struct {
	Major int
	Minor int
}

func (v HostVersion) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Negotiate picks the version a host answers a pack's hello with (CTX-021–023).
//
// It returns the HIGHEST implemented version satisfying the pack's range, which
// is what makes CTX-023's minor rule work: a range excluding the host's newest
// minor but admitting an older one negotiates DOWN rather than refusing, so a
// pack pinned to an older minor keeps running across a host upgrade. Only a
// range that admits nothing the host implements is INCOMPATIBLE_RANGE.
//
// The granted feature flags are the INTERSECTION of what the pack asked for and
// what the host supports — never the pack's list echoed back. Echoing would tell
// a pack a flag was granted because it asked, which is how a pack ends up
// depending on a capability the host does not have.
func Negotiate(ctxRange string, implemented []HostVersion, requested []string, supported map[string]bool) (HostVersion, []string, error) {
	vr, err := manifest.ParseVersionRange(ctxRange)
	if err != nil {
		// A malformed range is not a negotiation failure — nothing was compared.
		// It is still refused with INCOMPATIBLE_RANGE because that is the code
		// CTX-022 gives the pack for "we cannot agree a version", and inventing a
		// second code for it would split one outcome across two branches a pack
		// must handle identically.
		return HostVersion{}, nil, &FrameError{
			Code:    CodeIncompatibleRange,
			Message: fmt.Sprintf("ctx_range %q is not a valid version range (manifest/1 MAN-013): %v", ctxRange, err),
		}
	}

	best := HostVersion{Major: -1}
	for _, v := range implemented {
		if !vr.Allows(v.Major, v.Minor) {
			continue
		}
		if v.Major > best.Major || (v.Major == best.Major && v.Minor > best.Minor) {
			best = v
		}
	}
	if best.Major < 0 {
		return HostVersion{}, nil, &FrameError{
			Code:    CodeIncompatibleRange,
			Message: fmt.Sprintf("no host-implemented ctx/1 version satisfies %q (CTX-022)", ctxRange),
		}
	}

	granted := make([]string, 0, len(requested))
	for _, f := range requested {
		if supported[f] {
			granted = append(granted, f)
		}
	}
	// Sorted so an ack is deterministic: two hosts granting the same set must
	// produce the same frame, or a conformance suite comparing acks reports a
	// difference that is only map iteration order.
	sort.Strings(granted)
	return best, granted, nil
}

// ReadHello reads the pack's opening frame and returns its Hello body.
//
// CTX-020 requires the FIRST frame on a connection to be control.hello, and
// CTX-024 makes anything else a protocol violation. Enforced here rather than
// left to a caller's switch, because "the first frame" is a property of the
// connection that only the code owning the connection's start can check.
func ReadHello(r io.Reader) (Hello, error) {
	m, err := ReadMessage(r)
	if err != nil {
		return Hello{}, err
	}
	if m.Verb != VerbHello {
		return Hello{}, &FrameError{
			Code: CodeProtocolViolation,
			Message: fmt.Sprintf("first frame was %q; a connection MUST open with %s (CTX-020/024)",
				m.Verb, VerbHello),
		}
	}
	h := Hello{
		ManifestID:      stringField(m.Body, "manifest_id"),
		ManifestVersion: stringField(m.Body, "manifest_version"),
		CtxRange:        stringField(m.Body, "ctx_range"),
		FeatureFlags:    stringSliceField(m.Body, "feature_flags"),
	}
	// A hello with no range is not negotiable: CTX-021 answers with the highest
	// version satisfying `ctx_range`, and there is no such thing when the field
	// is absent. Refused as malformed rather than defaulted, because defaulting
	// would silently hand the pack the host's newest version — the one outcome a
	// compatibility range exists to prevent.
	if h.CtxRange == "" {
		return Hello{}, frameErr(CodeMalformed, "control.hello carries no ctx_range (CTX-020)")
	}
	if h.ManifestID == "" {
		return Hello{}, frameErr(CodeMalformed, "control.hello carries no manifest_id (CTX-020)")
	}
	return h, nil
}

// stringField reads a string out of a decoded body, yielding "" for a missing
// key OR a key of the wrong type. Wrong-type is deliberately not distinguished:
// every caller here treats an unusable value the same way, and the required
// fields are checked by name afterwards so nothing is silently defaulted.
func stringField(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}

// stringSliceField reads a []string out of a decoded body. msgpack decodes an
// array into []any, so each element is type-checked individually; a non-string
// element is DROPPED rather than failing the frame, because feature flags are
// requests the host is free to not grant and one unreadable entry should not
// cost a pack its whole connection.
func stringSliceField(body map[string]any, key string) []string {
	raw, ok := body[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Body renders a HelloAck as a frame body (CTX-021).
func (a HelloAck) Body() map[string]any {
	flags := make([]any, 0, len(a.FeatureFlags))
	for _, f := range a.FeatureFlags {
		flags = append(flags, f)
	}
	dep := make(map[string]any, len(a.Deprecated))
	for verb, d := range a.Deprecated {
		dep[verb] = map[string]any{
			"deprecated_in": d.DeprecatedIn,
			"removed_in":    d.RemovedIn,
			"message":       d.Message,
		}
	}
	return map[string]any{
		"negotiated_version": a.NegotiatedVersion,
		"feature_flags":      flags,
		"deprecated":         dep,
	}
}
