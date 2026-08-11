package main

import (
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
)

// restart.go is the PROCESS half of the api/1 restart operation (API-150-157):
// the part that decides whether this deployment can restart at all, and the part
// that actually stops it.
//
// The api layer decides whether the CALLER may restart and whether the box is in
// a state where one is safe (internal/app/api/restart.go). Everything that is a
// property of this process rather than of the request lives here.
//
// # The supervisor is declared, not detected
//
// `WAIVEO_FEEDER_SUPERVISOR` names whatever will start a replacement when this
// process exits — `systemd` on the appliance, `docker` under a compose file with
// a restart policy, the name of a wrapper script in a dev loop. UNSET means
// nothing will, and the api layer refuses RESTART_UNSUPPORTED rather than
// stopping a process that would stay stopped.
//
// It is not inferred, and the inference that was available is exactly why. Every
// systemd unit's processes carry `INVOCATION_ID`, including a unit written
// `Restart=no` — so sniffing it answers "supervised" with total confidence on the
// one deployment where being wrong means an operator presses a button in the
// console and the box never comes back. There is no environment variable that
// means "and it is configured to restart me"; only the person who wrote the unit
// knows that, so the unit is where it is said. deploy/systemd/waiveo-feeder.service
// sets this variable on the line below `Restart=always`, deliberately adjacent:
// the two are one statement and changing either without the other is the bug.
//
// # Stopping is the SIGTERM path, not a second one
//
// Arm does not shut anything down itself. It hands the order to main's shutdown
// select, which runs the identical drain a SIGTERM runs — one code path, so a
// restart cannot become a quietly less careful shutdown that diverges from the
// careful one as each is maintained. API-157 requires exactly that, and the way
// to be sure of it is to have nothing else to maintain.
//
// # Why it exits rather than re-execs
//
// The full argument is in internal/app/api/restart.go's package doc, where the
// alternatives are set out. The short of it: execve keeps the PID (so systemd
// never observes the restart), skips the Go runtime's unwind (so the drain above
// does not happen), cannot pick up a changed unit file or environment, and leaves
// nothing running to report the failure when the new image will not exec.

// restartDrainBudget bounds the graceful drain that follows a stop. It is the
// SAME budget main's shutdown path applies, taken from here by both, so the
// figure this process publishes to a client in the acceptance body is the figure
// it actually honours — a published window nothing keeps is worse than
// publishing none.
const restartDrainBudget = 5 * time.Second

// restartHold is how long the stop waits after being armed, so the 202 the api
// layer just wrote reaches the client before the listener stops accepting. It
// mirrors the `stopping_in_ms` that response publishes.
//
// It is a little longer than the published window on purpose. The published
// number is what a client plans around, and a process that stopped at exactly
// that instant would make the client's own timing a race it cannot win; a stop
// that is slightly late is invisible, and one that is early strands the
// acceptance.
const restartHold = 400 * time.Millisecond

// restarter owns this process's restart state: whether a restart is already
// armed, and the channel main's shutdown select is waiting on.
//
// The armed flag lives HERE, in the process, and not in the api server, because
// there is exactly one process to stop and therefore exactly one place that can
// decide race-free whether it is already stopping. Two components each holding
// half of that decision is how two accepted restarts happen.
type restarter struct {
	// supervisor is what the deployment declared. Empty means none.
	supervisor string
	// hold is how long to wait after arming before signalling the stop. A field
	// rather than the constant directly so a test can drive the real Arm without
	// waiting out a real hold.
	hold time.Duration
	// requests carries the armed order to main's shutdown select. Buffered by
	// one, so the goroutine that delivers it never blocks even if main has
	// already left the select on a SIGTERM that arrived first.
	requests chan api.RestartOrder

	mu    sync.Mutex
	armed bool
}

// newRestarter builds this process's restarter. supervisor is the declared
// value; empty is the ordinary state of a dev run and of any deployment that has
// not said otherwise.
func newRestarter(supervisor string) *restarter {
	return &restarter{
		supervisor: supervisor,
		hold:       restartHold,
		requests:   make(chan api.RestartOrder, 1),
	}
}

// config returns the wiring the api layer's restart operation is built from.
//
// A restarter with no declared supervisor still produces a config — with an
// empty Supervisor — rather than nothing, so the refusal an unsupervised
// deployment gives comes from ONE place (the api layer's published
// RESTART_UNSUPPORTED) instead of from two that could disagree: an unwired
// option and an empty declaration must not be able to answer differently.
func (rs *restarter) config() api.RestartConfig {
	return api.RestartConfig{
		Supervisor:    rs.supervisor,
		DrainBudgetMs: restartDrainBudget.Milliseconds(),
		Arm:           rs.arm,
	}
}

// arm accepts one restart order and reports whether THIS call armed it. A second
// call while one is already armed returns false and changes nothing — the first
// stop is the only one that can happen, so a second acceptance would report an
// act that will not occur.
//
// The armed flag is never cleared. There is no state after this worth returning
// to: either the process stops, or it failed to and a retry would fail the same
// way. Clearing it on some timeout would reopen the double-accept this exists to
// close, in the window where the process is least able to answer for itself.
func (rs *restarter) arm(order api.RestartOrder) bool {
	rs.mu.Lock()
	if rs.armed {
		rs.mu.Unlock()
		return false
	}
	rs.armed = true
	hold := rs.hold
	rs.mu.Unlock()

	go func() {
		time.Sleep(hold)
		// Non-blocking: the channel is buffered by one and only one order is ever
		// armed, so this cannot drop an order. It is written this way so that a
		// process already shutting down on a signal cannot leave this goroutine
		// parked forever on a receiver that has gone.
		select {
		case rs.requests <- order:
		default:
		}
	}()
	return true
}
