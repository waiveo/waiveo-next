package derive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// runner_test.go is where the PORTED GUARDS are proved. Every one of them exists
// because legacy hit the failure in production, and every one of them fails
// SILENTLY — as a hang, a starved queue or an unbounded retry, never as an
// error — so a test that only checks the happy path would pass with all three
// removed.
//
// None of these needs a browser. That is the point of Runner owning the guards
// rather than the Renderer: they are provable in CI on any machine.

// fakeRenderer is a Renderer under the test's control.
type fakeRenderer struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	calls    int32

	// block, when non-nil, is waited on before returning — used to hold jobs
	// in flight while concurrency is observed, and to simulate a hang.
	block chan struct{}
	// err, when non-nil, is returned instead of bytes.
	err error
	// out is the payload a successful render returns.
	out []byte
}

func (f *fakeRenderer) Render(ctx context.Context, _ Page) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxSeen {
		f.maxSeen = f.inFlight
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	out := f.out
	if out == nil {
		out = []byte("PNG")
	}
	return out, nil
}

func rectJob(key string) Job {
	return Job{Key: key, W: 200, H: 100, Spec: &wire.DeriveSpec{
		Kind: wire.DeriveKindRect,
		Fill: &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
	}}
}

// TestConcurrencyIsClamped is legacy's pool=2.
//
// Each concurrent job here is a whole Chromium, so an unbounded runner does not
// get slower under load — it takes the render host down, and every job then
// times out at once, which reads as "the renderer is broken" rather than as "we
// asked for too much at once". The clamp is the difference.
func TestConcurrencyIsClamped(t *testing.T) {
	const clamp = 2
	f := &fakeRenderer{block: make(chan struct{})}
	r := NewRunner(f, RunnerOptions{Concurrency: clamp, JobTimeout: 5 * time.Second})

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = r.Render(context.Background(), rectJob(fmt.Sprintf("job-%d", i)))
		}(i)
	}
	// Let the goroutines pile up against the semaphore before releasing them.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		seen := f.maxSeen
		f.mu.Unlock()
		if seen >= clamp {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(f.block)
	wg.Wait()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxSeen > clamp {
		t.Errorf("%d renders ran at once with the clamp set to %d", f.maxSeen, clamp)
	}
	if f.maxSeen < clamp {
		t.Errorf("only %d render(s) ever ran at once — the clamp is not a clamp, it is a serializer", f.maxSeen)
	}
	if got := atomic.LoadInt32(&f.calls); got != 12 {
		t.Errorf("%d of 12 jobs reached the renderer", got)
	}
	if r.Concurrency() != clamp {
		t.Errorf("Concurrency() = %d, want %d", r.Concurrency(), clamp)
	}
}

// TestAHungRenderIsBoundedByTheJobTimeout is the guard against the failure that
// has no error: a headless page that never commits a frame, where the capture
// blocks for as long as it is allowed to. Without the deadline the job never
// returns, its slot is never freed, and everything behind it stops.
func TestAHungRenderIsBoundedByTheJobTimeout(t *testing.T) {
	f := &fakeRenderer{block: make(chan struct{})}
	defer close(f.block)
	r := NewRunner(f, RunnerOptions{Concurrency: 1, JobTimeout: 80 * time.Millisecond})

	start := time.Now()
	_, err := r.Render(context.Background(), rectJob("hung"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a render that never returns was reported as a success")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline — the caller must be able to tell a hang from a failure", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("the render took %s to give up on an 80ms budget", elapsed)
	}
}

// fakeClock is a manually advanced clock, so the breaker's backoff schedule is
// testable in microseconds. A guard whose only test is "wait ten minutes" is a
// guard nobody ever runs.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestTheCircuitBreakerStopsHammeringABrokenLayer is legacy's per-element
// breaker.
//
// Without it, one layer that cannot render is retried on every pass forever. On
// legacy that meant it also held a pool slot each time, so a single broken
// element degraded every OTHER element's refresh rate — the failure is not the
// broken layer, it is what the broken layer does to the working ones.
//
// The escalation is asserted, not just the first refusal: a breaker that opened
// for a fixed interval would be a rate limiter, and the whole point of doubling
// is that a permanently broken layer costs asymptotically nothing.
func TestTheCircuitBreakerStopsHammeringABrokenLayer(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := &fakeRenderer{err: errors.New("boom")}
	r := NewRunner(f, RunnerOptions{Concurrency: 2, JobTimeout: time.Second, Now: clk.now})
	ctx := context.Background()
	job := rectJob("broken")

	// First attempt: reaches the renderer and fails.
	if _, err := r.Render(ctx, job); err == nil {
		t.Fatal("a failing render was reported as a success")
	}
	if got := atomic.LoadInt32(&f.calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}

	// Immediately after: the circuit is open, and nothing is attempted.
	_, err := r.Render(ctx, job)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if got := atomic.LoadInt32(&f.calls); got != 1 {
		t.Fatalf("the renderer was called %d times while the circuit was open", got)
	}

	// 59s in, still open. 60s in, one more attempt is allowed.
	clk.advance(59 * time.Second)
	if _, err := r.Render(ctx, job); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("the circuit reopened early: %v", err)
	}
	clk.advance(time.Second)
	if _, err := r.Render(ctx, job); errors.Is(err, ErrCircuitOpen) {
		t.Fatal("the circuit never reopened — a transient failure must not be permanent")
	}
	if got := atomic.LoadInt32(&f.calls); got != 2 {
		t.Fatalf("calls = %d, want 2 after the cooldown lapsed", got)
	}

	// Second consecutive failure DOUBLES the cooldown: 60s is no longer enough.
	clk.advance(90 * time.Second)
	if _, err := r.Render(ctx, job); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("the second failure did not escalate the backoff: %v", err)
	}
	clk.advance(31 * time.Second) // now past 120s
	if _, err := r.Render(ctx, job); errors.Is(err, ErrCircuitOpen) {
		t.Fatal("the circuit stayed open past its own doubled cooldown")
	}
}

