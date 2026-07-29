package apihttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// requestFrom builds a request whose RemoteAddr is remoteAddr — the only input
// RequestSource is allowed to read (a client-supplied X-Forwarded-For would let
// a caller mint a fresh budget per request).
func requestFrom(remoteAddr string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/anything", nil)
	r.RemoteAddr = remoteAddr
	return r
}

// TestRequestSourceKeysIPv6OnItsPrefix is the keying property an attempt budget
// (SEC-033) rests on: the key must name something an attacker has to ACQUIRE.
//
// A single IPv6 host is handed a /64 — the standard SLAAC allocation — and
// rotates its own address inside it as ordinary behaviour (RFC 8981 privacy
// extensions), no attacker required. Keyed on the full address, that one host
// holds 2^64 budgets: ten attempts each, unbounded in total, from a budget that
// reads as enforced everywhere it is described. Keyed on the /64, the addresses
// it can mint for free all spend from one bucket.
func TestRequestSourceKeysIPv6OnItsPrefix(t *testing.T) {
	sameSixtyFour := []string{
		"[2001:db8:0:1::1]:40000",
		"[2001:db8:0:1::2]:40001",
		"[2001:db8:0:1:dead:beef:cafe:1]:40002",
		"[2001:db8:0:1:ffff:ffff:ffff:ffff]:40003",
	}
	want := RequestSource(requestFrom(sameSixtyFour[0]))
	for _, addr := range sameSixtyFour[1:] {
		if got := RequestSource(requestFrom(addr)); got != want {
			t.Errorf("RequestSource(%s) = %q, want %q — addresses in one /64 must share a budget; a host mints these for free", addr, got, want)
		}
	}

	// A DIFFERENT /64 is a different allocation and must not share the bucket:
	// widening past the allocation boundary is how a per-source budget turns
	// back into a shared one an unrelated caller can exhaust for everybody.
	if got := RequestSource(requestFrom("[2001:db8:0:2::1]:40000")); got == want {
		t.Errorf("RequestSource of a different /64 = %q, want != %q — separate allocations must not share a budget", got, want)
	}
}

// TestRequestSourceKeysIPv4OnTheWholeAddress pins the deliberate asymmetry. An
// IPv4 host cannot mint sibling addresses the way SLAAC does, and keying a /24
// would put every host on a subnet — every screen in a building, every host
// behind one NAT — into the single shared bucket per-source keying exists to
// avoid.
func TestRequestSourceKeysIPv4OnTheWholeAddress(t *testing.T) {
	a := RequestSource(requestFrom("192.168.50.12:40000"))
	b := RequestSource(requestFrom("192.168.50.13:40001"))
	if a == b {
		t.Errorf("RequestSource(192.168.50.12) = RequestSource(192.168.50.13) = %q — neighbouring IPv4 hosts must not share a budget", a)
	}
	if a != "192.168.50.12" {
		t.Errorf("RequestSource = %q, want the bare address %q", a, "192.168.50.12")
	}
	// An IPv4-mapped IPv6 address names an IPv4 host, and must key like one
	// rather than collapsing every mapped address into one ::ffff:0:0/64 bucket.
	if got := RequestSource(requestFrom("[::ffff:192.168.50.12]:40000")); got != a {
		t.Errorf("RequestSource of the IPv4-mapped form = %q, want %q", got, a)
	}
	if got := RequestSource(requestFrom("[::ffff:192.168.50.13]:40000")); got == a {
		t.Errorf("two distinct IPv4-mapped hosts share the key %q", got)
	}
}

// TestRequestSourceStripsThePort pins the other half of the keying: a new
// connection gets a fresh ephemeral port, so a budget that counted (address,
// port) would hand every attempt its own bucket and count nothing at all.
func TestRequestSourceStripsThePort(t *testing.T) {
	if a, b := RequestSource(requestFrom("203.0.113.9:40000")), RequestSource(requestFrom("203.0.113.9:49999")); a != b {
		t.Errorf("RequestSource varies with the source port (%q vs %q) — the budget would count nothing", a, b)
	}
	if a, b := RequestSource(requestFrom("[2001:db8::5]:40000")), RequestSource(requestFrom("[2001:db8::5]:49999")); a != b {
		t.Errorf("RequestSource varies with the source port on IPv6 (%q vs %q)", a, b)
	}
}

// TestRequestSourceUnparseableSourcesDoNotCollapse pins the degrade: a
// RemoteAddr that will not parse falls back to the raw string, never to a shared
// empty key. An unparseable source must not be treated as "no source" and
// skipped, and two different ones must not spend from one bucket.
func TestRequestSourceUnparseableSourcesDoNotCollapse(t *testing.T) {
	a := RequestSource(requestFrom("not-an-address"))
	b := RequestSource(requestFrom("also-not-an-address"))
	if a == "" || b == "" {
		t.Fatalf("unparseable sources keyed as empty (%q, %q)", a, b)
	}
	if a == b {
		t.Errorf("two distinct unparseable sources share the key %q", a)
	}
	if got := RequestSource(requestFrom("")); got != "unknown" {
		t.Errorf("RequestSource of an absent RemoteAddr = %q, want %q", got, "unknown")
	}
	if got := RequestSource(nil); got != "unknown" {
		t.Errorf("RequestSource(nil) = %q, want %q", got, "unknown")
	}
}
