package derive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// runner.go holds the three guards that are NOT about a browser: the bounded
// concurrency clamp, the per-job wall-clock timeout, and the per-element circuit
// breaker. All three were paid for on legacy's production render pool, and all
// three are transplanted here deliberately independent of Chromium — a Runner
// drives any Renderer, which is what lets every one of them be tested without a
// browser installed.

// Renderer turns a built page into PNG bytes. Chromium is one implementation
// (browser.go); tests supply their own.
//
// A Renderer is NOT responsible for the timeout, the concurrency limit or the
// backoff — Runner owns those, so a second Renderer cannot ship without them.
// It IS responsible for honouring ctx cancellation, because that is the only
// signal it gets when Runner's clock runs out.
type Renderer interface {
	Render(ctx context.Context, page Page) ([]byte, error)
}

// ErrCircuitOpen is returned for a job whose key is in backoff. It is a distinct
// error because it is NOT a render failure: nothing was attempted, and a caller
// that reports it as one would tell an operator their layer is broken when the
// truth is "we are waiting before trying again".
var ErrCircuitOpen = errors.New("derive: render circuit open for this layer")

// Guard defaults. Each is overridable per Runner; the values are legacy's,
// carried across because they were chosen against observed behaviour and not by
// taste.
const (
	// DefaultConcurrency is TWO. Legacy ran a pool of two on Pi-class hardware
	// and the number stuck for a reason that survives the move off the
	// appliance: each concurrent job is a live Chromium page, and the failure
	// mode of over-subscribing is not slowness but a render host that swaps
	// itself to death while every job times out at once.
	DefaultConcurrency = 2
	// DefaultJobTimeout is the wall-clock ceiling on ONE render. Legacy's
	// per-job bridge timeout was 60s.
	DefaultJobTimeout = 60 * time.Second
	// breakerBase and breakerCeiling are the exponential backoff: 60s doubling
	// per consecutive failure, capped at 10 minutes. Legacy's numbers exactly
	// (SlideImageRenderer's _elementBreaker).
	breakerBase     = 60 * time.Second
	breakerCeiling  = 10 * time.Minute
	breakerMaxShift = 16 // guards the shift itself against an absurd failure count
)

// breakerEntry is one key's consecutive-failure state.
type breakerEntry struct {
	count    int
	failedAt time.Time
}

// Runner executes derive jobs under the ported guards.
//
// The zero value is not usable; construct with NewRunner.
type Runner struct {
	renderer Renderer
	sem      chan struct{}
	timeout  time.Duration
	now      func() time.Time

	mu      sync.Mutex
	breaker map[string]*breakerEntry
}

// RunnerOptions configures a Runner. Every field is optional; the zero value
// selects the ported default.
type RunnerOptions struct {
	Concurrency int
	JobTimeout  time.Duration
	// Now is the clock the circuit breaker measures backoff against. Injected so
	// the backoff schedule is testable without sleeping — a guard whose only
	// test is "wait ten minutes" is a guard nobody ever tests.
	Now func() time.Time
}

// NewRunner builds a Runner around a Renderer.
func NewRunner(r Renderer, opt RunnerOptions) *Runner {
	c := opt.Concurrency
	if c <= 0 {
		c = DefaultConcurrency
	}
	t := opt.JobTimeout
	if t <= 0 {
		t = DefaultJobTimeout
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{
		renderer: r,
		sem:      make(chan struct{}, c),
		timeout:  t,
		now:      now,
		breaker:  map[string]*breakerEntry{},
	}
}

// Concurrency reports the clamp this Runner enforces, so a caller can log the
// effective value rather than the value it asked for.
func (r *Runner) Concurrency() int { return cap(r.sem) }

// Render executes one job: the circuit breaker first, then the concurrency
// clamp, then the render under a per-job deadline.
//
// The ORDER is the design. Checking the breaker before taking a semaphore slot
// is what stops a permanently-failing layer from occupying render capacity it
// will only waste; legacy learned that the other way round, with a wedged
// element holding a pool slot until the outer timeout freed it, over and over.
func (r *Runner) Render(ctx context.Context, j Job) ([]byte, error) {
	if wait, open := r.circuitOpen(j.Key); open {
		return nil, fmt.Errorf("%w: %s (retry in %s)", ErrCircuitOpen, j.Key, wait.Round(time.Second))
	}

	page, err := BuildPage(j)
	if err != nil {
		// A spec that cannot even be turned into a page is a DETERMINISTIC
		// failure: it will fail identically every pass until the spec changes,
		// which is precisely what the breaker is for.
		r.recordFailure(j.Key)
		return nil, err
	}

	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		// Waiting for a slot is not a render attempt, so it must NOT count
		// against the layer's failure budget: a slow queue would otherwise open
		// the breaker on layers that were never tried.
		return nil, ctx.Err()
	}
	defer func() { <-r.sem }()

	jobCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	png, err := r.renderer.Render(jobCtx, page)
	if err != nil {
		r.recordFailure(j.Key)
		return nil, fmt.Errorf("derive: render %s: %w", j.Key, err)
	}
	if len(png) == 0 {
		// An empty buffer with no error is a renderer that lied. Treating it as
		// success would upload zero bytes, mint an asset_ref for nothing, and
		// put an empty PNG on a wall.
		r.recordFailure(j.Key)
		return nil, fmt.Errorf("derive: render %s: renderer returned no bytes", j.Key)
	}

	r.recordSuccess(j.Key)
	return png, nil
}

// circuitOpen reports whether key is in backoff, and how long is left.
func (r *Runner) circuitOpen(key string) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.breaker[key]
	if !ok {
		return 0, false
	}
	cool := backoffFor(e.count)
	elapsed := r.now().Sub(e.failedAt)
	if elapsed >= cool {
		return 0, false
	}
	return cool - elapsed, true
}

// backoffFor is the exponential schedule: 60s, 2m, 4m, 8m, then the 10m ceiling.
func backoffFor(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	shift := failures - 1
	if shift > breakerMaxShift {
		shift = breakerMaxShift
	}
	d := breakerBase << uint(shift)
	if d > breakerCeiling || d <= 0 {
		return breakerCeiling
	}
	return d
}

func (r *Runner) recordFailure(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.breaker[key]
	if !ok {
		e = &breakerEntry{}
		r.breaker[key] = e
	}
	e.count++
	e.failedAt = r.now()
}

// recordSuccess clears the key. A layer that renders once is not suspect, and
// leaving a decayed count behind would make the SECOND failure of a
// long-healthy layer back off as if it were the fifth.
func (r *Runner) recordSuccess(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.breaker, key)
}
