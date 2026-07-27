package heartbeat

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakePinger counts pings and fails from failAfter onward (0 = never fail).
type fakePinger struct {
	count     atomic.Int64
	failAfter int64
	blockCtx  bool // block until the ping ctx expires instead of failing fast
}

func (p *fakePinger) Ping(ctx context.Context) error {
	n := p.count.Add(1)
	if p.failAfter > 0 && n >= p.failAfter {
		if p.blockCtx {
			<-ctx.Done() // an unanswered ping: the pong never arrives
			return ctx.Err()
		}
		return errors.New("peer gone")
	}
	return nil
}

// TestRunKeepsPingingWhileHealthy: a responsive peer is pinged repeatedly
// and Run returns nil (no error) once the caller shuts the loop down.
func TestRunKeepsPingingWhileHealthy(t *testing.T) {
	p := &fakePinger{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- Run(ctx, p, 5*time.Millisecond, 50*time.Millisecond) }()

	deadline := time.Now().Add(2 * time.Second)
	for p.count.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := p.count.Load(); got < 3 {
		t.Fatalf("pings after 2s = %d, want >= 3", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after orderly cancel = %v, want nil", err)
	}
}

// TestRunReportsFailedPing: a ping that errors ends the loop with that
// error — the caller's teardown signal.
func TestRunReportsFailedPing(t *testing.T) {
	p := &fakePinger{failAfter: 2}
	err := Run(context.Background(), p, time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Run = nil after a failed ping, want the ping error")
	}
}

// TestRunReportsUnansweredPing: a ping whose pong never arrives (the
// half-open-connection case) ends the loop with the round-trip-timeout
// error rather than hanging forever.
func TestRunReportsUnansweredPing(t *testing.T) {
	p := &fakePinger{failAfter: 1, blockCtx: true}
	start := time.Now()
	err := Run(context.Background(), p, time.Millisecond, 30*time.Millisecond)
	if err == nil {
		t.Fatal("Run = nil for an unanswered ping, want the timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run took %v to notice an unanswered ping; the round-trip bound did not apply", elapsed)
	}
}
