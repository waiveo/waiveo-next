package arpsweep

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

// arpsweep_test.go pins the LEASH before the behaviour. This is the first thing
// in Discovery that puts unsolicited traffic on a whole segment, so what matters
// most is what it refuses to do: probe anything outside the relay's own private
// subnets, probe more addresses than its cap, or run at all without being asked.

// addrsOf builds the interface-address seam from CIDR strings.
func addrsOf(t *testing.T, cidrs ...string) func() ([]net.Addr, error) {
	t.Helper()
	var out []net.Addr
	for _, c := range cidrs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("ParseCIDR(%s): %v", c, err)
		}
		out = append(out, &net.IPNet{IP: ip, Mask: n.Mask})
	}
	return func() ([]net.Addr, error) { return out, nil }
}

// recorder is a Probe seam that records targets and emits nothing.
func recorder() (func(context.Context, string) error, func() []string) {
	var mu sync.Mutex
	var got []string
	return func(_ context.Context, ip string) error {
			mu.Lock()
			got = append(got, ip)
			mu.Unlock()
			return nil
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			out := make([]string, len(got))
			copy(out, got)
			return out
		}
}

func TestSweepsEveryHostOfItsOwnSubnetExceptItself(t *testing.T) {
	probe, seen := recorder()
	// A /29: 8 addresses = network + 6 hosts + broadcast. The relay is .1.
	res, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "192.168.50.1/29"),
		Probe:          probe,
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	got := map[string]bool{}
	for _, ip := range seen() {
		got[ip] = true
	}
	// .0 (network) and .7 (broadcast) are not hosts; .1 is the relay itself.
	for _, absent := range []string{"192.168.50.0", "192.168.50.1", "192.168.50.7"} {
		if got[absent] {
			t.Errorf("swept %s — the network, broadcast and the relay's own address are not targets", absent)
		}
	}
	for _, want := range []string{"192.168.50.2", "192.168.50.3", "192.168.50.4", "192.168.50.5", "192.168.50.6"} {
		if !got[want] {
			t.Errorf("did not sweep %s", want)
		}
	}
	if res.Probed != 5 {
		t.Errorf("Probed = %d, want 5", res.Probed)
	}
	if len(res.Subnets) != 1 {
		t.Errorf("Subnets = %v, want the one swept subnet", res.Subnets)
	}
}

// TestNeverLeavesPrivateSpace is the scope leash. A relay on a public or
// link-local address must not spray that network — and because the subnet is
// read from the machine rather than accepted from a caller, there is no input
// that could make it.
func TestNeverLeavesPrivateSpace(t *testing.T) {
	for _, cidr := range []string{
		"203.0.113.10/29", // public
		"169.254.10.1/29", // link-local
		"127.0.0.1/29",    // loopback
		"100.64.0.1/29",   // CGNAT — not RFC1918 private
	} {
		probe, seen := recorder()
		res, err := Sweep(context.Background(), Config{
			InterfaceAddrs: addrsOf(t, cidr),
			Probe:          probe,
		})
		if err != nil {
			t.Fatalf("Sweep(%s): %v", cidr, err)
		}
		if n := len(seen()); n != 0 {
			t.Errorf("%s: swept %d address(es), want none — only the relay's own PRIVATE subnets are sweepable", cidr, n)
		}
		if res.Probed != 0 {
			t.Errorf("%s: Probed = %d, want 0", cidr, res.Probed)
		}
	}
}

// TestSkipsAPrefixLargerThanTheCapAndSaysSo: a /16 is 65k addresses. Sweeping it
// would be indistinguishable from an attack, so it is skipped — and NAMED, so an
// operator is never told a network was looked at when it was not.
func TestSkipsAPrefixLargerThanTheCapAndSaysSo(t *testing.T) {
	probe, seen := recorder()
	res, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "10.0.0.1/16"),
		Probe:          probe,
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("a skipped subnet still drew %d probe(s) — the cap must be checked BEFORE any traffic", n)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "10.0.0.0/16") {
		t.Fatalf("Skipped = %v, want the oversized subnet named", res.Skipped)
	}
	if len(res.Subnets) != 0 {
		t.Errorf("Subnets = %v, want nothing reported as swept", res.Subnets)
	}
}

