package arpsweep

import (
	"context"
	"errors"
	"net"
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

// TestRefusesAPrefixLargerThanTheCap: a /16 is 65k addresses. Sweeping it would
// be indistinguishable from an attack, so the sweep is REFUSED rather than
// quietly truncated — a partial sweep reported as a sweep is a lie about what
// was looked at.
func TestRefusesAPrefixLargerThanTheCap(t *testing.T) {
	probe, seen := recorder()
	_, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "10.0.0.1/16"),
		Probe:          probe,
	})
	if !errors.Is(err, ErrPrefixTooLarge) {
		t.Fatalf("err = %v, want ErrPrefixTooLarge", err)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("a refused sweep still probed %d address(es) — the cap must be checked BEFORE any traffic", n)
	}
}

// TestHonoursTheHostCap: the cap is configurable, and the refusal is about size
// rather than about the particular prefix length.
func TestHonoursTheHostCap(t *testing.T) {
	probe, _ := recorder()
	_, err := Sweep(context.Background(), Config{
		InterfaceAddrs: addrsOf(t, "192.168.50.1/24"), // 254 hosts
		Probe:          probe,
		MaxHosts:       16,
	})
	if !errors.Is(err, ErrPrefixTooLarge) {
		t.Fatalf("err = %v, want ErrPrefixTooLarge at a cap of 16", err)
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
