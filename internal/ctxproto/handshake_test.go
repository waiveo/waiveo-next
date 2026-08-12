package ctxproto

import (
	"bytes"
	"testing"
)

// hostVersions is the fixture host: two minors of ctx/1, newest last, so a test
// asserting "the highest satisfying version" cannot pass by accidentally taking
// the first or the last entry.
var hostVersions = []HostVersion{{1, 0}, {1, 2}, {1, 1}}

// The host answers with the HIGHEST implemented version the range admits.
func TestNegotiateTakesTheHighestSatisfyingVersion(t *testing.T) {
	got, _, err := Negotiate(">=1.0 <2.0", hostVersions, nil, nil)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if got.String() != "1.2" {
		t.Fatalf("negotiated %s, want 1.2 (the highest implemented minor the range admits)", got)
	}
}

// CTX-023's minor rule: a range that excludes the host's NEWEST minor but admits
// an older one must negotiate DOWN, not refuse. This is what keeps a pack pinned
// to an older minor running across a host upgrade — the difference between a
// compatible ecosystem and one where every host release breaks every pack.
func TestAMinorMismatchNegotiatesDownRatherThanRefusing(t *testing.T) {
	got, _, err := Negotiate(">=1.0 <1.2", hostVersions, nil, nil)
	if err != nil {
		t.Fatalf("a range admitting an older implemented minor must negotiate, not refuse: %v", err)
	}
	if got.String() != "1.1" {
		t.Fatalf("negotiated %s, want 1.1", got)
	}
}

// CTX-022/023's major rule: a range admitting nothing the host implements is
// INCOMPATIBLE_RANGE. Distinguished from the minor case above — collapsing the
// two would either refuse a pack that could have run, or run a pack against a
// major it cannot speak.
func TestAMajorMismatchIsRefusedAsIncompatible(t *testing.T) {
	_, _, err := Negotiate(">=2.0 <3.0", hostVersions, nil, nil)
	assertFrameCode(t, err, CodeIncompatibleRange)
}

// A range string that does not parse is refused with the SAME code a caller
// already handles, rather than a second one meaning the same thing to the pack.
func TestAnUnparseableRangeIsRefusedAsIncompatible(t *testing.T) {
	_, _, err := Negotiate("~>1.0.0", hostVersions, nil, nil)
	assertFrameCode(t, err, CodeIncompatibleRange)
}

// Granted flags are the INTERSECTION of asked-for and supported — never the
// pack's list echoed back. An echo would tell a pack a capability was granted
// because it asked for it.
func TestGrantedFeatureFlagsAreTheIntersectionNotAnEcho(t *testing.T) {
	_, granted, err := Negotiate("1.0", hostVersions,
		[]string{"streaming", "telepathy", "batching"},
		map[string]bool{"streaming": true, "batching": true, "unrequested": true})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if len(granted) != 2 || granted[0] != "batching" || granted[1] != "streaming" {
		t.Fatalf("granted = %v, want exactly [batching streaming] (sorted intersection)", granted)
	}
}

// A pack asking for nothing is granted nothing, and a host supporting nothing
// grants nothing — the two empty directions, since a bug in either would only
// show up as a pack silently believing it had a capability.
func TestNoFlagsAreGrantedWhenEitherSideOffersNone(t *testing.T) {
	if _, granted, _ := Negotiate("1.0", hostVersions, nil, map[string]bool{"streaming": true}); len(granted) != 0 {
		t.Fatalf("granted %v for a pack that asked for nothing", granted)
	}
	if _, granted, _ := Negotiate("1.0", hostVersions, []string{"streaming"}, nil); len(granted) != 0 {
		t.Fatalf("granted %v against a host supporting nothing", granted)
	}
}

