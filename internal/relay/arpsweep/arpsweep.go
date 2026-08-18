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
//   - SIZE: a hard cap on hosts. A /16 is 65k addresses; sweeping one would be
//     indistinguishable from an attack and would take the relay off its real
//     work. A prefix too large is REFUSED, not silently truncated — a partial
//     sweep reported as a sweep is a lie about what was looked at.
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

// ErrPrefixTooLarge is returned when the relay's own subnet implies more hosts
// than MaxHosts. It names the count so an operator can see the refusal is about
// SIZE and not about permission.
var ErrPrefixTooLarge = errors.New("arpsweep: the relay's subnet implies more hosts than the sweep cap")

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
// it walked.
type Result struct {
	Probed  int
	Subnets []string
}

// Sweep probes every host address on the relay's own private subnets, so the
// kernel neighbour table learns hosts that have not spoken recently.
//
// It returns when every probe has been attempted or ctx is done. A probe error
// is not a sweep error: a host that refuses, is absent, or is unreachable is the
// ordinary case — the point was to make the kernel try, and it did.
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
	for _, a := range found {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		hosts, ok := sweepableHosts(ipnet)
		if !ok {
			continue
		}
		// Checked per-subnet BEFORE accumulating: refusing the whole sweep is the
		// honest answer to "this segment is bigger than the cap", and it must not
		// depend on which interface happened to be read first.
		if len(hosts) > maxHosts {
			return Result{}, fmt.Errorf("%w: %s implies %d hosts, cap is %d", ErrPrefixTooLarge, ipnet.String(), len(hosts), maxHosts)
		}
		subnets = append(subnets, ipnet.String())
		targets = append(targets, hosts...)
	}
	if len(targets) == 0 {
		return Result{Subnets: subnets}, nil
	}
	if len(targets) > maxHosts {
		return Result{}, fmt.Errorf("%w: %d hosts across %d subnet(s), cap is %d", ErrPrefixTooLarge, len(targets), len(subnets), maxHosts)
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

	return Result{Probed: probed, Subnets: subnets}, ctx.Err()
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
