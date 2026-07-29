package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
)

// Lockout implements SEC-090's failed-authentication policy: exponential backoff
// scoped per `(credential, source-IP class)`, explicitly NOT a single global
// switch on the target principal.
//
// That scoping is the whole point of the requirement, so it is worth stating
// what it buys: an attacker spraying the login endpoint from one source burns
// down the budget for THEIR OWN (credential, ip-class) key only. The legitimate
// owner, arriving from a different source class, has an untouched budget and
// can still log in. A principal-wide counter — the obvious implementation —
// would hand that attacker a denial-of-service against the account they are
// attacking, which is why the contract forbids it by name.
//
// Time is supplied by the caller on every call rather than read from a wall
// clock, matching reenroll.RateLimiter's own seam, so backoff windows are
// exercised deterministically by a test rather than by sleeping.
type Lockout struct {
	// threshold is how many consecutive failures are tolerated before the key
	// locks at all — a fat-fingered password should not cost a real user a
	// wait.
	threshold int
	// baseMs is the first lock duration once the threshold is crossed;
	// each further failure doubles it, capped at maxMs.
	baseMs int64
	maxMs  int64

	mu      sync.Mutex
	entries map[string]*lockoutEntry
}

// lockoutEntry is one (credential, ip-class) key's state: how many consecutive
// failures it has accumulated and, when locked, the instant the lock lifts.
type lockoutEntry struct {
	failures  int
	lockedTil int64
}

// Lockout defaults. Five tolerated failures then a 30s lock doubling to a 15m
// ceiling: slow enough that an online guessing attack is hopeless against any
// non-trivial password, short enough that a legitimate user who mistyped five
// times is not locked out for the afternoon. These are operational numbers the
// contract does not fix (SEC-090 mandates "exponential backoff", not a
// schedule), so they live here as named constants rather than as contract
// values.
const (
	DefaultLockoutThreshold       = 5
	DefaultLockoutBaseMs    int64 = 30_000
	DefaultLockoutMaxMs     int64 = 15 * 60_000
	lockoutMaxShift         uint  = 20 // guards the doubling against int64 overflow
)

// NewLockout returns a Lockout tolerating threshold consecutive failures per key
// before locking for baseMs, doubling per further failure up to maxMs.
func NewLockout(threshold int, baseMs, maxMs int64) *Lockout {
	return &Lockout{
		threshold: threshold,
		baseMs:    baseMs,
		maxMs:     maxMs,
		entries:   make(map[string]*lockoutEntry),
	}
}

// NewDefaultLockout returns a Lockout on the default schedule above.
func NewDefaultLockout() *Lockout {
	return NewLockout(DefaultLockoutThreshold, DefaultLockoutBaseMs, DefaultLockoutMaxMs)
}

// Locked reports whether key is currently locked out at nowMs, and for how many
// more milliseconds. A caller that gets locked=true MUST refuse with
// CREDENTIAL_LOCKED (SEC-090) WITHOUT verifying the presented credential — the
// point of the lock is to stop the verification work, not merely to hide its
// result.
func (l *Lockout) Locked(key string, nowMs int64) (locked bool, retryAfterMs int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || e.lockedTil <= nowMs {
		return false, 0
	}
	return true, e.lockedTil - nowMs
}

// Fail records one failed authentication against key at nowMs and returns the
// resulting lock duration (0 while the key is still under the tolerated
// threshold). The backoff is `baseMs << (failures - threshold)`, capped at maxMs.
func (l *Lockout) Fail(key string, nowMs int64) (lockedForMs int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		e = &lockoutEntry{}
		l.entries[key] = e
	}
	e.failures++
	if e.failures <= l.threshold {
		return 0
	}
	shift := uint(e.failures - l.threshold - 1)
	if shift > lockoutMaxShift {
		shift = lockoutMaxShift
	}
	dur := l.baseMs << shift
	if dur > l.maxMs || dur <= 0 {
		dur = l.maxMs
	}
	e.lockedTil = nowMs + dur
	return dur
}

// Succeed clears key's failure history — a successful authentication resets the
// backoff, so a user who eventually remembers their password is not still
// paying for the attempts before it.
func (l *Lockout) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// LockoutKey builds SEC-090's `(credential, source-IP class)` composite key.
//
// credentialRef is the credential's own id where the presented identifier
// resolved to a real credential row. Where it did NOT — a login attempt naming
// an identifier that does not exist — there is no credential to key on, so the
// caller passes IdentifierRef(identifier) instead: a hash of the submitted
// identifier, which keeps an enumeration attack against non-existent accounts
// bounded by the same policy without ever storing what was guessed.
func LockoutKey(credentialRef, ipClass string) string {
	return credentialRef + "|" + ipClass
}

// IdentifierRef is the stand-in credential reference for a login attempt whose
// identifier resolved to no credential row (see LockoutKey). It is a truncated
// SHA-256 of the identifier so the lockout map never holds attacker-supplied
// strings verbatim.
func IdentifierRef(identifier string) string {
	sum := sha256.Sum256([]byte("auth/identifier:" + identifier))
	return "id:" + hex.EncodeToString(sum[:8])
}

// Source-IP classes (SEC-090's "source-IP class"; the same vocabulary a
// session's device_metadata.ip_class carries, security-model/1 Wire shapes).
const (
	IPClassLoopback = "loopback"
	IPClassLAN      = "lan"
	IPClassWAN      = "wan"
)

// IPClass classifies a request's remote address into the coarse source class
// SEC-090 scopes lockout by. Classifying by CLASS rather than by exact address
// is deliberate: a per-address key would be trivially defeated by an attacker
// with a /64 of IPv6 to rotate through, while a per-class key still separates
// the LAN operator from the internet-facing attacker, which is the separation
// the requirement actually needs.
func IPClass(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// An unparseable remote address is treated as the least-trusted class
		// rather than skipped, so a malformed value can never buy a fresh budget.
		return IPClassWAN
	}
	switch {
	case ip.IsLoopback():
		return IPClassLoopback
	case ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return IPClassLAN
	default:
		return IPClassWAN
	}
}

// RequestIPClass is IPClass over r.RemoteAddr. It deliberately does NOT consult
// X-Forwarded-For: that header is client-supplied, and honoring it would let an
// attacker mint a fresh lockout budget per request by varying it — the exact
// bypass SEC-090's per-source scoping exists to prevent. A deployment behind a
// real reverse proxy must terminate and rewrite RemoteAddr at that proxy.
func RequestIPClass(r *http.Request) string {
	return IPClass(r.RemoteAddr)
}

// RequestSource is the attempt-source key the grant attempt budget is spent
// from: the request's source ADDRESS with any port stripped.
//
// It is deliberately not IPClass. A class has three values, so a class-keyed
// budget is one bucket for the whole LAN and exhausting it denies the operation
// to every other host in it. An address-keyed budget confines a guesser's spend
// to themselves. The port is stripped because a new connection gets a new
// ephemeral port, which would otherwise hand every attempt its own bucket and
// make the budget count nothing.
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
