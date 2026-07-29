package apihttp

import (
	"net"
	"net/http"
)

// RequestSource is the attempt-source key an attempt budget is spent from: the
// request's source ADDRESS with any port stripped.
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
// correctly takes the operation away from everybody. An address-keyed budget
// confines a guesser's spend to themselves. That is not a complete answer
// either (an attacker with many source addresses gets a bucket each), but the
// two failure modes are not comparable: per-address a guesser buys attempts
// only against themselves, per-class they buy a denial against everybody.
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
		return r.RemoteAddr
	}
	return host
}
