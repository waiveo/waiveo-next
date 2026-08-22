// Package arpsweep is Discovery's first ACTIVE lane: it walks the relay's own
// subnet and pokes each address just hard enough that the kernel learns its MAC.
//
// # What it does, and what it deliberately does not
//
// It mints no candidates and touches no store. Sending one datagram to an
// address forces the kernel to resolve that address to a hardware address before
// it can put the frame on the wire, and the result lands in the neighbour table
// the PASSIVE lane already reads (internal/relay/neighbor). So the sweep's whole
// job is to make the passive lane's next read see hosts that had simply not
// spoken recently. One discovery path, not two.
//
// That is also why it needs no privilege: an unprivileged UDP write is enough to
// trigger address resolution. Nothing here opens a raw socket, forges a frame, or
// needs CAP_NET_RAW.
//
// # Why an active lane needs a leash, and what this one's is
//
// This is the first thing in Discovery that puts unsolicited traffic on a whole
// segment, so the constraints are the point rather than an afterthought
// (Discovery spec §8, review H2):
//
//   - SCOPE: only the relay's OWN interface subnets, and only private ones. A
//     caller cannot hand it a CIDR; it reads what the machine is actually on. A
//     relay must not be able to probe a network the operator never enabled, and
//     the surest way to guarantee that is to never accept the address from
//     anywhere it could be influenced.
//   - SIZE: a per-subnet cap on hosts. A /16 is 65k addresses; sweeping one
//     would be indistinguishable from an attack and would take the relay off its
//     real work. An oversized subnet is SKIPPED and named in the result, while
//     the subnets that fit are still swept. Skipping one interface is not the
//     silent truncation of a sweep — the result says exactly which subnets were
//     covered and which were not, so nothing is reported as looked-at that was
//     not. (An appliance typically holds both a LAN /23 and a docker0 /16;
//     refusing the whole sweep because of the second would mean never sweeping
//     the first, which hardware taught this lane on its first real run.)
//   - RATE: bounded concurrency and a per-probe timeout, so the segment sees a
//     trickle rather than a flood.
//   - TRIGGER: nothing in this package runs on a timer. It is called by the scan
//     path and by nothing else (owner, 2026-08-17: nothing active until you ask
//     for a scan).
package arpsweep

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Defaults. Deliberately conservative: this runs on an appliance whose real job
// is serving screens, on a segment full of other people's devices.
const (
	// defaultMaxHosts is the largest sweep this lane will perform. 1024 covers a
	// /22 and every home or small-site network; a prefix implying more is refused
	// so nobody discovers by accident that "scan" meant "scan 65,000 addresses".
	defaultMaxHosts = 1024

	// defaultConcurrency is how many probes are in flight at once.
	defaultConcurrency = 32

	// defaultProbeTimeout bounds ONE probe. It is short because a probe is not
	// waiting for an answer — nothing replies to this — it is only waiting for
	// the local send to complete, which includes the kernel's own address
	// resolution.
	defaultProbeTimeout = 300 * time.Millisecond

	// probePort is where the datagram is aimed: RFC 863 discard. The payload is
	// never read by anything; what matters is that sending it forces resolution.
	probePort = 9
)

// ErrPrefixTooLarge annotates a subnet skipped for SIZE, so an operator reading
// Result.Skipped can see the reason is scale and not permission.
var ErrPrefixTooLarge = errors.New("more hosts than the sweep cap")

// Config configures a sweep.
type Config struct {
	// InterfaceAddrs returns the addresses this machine holds. The seam exists so
	// a test can sweep a fabricated /29 without a real NIC; production leaves it
	// nil and the real interfaces are read. It is deliberately the ONLY way a
	// subnet enters this package — see the package doc on scope.
	InterfaceAddrs func() ([]net.Addr, error)

	// Probe sends one datagram at addr. Nil uses the real unprivileged UDP write.
	// A test replaces it to count targets without emitting anything.
	Probe func(ctx context.Context, ip string) error

	MaxHosts     int
	Concurrency  int
	ProbeTimeout time.Duration
}

// Result reports what a sweep actually did, so a caller can say so rather than
// assume. Probed counts addresses the sweep attempted; Subnets names the CIDRs
// it walked; Skipped names the ones it did not, WITH the reason.
//
// Skipped is not decoration. A sweep that quietly ignored an interface would let
// an operator believe their network had been looked at when part of it never
// was — the same class of lie as reporting a truncated sweep as a whole one.
type Result struct {
	Probed  int
	Subnets []string
	Skipped []string
}

// Sweep probes every host address on the relay's own private subnets, so the
// kernel neighbour table learns hosts that have not spoken recently.
//
// It returns when every probe has been attempted or ctx is done. A probe error
// is not a sweep error: a host that refuses, is absent, or is unreachable is the
// ordinary case — the point was to make the kernel try, and it did.
// ReachableSubnets reports the subnets a Sweep under cfg would actually cover:
// this machine's own private IPv4 networks, filtered by exactly the rule Sweep
// applies (sweepableHosts).
//
// It exists so a caller can REFUSE a requested scope instead of silently
// discarding it. A relay physically cannot probe a network it has no interface
// on — that safety is structural, not a check — but "cannot" and "will tell you
// it did not" are different things, and an operator who scopes a scan to a
// subnet this relay cannot see must learn that, rather than be told the scan
// started and shown results from somewhere else entirely.
//
// Deliberately derived from the same helper Sweep uses rather than
// re-implementing the filter: a check that disagreed with the sweep would refuse
// scopes that would have worked, or admit ones that would not.
func ReachableSubnets(cfg Config) ([]*net.IPNet, error) {
	addrs := cfg.InterfaceAddrs
	if addrs == nil {
		addrs = net.InterfaceAddrs
	}
	found, err := addrs()
	if err != nil {
		return nil, fmt.Errorf("arpsweep: read interface addresses: %w", err)
	}
	var out []*net.IPNet
	for _, a := range found {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if _, ok := sweepableHosts(ipnet); ok {
			out = append(out, ipnet)
		}
	}
	return out, nil
}

