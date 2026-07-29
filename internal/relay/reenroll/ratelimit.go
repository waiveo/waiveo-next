package reenroll

import "sync"

// RateLimiter bounds how often a keyed operation may be exercised: a fixed-size
// counting window per key, denying once that key's per-window attempt count is
// exceeded and resetting the count once a later window begins.
//
// It was written for Expired-certificate re-enrollment per relay_id (REL-025 —
// it cannot be used to force repeated identity churn against a single relay
// identity) and is reused verbatim by every other attempt budget in this
// codebase (internal/app/auth.GrantAttemptBudget's SEC-033 grant-redemption
// budget, internal/relay/playerserver's pairing-redemption budget), so "key" is
// whatever the caller attributes an attempt to — a relay_id, or a request
// source.
//
// Time is supplied by the caller as nowMs on every Allow call rather than
// read from the wall clock, so callers can drive it from the app peer's own
// trusted time (REL-023) and a driver harness can exercise window boundaries
// deterministically (conformance notes, "exercised against an injectable
// clock ... not wall-clock sleeps").
//
// The tracked-key map is BOUNDED (maxKeys). It has to be: the keys are supplied
// by whoever is making attempts, and on the unauthenticated surfaces this now
// backs — pairing redemption, grant redemption — that is an anonymous caller.
// An unbounded map is then a memory-growth primitive reachable by anyone who can
// reach the endpoint at all, needing no correct guess and no credential: one
// retained window per distinct source, forever. Bounding it costs nothing a
// real deployment will ever notice (see maxKeys) and removes that.
type RateLimiter struct {
	limit    int
	windowMs int64
	maxKeys  int

	mu      sync.Mutex
	windows map[string]*window
}

// window is one key's current counting window: the window's own start
// timestamp (the floor of some nowMs to a windowMs boundary) and how many
// attempts have been allowed since that start.
type window struct {
	start int64
	count int
}

// maxKeys is how many keys one limiter tracks at once. It is far above any
// legitimate concurrent population — a site has a handful of relays and a
// building's worth of screens, orders of magnitude below this — so eviction is
// unreachable in ordinary operation and only ever trims a caller who is
// churning keys deliberately.
const maxKeys = 4096

// NewRateLimiter constructs a RateLimiter permitting up to limit attempts per
// key within any windowMs-long window, tracking at most maxKeys keys.
func NewRateLimiter(limit int, windowMs int64) *RateLimiter {
	return &RateLimiter{
		limit:    limit,
		windowMs: windowMs,
		maxKeys:  maxKeys,
		windows:  make(map[string]*window),
	}
}

// Allow reports whether key may make another attempt at nowMs. It returns false
// — mapping to RE_ENROLL_RATE_LIMITED on the re-enrollment path, to the calling
// surface's own refusal elsewhere — once key's count within its current window
// has already reached the configured limit; otherwise it records this attempt
// and returns true. A nowMs that falls in a later window than the one currently
// tracked for key resets that key's count before evaluating the limit, so the
// bound governs attempts within a given window, never a lifetime cap.
//
// A key this limiter is not already tracking is admitted even when the key map
// is full: the map's bound is a memory bound, never an admission decision.
// Refusing an unknown key at capacity would hand anyone able to fill the map a
// denial of the whole operation for everybody — precisely the outcome per-key
// budgets exist to avoid — so a full map evicts instead (evictLocked).
func (r *RateLimiter) Allow(key string, nowMs int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.windows[key]
	if !ok || nowMs-w.start >= r.windowMs || nowMs < w.start {
		if !ok && len(r.windows) >= r.maxKeys {
			r.evictLocked(nowMs)
		}
		w = &window{start: nowMs, count: 0}
		r.windows[key] = w
	}

	if w.count >= r.limit {
		return false
	}
	w.count++
	return true
}

// evictLocked makes room for one new key. It first drops every key whose window
// has already lapsed at nowMs (or whose start is in the future, which only a
// caller's clock going backwards produces): those carry no live count — the next
// Allow for them would reset the window anyway — so dropping them changes no
// decision this limiter would make. Only if that frees nothing does it drop the
// single oldest live window, the one closest to lapsing on its own.
//
// Evicting a live window does return that key a fresh budget, so it is worth
// stating what it costs an attacker: the eviction has to be paid for with
// maxKeys attempts from keys they actually hold — real distinct sources, since
// the key comes from the connection and not from anything they can assert — to
// buy back one key's limit. That is a losing trade against simply spending the
// budgets they already have, which is why bounded memory is worth it.
// The caller holds r.mu.
func (r *RateLimiter) evictLocked(nowMs int64) {
	oldestKey := ""
	var oldestStart int64
	haveOldest := false
	for k, w := range r.windows {
		if nowMs-w.start >= r.windowMs || nowMs < w.start {
			delete(r.windows, k)
			continue
		}
		if !haveOldest || w.start < oldestStart {
			oldestKey, oldestStart, haveOldest = k, w.start, true
		}
	}
	if len(r.windows) >= r.maxKeys && haveOldest {
		delete(r.windows, oldestKey)
	}
}

// trackedKeys is how many keys this limiter currently holds state for — the
// bound maxKeys enforces, exposed for this package's own tests.
func (r *RateLimiter) trackedKeys() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.windows)
}
