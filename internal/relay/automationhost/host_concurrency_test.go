package automationhost

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

// TestApplyEdgeRulesRacesClockTrust is the regression guard for the live-loop
// concurrency hazard: the running binary drives ApplyEdgeRules from the
// background desired-state re-pull loop (cmd/waiveo-relay/livepull.go) while the
// connection-layer clock.hint HTTP handler drives SetClockTrust from a separate
// per-request goroutine (REL-133 -> the clocktrust.Controller callback). Both
// advance the SAME underlying engine.Engine, which has no internal locking:
// ApplyEdgeRules -> engine.ApplyGeneration replaces e.rules/e.gen while the
// untrusted->trusted transition in SetClockTrust reads e.rules (replayMissedSchedules)
// and mutates e.lastTickWall. Without Host serializing these on its own lock the
// two goroutines race on the same unsynchronized fields — undefined behavior that
// can panic or silently corrupt automation state (wrong device commands firing).
//
// The two loops below hammer both entry points concurrently; the assertion is the
// race detector itself (`go test -race`). Before Host guarded its engine access
// with a mutex, `-race` flagged a data race on e.rules here.
func TestApplyEdgeRulesRacesClockTrust(t *testing.T) {
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), &recordController{})

	rules := []json.RawMessage{json.RawMessage(demoEdgeRule)}
	// Prime an initial generation so the re-pull loop below applies strictly
	// higher ones (REL-052/070) and the engine already carries a rule for the
	// clock-trust replay to walk.
	if err := host.ApplyEdgeRules(rules, 1); err != nil {
		t.Fatalf("ApplyEdgeRules (seed): %v", err)
	}

	const iterations = 400

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine A: the re-pull loop applying an ever-higher generation, exactly as
	// rePuller.tick does on every content change (a new gen each time so
	// ApplyGeneration actually runs and replaces e.rules, not the idempotent no-op).
	go func() {
		defer wg.Done()
		for gen := 2; gen < 2+iterations; gen++ {
			if err := host.ApplyEdgeRules(rules, gen); err != nil {
				t.Errorf("ApplyEdgeRules(gen=%d): %v", gen, err)
				return
			}
		}
	}()

	// Goroutine B: the clock.hint handler toggling trust. Each untrusted->trusted
	// transition drives the engine's missed-schedule replay, which reads e.rules
	// concurrently with goroutine A's ApplyGeneration write.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := host.SetClockTrust("untrusted"); err != nil {
				t.Errorf("SetClockTrust(untrusted): %v", err)
				return
			}
			if _, err := host.SetClockTrust("trusted"); err != nil {
				t.Errorf("SetClockTrust(trusted): %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
