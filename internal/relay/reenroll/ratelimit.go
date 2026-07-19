package reenroll

import "sync"

// RateLimiter bounds how often Expired-certificate re-enrollment (REL-025)
// may be exercised for a single relay_id: a fixed-size counting window per
// relay_id, denying once that relay_id's per-window attempt count is
// exceeded and resetting the count once a later window begins. It cannot be
// used to force repeated identity churn against a single relay identity
// (REL-025).
//
// Time is supplied by the caller as nowMs on every Allow call rather than
// read from the wall clock, so callers can drive it from the app peer's own
// trusted time (REL-023) and a driver harness can exercise window boundaries
// deterministically (conformance notes, "exercised against an injectable
// clock ... not wall-clock sleeps").
type RateLimiter struct {
	limit    int
	windowMs int64

	mu      sync.Mutex
	windows map[string]*window
}

// window is one relay_id's current counting window: the window's own start
// timestamp (the floor of some nowMs to a windowMs boundary) and how many
// attempts have been allowed since that start.
type window struct {
	start int64
	count int
}

// NewRateLimiter constructs a RateLimiter permitting up to limit attempts per
// relay_id within any windowMs-long window.
func NewRateLimiter(limit int, windowMs int64) *RateLimiter {
	return &RateLimiter{
		limit:    limit,
		windowMs: windowMs,
		windows:  make(map[string]*window),
	}
}

// Allow reports whether relayID may make another Expired-certificate
// re-enrollment attempt at nowMs. It returns false — mapping to
// RE_ENROLL_RATE_LIMITED — once relayID's count within its current window has
// already reached the configured limit; otherwise it records this attempt
// and returns true. A nowMs that falls in a later window than the one
// currently tracked for relayID resets that relay_id's count before
// evaluating the limit, so the bound governs attempts within a given window,
// never a lifetime cap.
func (r *RateLimiter) Allow(relayID string, nowMs int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.windows[relayID]
	if !ok || nowMs-w.start >= r.windowMs || nowMs < w.start {
		w = &window{start: nowMs, count: 0}
		r.windows[relayID] = w
	}

	if w.count >= r.limit {
		return false
	}
	w.count++
	return true
}
