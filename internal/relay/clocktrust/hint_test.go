package clocktrust

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// The REL-133 corpus fixture, inlined field-for-field so the test asserts
// against the exact oracle shape
// (conformance/corpora/relay-1/REL-133-valid-clock-hint-bounded.json).
const (
	rel133PersistedFloor = int64(1752537000000)
	rel133CertNotAfter   = int64(1784073600000)
	rel133BoundedGrace   = int64(300000)
	rel133Hint1TS        = int64(1752537600000) // within not_after + grace -> accepted
	rel133Hint2TS        = int64(1784074200001) // exceeds not_after + grace -> rejected
)

// newTestClock builds a RuntimeClock whose monotonic source is fixed at
// nowMono so WallMillis is deterministic in tests.
func newTestClock(nowMono int64) *RuntimeClock {
	rc := NewRuntimeClock()
	rc.mono = func() int64 { return nowMono }
	return rc
}

func TestClockHintUnmarshalWireShape(t *testing.T) {
	raw := `{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1752537600000}}`
	var h ClockHint
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatalf("unmarshal clock.hint: %v", err)
	}
	if h.Type != "clock.hint" {
		t.Errorf("Type = %q, want clock.hint", h.Type)
	}
	if h.RelayID != "01J8Z4K4N5P6Q7R8S9T0V1W3A1" {
		t.Errorf("RelayID = %q", h.RelayID)
	}
	if h.Body.TS != rel133Hint1TS {
		t.Errorf("Body.TS = %d, want %d", h.Body.TS, rel133Hint1TS)
	}
}

// REL-133 corpus hint_1: a hint whose ts is within not_after + grace is
// accepted and adjusts the runtime clock only.
func TestAcceptHint_InBound_AdjustsRuntimeOnly(t *testing.T) {
	const nowMono = int64(5000)
	rc := newTestClock(nowMono)

	if rc.Adjusted() {
		t.Fatal("fresh RuntimeClock reports Adjusted() = true")
	}

	hint := ClockHint{Type: "clock.hint", RelayID: "01J8Z4K4N5P6Q7R8S9T0V1W3A1", Body: ClockHintBody{TS: rel133Hint1TS}}
	accepted := rc.AcceptHint(hint, rel133CertNotAfter, rel133BoundedGrace, nowMono)
	if !accepted {
		t.Fatalf("in-bound hint (ts=%d, not_after=%d, grace=%d) rejected, want accepted", rel133Hint1TS, rel133CertNotAfter, rel133BoundedGrace)
	}
	if !rc.Adjusted() {
		t.Error("after accepting hint, Adjusted() = false")
	}
	// With the monotonic source unchanged, wall time equals the hint ts.
	if got := rc.WallMillis(); got != rel133Hint1TS {
		t.Errorf("WallMillis() = %d, want %d (offset from monotonic to hinted wall time)", got, rel133Hint1TS)
	}
}

// The runtime clock advances with monotonic time after a hint: WallMillis is
// an offset from the monotonic reading, not a frozen snapshot.
func TestAcceptHint_WallTracksMonotonic(t *testing.T) {
	const nowMono = int64(5000)
	rc := newTestClock(nowMono)
	hint := ClockHint{Type: "clock.hint", Body: ClockHintBody{TS: rel133Hint1TS}}
	if !rc.AcceptHint(hint, rel133CertNotAfter, rel133BoundedGrace, nowMono) {
		t.Fatal("in-bound hint rejected")
	}
	// Monotonic advances by 10s; wall time must advance by the same delta.
	rc.mono = func() int64 { return nowMono + 10000 }
	if got, want := rc.WallMillis(), rel133Hint1TS+10000; got != want {
		t.Errorf("WallMillis() after +10s monotonic = %d, want %d", got, want)
	}
}

