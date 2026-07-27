package webhookdeliver

// loop.go is the Deliverer's scheduler: the piece that decides WHEN a pass is
// taken, so a registered endpoint actually receives events instead of waiting
// for a caller that never comes. Without it the registration surface accepts an
// endpoint, seals its signing secret, and delivers nothing — a surface that
// promises work no code performs.
//
// # Why a wake channel and not only a ticker
//
// Delivery latency should not be a poll interval. Every event this platform
// records lands through ONE seam — the live-transport hub's Append — so a
// deployment can hand this loop a wake on that seam (Loop.Notify) and a fresh
// event moves to a receiver as soon as it is durable rather than up to an
// interval later. The ticker stays because a wake cannot cover everything a
// pass is responsible for:
//
//   - A failed delivery's retry deadline (EVT-153) is an instant in the future
//     with no event behind it. Nothing appends when it arrives.
//   - A deleted endpoint's sealed secret is pruned by a pass (pruneOrphans),
//     and a delete is a store write, not an event append.
//
// So the loop is woken by both, and the ticker interval is chosen against the
// retry deadline rather than against delivery latency — which the wake owns.
//
// # Why a delivered event wakes the loop again
//
// Tick makes at most ONE attempt per endpoint per pass, deliberately: that is
// what stops a slow endpoint from monopolizing a pass. But an endpoint that has
// just been given its first signing secret owes the WHOLE retained log
// (EndpointState.PendingAfter with an empty cursor), so on a ticker alone a
// backlog would drain at one event per interval — hours or days for a log a
// busy box accumulated, while every read of the endpoint's delivery state
// looked healthy. The loop therefore re-arms its own wake whenever a pass
// actually delivered something, so a backlog drains at the receiver's pace and
// stops the moment a pass delivers nothing. It cannot spin: the wake is re-armed
// only by an observed 2xx, never by an empty pass.

import (
	"context"
	"sync"
	"time"
)

// DefaultInterval is how often a Loop takes a pass with no wake to prompt it.
//
// It is sized against EVT-153's retry deadline, NOT against delivery latency —
// a fresh event's latency belongs to the wake (Loop.Notify), and a deployment
// that wires one never waits an interval for a first delivery. What the ticker
// alone governs is how late a RETRY can be: a backoff deadline is checked at the
// top of a pass, so the wait an endpoint actually experiences is its persisted
// deadline plus up to one interval. Against events.DefaultBackoffBaseMs (30s)
// an interval equal to the base would make the first retry land anywhere in
// [30s, 60s] — doubling the contract's own proposed spacing as a side effect of
// how often this loop happens to look. Five seconds keeps that overshoot under
// a sixth of the base while costing an idle deployment three indexed reads and
// zero HTTP per pass.
const DefaultInterval = 5 * time.Second

// Loop drives a Deliverer on a schedule and owns its lifecycle: when passes
// begin (Start), what prompts one (the ticker and Notify), and how they end
// (Shutdown).
//
// A Loop is created STOPPED, the same posture api.JobRunner takes, so a process
// wires its collaborators — and a test arranges the world a pass will run
// against — before any delivery goes out.
//
// The zero value is not usable; use NewLoop.
type Loop struct {
	deliverer *Deliverer
	interval  time.Duration
	onErr     func(error)

	// wake is the pass prompt, buffered(1) and coalescing: a wake that arrives
	// while one is already pending is dropped, because a pass reads the whole
	// owed backlog rather than one queued item. It is never closed, so Notify
	// is safe to call from a hub hook at any time, including after Shutdown.
	wake chan struct{}

	// stop tells the run goroutine to take no further pass. Distinct from
	// cancel: closing stop ends SCHEDULING, cancelling ends the pass already in
	// flight. Shutdown does the first immediately and the second only if it runs
	// out of time.
	stop chan struct{}
	done chan struct{}

	// ctx is the context every pass runs under. It is deliberately not any
	// request's context — a delivery is not made on behalf of a caller who is
	// waiting for it.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	started  bool
	stopped  bool
	stopOnce sync.Once
}

