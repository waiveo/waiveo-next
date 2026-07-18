package clock

import "testing"

func TestFakeClockStartsAtZero(t *testing.T) {
	c := NewFakeClock()
	if got := c.Mono(); got != 0 {
		t.Fatalf("Mono() = %d, want 0", got)
	}
	if got := c.WallMillis(); got != 0 {
		t.Fatalf("WallMillis() = %d, want 0", got)
	}
}

func TestFakeClockAdvanceIsMonotonicAndCumulative(t *testing.T) {
	c := NewFakeClock()
	c.Advance(500)
	if got := c.Mono(); got != 500 {
		t.Fatalf("Mono() after Advance(500) = %d, want 500", got)
	}
	c.Advance(250)
	if got := c.Mono(); got != 750 {
		t.Fatalf("Mono() after second Advance(250) = %d, want 750", got)
	}
	// Advance(0) is a valid no-op (e.g. a for:0 hold treats 0 as absent).
	c.Advance(0)
	if got := c.Mono(); got != 750 {
		t.Fatalf("Mono() after Advance(0) = %d, want unchanged 750", got)
	}
}

func TestFakeClockAdvancePanicsOnNegative(t *testing.T) {
	c := NewFakeClock()
	defer func() {
		if recover() == nil {
			t.Fatal("Advance(-1) did not panic; monotonic time must never run backward")
		}
	}()
	c.Advance(-1)
}

func TestFakeClockWallIsIndependentOfMono(t *testing.T) {
	c := NewFakeClock()
	c.Advance(1_000)
	c.SetWall(1_700_000_000_000)
	if got := c.Mono(); got != 1_000 {
		t.Fatalf("Mono() = %d, want 1000 (unaffected by SetWall)", got)
	}
	if got := c.WallMillis(); got != 1_700_000_000_000 {
		t.Fatalf("WallMillis() = %d, want 1700000000000", got)
	}

	// SetWall again, independently, without touching Mono — models a
	// wall-clock jump (NTP step / DST) that must never perturb a monotonic
	// duration accounting (RUL-024/361).
	c.SetWall(500)
	if got := c.Mono(); got != 1_000 {
		t.Fatalf("Mono() = %d after unrelated SetWall, want unchanged 1000", got)
	}
	if got := c.WallMillis(); got != 500 {
		t.Fatalf("WallMillis() = %d, want 500", got)
	}
}

func TestFakeClockImplementsClock(t *testing.T) {
	var _ Clock = NewFakeClock()
}
