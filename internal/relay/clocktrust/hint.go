// Package clocktrust implements the relay/1 connection-layer clock-trust
// exchange (contracts/relay-1.md REL-132–137): a relay accepts a bounded
// `clock.hint` from its app peer that adjusts its runtime clock but never
// advances its verified floor (REL-132/133), completes a mutually
// authenticated connection even when its own clock is untrusted via
// skew-tolerant/deferred temporal validation pinned to the enrollment-anchored
// app-peer key (REL-136/137), and drives the untrusted->trusted transition
// that re-evaluates time-based rules (REL-134).
//
// This file is the `clock.hint` receiver (REL-132/133). A hint carries no
// verifiable signature, so it adjusts the relay's RUNTIME clock ONLY — never
// the persisted floor (internal/relay/identity). The RuntimeClock here has no
// access to the identity store by construction, which is what structurally
// guarantees REL-132's "a hint never advances the floor".
package clocktrust

import (
	"sync"
	"time"
)

// ClockHint is the app peer's `clock.hint {ts}` message (REL-133), which it
// MAY send at any time on an established connection. json struct tags match
// contracts/relay-1.md's wire shapes and the
// REL-133-valid-clock-hint-bounded.json corpus fixture field-for-field.
type ClockHint struct {
	Type    string        `json:"type"`
	RelayID string        `json:"relay_id"`
	Body    ClockHintBody `json:"body"`
}

// ClockHintBody carries the hinted wall time in Unix milliseconds.
type ClockHintBody struct {
	TS int64 `json:"ts"`
}

// RuntimeClock is the relay's hint-adjustable runtime wall clock. It holds an
// offset from a monotonic reading to a hinted wall time: once a hint is
// accepted, WallMillis returns mono()+offset, so the runtime clock tracks the
// monotonic clock forward from the hinted instant. It is deliberately
// decoupled from the persisted clock floor (REL-132): adjusting the runtime
// clock never touches the floor.
type RuntimeClock struct {
	mono func() int64 // monotonic milliseconds source (injectable for tests)

	mu       sync.Mutex
	offsetMs int64
	adjusted bool
}

// NewRuntimeClock returns a RuntimeClock backed by a real monotonic source
// (uptime milliseconds, independent of any wall-clock reading). Before any
// hint is accepted the clock is unadjusted.
func NewRuntimeClock() *RuntimeClock {
	base := time.Now()
	return &RuntimeClock{
		mono: func() int64 { return int64(time.Since(base) / time.Millisecond) },
	}
}

// AcceptHint applies hint to the runtime clock if — and only if — its ts is
// within the relay's own certificate not_after plus a bounded grace period
// (REL-133): an app peer MUST NOT be able to use a hint alone to make the
// relay believe its own credential has not yet expired. The bound is
// inclusive of not_after+grace. On accept, the runtime clock's offset is set
// so WallMillis reports the hinted wall time; the persisted floor is never
// touched (REL-132). nowMonoMs is the monotonic reading the hint is anchored
// against. Returns whether the hint was accepted.
func (c *RuntimeClock) AcceptHint(hint ClockHint, certNotAfterMs, boundedGraceMs, nowMonoMs int64) (accepted bool) {
	if hint.Body.TS > certNotAfterMs+boundedGraceMs {
		return false
	}
	c.mu.Lock()
	c.offsetMs = hint.Body.TS - nowMonoMs
	c.adjusted = true
	c.mu.Unlock()
	return true
}

// WallMillis returns the current hint-adjusted runtime wall time in Unix
// milliseconds: the monotonic reading plus the offset set by the most recent
// accepted hint. Before any hint is accepted the offset is zero, so it reports
// the bare monotonic reading (an unanchored value the relay treats as
// untrusted).
func (c *RuntimeClock) WallMillis() int64 {
	c.mu.Lock()
	offset := c.offsetMs
	c.mu.Unlock()
	return c.mono() + offset
}

// Adjusted reports whether an accepted hint has ever set the runtime clock's
// offset. It never becomes true from a rejected (over-bound) hint.
func (c *RuntimeClock) Adjusted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.adjusted
}