// NewLoop validates cfg exactly as New does, builds the Deliverer, and returns
// a stopped Loop over it. interval <= 0 takes DefaultInterval.
//
// cfg.OnDelivered is preserved and additionally re-arms the loop's own wake, so
// a caller's hook still fires and a backlog still drains (see the file's own
// doc). Everything else in cfg is passed through untouched.
func NewLoop(cfg Config, interval time.Duration) (*Loop, error) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	l := &Loop{
		interval: interval,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	l.ctx, l.cancel = context.WithCancel(context.Background())

	caller := cfg.OnDelivered
	cfg.OnDelivered = func(endpointID, eventID string) {
		if caller != nil {
			caller(endpointID, eventID)
		}
		l.Notify()
	}

	d, err := New(cfg)
	if err != nil {
		l.cancel()
		return nil, err
	}
	l.deliverer = d
	l.onErr = func(err error) {
		if cfg.OnError != nil {
			cfg.OnError("", err)
		}
	}
	return l, nil
}

// Notify asks for a pass as soon as one can be taken. It never blocks and never
// panics — a deployment registers it as the event hub's append hook, where a
// blocking call would hold the log's write lock and a panic would take down the
// producer.
//
// Calling it before Start is meaningful: the wake is retained, and the loop
// takes its pass as soon as it starts. Calling it after Shutdown is inert.
func (l *Loop) Notify() {
	select {
	case l.wake <- struct{}{}:
	default: // a pass is already pending — coalesced
	}
}

// Start begins taking passes, starting with one immediately: a process that was
// down while events accumulated owes them now, not one interval from now.
// Starting an already-started or already-shut-down Loop is a no-op.
func (l *Loop) Start() {
	l.mu.Lock()
	if l.started || l.stopped {
		l.mu.Unlock()
		return
	}
	l.started = true
	l.mu.Unlock()
	go l.run()
}

// Shutdown stops scheduling and waits for the pass already in flight, returning
// nil once it has finished.
//
// The in-flight pass is NOT cancelled on entry. It is allowed to finish, so a
// delivery the receiver has already accepted gets its cursor committed rather
// than being redelivered after the restart — a graceful stop should not behave
// like a crash when it does not have to.
//
// If ctx expires first the pass IS cancelled: its in-flight HTTP attempt aborts
// and its store writes stop, and ctx.Err() is returned rather than waiting any
// longer. A receiver that accepts a connection and then stalls therefore costs
// the shutdown its deadline and not the process's ability to exit.
//
// Whichever way it ends, the guarantee is the one EVT-156 states: delivery is
// at-least-once. A cursor advances only after a 2xx AND a committed write, so an
// abandoned attempt is redelivered on the next boot and is never skipped. What
// an expired deadline can cost is one duplicate — which is exactly what the
// stable X-Waiveo-Delivery-Id exists for a receiver to absorb.
func (l *Loop) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	started := l.started
	l.stopped = true
	l.mu.Unlock()

	l.stopOnce.Do(func() { close(l.stop) })
	if !started {
		l.cancel()
		return nil
	}
	select {
	case <-l.done:
		l.cancel()
		return nil
	case <-ctx.Done():
		l.cancel()
		return ctx.Err()
	}
}

// run is the scheduling goroutine: pass, then wait for whichever of the ticker,
// a wake, or the stop signal arrives first.
func (l *Loop) run() {
	defer close(l.done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		l.pass()

		// Stop wins over a ready wake. Without this pre-check a loop draining a
		// backlog — which re-arms its own wake on every delivery — could keep
		// choosing the wake arm and never observe that shutdown was requested.
		select {
		case <-l.stop:
			return
		default:
		}

		select {
		case <-l.stop:
			return
		case <-ticker.C:
		case <-l.wake:
		}
	}
}

// pass takes one delivery pass. A pass-level failure (the endpoint inventory
// could not be read) is reported and the loop keeps its schedule: the next pass
// may well succeed, and stopping delivery for the whole deployment because one
// read failed is a larger outage than the one being reported.
//
// A failure caused by the shutdown cancellation itself is not reported — it is
// this process ending, not a condition an operator should be told about.
func (l *Loop) pass() {
	if err := l.deliverer.Tick(l.ctx); err != nil && l.ctx.Err() == nil && l.onErr != nil {
		l.onErr(err)
	}
}
