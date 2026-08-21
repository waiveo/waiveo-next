package main

// enginestate.go reports `discovery.engine_state` upward: what this relay's
// discovery engine is currently watching for.
//
// The numbers themselves are computed in packwatches.go, at the one place that
// knows the whole live watch set — the moment a signed generation applies. This
// file's whole job is the SECOND delivery: holding that state so it can be
// re-stated to an app peer that has not heard it.
//
// # Why the state is held rather than recomputed
//
// The watch set changes only when a generation applies, and REL-070 deliberately
// SUPPRESSES re-applying an unchanged generation. So a relay that reconnects
// without any pack having changed never runs the applier again — and if the only
// report were the one at apply time, the app peer would hold no engine state for
// a relay that has been watching correctly the entire time. The console would
// show "nothing reported" for a healthy relay, which is the same class of lie
// this frame exists to remove.
//
// Holding the last computed value costs one struct and makes reconnect honest.

import (
	"log"
	"sync"

	"github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// engineStateReporter holds the latest computed engine state and pushes it up
// the current live connection.
//
// A zero-value reporter (or a nil one) is inert rather than a panic: several
// relay builds run with discovery off entirely, and a test may exercise the
// watch merge with no connection at all.
type engineStateReporter struct {
	mu   sync.RWMutex
	body wire.DiscoveryEngineStateBody
	// have distinguishes "no generation has applied yet" from "a generation
	// applied and every count was zero". Only the second is worth reporting;
	// the first has genuinely nothing to say, and sending a zeroed body for it
	// would tell the app peer this relay is watching nothing when in truth it
	// has not looked yet.
	have bool
	live *connHolder
}

func newEngineStateReporter(live *connHolder) *engineStateReporter {
	return &engineStateReporter{live: live}
}

// record stores this generation's engine state and reports it immediately.
//
// Best-effort delivery, exactly like reportScanStatus: discovery is a local
// activity and must not fail because the app peer is momentarily unreachable.
// The state is stored FIRST and unconditionally, so a report that could not be
// sent is still owed and still paid on the next connect.
func (r *engineStateReporter) record(body wire.DiscoveryEngineStateBody) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.body = body
	r.have = true
	live := r.live
	r.mu.Unlock()

	if live == nil {
		return
	}
	c := live.get()
	if c == nil {
		return
	}
	if err := c.SendDiscoveryEngineState(body); err != nil {
		log.Printf("waiveo-relay: reporting discovery engine state failed (the watches themselves are unaffected): %v", err)
	}
}

// pending reports what a connect-time resend would send, and whether there is
// anything to send at all.
//
// Split out from resend so the DECISION is testable without a live connection:
// "send nothing before any generation has applied" and "send the latest, not the
// first" are the two rules that make reconnect honest, and both are decided
// here rather than inside an I/O path a unit test cannot reach.
func (r *engineStateReporter) pending() (wire.DiscoveryEngineStateBody, bool) {
	if r == nil {
		return wire.DiscoveryEngineStateBody{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.body, r.have
}

// resend re-reports the last recorded state on a freshly-established
// connection. A relay that has applied no generation yet sends nothing.
func (r *engineStateReporter) resend(c *relayconn.Client) {
	body, have := r.pending()
	if !have || c == nil {
		return
	}
	if err := c.SendDiscoveryEngineState(body); err != nil {
		log.Printf("waiveo-relay: re-reporting discovery engine state on connect failed: %v", err)
	}
}
