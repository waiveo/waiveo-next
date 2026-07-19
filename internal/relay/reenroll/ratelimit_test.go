package reenroll

import "testing"

// TestRateLimiterAllowsUpToLimit is REL-025's bound: N attempts within a
// single window are allowed, and the N+1th within that same window MUST be
// denied ("yes — after the bound's window elapses" implies it is NOT allowed
// before the window elapses).
func TestRateLimiterAllowsUpToLimit(t *testing.T) {
	rl := NewRateLimiter(3, 60_000) // 3 attempts per 60s window

	const relayID = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"
	const windowStart int64 = 1_000_000

	for i := 1; i <= 3; i++ {
		if !rl.Allow(relayID, windowStart+int64(i)) {
			t.Fatalf("Allow() attempt %d = false, want true (within the %d-attempt window)", i, 3)
		}
	}

	if rl.Allow(relayID, windowStart+4) {
		t.Fatal("Allow() attempt 4 = true, want false (RE_ENROLL_RATE_LIMITED once the bound is exceeded, REL-025)")
	}
}

// TestRateLimiterResetsAcrossWindows: once a later window begins, the same
// relay_id's attempt count MUST reset — the bound governs "within a given
// window," not a lifetime cap.
func TestRateLimiterResetsAcrossWindows(t *testing.T) {
	rl := NewRateLimiter(2, 60_000)

	const relayID = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"

	if !rl.Allow(relayID, 0) {
		t.Fatal("Allow() first attempt in window 0 = false, want true")
	}
	if !rl.Allow(relayID, 1_000) {
		t.Fatal("Allow() second attempt in window 0 = false, want true")
	}
	if rl.Allow(relayID, 2_000) {
		t.Fatal("Allow() third attempt still in window 0 = true, want false (limit exceeded)")
	}

	// A timestamp in a later window (>= windowMs after the window's start)
	// resets this relay_id's count.
	if !rl.Allow(relayID, 60_000) {
		t.Fatal("Allow() first attempt in the next window = false, want true (window reset)")
	}
	if !rl.Allow(relayID, 60_500) {
		t.Fatal("Allow() second attempt in the next window = false, want true")
	}
	if rl.Allow(relayID, 60_999) {
		t.Fatal("Allow() third attempt still in the next window = true, want false")
	}
}

// TestRateLimiterPerRelayIDIndependent: the bound is per relay_id (REL-025) —
// one relay_id being rate-limited MUST NOT affect another's independent
// budget.
func TestRateLimiterPerRelayIDIndependent(t *testing.T) {
	rl := NewRateLimiter(1, 60_000)

	const relayA = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"
	const relayB = "01J8Z4K4N5P6Q7R8S9T0V1W3B2"

	if !rl.Allow(relayA, 0) {
		t.Fatal("Allow() first attempt for relayA = false, want true")
	}
	if rl.Allow(relayA, 100) {
		t.Fatal("Allow() second attempt for relayA = true, want false (limit exceeded)")
	}
	if !rl.Allow(relayB, 100) {
		t.Fatal("Allow() first attempt for relayB = false, want true (independent budget from relayA, REL-025)")
	}
}

// TestRateLimiterErrorMapping documents how a denied Allow() maps onto the
// Error-taxonomy code this path raises (REL-025): callers translate a false
// Allow() into a ReEnrollError with CodeRateLimited (wired at the Task 4
// endpoint layer, asserted here for the code constant's own shape).
func TestRateLimiterErrorMapping(t *testing.T) {
	if CodeRateLimited != "RE_ENROLL_RATE_LIMITED" {
		t.Fatalf("CodeRateLimited = %q, want %q", CodeRateLimited, "RE_ENROLL_RATE_LIMITED")
	}
}
