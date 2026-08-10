package lanaddr

import "testing"

// lanaddr_test.go is the policy stated as cases. Everything Host refuses is
// something a single unauthenticated UDP packet could otherwise make this relay
// dial, so the refusals are the point and the acceptances exist to prove the
// policy still admits a real LAN.

func TestHostAcceptsWhatALANActuallyLooksLike(t *testing.T) {
	for _, host := range []string{
		"192.168.50.31", // RFC 1918 /16 — the dev lab
		"10.0.0.7",      // RFC 1918 /8
		"172.16.4.9",    // RFC 1918 /12, low edge
		"172.31.255.254",
		"fd00:50::31", // IPv6 ULA
		"127.0.0.1",   // a co-located stand-in device (see the package doc)
		"::1",
	} {
		if !Host(host) {
			t.Errorf("Host(%q) = false, want true — this is an address a real deployment has", host)
		}
	}
}

func TestHostRefusesEverythingElse(t *testing.T) {
	cases := map[string]string{
		"a dns name":                    "attacker.example",
		"a local-looking dns name":      "roku-lobby.local",
		"the empty host":                "",
		"a globally routable v4":        "93.184.216.34",
		"a globally routable v6":        "2606:2800:220:1:248:1893:25c8:1946",
		"link-local (cloud metadata)":   "169.254.169.254",
		"ipv6 link-local":               "fe80::1",
		"ipv6 link-local with a zone":   "fe80::1%eth0",
		"ssdp's own multicast group":    "239.255.255.250",
		"mdns' multicast group":         "224.0.0.251",
		"ipv6 multicast":                "ff02::c",
		"broadcast":                     "255.255.255.255",
		"unspecified v4":                "0.0.0.0",
		"unspecified v6":                "::",
		"carrier-grade nat (not a LAN)": "100.64.0.1",
		"not an address at all":         "not-an-address",
	}
	for name, host := range cases {
		if Host(host) {
			t.Errorf("Host(%q) = true (%s), want false — one spoofed packet would aim this relay's HTTP client at it", host, name)
		}
	}
}

// TestHostRefusesAV4MappedPublicAddress: ::ffff:93.184.216.34 is the same
// public host in a different notation, and a policy that reads notation rather
// than address is a policy with a bypass.
func TestHostRefusesAV4MappedPublicAddress(t *testing.T) {
	if Host("::ffff:93.184.216.34") {
		t.Error("Host(::ffff:93.184.216.34) = true, want false")
	}
	if !Host("::ffff:192.168.50.31") {
		t.Error("Host(::ffff:192.168.50.31) = false, want true — the same private host, differently spelled")
	}
}

func TestSplitReadsTheThreeShapes(t *testing.T) {
	cases := []struct {
		addr string
		host string
		port int
	}{
		{"http://192.168.50.31:8060/", "192.168.50.31", 8060},
		{"http://192.168.50.31:8060/dial/dd.xml", "192.168.50.31", 8060},
		{"http://192.168.50.31/", "192.168.50.31", 0}, // the driver's default
		{"192.168.50.31:8060", "192.168.50.31", 8060},
		{"192.168.50.31", "192.168.50.31", 0},
		{"[fd00:50::31]:8060", "fd00:50::31", 8060},
		{"fd00:50::31", "fd00:50::31", 0},
		{"  192.168.50.31:8060  ", "192.168.50.31", 8060},
	}
	for _, tc := range cases {
		host, port, ok := Split(tc.addr)
		if !ok || host != tc.host || port != tc.port {
			t.Errorf("Split(%q) = (%q, %d, %v), want (%q, %d, true)", tc.addr, host, port, ok, tc.host, tc.port)
		}
	}
}

func TestSplitRefusesWhatNothingCouldDial(t *testing.T) {
	for _, addr := range []string{"", "   ", "http://", "http:///desc.xml", "192.168.50.31:0", "192.168.50.31:nope", ":8060"} {
		if host, port, ok := Split(addr); ok {
			t.Errorf("Split(%q) = (%q, %d, true), want not ok", addr, host, port)
		}
	}
}

// TestDialableIsTheTwoQuestionsAtOnce: the callers that hold a stored address
// ask both halves, and asking only one is exactly the defect this package
// exists to close — "http://attacker.example:8060/" parses perfectly.
func TestDialableIsTheTwoQuestionsAtOnce(t *testing.T) {
	if !Dialable("http://192.168.50.31:8060/") {
		t.Error("Dialable(a real LAN LOCATION) = false")
	}
	if Dialable("http://attacker.example:8060/") {
		t.Error("Dialable(a well-formed URL naming a public name) = true, want false")
	}
	if Dialable("nonsense") {
		t.Error(`Dialable("nonsense") = true, want false`)
	}
}