// REL-133 corpus hint_2: a hint whose ts exceeds not_after + grace is
// rejected outright and does NOT adjust the runtime clock.
func TestAcceptHint_OverBound_Rejected(t *testing.T) {
	const nowMono = int64(5000)
	rc := newTestClock(nowMono)

	over := ClockHint{Type: "clock.hint", Body: ClockHintBody{TS: rel133Hint2TS}}
	if rc.AcceptHint(over, rel133CertNotAfter, rel133BoundedGrace, nowMono) {
		t.Fatalf("over-bound hint (ts=%d > not_after=%d + grace=%d) accepted, want rejected", rel133Hint2TS, rel133CertNotAfter, rel133BoundedGrace)
	}
	if rc.Adjusted() {
		t.Error("rejected hint adjusted the runtime clock")
	}
}

// The bound is inclusive: ts == not_after + grace is accepted; one past is
// rejected.
func TestAcceptHint_BoundaryInclusive(t *testing.T) {
	const nowMono = int64(5000)
	edge := rel133CertNotAfter + rel133BoundedGrace

	rcEdge := newTestClock(nowMono)
	if !rcEdge.AcceptHint(ClockHint{Body: ClockHintBody{TS: edge}}, rel133CertNotAfter, rel133BoundedGrace, nowMono) {
		t.Errorf("hint at exactly not_after+grace (%d) rejected, want accepted", edge)
	}

	rcPast := newTestClock(nowMono)
	if rcPast.AcceptHint(ClockHint{Body: ClockHintBody{TS: edge + 1}}, rel133CertNotAfter, rel133BoundedGrace, nowMono) {
		t.Errorf("hint one past not_after+grace (%d) accepted, want rejected", edge+1)
	}
}

// A rejected hint that arrives AFTER an accepted one must not perturb the
// already-adjusted runtime clock.
func TestAcceptHint_RejectAfterAccept_LeavesClockUntouched(t *testing.T) {
	const nowMono = int64(5000)
	rc := newTestClock(nowMono)

	if !rc.AcceptHint(ClockHint{Body: ClockHintBody{TS: rel133Hint1TS}}, rel133CertNotAfter, rel133BoundedGrace, nowMono) {
		t.Fatal("in-bound hint rejected")
	}
	wallAfterAccept := rc.WallMillis()

	if rc.AcceptHint(ClockHint{Body: ClockHintBody{TS: rel133Hint2TS}}, rel133CertNotAfter, rel133BoundedGrace, nowMono) {
		t.Fatal("over-bound hint accepted")
	}
	if got := rc.WallMillis(); got != wallAfterAccept {
		t.Errorf("WallMillis() changed after rejected hint: got %d, want %d", got, wallAfterAccept)
	}
}

// REL-132/133: driving both corpus hints through the runtime clock must NEVER
// advance the relay's persisted clock floor. The RuntimeClock has no access to
// the identity store at all — this test proves the floor the store persists is
// exactly the corpus's persisted_floor_after_both_hints.
func TestAcceptHint_NeverAdvancesPersistedFloor(t *testing.T) {
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open identity store: %v", err)
	}
	defer store.Close()

	if _, err := store.SetClockFloor(rel133PersistedFloor); err != nil {
		t.Fatalf("seed clock floor: %v", err)
	}

	const nowMono = int64(5000)
	rc := newTestClock(nowMono)
	rc.AcceptHint(ClockHint{Body: ClockHintBody{TS: rel133Hint1TS}}, rel133CertNotAfter, rel133BoundedGrace, nowMono)
	rc.AcceptHint(ClockHint{Body: ClockHintBody{TS: rel133Hint2TS}}, rel133CertNotAfter, rel133BoundedGrace, nowMono)

	floor, ok, err := store.ClockFloor()
	if err != nil || !ok {
		t.Fatalf("read clock floor: ok=%v err=%v", ok, err)
	}
	if floor != rel133PersistedFloor {
		t.Errorf("persisted floor after both hints = %d, want %d (a hint never advances the floor)", floor, rel133PersistedFloor)
	}
}
