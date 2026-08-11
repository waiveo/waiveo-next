// Package lanaddr decides one question, in one place: is this an address the
// relay may DIAL?
//
// It exists because of a specific hole. Every dialable device address this
// relay holds that an operator did not type in came off UNAUTHENTICATED
// multicast — an SSDP LOCATION header, or the source address of an SSDP
// packet. Anything on the LAN can send those, at any time, as often as it
// likes, naming any string it wants. Before this package the only checks on
// that string were "it parses as a URL", "it has a host", and "it is under 256
// bytes", after which the host went into the candidate store, out of
// AddressFor, through the adoption gate, and into an HTTP request — so one
// spoofed NOTIFY carrying the USN of an already-adopted screen and
// `LOCATION: http://attacker.example:80/` re-pointed every subsequent command
// and every state poll for that screen at a host of the sender's choosing. The
// relay was an HTTP request forwarder with an unauthenticated control channel.
//
// # The policy
//
// An address is dialable only if its host is an IP LITERAL that names a
// plausible peer on this box's own network:
//
//   - It must be an IP literal. A DNS name is refused outright, because
//     accepting one hands the choice of the final destination to whoever
//     answers the query — a name resolves wherever its owner points it, can
//     point somewhere different on the second lookup (rebinding), and drags a
//     resolver into a path whose whole premise is that its input is hostile.
//     No real device needs one: SSDP responders state an address because the
//     control point has to reach them without a resolver.
//   - It must be a private unicast address (RFC 1918 10/8, 172.16/12,
//     192.168/16, or IPv6 ULA fc00::/7) or loopback. Everything else is
//     refused: globally routable addresses (which is what makes this relay
//     unusable as a forwarder to the internet), link-local — which is where
//     the cloud metadata endpoint 169.254.169.254 lives — multicast, the
//     broadcast address, and the unspecified address.
//
// # Why loopback is allowed
//
// It is the one exception, and it is narrow on purpose. Loopback is where a
// co-located fake device answers in this repo's own end-to-end tests and where
// a developer runs a stand-in on the box itself; refusing it would mean the
// dial path that ships is not the dial path anything exercises.
//
// What allowing it costs is small and bounded, because the marginal capability
// it hands an attacker is the one capability no host policy can take away.
// Redirecting an adopted device's commands somewhere useless is already
// possible with a private address (a spoofer can name any LAN IP, and the
// (driver, native_id) match — not the address — is what binds a sighting to a
// device), so loopback adds no new denial of control. What the rest of the
// policy takes away is the ability to make this relay talk to a host the LAN's
// own operator does not control, which is the part that turns a nuisance into
// an exfiltration and SSRF primitive.
//
// # Where the policy is applied
//
// At every layer that handles an address, not just the outermost one, because
// each is a place a future lane could arrive at:
//
//   - internal/relay/discovery/address.go — the parse of a LOCATION header or
//     a packet source, i.e. the point where hostile bytes first become an
//     address at all. Nothing that fails here is ever stored.
//   - internal/relay/deviceplane.Store.Observe — the candidate store, which is
//     the one place every discovery lane converges. An address that fails here
//     is BLANKED rather than dropping the whole sighting: the device was
//     genuinely seen, and "seen, not reachable" is the honest record.
//   - internal/relay/devicetargets.Registry.Target — the resolution that
//     immediately precedes a dial. The deployment OVERRIDE map is deliberately
//     exempt there: an override is a fact an operator typed into this relay's
//     own configuration, and this package's whole subject is values nobody
//     typed.
package lanaddr

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Split reads the three address shapes this relay actually holds into a host
// and a port, with port 0 meaning "the driver's own default":
//
//	http://192.168.50.31:8060/     an SSDP LOCATION URL (Roku's own form)
//	192.168.50.31:8060             a bare host:port authority
//	192.168.50.31                  a bare host
//
// It is SHAPE only — it says what the string names, never whether the relay
// may dial it; that is Host's job, and every caller asks both. The two are one
// package because a policy applied to a differently-parsed host is a policy
// with a hole: the store holds LOCATION URLs while discovery holds
// authorities, and reading them apart is how one lane ends up checking a
// different string from the one it later dials.
//
// A URL's port is TAKEN, not defaulted, because a Roku states its ECP port in
// LOCATION and a device on a non-standard port would otherwise be dialed on
// 8060 forever. A URL with no port yields port 0 — the scheme's implied port
// is the driver's business, not this parser's.
func Split(addr string) (host string, port int, ok bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, false
	}

	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil || u.Hostname() == "" {
			return "", 0, false
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 {
				return "", 0, false
			}
			return u.Hostname(), n, true
		}
		return u.Hostname(), 0, true
	}

	h, portText, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present: the whole string is the host. An unbracketed IPv6
		// literal lands here too and is used as-is — net.JoinHostPort at dial
		// time re-brackets it correctly.
		return strings.Trim(addr, "[]"), 0, true
	}
	if h == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(portText)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return h, n, true
}

// Dialable reports whether addr — in any of Split's three shapes — both parses
// and names a host this relay may dial. It is the whole question in one call,
// for the callers that hold a stored address rather than a parsed one.
func Dialable(addr string) bool {
	host, _, ok := Split(addr)
	return ok && Host(host)
}

// Host reports whether host — a bare host with no port and no brackets, as
// net.SplitHostPort or url.Hostname yields it — is one this relay may dial.
//
// See the package doc for the policy and its reasoning. An IPv6 zone suffix
// ("fe80::1%eth0") is refused along with the rest of link-local rather than
// stripped: a zone is only meaningful for a link-local address, so the two
// questions have the same answer.
func Host(host string) bool {
	if host == "" || strings.Contains(host, "%") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Not an IP literal — a DNS name, or nonsense. Either way, refused.
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// Ordered narrowest-first so each refusal is legible on its own; IsPrivate
	// alone would already exclude all of these, and stating them keeps the
	// policy readable as a policy rather than as a library call.
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.Equal(net.IPv4bcast) {
		return false
	}
	return ip.IsPrivate()
}