// TestBackoffSchedule pins the numbers themselves — 60s doubling to a 10m
// ceiling — because an off-by-one in the shift is invisible in behaviour tests
// and turns "back off politely" into "never retry".
func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for failures, w := range want {
		if got := backoffFor(failures); got != w {
			t.Errorf("backoffFor(%d) = %s, want %s", failures, got, w)
		}
	}
	// A pathological failure count must not shift into a negative duration.
	if got := backoffFor(1 << 20); got != breakerCeiling {
		t.Errorf("backoffFor(huge) = %s, want the %s ceiling", got, breakerCeiling)
	}
}

// TestASuccessClearsTheBreaker: a layer that renders is not suspect. Leaving a
// decayed failure count behind would make the SECOND failure of a long-healthy
// layer back off as though it were the fifth, which is how a transient blip
// turns into a layer that is missing from a wall for ten minutes.
func TestASuccessClearsTheBreaker(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := &fakeRenderer{err: errors.New("boom")}
	r := NewRunner(f, RunnerOptions{Concurrency: 1, JobTimeout: time.Second, Now: clk.now})
	ctx := context.Background()
	job := rectJob("flaky")

	if _, err := r.Render(ctx, job); err == nil {
		t.Fatal("expected the first render to fail")
	}
	clk.advance(61 * time.Second)
	f.err = nil
	if _, err := r.Render(ctx, job); err != nil {
		t.Fatalf("the recovered render failed: %v", err)
	}

	// A fresh failure must be treated as the FIRST one: 61s of cooldown, not 121s.
	f.err = errors.New("boom again")
	if _, err := r.Render(ctx, job); err == nil {
		t.Fatal("expected the render to fail again")
	}
	clk.advance(61 * time.Second)
	if _, err := r.Render(ctx, job); errors.Is(err, ErrCircuitOpen) {
		t.Fatal("the breaker escalated across a success — the failure count was never cleared")
	}
}

// TestAnUnrenderableSpecOpensTheBreakerWithoutTouchingTheBrowser: a spec that
// cannot even be turned into a page fails identically every pass until the spec
// changes, so it is exactly what the breaker is for — and it must not consume a
// render slot on the way to failing.
func TestAnUnrenderableSpecOpensTheBreakerWithoutTouchingTheBrowser(t *testing.T) {
	f := &fakeRenderer{}
	r := NewRunner(f, RunnerOptions{Concurrency: 1, JobTimeout: time.Second})
	bad := Job{Key: "bad", W: 10, H: 10, Spec: &wire.DeriveSpec{
		Kind:   wire.DeriveKindRect,
		Fill:   &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
		Shadow: &wire.DeriveShadow{Blur: 200},
	}}
	if _, err := r.Render(context.Background(), bad); err == nil {
		t.Fatal("an unbuildable page was reported as a success")
	}
	if got := atomic.LoadInt32(&f.calls); got != 0 {
		t.Errorf("the renderer was invoked %d time(s) for a page that could not be built", got)
	}
	if _, err := r.Render(context.Background(), bad); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("a deterministic build failure did not open the breaker: %v", err)
	}
}

// TestAnEmptyRenderIsAFailure: a Renderer that returns no bytes and no error is
// lying, and believing it uploads zero bytes, mints an asset_ref for nothing,
// and hangs an invisible rectangle on a wall — a defect with no error anywhere.
func TestAnEmptyRenderIsAFailure(t *testing.T) {
	f := &fakeRenderer{out: []byte{}}
	r := NewRunner(f, RunnerOptions{Concurrency: 1, JobTimeout: time.Second})
	if _, err := r.Render(context.Background(), rectJob("empty")); err == nil {
		t.Fatal("an empty render was accepted")
	}
}

// TestQueueCancellationIsNotACircuitFailure closes the mirror direction of the
// breaker, which is the direction this codebase keeps getting wrong: the guard
// is written and then it fires on the wrong thing. A job cancelled while WAITING
// for a slot was never attempted, so counting it as a failure would let a slow
// queue — or one interrupted with ctrl-C — open the breaker on layers that are
// perfectly fine, and they would then be skipped for a minute on the next run.
func TestQueueCancellationIsNotACircuitFailure(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := &fakeRenderer{block: make(chan struct{})}
	r := NewRunner(f, RunnerOptions{Concurrency: 1, JobTimeout: time.Minute, Now: clk.now})

	// Occupy the single slot.
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = r.Render(context.Background(), rectJob("occupier"))
	}()
	<-started
	for i := 0; i < 200; i++ {
		f.mu.Lock()
		busy := f.inFlight
		f.mu.Unlock()
		if busy > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Render(ctx, rectJob("queued")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	close(f.block)

	if wait, open := r.circuitOpen("queued"); open {
		t.Errorf("a job cancelled while queued opened its circuit for %s — it was never attempted", wait)
	}
}