// The pack's hello round-trips through a real frame into the host's reader.
func TestAHelloRoundTripsFromPackToHost(t *testing.T) {
	var wire bytes.Buffer
	sendHello(t, &wire, Hello{
		ManifestID: "waiveo/slidecast", ManifestVersion: "2.0.0",
		CtxRange: ">=1.0 <2.0", FeatureFlags: []string{"streaming"},
	})

	got, err := ReadHello(&wire)
	if err != nil {
		t.Fatalf("ReadHello: %v", err)
	}
	if got.ManifestID != "waiveo/slidecast" || got.ManifestVersion != "2.0.0" || got.CtxRange != ">=1.0 <2.0" {
		t.Fatalf("hello = %+v", got)
	}
	if len(got.FeatureFlags) != 1 || got.FeatureFlags[0] != "streaming" {
		t.Fatalf("feature flags = %v, want [streaming]", got.FeatureFlags)
	}
}

// CTX-020/024: the FIRST frame must be control.hello. Any other verb is a
// protocol violation, distinct from a malformed frame — the frame was perfectly
// well-formed, it simply arrived out of order, and the two failures tell an
// operator different things about what is wrong with the pack.
func TestAFirstFrameThatIsNotHelloIsAProtocolViolation(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteMessage(&wire, Message{
		Type: TypeRequest, ID: "01J8Z2Q1A0000000000000000A", TraceID: "01J8Z2Q1B0000000000000000B",
		Verb: "data.read", Body: map[string]any{},
	}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	_, err := ReadHello(&wire)
	assertFrameCode(t, err, CodeProtocolViolation)
}

// A hello with no ctx_range is malformed, NOT silently defaulted. Defaulting
// would hand the pack the host's newest version — precisely the outcome a
// compatibility range exists to prevent.
func TestAHelloWithoutARangeIsRefusedRatherThanDefaulted(t *testing.T) {
	var wire bytes.Buffer
	sendHello(t, &wire, Hello{ManifestID: "waiveo/slidecast", ManifestVersion: "2.0.0"})
	_, err := ReadHello(&wire)
	assertFrameCode(t, err, CodeMalformed)
}

// The ack renders the fields CTX-021 names, including a deprecated map a pack
// reads to know what is going away.
func TestTheAckCarriesTheNegotiatedVersionAndDeprecations(t *testing.T) {
	body := HelloAck{
		NegotiatedVersion: "1.2",
		FeatureFlags:      []string{"streaming"},
		Deprecated: map[string]DeprecatedVerb{
			"data.scan": {DeprecatedIn: "1.1", RemovedIn: "2.0", Message: "use data.read"},
		},
	}.Body()

	if body["negotiated_version"] != "1.2" {
		t.Fatalf("negotiated_version = %v", body["negotiated_version"])
	}
	dep, ok := body["deprecated"].(map[string]any)
	if !ok {
		t.Fatalf("deprecated = %T, want a map", body["deprecated"])
	}
	entry, ok := dep["data.scan"].(map[string]any)
	if !ok {
		t.Fatalf("deprecated[data.scan] = %T", dep["data.scan"])
	}
	if entry["removed_in"] != "2.0" || entry["message"] != "use data.read" {
		t.Fatalf("deprecation entry = %v", entry)
	}
}

// helloBody renders a Hello as a frame body — the PACK side of CTX-020.
//
// It lives in the test rather than beside Hello because every root in this repo
// is a host: nothing in production sends a hello, so a production method for it
// would be a capability with no caller, which the deadcode gate correctly
// refuses. It comes back the day a Go pack client exists.
func helloBody(h Hello) map[string]any {
	flags := make([]any, 0, len(h.FeatureFlags))
	for _, f := range h.FeatureFlags {
		flags = append(flags, f)
	}
	return map[string]any{
		"manifest_id":      h.ManifestID,
		"manifest_version": h.ManifestVersion,
		"ctx_range":        h.CtxRange,
		"feature_flags":    flags,
	}
}

// sendHello writes a pack-side control.hello frame into w.
func sendHello(t *testing.T, w *bytes.Buffer, h Hello) {
	t.Helper()
	if err := WriteMessage(w, Message{
		Type: TypeRequest, ID: "01J8Z2Q1A0000000000000000A", TraceID: "01J8Z2Q1B0000000000000000B",
		Verb: VerbHello, Body: helloBody(h),
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
}