// CoversRequestedScope reports whether `requested` — a CIDR an app peer asked a
// scan be scoped to — is one this relay can actually sweep.
//
// Matched by network identity rather than by string: `192.168.50.0/23` and
// `192.168.51.14/23` name the same network, and an operator typing either has
// asked for the same thing. A requested CIDR that does not parse is not
// reachable, so it draws the same refusal as one that is simply elsewhere —
// there is no reading of an unparseable scope that makes it safe to widen to
// the default.
func CoversRequestedScope(reachable []*net.IPNet, requested string) bool {
	_, want, err := net.ParseCIDR(requested)
	if err != nil || want == nil {
		return false
	}
	wantOnes, wantBits := want.Mask.Size()
	for _, have := range reachable {
		haveOnes, haveBits := have.Mask.Size()
		if haveOnes == wantOnes && haveBits == wantBits && have.IP.Mask(have.Mask).Equal(want.IP) {
			return true
		}
	}
	return false
}

func Sweep(ctx context.Context, cfg Config) (Result, error) {
	addrs := cfg.InterfaceAddrs
	if addrs == nil {
		addrs = net.InterfaceAddrs
	}
	probe := cfg.Probe
	if probe == nil {
		probe = udpProbe
	}
	maxHosts := cfg.MaxHosts
	if maxHosts <= 0 {
		maxHosts = defaultMaxHosts
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	timeout := cfg.ProbeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}

	found, err := addrs()
	if err != nil {
		return Result{}, fmt.Errorf("arpsweep: read interface addresses: %w", err)
	}

	var targets []string
	var subnets []string
	var skipped []string
	for _, a := range found {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		hosts, ok := sweepableHosts(ipnet)
		if !ok {
			continue
		}
		// Checked per-subnet, and a failure here skips THIS subnet rather than the
		// sweep: an appliance holding a LAN /23 beside a docker0 /16 must still
		// sweep the /23.
		// Named by its NETWORK rather than by the interface address that revealed
		// it: what was skipped is a subnet, and "10.0.0.1/16" would read as an
		// address to anyone chasing the message.
		network := (&net.IPNet{IP: ipnet.IP.Mask(ipnet.Mask), Mask: ipnet.Mask}).String()
		if len(hosts) > maxHosts {
			skipped = append(skipped, fmt.Sprintf("%s (%v: %d hosts, cap %d)", network, ErrPrefixTooLarge, len(hosts), maxHosts))
			continue
		}
		// The cap is a budget across the whole sweep as well as per subnet, so a
		// machine on many segments cannot exceed it by addition.
		if len(targets)+len(hosts) > maxHosts {
			skipped = append(skipped, fmt.Sprintf("%s (%v: would exceed the %d-host budget already spent on %d address(es))", network, ErrPrefixTooLarge, maxHosts, len(targets)))
			continue
		}
		subnets = append(subnets, network)
		targets = append(targets, hosts...)
	}
	if len(targets) == 0 {
		return Result{Subnets: subnets, Skipped: skipped}, nil
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		probed int
	)
	sem := make(chan struct{}, concurrency)
	for _, ip := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			// The error is deliberately ignored: nothing answers a discard-port
			// datagram, and an unreachable host is the ordinary case. What counts
			// is that the kernel was made to resolve the address.
			_ = probe(pctx, ip)
			mu.Lock()
			probed++
			mu.Unlock()
		}(ip)
	}
	wg.Wait()

	return Result{Probed: probed, Subnets: subnets, Skipped: skipped}, ctx.Err()
}

// sweepableHosts lists the host addresses of an IPv4 private subnet, excluding
// the network and broadcast addresses and the relay's own address.
//
// IPv6 is skipped entirely: its subnets are astronomically large, its neighbour
// discovery is multicast rather than per-address, and the passive lane's identity
// rule is keyed on the MAC a v4 neighbour entry carries anyway. A v6-only segment
// is a real deployment this lane simply does not serve yet, which is better than
// pretending to sweep 2^64 addresses.
func sweepableHosts(n *net.IPNet) ([]string, bool) {
	ip4 := n.IP.To4()
	if ip4 == nil || !ip4.IsPrivate() {
		return nil, false
	}
	ones, bits := n.Mask.Size()
	if bits != 32 || ones >= 31 {
		// /31 and /32 have no host range worth sweeping.
		return nil, false
	}

	network := ip4.Mask(n.Mask)
	base := uint32(network[0])<<24 | uint32(network[1])<<16 | uint32(network[2])<<8 | uint32(network[3])
	count := uint32(1) << uint(32-ones)
	self := uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])

	out := make([]string, 0, count-2)
	for i := uint32(1); i < count-1; i++ {
		addr := base + i
		if addr == self {
			continue // the relay does not probe itself
		}
		out = append(out, fmt.Sprintf("%d.%d.%d.%d", byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr)))
	}
	return out, true
}

// udpProbe is the real probe: open a UDP socket to the address and write one
// byte. Unprivileged, and enough to make the kernel resolve the hardware address
// before the frame can leave.
func udpProbe(ctx context.Context, ip string) error {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "udp4", net.JoinHostPort(ip, fmt.Sprint(probePort)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
	}
	_, err = conn.Write([]byte{0})
	return err
}
