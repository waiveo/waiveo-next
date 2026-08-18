// Package portscan is Discovery's second ACTIVE lane: it asks a host which of a
// small, curated set of ports will accept a connection.
//
// # Why ports at all
//
// After the ARP sweep the inventory knows a host EXISTS and what its MAC vendor
// is, and for most hosts that is where passive discovery stops — a box that
// announces nothing over mDNS or SSDP is just "something at 192.168.50.84". An
// open port is the cheapest honest evidence of what it does: 9100 or 631 is a
// printer, 8060 is a Roku, 445 is file storage. That is what turns a row from
// "something is there" into something an operator can act on, and it is what
// spec §7's open-ports column is for.
//
// # Why so few ports, and why these
//
// This is a CLASSIFICATION aid, not a security scanner. The set is deliberately
// tiny and service-identifying: each port here maps to a device kind an operator
// would recognise. Scanning more would take longer, look far more like an attack
// to anything watching the segment, and tell Discovery nothing it acts on.
//
// # The leash
//
// Same posture as the ARP sweep, and for the same reasons (spec §8, review H2):
// bounded concurrency, a short per-probe timeout, targets supplied by the caller
// from what the relay itself already saw, and no timer anywhere in the package —
// a scan calls it or nothing does.
//
// A connection that is refused, times out, or is reset is a CLOSED port and a
// perfectly good answer. Nothing here retries: a scan reports what it saw at one
// moment, and pretending otherwise would make an intermittent host look open.
package portscan

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"
)

// Ports is the curated set. Each entry earns its place by identifying a KIND of
// device an operator recognises, not by being interesting to an attacker.
var Ports = []int{
	22,    // ssh — a computer, a NAS, an appliance
	80,    // http — a web UI, which most managed devices have
	443,   // https — the same, secured
	445,   // smb — file storage
	631,   // ipp — a printer
	1400,  // sonos — a speaker
	5353,  // mdns — announces itself, even if this relay's lane is off
	8060,  // roku ecp — a Roku, which this platform drives
	9100,  // jetdirect — a raw-print printer
	32400, // plex — a media server
}

// Defaults, deliberately conservative: this runs on an appliance whose real job
// is serving screens, against other people's devices.
const (
	defaultConcurrency = 32

	// defaultTimeout bounds ONE connect. Short on purpose: on a LAN a listening
	// port answers in single-digit milliseconds, so a longer wait only lengthens
	// the scan by waiting on hosts that are not there.
	defaultTimeout = 400 * time.Millisecond

	// maxOpenPortsPerHost bounds what one host may contribute. A host answering
	// on every probed port is either a honeypot or a middlebox forwarding
	// everything, and either way the list stops being evidence of anything.
	maxOpenPortsPerHost = 16
)

// Config configures a scan.
type Config struct {
	// Dial opens one connection. Nil uses the real TCP dialer. The seam lets a
	// test assert which addresses were probed without opening a socket.
	Dial func(ctx context.Context, address string) (net.Conn, error)

	// Ports overrides the curated set (a test uses this; production does not).
	Ports []int

	Concurrency int
	Timeout     time.Duration
}

// Scan probes each host in hosts and returns the open ports found, keyed by
// host. A host with nothing open is ABSENT from the result rather than present
// with an empty list: "we looked and found nothing open" and "we did not look"
// are different facts, and only the caller knows which hosts it asked about.
func Scan(ctx context.Context, hosts []string, cfg Config) map[string][]int {
	dial := cfg.Dial
	if dial == nil {
		dial = tcpDial
	}
	ports := cfg.Ports
	if len(ports) == 0 {
		ports = Ports
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	var (
		mu  sync.Mutex
		out = map[string][]int{}
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, concurrency)

	for _, host := range hosts {
		for _, port := range ports {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(host string, port int) {
				defer wg.Done()
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				pctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				conn, err := dial(pctx, net.JoinHostPort(host, itoa(port)))
				if err != nil {
					return // refused, timed out, reset — all mean CLOSED
				}
				_ = conn.Close()
				mu.Lock()
				if len(out[host]) < maxOpenPortsPerHost {
					out[host] = append(out[host], port)
				}
				mu.Unlock()
			}(host, port)
		}
	}
	wg.Wait()

	// Sorted so a re-scan of an unchanged host produces an identical list — the
	// merge downstream compares these, and goroutine completion order is not a
	// property of the network.
	for host := range out {
		sort.Ints(out[host])
	}
	return out
}

func tcpDial(ctx context.Context, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", address)
}

// itoa avoids pulling strconv in for one call site's readability.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
