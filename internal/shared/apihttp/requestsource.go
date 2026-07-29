package apihttp

import (
	"net"
	"net/http"
	"net/netip"
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
// # The interface zone, and the link-local bucket it forces
//
// Every link-local IPv6 peer arrives here ZONED: net/http fills RemoteAddr from
// net.TCPAddr.String(), which renders "[fe80::1%en0]:41000" for any peer on a
// link-local address. That zone is not part of the textual address net.ParseIP
// accepts — it returns nil for "fe80::1%en0" — so parsing with net.ParseIP sent
// every zoned source down the raw-string branch below and the /64 reduction
// never ran for it. One on-link host could then mint an unbounded number of
// distinct budget keys just by picking fresh interface identifiers inside its
// own fe80::/64: no router, no RA, no DHCP lease, and a budget that read as
// enforced everywhere it was described. sourceKey therefore parses with
// net/netip and strips the zone before doing anything else.
//
// Stripping it has a cost that is accepted here rather than overlooked, and it
// is worth stating bluntly: link-local has NO per-host allocation below /64 —
// the whole link shares fe80::/64 and each host picks its own interface
// identifier — so masking collapses every on-link IPv6 peer, on every
// interface, into ONE bucket. A single on-link host can therefore deny
// link-local pairing and grant redemption to every other on-link host.
//
// Do not read that as bounded by the window (15 minutes at today's constants).
// The window bounds ONE exhaustion: a refused attempt does not extend it — the
// limiter returns false without touching the window's start or count, so
// hammering continuously still releases at exactly windowMs. But the block
// renews for the price of ten requests, so an attacker who is still on-link
// when the window opens takes it again immediately. The real bound is how long
// they hold layer-2 access, not fifteen minutes. That is equally true of the
// routable-/64 aggregation this function already did, and is stated here
// because this is where a reader forms the impression.
//
// That is the deliberate trade, not an oversight. The alternative — keying
// link-local on the full address — hands anyone with layer-2 access an
// unbounded, unenforced budget, which is the same decorative bound this
// function exists to remove, differing only in being written down. A budget
// that reads as enforced and is not is the worst of the available outcomes, so
// the shared bucket is taken instead. Two things bound the damage in practice:
// the refusal is UNAVAILABLE rather than PAIRING_CODE_INVALID (playerserver's
// own handlePair doc), so an operator is never told to discard a good pairing
// code; and link-local is not the provisioning dial path — a pairing code
// encodes an operator-configured dial {host, port} (REL-126), and a link-local
// target is only reachable with a zone attached.
//
// That second argument covers PAIRING. It is not made for the app's own
// setup-code and credential-reset budgets, which key through this same function
// and are named in the cost above — nothing here establishes that those cannot
// be reached over link-local, and the honest position is that their reachability
// is unargued rather than shown to be nil. The residual is therefore stated at
// its widest and mitigated only where the mitigation is actually demonstrated.
//
// The limit is NOT raised for this bucket to compensate. It is the bucket an
// attacker reaches most cheaply — no routing required — so relaxing it where
// aggregation is highest inverts the trade; and a per-key limit would need the
// same "is this key aggregated?" rule duplicated inside two independent budget
// implementations, which is exactly the divergence this shared function exists
// to prevent.
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
//
// It parses with net/netip rather than net.ParseIP for ONE reason: net.ParseIP
// returns nil for a zoned address ("fe80::1%en0"), which is the form net/http
// hands every link-local peer, so the /64 reduction below silently never ran for
// them. See RequestSource's own doc for what that cost and why the zone is
// stripped rather than kept.
func sourceKey(host string) string {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	// Zone first, then unmap: an IPv4-mapped IPv6 address names an IPv4 host and
	// must key like one rather than collapsing every mapped address into a single
	// ::ffff:0:0/64 bucket.
	addr = addr.WithZone("").Unmap()
	if addr.Is4() {
		return addr.String()
	}
	// The /64 the address sits in, rendered with the prefix length so it can
	// never collide with a bare address key. Prefix cannot fail here: addr is a
	// 128-bit address and 64 is within its length.
	prefix, _ := addr.Prefix(64)
	return prefix.String()
}
