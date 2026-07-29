package apihttp

import (
	"net"
	"net/http"
)

// RequestSource is the attempt-source key an attempt budget is spent from: the
// smallest network allocation the request's source address can be attributed to
// — the address itself for IPv4, its /64 prefix for IPv6.
//
// It lives here, shared, because more than one surface has to bound repeated
// guesses at a secret it holds — the app's grant-redemption endpoints
// (security-model/1 SEC-033, internal/app/auth.GrantAttemptBudget) and the
// relay's pairing-grant redemption (internal/relay/playerserver) — and the
// keying is the part that is easy to get wrong in a way nothing fails on. A
// budget keyed too coarsely is worse than no budget, so the two surfaces read
// the same function rather than each deciding again.
//
// It is deliberately not an address CLASS. A class has a handful of values, so
// a class-keyed budget is one bucket for the whole LAN and exhausting it denies
// the operation to every other host in it — a caller who never guesses
// correctly takes the operation away from everybody. A per-allocation budget
// confines a guesser's spend to themselves. That is not a complete answer
// either (an attacker holding many allocations gets a bucket each), but the two
// failure modes are not comparable: per-allocation a guesser buys attempts only
// against themselves, per-class they buy a denial against everybody.
//
// # Why the IPv6 prefix rather than the IPv6 address
//
// The unit an attacker has to acquire is what a budget can bound; anything they
// can mint for free bounds nothing. A single IPv6 host is routinely handed a /64
// — that is the standard SLAAC allocation — and privacy extensions (RFC 8981)
// make it rotate its own address within that prefix as a matter of ordinary
// operation, with no attacker involved at all. Keying on the full 128-bit
// address therefore hands one host 2^64 fresh budgets it does not even have to
// try for: ten attempts each, unbounded in aggregate, from a budget that reads
// as enforced. The /64 is the allocation boundary, so it is the key.
//
// IPv4 is keyed on the whole address (its /32), and the asymmetry is deliberate
// rather than an oversight: an IPv4 host cannot mint sibling addresses the way
// SLAAC does, and widening to a /24 would bucket a whole subnet — every screen
// in a building, and every host behind one NAT — into the shared bucket that
// per-source keying exists to avoid.
//
// The port is stripped because a new connection gets a new ephemeral port,
// which would otherwise hand every attempt its own bucket and make the budget
// count nothing.
//
// It deliberately does NOT consult X-Forwarded-For: that header is
// client-supplied, and honoring it would let a caller mint a fresh budget per
// request by varying it — the exact bypass per-source scoping exists to
// prevent. A deployment behind a real reverse proxy must terminate and rewrite
// RemoteAddr at that proxy.
//
// A malformed or absent RemoteAddr falls back to the raw string rather than to
// an empty key: an unparseable source must not collapse into one shared bucket
// with every other unparseable source's attempts, and must never be treated as
// "no source" and skipped.
func RequestSource(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		if r.RemoteAddr == "" {
			return "unknown"
		}
		return sourceKey(r.RemoteAddr)
	}
	return sourceKey(host)
}

// sourceKey reduces one source host to the allocation a budget is keyed on: the
// /64 prefix for an IPv6 address, the address itself for IPv4 (including an
// IPv4-mapped IPv6 address, which names an IPv4 host and must key like one), and
// the raw string for anything that does not parse as an IP at all.
func sourceKey(host string) string {
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	// The /64 the address sits in — its first 8 bytes, the rest zeroed — rendered
	// with the prefix length so it can never collide with a bare address key.
	prefix := ip.Mask(net.CIDRMask(64, 128))
	return prefix.String() + "/64"
}
