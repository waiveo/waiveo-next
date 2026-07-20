package manifest

import "testing"

// TestIsPublisherNameID covers MAN-001: id is <publisher>/<name>, each segment
// ^[a-z][a-z0-9-]{1,38}$ — exactly one slash, both segments lowercase.
func TestIsPublisherNameID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"acme/minimal-pack", true},
		{"acme/weather-widget", true},
		{"a1/b2", true},
		{"Acme/Weather", false},       // uppercase publisher
		{"acme/Weather", false},       // uppercase name
		{"acme", false},               // no slash
		{"acme/", false},              // empty name
		{"/weather", false},           // empty publisher
		{"acme/weather/extra", false}, // two slashes
		{"1acme/weather", false},      // publisher must start with a letter
		{"ac_me/weather", false},      // underscore not allowed
		{"a/b", false},                // each segment MUST be >= 2 chars ({1,38} after the lead)
	}
	for _, c := range cases {
		if got := IsPublisherNameID(c.id); got != c.want {
			t.Errorf("IsPublisherNameID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// TestIsMsgRef covers MAN-003: a locale-catalog reference carries the msg: prefix
// and a non-empty key suffix; a raw string is not a msg-ref.
func TestIsMsgRef(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"msg:pack.displayName", true},
		{"msg:cap.egress", true},
		{"Weather", false},
		{"msg:", false}, // empty suffix
		{"", false},
		{"prefix-msg:x", false},
	}
	for _, c := range cases {
		if got := IsMsgRef(c.s); got != c.want {
			t.Errorf("IsMsgRef(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// TestIsThreeComponentVersion covers MAN-002: version is MAJOR.MINOR.PATCH,
// digits only per component.
func TestIsThreeComponentVersion(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"0.1.0", true},
		{"1.2.0", true},
		{"10.20.30", true},
		{"1.2", false},     // two components
		{"1.2.0.1", false}, // four components
		{"1.a.0", false},   // non-digit component
		{"1..0", false},    // empty component
		{"v1.2.0", false},  // leading v
		{"", false},
	}
	for _, c := range cases {
		if got := IsThreeComponentVersion(c.v); got != c.want {
			t.Errorf("IsThreeComponentVersion(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}
