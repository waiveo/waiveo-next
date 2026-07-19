package deviceplane

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestParseMatchValidForms asserts each of the three manifest/1 MAN-071
// discovery-match forms parses into the expected Match value.
func TestParseMatchValidForms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Match
	}{
		{
			name: "ssdp",
			raw:  `{"ssdp":"urn:roku-com:device:player:1"}`,
			want: Match{SSDP: "urn:roku-com:device:player:1"},
		},
		{
			name: "mdns",
			raw:  `{"mdns":"_googlecast._tcp"}`,
			want: Match{MDNS: "_googlecast._tcp"},
		},
		{
			name: "macOui",
			raw:  `{"macOui":"B827EB"}`,
			want: Match{MacOui: "B827EB"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMatch(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatalf("ParseMatch(%s) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("ParseMatch(%s) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestParseMatchUnknownOrAmbiguousForms asserts an unrecognized form, an
// empty object, and a multi-field object all fail with
// ErrUnknownDeviceMatchForm (UNKNOWN_DEVICE_MATCH_FORM).
func TestParseMatchUnknownOrAmbiguousForms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"unrecognized form", `{"bluetooth":"some-uuid"}`},
		{"empty object", `{}`},
		{"two fields set", `{"ssdp":"a","mdns":"b"}`},
		{"all three fields set", `{"ssdp":"a","mdns":"b","macOui":"AABBCC"}`},
		{"not an object", `"ssdp:foo"`},
		// REL-110/111: an empty-string value is representationally the zero
		// (unset) Match, so it must be rejected rather than collapse two
		// textually-distinct degenerate matches into one Store entry / corrupt
		// the report envelope on marshal.
		{"empty ssdp value", `{"ssdp":""}`},
		{"empty mdns value", `{"mdns":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMatch(json.RawMessage(tt.raw))
			if err == nil {
				t.Fatalf("ParseMatch(%s) = nil error, want ErrUnknownDeviceMatchForm", tt.raw)
			}
			if !errors.Is(err, ErrUnknownDeviceMatchForm) {
				t.Errorf("ParseMatch(%s) error = %v, want it to wrap ErrUnknownDeviceMatchForm", tt.raw, err)
			}
		})
	}
}

// TestParseMatchMacOuiValidation asserts macOui MUST be a 6-hex-digit
// prefix (MAN-071) — non-hex characters or the wrong length both error
// UNKNOWN_DEVICE_MATCH_FORM.
func TestParseMatchMacOuiValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"too short", `{"macOui":"B827E"}`},
		{"too long", `{"macOui":"B827EB00"}`},
		{"non-hex characters", `{"macOui":"ZZYYXX"}`},
		{"empty string", `{"macOui":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMatch(json.RawMessage(tt.raw))
			if err == nil {
				t.Fatalf("ParseMatch(%s) = nil error, want ErrUnknownDeviceMatchForm", tt.raw)
			}
			if !errors.Is(err, ErrUnknownDeviceMatchForm) {
				t.Errorf("ParseMatch(%s) error = %v, want it to wrap ErrUnknownDeviceMatchForm", tt.raw, err)
			}
		})
	}
}

// TestParseMatchMacOuiLowercaseAccepted asserts lowercase hex digits are
// accepted (MAN-071 says "6-hex-digit", not upper-case-only).
func TestParseMatchMacOuiLowercaseAccepted(t *testing.T) {
	got, err := ParseMatch(json.RawMessage(`{"macOui":"b827eb"}`))
	if err != nil {
		t.Fatalf("ParseMatch lowercase macOui: unexpected error: %v", err)
	}
	if got.MacOui != "b827eb" {
		t.Errorf("got.MacOui = %q, want %q", got.MacOui, "b827eb")
	}
}

// TestMatchKeyStableAndDistinct asserts Key() is stable for equal Match
// values and distinct across the three forms (REL-111 dedup-by-match).
func TestMatchKeyStableAndDistinct(t *testing.T) {
	ssdp := Match{SSDP: "urn:roku-com:device:player:1"}
	mdns := Match{MDNS: "_googlecast._tcp"}
	macOui := Match{MacOui: "B827EB"}

	if ssdp.Key() != ssdp.Key() {
		t.Error("Key() is not stable for the same ssdp Match value")
	}
	if mdns.Key() != mdns.Key() {
		t.Error("Key() is not stable for the same mdns Match value")
	}
	if macOui.Key() != macOui.Key() {
		t.Error("Key() is not stable for the same macOui Match value")
	}

	keys := map[string]bool{ssdp.Key(): true, mdns.Key(): true, macOui.Key(): true}
	if len(keys) != 3 {
		t.Errorf("Key() collided across distinct match forms: ssdp=%q mdns=%q macOui=%q",
			ssdp.Key(), mdns.Key(), macOui.Key())
	}

	// A value coincidentally shared across two different forms must still
	// produce distinct keys — REL-111 dedups by the whole match, not by the
	// bare string value.
	sameValueDifferentForm := Match{MDNS: "urn:roku-com:device:player:1"}
	if ssdp.Key() == sameValueDifferentForm.Key() {
		t.Errorf("Key() collided across forms sharing the same raw value: ssdp.Key()=%q mdns.Key()=%q",
			ssdp.Key(), sameValueDifferentForm.Key())
	}
}

// TestMatchMarshalJSONRoundTrip asserts Match marshals back to exactly one
// of the three MAN-071 wire forms, and that ParseMatch(marshal(m)) == m —
// the JSON shape Match carries downstream into device.candidates (REL-110).
func TestMatchMarshalJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		m        Match
		wantJSON string
	}{
		{"ssdp", Match{SSDP: "urn:roku-com:device:player:1"}, `{"ssdp":"urn:roku-com:device:player:1"}`},
		{"mdns", Match{MDNS: "_googlecast._tcp"}, `{"mdns":"_googlecast._tcp"}`},
		{"macOui", Match{MacOui: "B827EB"}, `{"macOui":"B827EB"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.m)
			if err != nil {
				t.Fatalf("json.Marshal(%+v): %v", tt.m, err)
			}
			if string(got) != tt.wantJSON {
				t.Errorf("json.Marshal(%+v) = %s, want %s", tt.m, got, tt.wantJSON)
			}

			roundTripped, err := ParseMatch(got)
			if err != nil {
				t.Fatalf("ParseMatch(marshal(%+v)): unexpected error: %v", tt.m, err)
			}
			if roundTripped != tt.m {
				t.Errorf("ParseMatch(marshal(%+v)) = %+v, want %+v", tt.m, roundTripped, tt.m)
			}
		})
	}
}
