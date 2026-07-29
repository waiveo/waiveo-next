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

// TestRateLimiterBoundsItsKeyMap: the tracked-key map MUST NOT grow without
// limit. The keys are whatever the calling surface attributes an attempt to, and
// on the surfaces this limiter now backs — pairing-grant redemption on the
// relay, grant redemption on the app — an attempt is unauthenticated, so the key
// is chosen by whoever is making it. An unbounded map is then a memory-growth
// primitive that needs no correct guess and no credential: one retained window
// per distinct source, forever, from anyone who can reach the endpoint.
func TestRateLimiterBoundsItsKeyMap(t *testing.T) {
	rl := NewRateLimiter(10, 15*60_000)

	// Every attempt inside ONE window, so nothing lapses on its own: this is the
	// hostile shape, not the benign one.
	const nowMs int64 = 1_000_000
	for i := 0; i < maxKeys*2; i++ {
		rl.Allow("2001:db8:"+itoa(i)+"::/64", nowMs)
	}

	if got := rl.trackedKeys(); got > maxKeys {
		t.Errorf("tracked keys = %d after %d distinct keys in one window, want <= %d — the key map is unbounded", got, maxKeys*2, maxKeys)
	}
}

// TestRateLimiterAdmitsANewKeyAtCapacity: a key the limiter is not already
// tracking MUST still be admitted when the map is full. The bound is a memory
// bound, never an admission decision — refusing at capacity would hand anyone
// able to fill the map a denial of the whole operation for everybody, which is
// precisely what per-key budgets exist to prevent, and strictly worse than the
// unbounded memory it would be trading away.
func TestRateLimiterAdmitsANewKeyAtCapacity(t *testing.T) {
	rl := NewRateLimiter(10, 15*60_000)

	const nowMs int64 = 1_000_000
	for i := 0; i < maxKeys; i++ {
		rl.Allow("filler-"+itoa(i), nowMs)
	}

	if !rl.Allow("a-real-screen-pairing", nowMs) {
		t.Error("Allow() for a fresh key at capacity = false, want true — a full key map denied an operation to a caller who had spent nothing")
	}
}

// TestRateLimiterEvictionPrefersLapsedWindows: making room MUST first drop keys
// whose window has already elapsed. Those carry no live count — the next Allow
// for them resets the window anyway — so dropping them changes no decision this
// limiter would make, while dropping a live one hands that key a fresh budget.
func TestRateLimiterEvictionPrefersLapsedWindows(t *testing.T) {
	const windowMs int64 = 60_000
	rl := NewRateLimiter(2, windowMs)

	// A full map of keys whose windows have all lapsed by the time the newcomer
	// arrives, plus ONE live key that has already spent its budget.
	for i := 0; i < maxKeys-1; i++ {
		rl.Allow("stale-"+itoa(i), 0)
	}
	const live = "live-key"
	nowMs := windowMs * 2
	rl.Allow(live, nowMs)
	rl.Allow(live, nowMs)
	if rl.Allow(live, nowMs) {
		t.Fatal("fixture: the live key's budget was not spent")
	}

	// The newcomer forces an eviction; the lapsed keys are what must go.
	if !rl.Allow("newcomer", nowMs) {
		t.Fatal("Allow() for a fresh key at capacity = false, want true")
	}
	if rl.Allow(live, nowMs) {
		t.Error("the spent key's budget came back — eviction dropped a LIVE window while lapsed ones were available to drop")
	}
}

// itoa is strconv.Itoa without the import churn in this file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
