package discovery

import (
	"net"
	"testing"
)

// address_test.go is the parser's adversarial half. Every input here arrives
// from unauthenticated multicast, so the interesting cases are not the happy
// ones — they are the values that must NOT produce an address, because each of
// them, accepted, becomes a host this relay dials on a schedule.

func TestAddressFromLocationParsesUsableURLs(t *testing.T) {
	cases := []struct {
		name     string
		location string
		want     string
	}{
		{
			// The real shape: a Roku's LOCATION is its ECP root, port and all.
			name:     "roku ecp root",
			location: "http://192.168.50.31:8060/",
			want:     "192.168.50.31:8060",
		},
		{
			name:     "descriptor path is discarded, authority kept",
			location: "http://192.168.50.31:8060/dial/dd.xml",
			want:     "192.168.50.31:8060",
		},
		{
			// A port-less http URL is port 80, not "no address": the responder
			// named a host over a scheme with a defined default.
			name:     "http default port",
			location: "http://10.0.0.7/desc.xml",
			want:     "10.0.0.7:80",
		},
		{
			name:     "https default port",
			location: "https://10.0.0.7/desc.xml",
			want:     "10.0.0.7:443",
		},
		{
			name:     "hostname rather than literal ip",
			location: "http://roku-lobby.local:8060/",
			want:     "roku-lobby.local:8060",
		},
		{
			// Bracketed on the way out, which is what makes the result usable
			// both as a URL authority and as a net.Dial target.
			name:     "ipv6 literal stays bracketed",
			location: "http://[fe80::1%25eth0]:8060/",
			want:     "[fe80::1%eth0]:8060",
		},
		{
			name:     "surrounding whitespace is tolerated",
			location: "  http://192.168.50.31:8060/  ",
			want:     "192.168.50.31:8060",
		},
		{
			// An unusual scheme with an EXPLICIT port is honored: nothing is
			// being guessed, and refusing it would drop a real address.
			name:     "non-http scheme with an explicit port",
			location: "rtsp://192.168.50.31:554/stream",
			want:     "192.168.50.31:554",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := addressFromLocation(tc.location)
			if !ok {
				t.Fatalf("addressFromLocation(%q) reported no address, want %q", tc.location, tc.want)
			}
			if got != tc.want {
				t.Errorf("addressFromLocation(%q) = %q, want %q", tc.location, got, tc.want)
			}
		})
	}
}

func TestAddressFromLocationRefusesUnusableValues(t *testing.T) {
	cases := []struct {
		name     string
		location string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"unparseable url", "http://[::1"},
		{"control characters", "http://192.168.1.5:8060/\x00\x01"},
		{"scheme and no host", "http:///desc.xml"},
		{
			// Parses as a PATH, not a host — which is exactly the trap: a naive
			// string split on ":" would happily produce "192.168.1.5:80".
			name:     "bare host with no scheme",
			location: "192.168.1.5/desc.xml",
		},
		{"non-http scheme with no port", "rtsp://192.168.1.5/stream"},
		{"port out of range", "http://192.168.1.5:99999/"},
		{"port zero", "http://192.168.1.5:0/"},
		{"non-numeric port", "http://192.168.1.5:eighty/"},
		{
			// The byte cap: a hostile responder must not be able to make this
			// relay build an authority the candidate store would then drop the
			// whole sighting over.
			name:     "host over the byte cap",
			location: "http://" + longHost(maxAddressBytes+10) + ":8060/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := addressFromLocation(tc.location); ok {
				t.Errorf("addressFromLocation(%q) = %q, want no address", tc.location, got)
			}
		})
	}
}

func TestAddressFromSourceUsesTheWatchesDeclaredPort(t *testing.T) {
	from := &net.UDPAddr{IP: net.ParseIP("192.168.50.31"), Port: 41234}

	// The packet's own source PORT is ephemeral and must never be dialed; the
	// declared control port is what completes the address.
	got, ok := addressFromSource(from, 8060)
	if !ok {
		t.Fatalf("addressFromSource reported no address for a real UDP sender")
	}
	if want := "192.168.50.31:8060"; got != want {
		t.Errorf("addressFromSource = %q, want %q (the watch's control port, never the sender's %d)", got, want, from.Port)
	}
}

func TestAddressFromSourceRefusesWithoutADeclaredPort(t *testing.T) {
	from := &net.UDPAddr{IP: net.ParseIP("192.168.50.31"), Port: 41234}

	for _, port := range []int{0, -1, 70000} {
		if got, ok := addressFromSource(from, port); ok {
			t.Errorf("addressFromSource(port=%d) = %q, want no address — a watch that declares no control port gets no fallback rather than a guessed one", port, got)
		}
	}
	if got, ok := addressFromSource(nil, 8060); ok {
		t.Errorf("addressFromSource(nil sender) = %q, want no address", got)
	}
}

// longHost builds a syntactically valid host label of n bytes.
func longHost(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
