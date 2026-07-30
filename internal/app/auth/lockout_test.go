package auth

import (
	"fmt"
	"testing"
)

// TestLockoutMapIsBounded: sustained pressure from distinct attacker-chosen
// identifiers cannot grow the map without limit.
//
// This is the leak it fixes: the key's identifier half is whatever an
// unauthenticated caller typed, and the only pruning path (Succeed) requires a
// SUCCESSFUL authentication on that exact key — impossible for a name that does
// not exist. So every attempt against a made-up identifier used to leave a
// permanent entry.
func TestLockoutMapIsBounded(t *testing.T) {
	l := NewDefaultLockout()
	now := int64(1_000_000)
	for i := 0; i < maxLockoutEntries*3; i++ {
		l.Fail(LockoutKey(IdentifierRef(fmt.Sprintf("ghost-%d", i)), "lan"), now)
		now += 10 // each from a slightly later instant, so lastMs orders them
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n > maxLockoutEntries {
		t.Fatalf("map holds %d entries after %d distinct keys, want at most %d — the bound is not holding",
			n, maxLockoutEntries*3, maxLockoutEntries)
	}
}

// TestAnInForceLockoutSurvivesMapPressure is the property that matters more than
// the bound: an attacker must NOT be able to clear their own lockout by flooding
// the map with fresh keys.
//
// A bound that evicted live locks would turn this memory fix into an
// authentication bypass, which is far worse than the leak. So the victim here is
// locked first, then buried under many times the cap in unrelated keys, and must
// still be locked afterwards.
func TestAnInForceLockoutSurvivesMapPressure(t *testing.T) {
	l := NewDefaultLockout()
	now := int64(1_000_000)
	victim := LockoutKey(IdentifierRef("victim@example.invalid"), "lan")

	// Drive the victim past the threshold so a lock is genuinely in force.
	var locked int64
	for i := 0; i <= DefaultLockoutThreshold; i++ {
		locked = l.Fail(victim, now)
	}
	if locked <= 0 {
		t.Fatalf("fixture did not lock the victim: Fail returned %d", locked)
	}
	if isLocked, _ := l.Locked(victim, now); !isLocked {
		t.Fatal("fixture did not lock the victim")
	}

	// Now flood, staying inside the lock's own window so it is in force throughout.
	for i := 0; i < maxLockoutEntries*3; i++ {
		l.Fail(LockoutKey(IdentifierRef(fmt.Sprintf("flood-%d", i)), "lan"), now+1)
	}

	isLocked, retry := l.Locked(victim, now+1)
	if !isLocked {
		t.Fatal("the victim's in-force lockout was evicted by map pressure — a bound that does this is an authentication bypass, not a memory fix")
	}
	if retry <= 0 {
		t.Fatalf("victim still locked but retryAfterMs = %d", retry)
	}
}
