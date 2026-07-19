package hello

import (
	"errors"
	"reflect"
	"testing"
)

// TestNegotiateHighestSatisfyingMinor asserts Negotiate picks the highest
// minor the app peer implements that is <= the relay's declared minor
// (REL-033), across a relay lagging behind, a relay exactly matching, and
// an app peer implementing minors both above and below the relay's.
func TestNegotiateHighestSatisfyingMinor(t *testing.T) {
	cases := []struct {
		name              string
		relayVersion      string
		implementedMinors []string
		want              string
	}{
		{
			name:              "relay one minor behind app peer's current (the REL-030 corpus case)",
			relayVersion:      "1.0",
			implementedMinors: []string{"1.0", "1.1"},
			want:              "1.0",
		},
		{
			name:              "relay exactly at app peer's current minor",
			relayVersion:      "1.1",
			implementedMinors: []string{"1.0", "1.1"},
			want:              "1.1",
		},
		{
			name:              "relay ahead of every minor the app peer implements caps at the highest implemented",
			relayVersion:      "1.5",
			implementedMinors: []string{"1.0", "1.1"},
			want:              "1.1",
		},
		{
			name:              "unordered implementedMinors input",
			relayVersion:      "1.2",
			implementedMinors: []string{"1.3", "1.0", "1.2", "1.1"},
			want:              "1.2",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Negotiate(c.relayVersion, c.implementedMinors)
			if err != nil {
				t.Fatalf("Negotiate(%q, %v): %v", c.relayVersion, c.implementedMinors, err)
			}
			if got != c.want {
				t.Errorf("Negotiate(%q, %v) = %q, want %q", c.relayVersion, c.implementedMinors, got, c.want)
			}
		})
	}
}

// TestNegotiateMajorMismatchUnsupported asserts a relay declaring a major
// the app peer implements no minor of is refused with
// ErrProtocolVersionUnsupported (REL-033) — never a best-effort match
// against an unrelated major.
func TestNegotiateMajorMismatchUnsupported(t *testing.T) {
	_, err := Negotiate("2.0", []string{"1.0", "1.1"})
	if !errors.Is(err, ErrProtocolVersionUnsupported) {
		t.Errorf("Negotiate(major mismatch) = %v, want ErrProtocolVersionUnsupported", err)
	}
}

// TestNegotiateMalformedRelayVersionErrors asserts Negotiate returns an
// error (not ErrProtocolVersionUnsupported, and not a panic) when the
// relay's OWN declared protocol_version fails to parse — a distinct failure
// mode from a valid major that simply isn't implemented.
func TestNegotiateMalformedRelayVersionErrors(t *testing.T) {
	_, err := Negotiate("not-a-version", []string{"1.0", "1.1"})
	if err == nil {
		t.Fatal("Negotiate(malformed relay version) = nil error, want an error")
	}
	if errors.Is(err, ErrProtocolVersionUnsupported) {
		t.Errorf("Negotiate(malformed relay version) = ErrProtocolVersionUnsupported, want a distinct parse error")
	}
}

// TestNegotiateIgnoresMalformedImplementedMinorEntries asserts a malformed
// entry in implementedMinors is skipped rather than failing the whole
// negotiation, so long as some other entry still satisfies the relay's
// declared major/minor.
func TestNegotiateIgnoresMalformedImplementedMinorEntries(t *testing.T) {
	got, err := Negotiate("1.0", []string{"garbage", "1.0"})
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if got != "1.0" {
		t.Errorf("Negotiate = %q, want %q", got, "1.0")
	}
}

// TestAppPeerImplementedMinorsUpholdsNMinus1 asserts AppPeerImplementedMinors
// structurally satisfies REL-034 (an app peer implementing minor M also
// implements M-1 of the same major): a relay declaring exactly M-1 must
// always find a satisfying negotiated_version through Negotiate fed this
// helper's output, never a major-mismatch refusal solely for lagging by one
// minor.
func TestAppPeerImplementedMinorsUpholdsNMinus1(t *testing.T) {
	minors := AppPeerImplementedMinors(1, 3)
	want := []string{"1.3", "1.2"}
	if !reflect.DeepEqual(minors, want) {
		t.Fatalf("AppPeerImplementedMinors(1, 3) = %v, want %v", minors, want)
	}

	got, err := Negotiate("1.2", minors)
	if err != nil {
		t.Fatalf("Negotiate(relay one minor behind): %v", err)
	}
	if got != "1.2" {
		t.Errorf("Negotiate(relay one minor behind) = %q, want %q", got, "1.2")
	}
}

// TestAppPeerImplementedMinorsZeroFloor asserts the app peer's own minor 0
// has no earlier minor to also implement — AppPeerImplementedMinors(major,
// 0) reports only minor 0, not a negative minor.
func TestAppPeerImplementedMinorsZeroFloor(t *testing.T) {
	got := AppPeerImplementedMinors(1, 0)
	want := []string{"1.0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AppPeerImplementedMinors(1, 0) = %v, want %v", got, want)
	}
}

// TestIntersectFeaturesDropsUnrecognizedSilently asserts IntersectFeatures
// returns exactly the relay-declared features the app peer recognizes, in
// the relay's own declared order, silently excluding anything unrecognized
// (REL-035) — never a distinct "rejected" signal for the dropped flag.
func TestIntersectFeaturesDropsUnrecognizedSilently(t *testing.T) {
	relayFeatures := []string{"telemetry.latest_only_v1", "an-unrecognized-future-flag"}
	appPeerRecognized := []string{"telemetry.latest_only_v1"}

	got := IntersectFeatures(relayFeatures, appPeerRecognized)
	want := []string{"telemetry.latest_only_v1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IntersectFeatures = %v, want %v", got, want)
	}
}

// TestIntersectFeaturesEmptyRelayFeaturesYieldsEmptyNotNil asserts an empty
// relay features list yields an empty (non-nil) slice, matching HelloAckBody
// (json-marshals to `[]`, not `null`).
func TestIntersectFeaturesEmptyRelayFeaturesYieldsEmptyNotNil(t *testing.T) {
	got := IntersectFeatures(nil, []string{"telemetry.latest_only_v1"})
	if got == nil {
		t.Fatal("IntersectFeatures(nil relay features) = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("IntersectFeatures(nil relay features) = %v, want empty", got)
	}
}
