package clocktrust

import (
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// The REL-133 corpus fixture values (rel133PersistedFloor / rel133CertNotAfter /
// rel133BoundedGrace / rel133Hint1TS / rel133Hint2TS) and the REL-136 fixture
// values (rel136CertNotBeforeMs / rel136CertNotAfterMs / rel136ColdBootMs) are
// declared once in hint_test.go and peercert_test.go respectively, and reused
// here — this file asserts the state machine's behavior against the same frozen
// oracle values.

// memStore opens an ephemeral in-memory identity store — the real FloorStore
// implementation the controller advances its verified floor against.
func memStore(t *testing.T) *identity.Store {
	t.Helper()
	s, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type spyHook struct{ calls []string }

func (s *spyHook) fn(state string) error {
	s.calls = append(s.calls, state)
	return nil
}

// TestController_BootsUntrusted: every boot starts untrusted — even a warm boot
// with a persisted floor, because the floor is a past observed time, not a
// currently verified wall reading (REL-130/131). The connect-time clockTrusted
// input (Trusted) is false, so peer-cert temporal validation defers (REL-136).
func TestController_BootsUntrusted(t *testing.T) {
	store := memStore(t)
	if _, err := store.SetClockFloor(rel133PersistedFloor); err != nil { // warm boot: floor already persisted
		t.Fatalf("seed floor: %v", err)
	}

	c := NewController(store, NewRuntimeClock(), nil)

	if got := c.State(); got != StateUntrusted {
		t.Errorf("boot State() = %q, want %q (a persisted floor is not a trusted wall, REL-131)", got, StateUntrusted)
	}
	if c.Trusted() {
		t.Errorf("boot Trusted() = true, want false — connect-time temporal validation must defer (REL-136)")
	}
	if state, _ := c.ClockStateReport(); state != "untrusted" {
		t.Errorf("boot ClockStateReport state = %q, want untrusted (REL-038)", state)
	}
}

// TestController_UntrustedDefersPeerCertTemporal (REL-136): the controller's
// Trusted() is the clockTrusted input the connect-time peer-cert validation
// reads; while untrusted, ValidatePeerCert on an app-peer cert whose window is
// decades ahead of the cold-boot clock DEFERS temporal validation and completes
// on the pin alone.
func TestController_UntrustedDefersPeerCertTemporal(t *testing.T) {
	store := memStore(t) // cold boot: no floor persisted
	c := NewController(store, NewRuntimeClock(), nil)

	leaf, spki, _ := makeLeaf(t, time.UnixMilli(rel136CertNotBeforeMs), time.UnixMilli(rel136CertNotAfterMs))

	if err := ValidatePeerCert(leaf.Raw, spki, rel136ColdBootMs, DefaultBoundedGraceMs, c.Trusted()); err != nil {
		t.Fatalf("cold-boot untrusted connect on the pin rejected: %v (REL-136 deferral must complete the handshake)", err)
	}
}

// TestController_VerifiedAdvanceFlipsTrustedAndReEvaluates (REL-132/134): a
// VERIFIED time (a signed timestamp under the desired-state key, or verified
// NTP) applied from the untrusted state advances the persisted floor AND flips
// clock_state to trusted, firing the engine re-evaluation hook exactly once
// (REL-134 -> RUL-371).
func TestController_VerifiedAdvanceFlipsTrustedAndReEvaluates(t *testing.T) {
	store := memStore(t)
	if _, err := store.SetClockFloor(rel133PersistedFloor); err != nil {
		t.Fatalf("seed floor: %v", err)
	}
	spy := &spyHook{}
	c := NewController(store, NewRuntimeClock(), spy.fn)

	advanced, transitioned, err := c.ApplyVerifiedTime(rel133PersistedFloor + 60_000)
	if err != nil {
		t.Fatalf("ApplyVerifiedTime: %v", err)
	}
	if !advanced {
		t.Errorf("floorAdvanced = false, want true (a later verified time advances the floor, REL-130)")
	}
	if !transitioned {
		t.Errorf("transitioned = false, want true (untrusted->trusted, REL-134)")
	}
	if c.State() != StateTrusted || !c.Trusted() {
		t.Errorf("after verified advance State()=%q Trusted()=%v, want trusted/true", c.State(), c.Trusted())
	}
	if len(spy.calls) != 1 || spy.calls[0] != "trusted" {
		t.Errorf("engine hook calls = %v, want exactly one [trusted] (REL-134 immediate re-eval)", spy.calls)
	}
	if state, source := c.ClockStateReport(); state != "trusted" || source == "" {
		t.Errorf("ClockStateReport = (%q,%q), want (trusted, non-empty source) (REL-038)", state, source)
	}
	// The floor actually advanced in the persisted store.
	if got, ok, _ := store.ClockFloor(); !ok || got != rel133PersistedFloor+60_000 {
		t.Errorf("persisted floor = (%d,%v), want %d", got, ok, rel133PersistedFloor+60_000)
	}
}

// TestController_SecondVerifiedApplyIsIdempotent: once trusted, a further
// verified-time application does not re-fire the engine re-eval hook (the
// untrusted->trusted edge fires once, RUL-371), though the floor still advances
// monotonically.
func TestController_SecondVerifiedApplyIsIdempotent(t *testing.T) {
	store := memStore(t)
	spy := &spyHook{}
	c := NewController(store, NewRuntimeClock(), spy.fn)

	if _, _, err := c.ApplyVerifiedTime(rel133PersistedFloor); err != nil {
		t.Fatalf("first ApplyVerifiedTime: %v", err)
	}
	advanced, transitioned, err := c.ApplyVerifiedTime(rel133PersistedFloor + 1000)
	if err != nil {
		t.Fatalf("second ApplyVerifiedTime: %v", err)
	}
	if !advanced {
		t.Errorf("second apply floorAdvanced = false, want true (later time advances)")
	}
	if transitioned {
		t.Errorf("second apply transitioned = true, want false (already trusted)")
	}
	if len(spy.calls) != 1 {
		t.Errorf("engine hook called %d times, want exactly 1 (the transition fires once, RUL-371)", len(spy.calls))
	}
}

// TestController_VerifiedApplyNeverRegressesFloor: a verified time at or below
// the persisted floor leaves the floor untouched (REL-130 monotonic advance),
// while the clock is still trusted (verification succeeded).
func TestController_VerifiedApplyNeverRegressesFloor(t *testing.T) {
	store := memStore(t)
	c := NewController(store, NewRuntimeClock(), nil)

	if _, _, err := c.ApplyVerifiedTime(rel133PersistedFloor + 10_000); err != nil {
		t.Fatalf("ApplyVerifiedTime high: %v", err)
	}
	advanced, _, err := c.ApplyVerifiedTime(rel133PersistedFloor) // lower than the current floor
	if err != nil {
		t.Fatalf("ApplyVerifiedTime low: %v", err)
	}
	if advanced {
		t.Errorf("floorAdvanced = true for a lower verified time, want false (REL-130 never regress)")
	}
	if !c.Trusted() {
		t.Errorf("Trusted() = false after a non-advancing verified apply, want true")
	}
	if got, _, _ := store.ClockFloor(); got != rel133PersistedFloor+10_000 {
		t.Errorf("floor regressed to %d, want unchanged %d", got, rel133PersistedFloor+10_000)
	}
}

// TestController_HintStaysAdvisory (REL-132/133): a clock.hint adjusts the
// relay's RUNTIME clock only — it NEVER flips clock_state to trusted, NEVER
// fires the engine re-eval hook, and NEVER advances the persisted floor. Wired
// to the REL-133 corpus's two hints: the in-bound one is accepted, the
// over-bound one rejected, and neither disturbs trust or the floor.
func TestController_HintStaysAdvisory(t *testing.T) {
	store := memStore(t)
	if _, err := store.SetClockFloor(rel133PersistedFloor); err != nil {
		t.Fatalf("seed floor: %v", err)
	}
	spy := &spyHook{}
	c := NewController(store, NewRuntimeClock(), spy.fn)

	hint1 := ClockHint{Type: "clock.hint", RelayID: "01J8Z4K4N5P6Q7R8S9T0V1W3A1", Body: ClockHintBody{TS: rel133Hint1TS}}
	hint2 := ClockHint{Type: "clock.hint", RelayID: "01J8Z4K4N5P6Q7R8S9T0V1W3A1", Body: ClockHintBody{TS: rel133Hint2TS}}

	if accepted := c.AcceptHint(hint1, rel133CertNotAfter, rel133BoundedGrace, 0); !accepted {
		t.Errorf("in-bound hint1 rejected, want accepted (ts within not_after+grace, REL-133)")
	}
	if accepted := c.AcceptHint(hint2, rel133CertNotAfter, rel133BoundedGrace, 0); accepted {
		t.Errorf("over-bound hint2 accepted, want rejected (ts exceeds not_after+grace, REL-133)")
	}

	if c.State() != StateUntrusted || c.Trusted() {
		t.Errorf("a clock.hint flipped clock_state to %q (Trusted=%v); a hint is advisory and NEVER trusts the clock (REL-132)", c.State(), c.Trusted())
	}
	if len(spy.calls) != 0 {
		t.Errorf("a clock.hint fired the engine re-eval hook (%v); only a verified transition may (REL-132/134)", spy.calls)
	}
	if got, ok, _ := store.ClockFloor(); !ok || got != rel133PersistedFloor {
		t.Errorf("a clock.hint advanced the persisted floor to (%d,%v), want unchanged %d (REL-132)", got, ok, rel133PersistedFloor)
	}
	// The runtime clock DID move (the hint is advisory but not inert).
	if !c.Clock().Adjusted() {
		t.Errorf("in-bound hint did not adjust the runtime clock, want adjusted (REL-133)")
	}
}

// TestController_NilHookNoOp: a controller with no engine attached (nil hook)
// still tracks and transitions its clock-trust state without panicking — the
// clock-trust state is meaningful even before an engine is wired.
func TestController_NilHookNoOp(t *testing.T) {
	store := memStore(t)
	c := NewController(store, NewRuntimeClock(), nil)

	_, transitioned, err := c.ApplyVerifiedTime(rel133PersistedFloor)
	if err != nil {
		t.Fatalf("ApplyVerifiedTime with nil hook: %v", err)
	}
	if !transitioned || !c.Trusted() {
		t.Errorf("nil-hook transition did not flip to trusted (transitioned=%v, Trusted=%v)", transitioned, c.Trusted())
	}
}

// TestController_TrustDrivenOnlyByVerifiedTime (REL-132/135 clock independence):
// the ONLY path to a trusted clock is a verified-time application. Neither
// constructing the controller, nor accepting hints, nor validating a peer cert
// ever trusts the clock — so an untrusted/stale clock is a pure function of
// whether verified time has been applied, independent of certificate validity
// (the property that keeps expired-cert re-enrollment, REL-023/135, unblocked).
func TestController_TrustDrivenOnlyByVerifiedTime(t *testing.T) {
	store := memStore(t)
	c := NewController(store, NewRuntimeClock(), nil)

	// A valid, in-bound hint does not trust the clock.
	c.AcceptHint(ClockHint{Type: "clock.hint", Body: ClockHintBody{TS: rel133Hint1TS}}, rel133CertNotAfter, rel133BoundedGrace, 0)
	// A successful peer-cert validation does not trust the clock.
	leaf, spki, _ := makeLeaf(t, time.UnixMilli(rel136CertNotBeforeMs), time.UnixMilli(rel136CertNotAfterMs))
	_ = ValidatePeerCert(leaf.Raw, spki, rel136ColdBootMs, DefaultBoundedGraceMs, c.Trusted())

	if c.Trusted() {
		t.Fatalf("clock became trusted without a verified time — only ApplyVerifiedTime may trust the clock (REL-132)")
	}
}
