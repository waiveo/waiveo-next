package arpsweep

import (
	"net"
	"testing"
)

// scope_test.go covers REL-128's refusal half.
//
// The SAFETY half was never in doubt and is not what these test: arpsweep reads
// this machine's own interfaces and nothing else, so no message can point a
// relay's probes at a foreign network. What these cover is the half that was
// missing entirely — a relay must be able to say it cannot reach a requested
// scope, because until now the scope reached no code at all and an operator who
// asked to sweep a network this relay cannot see was told the scan STARTED and
// then shown findings from the relay's own segment.

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}

func TestAScopeThisRelayIsAttachedToIsAccepted(t *testing.T) {
	reachable := []*net.IPNet{mustCIDR(t, "192.168.50.0/23"), mustCIDR(t, "10.0.5.0/24")}
	for _, want := range []string{"192.168.50.0/23", "10.0.5.0/24"} {
		if !CoversRequestedScope(reachable, want) {
			t.Errorf("%s refused, but this relay is attached to it", want)
		}
	}
}

func TestTheSAMENetworkWrittenFromAHostAddressIsAccepted(t *testing.T) {
	// An operator typing the address they can see — `192.168.51.14/23` — has
	// asked for the same network as `192.168.50.0/23`. Matching on the string
	// would refuse a scope this relay is sitting on, and the operator would have
	// no way to tell that from being on the wrong network.
	reachable := []*net.IPNet{mustCIDR(t, "192.168.50.0/23")}
	if !CoversRequestedScope(reachable, "192.168.51.14/23") {
		t.Error("a host address inside the reachable network was refused — the match must be on network identity, not on spelling")
	}
}

func TestAScopeThisRelayCannotSeeIsRefused(t *testing.T) {
	// The rule's whole point. Note it is refused rather than widened: silently
	// falling back to the default scope is what produced findings from the wrong
	// segment under a "started" answer.
	reachable := []*net.IPNet{mustCIDR(t, "192.168.50.0/23")}
	for _, elsewhere := range []string{"10.9.9.0/24", "172.16.0.0/16", "192.168.60.0/24"} {
		if CoversRequestedScope(reachable, elsewhere) {
			t.Errorf("%s accepted, but this relay has no interface on it", elsewhere)
		}
	}
}

func TestADifferentPREFIXOnTheSameBaseIsRefused(t *testing.T) {
	// `192.168.50.0/24` is not `192.168.50.0/23`: it names half the network. A
	// check comparing only the base address would accept it and then sweep twice
	// what was asked for, which is the direction that matters — a scan is
	// probe traffic, and widening one silently is the thing scoping exists to
	// prevent.
	reachable := []*net.IPNet{mustCIDR(t, "192.168.50.0/23")}
	if CoversRequestedScope(reachable, "192.168.50.0/24") {
		t.Error("a narrower prefix on the same base was accepted as if it were the reachable network")
	}
}

func TestAnUnparseableScopeIsRefusedRatherThanWidened(t *testing.T) {
	// There is no reading of a malformed scope that makes it safe to fall back
	// to the default: the operator asked for something specific and typed it
	// wrong, and answering "started" would sweep a network they never named.
	reachable := []*net.IPNet{mustCIDR(t, "192.168.50.0/23")}
	for _, bad := range []string{"", "not-a-cidr", "192.168.50.0", "192.168.50.0/99"} {
		if CoversRequestedScope(reachable, bad) {
			t.Errorf("%q accepted as a reachable scope", bad)
		}
	}
}

func TestARelayWithNoSweepableInterfaceRefusesEveryScope(t *testing.T) {
	// A loopback-only or fully-public host has no private network to sweep, so
	// every scope is unreachable — including one that would be valid elsewhere.
	if CoversRequestedScope(nil, "192.168.50.0/23") {
		t.Error("a relay with no sweepable interface accepted a scope")
	}
}

func TestReachableSubnetsUsesTheSameFilterAsTheSweep(t *testing.T) {
	// The check must not drift from what a sweep would actually cover: a filter
	// that disagreed would refuse scopes that would have worked, or admit ones
	// that would not. Both derive from sweepableHosts, and this pins that they
	// agree on a public range (excluded) and a private one (included).
	addrs := func() ([]net.Addr, error) {
		return []net.Addr{
			mustCIDR(t, "192.168.50.0/23"), // private — sweepable
			mustCIDR(t, "8.8.8.0/24"),      // public — never swept
		}, nil
	}
	got, err := ReachableSubnets(Config{InterfaceAddrs: addrs})
	if err != nil {
		t.Fatalf("ReachableSubnets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("reachable = %v, want only the private network", got)
	}
	if !CoversRequestedScope(got, "192.168.50.0/23") {
		t.Error("the private network was not reported reachable")
	}
	if CoversRequestedScope(got, "8.8.8.0/24") {
		t.Error("a PUBLIC range was reported reachable — a scan must never be scopeable onto one")
	}
}
