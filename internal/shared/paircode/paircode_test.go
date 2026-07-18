package paircode

import (
	"bytes"
	"strings"
	"testing"
)

// TestRoundTrip is Step 1's core assertion: Decode(Encode(...)) returns
// exactly the inputs Encode was given, for a realistic set of values (a
// loopback dial address, a grant_selector shaped like
// internal/feeder/grant's own "grant-<hex>" convention, and a 16-byte
// PLY-052 commitment).
func TestRoundTrip(t *testing.T) {
	wantHost := "127.0.0.1"
	wantPort := 7421
	wantSelector := "grant-7f3c9a1b2d4e5f60112233445566"
	wantCommitment := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}

	code := Encode(wantHost, wantPort, wantSelector, wantCommitment)

	gotHost, gotPort, gotSelector, gotCommitment, err := Decode(code)
	if err != nil {
		t.Fatalf("Decode(%q) error: %v", code, err)
	}

	if gotHost != wantHost {
		t.Errorf("host = %q, want %q", gotHost, wantHost)
	}
	if gotPort != wantPort {
		t.Errorf("port = %d, want %d", gotPort, wantPort)
	}
	if gotSelector != wantSelector {
		t.Errorf("grant_selector = %q, want %q", gotSelector, wantSelector)
	}
	if !bytes.Equal(gotCommitment, wantCommitment) {
		t.Errorf("commitment = %x, want %x", gotCommitment, wantCommitment)
	}
}

// TestRoundTripVariousLengths confirms the codec isn't accidentally tuned
// to one fixed set of lengths — a short host/selector and a longer one both
// round-trip cleanly.
func TestRoundTripVariousLengths(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		port       int
		selector   string
		commitment []byte
	}{
		{"short", "a", 1, "s", []byte{0xff}},
		{"loopback-min-port", "127.0.0.1", 0, "grant-1", make([]byte, 16)},
		{"max-port", "relay.example.internal", 65535, "grant-abcdef0123456789", make([]byte, 16)},
		{"long-host-and-selector", strings.Repeat("h", 200), 7421, strings.Repeat("s", 200), make([]byte, 16)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := Encode(tc.host, tc.port, tc.selector, tc.commitment)
			gotHost, gotPort, gotSelector, gotCommitment, err := Decode(code)
			if err != nil {
				t.Fatalf("Decode(Encode(...)) error: %v", err)
			}
			if gotHost != tc.host || gotPort != tc.port || gotSelector != tc.selector || !bytes.Equal(gotCommitment, tc.commitment) {
				t.Fatalf("round trip mismatch: got (%q, %d, %q, %x), want (%q, %d, %q, %x)",
					gotHost, gotPort, gotSelector, gotCommitment, tc.host, tc.port, tc.selector, tc.commitment)
			}
		})
	}
}

// TestEncodeIsAlphanumericHyphenGrouped confirms PLY-024's presentation
// requirement: the code is an alphanumeric alphabet, presented in
// hyphen-separated groups a decoder strips before decoding.
func TestEncodeIsAlphanumericHyphenGrouped(t *testing.T) {
	code := Encode("127.0.0.1", 7421, "grant-deadbeef", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})

	if !strings.Contains(code, "-") {
		t.Fatalf("Encode() = %q, want hyphen-separated groups", code)
	}

	for _, group := range strings.Split(code, "-") {
		if group == "" {
			t.Fatalf("Encode() = %q, contains an empty group", code)
		}
		for _, r := range group {
			isAlnum := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !isAlnum {
				t.Fatalf("Encode() = %q, group %q contains non-alphanumeric rune %q", code, group, r)
			}
		}
	}
}

// TestEncodeDeterministic confirms encoding the same inputs twice produces
// byte-identical codes — a player decoding a code typed by an operator must
// get the same answer every time, with no hidden randomness.
func TestEncodeDeterministic(t *testing.T) {
	a := Encode("127.0.0.1", 7421, "grant-deadbeef", []byte{1, 2, 3, 4})
	b := Encode("127.0.0.1", 7421, "grant-deadbeef", []byte{1, 2, 3, 4})
	if a != b {
		t.Fatalf("Encode() not deterministic: %q != %q", a, b)
	}
}

// TestDecodeAcceptsLowercase confirms a human-retyped code (lowercased by
// an operator, or a remote's own case-folding) still decodes — PLY-024's
// "an operator can key it on a bounded-input remote control" is friendlier
// if case isn't load-bearing.
func TestDecodeAcceptsLowercase(t *testing.T) {
	code := Encode("127.0.0.1", 7421, "grant-deadbeef", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})

	_, _, _, _, err := Decode(strings.ToLower(code))
	if err != nil {
		t.Fatalf("Decode(lowercased code) error: %v", err)
	}
}

// TestDecodeRejectsGarbage confirms Decode fails closed — an error, never a
// panic — on input that isn't a validly encoded pairing code.
func TestDecodeRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"not-a-valid-code-at-all!!!",
		"----",
		Encode("127.0.0.1", 7421, "s", []byte{1, 2, 3, 4}) + "-XXXXX", // trailing garbage
	}

	for _, c := range cases {
		if _, _, _, _, err := Decode(c); err == nil {
			t.Errorf("Decode(%q) error = nil, want non-nil", c)
		}
	}
}
