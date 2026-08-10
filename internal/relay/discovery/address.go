package discovery

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// address.go turns an SSDP response into somewhere the relay can actually DIAL.
//
// This is the difference between a discovery lane that finds devices and one
// that finds device NAMES. An M-SEARCH response's ST and USN say a device of
// some declared type exists and how to identify it; neither says where it is.
// A candidate reported without an address can be listed and adopted, and then
// every command to it fails with nowhere to send — which is the shape the lane
// was in before this file: it read ST and USN off each response and dropped the
// LOCATION header on the floor.
//
// SSDP gives two places the address can come from, and they are not equivalent:
//
//   - LOCATION, a URL the responder chose. It is the AUTHORITATIVE answer
//     because it carries a PORT — the responder saying which port its control
//     surface listens on — and for Roku ECP it is literally
//     "http://<ip>:8060/", the exact endpoint a command dispatches to.
//   - The UDP packet's own source address, which the OS observed rather than
//     the device asserted. It survives a LOCATION that is absent or garbage,
//     but it carries only the sender's EPHEMERAL source port, never the
//     device's control port — so it is usable only alongside a port the
//     declaration side supplies (Watch.DefaultPort).
//
// Both are attacker-influenced (anything on the LAN can answer a multicast
// search), so both are parsed defensively and a value that does not resolve to
// a dialable host/port pair yields no address at all rather than a guess. A
// candidate with no address is still reported: the device WAS seen, and saying
// "seen, not reachable" is honest where inventing a port is not.

// Default ports for the two URL schemes a LOCATION may omit a port from. A
// LOCATION with any other scheme and no explicit port has no defensible default
// and yields no address.
const (
	defaultHTTPPort  = 80
	defaultHTTPSPort = 443
)

// maxAddressBytes caps the authority this package will build. It matches the
// observation-field cap the candidate store applies next (internal/relay/
// deviceplane), so an address that could not survive Observe is never built —
// an over-long host would otherwise silently drop the WHOLE sighting one layer
// down, losing a real device because of a field that is merely decorative to
// its identity.
const maxAddressBytes = 256

// addressFromLocation extracts a dialable "host:port" authority from an SSDP
// LOCATION header value, reporting false when the header carries nothing this
// relay could connect to.
//
// Refused, each for its own reason:
//
//   - empty or unparseable — there is no URL to read a host out of;
//   - a scheme other than http/https — SSDP descriptors are fetched over HTTP,
//     and any other scheme's default port is a guess this package will not make
//     (an explicit port is still honored, since then nothing is being guessed);
//   - no host — "http:///desc.xml" names a path on nowhere;
//   - a port that is not a number in 1-65535 — url.Parse accepts a port that
//     dialing cannot;
//   - an authority over the byte cap — see maxAddressBytes.
//
// An IPv6 literal is returned bracketed (net.JoinHostPort's own doing), so the
// result is directly usable as the authority of a URL and as a net.Dial target
// without the caller re-deriving the bracketing rule.
func addressFromLocation(location string) (string, bool) {
	raw := strings.TrimSpace(location)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	if host == "" {
		return "", false
	}

	portText := u.Port()
	if portText == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			portText = strconv.Itoa(defaultHTTPPort)
		case "https":
			portText = strconv.Itoa(defaultHTTPSPort)
		default:
			// No scheme (a bare "192.168.1.5/desc.xml" parses as a PATH, not a
			// host, so this branch is mostly reached by exotic schemes) and no
			// port: nothing to dial.
			return "", false
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	return joinIfSane(host, port)
}

// addressFromSource builds an authority from the UDP packet's own source
// address plus a port the DECLARATION supplies, reporting false when either
// half is missing.
//
// The port cannot come from the packet: a device's SSDP socket sends from an
// ephemeral port that has nothing to do with the port its control surface
// listens on, so connecting to it would reach nothing. defaultPort is therefore
// the watch's own declared control port (Watch.DefaultPort) — a declaration-side
// fact, exactly like the driver and device class a sighting is reported under —
// and a watch that declares none gets no fallback address rather than a fiction.
func addressFromSource(from net.Addr, defaultPort int) (string, bool) {
	if from == nil || defaultPort < 1 || defaultPort > 65535 {
		return "", false
	}
	host, _, err := net.SplitHostPort(from.String())
	if err != nil || host == "" {
		return "", false
	}
	return joinIfSane(host, defaultPort)
}

// joinIfSane renders host/port as an authority, refusing one over the byte cap.
func joinIfSane(host string, port int) (string, bool) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if len(addr) > maxAddressBytes {
		return "", false
	}
	return addr, true
}