// TestAnOversizedInterfaceDoesNotBlockTheOthers is the case HARDWARE taught this
// lane on its first real run: the appliance holds a LAN 192.168.50.12/23 AND a
// docker0 172.17.0.1/16. Failing the whole sweep on the /16 meant the LAN — the
// only segment anyone cares about — was never swept, on every box with Docker.
func TestAnOversizedInterfaceDoesNotBlockTheOthers(t *testing.T) {
	probe, seen := recorder()
	res, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "172.17.0.1/16", "192.168.50.12/29"),
		Probe:          probe,
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Probed == 0 {
		t.Fatal("the oversized docker bridge suppressed the LAN sweep — the exact defect the box exposed")
	}
	for _, ip := range seen() {
		if strings.HasPrefix(ip, "172.17.") {
			t.Errorf("swept %s on the oversized bridge", ip)
		}
	}
	if len(res.Subnets) != 1 || !strings.Contains(res.Subnets[0], "192.168.50") {
		t.Errorf("Subnets = %v, want only the LAN", res.Subnets)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "172.17.0.0/16") {
		t.Errorf("Skipped = %v, want the bridge named", res.Skipped)
	}
}

// TestHonoursTheHostCap: the cap is configurable, and skipping is about size
// rather than about the particular prefix length.
func TestHonoursTheHostCap(t *testing.T) {
	probe, seen := recorder()
	res, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "192.168.50.1/24"), // 254 hosts
		Probe:          probe,
		MaxHosts:       16,
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(seen()) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("probed %d with Skipped=%v, want none probed and the subnet skipped at a cap of 16", len(seen()), res.Skipped)
	}
}

// TestTheCapIsABudgetAcrossSubnets: many small segments must not add up past the
// cap, or the leash could be evaded by a machine with enough interfaces.
func TestTheCapIsABudgetAcrossSubnets(t *testing.T) {
	probe, seen := recorder()
	res, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "192.168.1.1/29", "192.168.2.1/29", "192.168.3.1/29"),
		Probe:          probe,
		MaxHosts:       12, // two /29s (5 hosts each) fit; the third does not
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(seen()) > 12 {
		t.Errorf("probed %d addresses, over the %d budget", len(seen()), 12)
	}
	if len(res.Skipped) == 0 {
		t.Error("the subnet that did not fit the budget was not reported as skipped")
	}
}

// TestACancelledSweepStopsAndSaysSo. A scan the operator abandoned must not keep
// putting traffic on the segment.
func TestACancelledSweepStopsAndSaysSo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probe, seen := recorder()
	_, err := Sweep(ctx, Config{
		InterfaceAddrs: addrsOf(t, "192.168.50.1/24"),
		Probe:          probe,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("a cancelled sweep probed %d address(es), want none", n)
	}
}

// TestSkipsSubnetsWithNoHostRange: /31 and /32 carry no sweepable range, and a
// machine holding one must not produce a nonsense target list.
func TestSkipsSubnetsWithNoHostRange(t *testing.T) {
	probe, seen := recorder()
	res, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "192.168.50.1/32", "192.168.50.4/31"),
		Probe:          probe,
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(seen()) != 0 || res.Probed != 0 {
		t.Errorf("swept %v on prefixes with no host range", seen())
	}
}

// TestSweepsEveryPrivateInterface: a relay with two segments sweeps both, and
// reports both, so an operator can see what was actually covered.
func TestSweepsEveryPrivateInterface(t *testing.T) {
	probe, seen := recorder()
	res, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "192.168.50.1/30", "10.9.9.1/30"),
		Probe:          probe,
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	// Each /30 has exactly 2 hosts; the relay is one of them on each.
	if res.Probed != 2 || len(seen()) != 2 {
		t.Fatalf("Probed = %d over %v, want 2", res.Probed, seen())
	}
	if len(res.Subnets) != 2 {
		t.Errorf("Subnets = %v, want both segments reported", res.Subnets)
	}
}
