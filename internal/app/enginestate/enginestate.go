// Package enginestate holds the latest discovery-engine state each relay has
// reported (`discovery.engine_state`), so the console can tell "this relay is
// watching for 7 patterns" from "this relay is watching for nothing" from "this
// relay has not said".
//
// It is the sibling of internal/app/scanstatus and is deliberately built the
// same way: a tiny latest-wins registry rather than a store table, because
// nothing authors an engine state, it carries no revision, it is replaced
// wholesale by the next report, and none of it is signed or shipped anywhere.
//
// # Why this one IS re-stated on connect, where scan status is not
//
// scanstatus documents its own non-persistence by saying a scan is an in-flight
// activity and a restarted feeder has genuinely lost track of what a relay was
// doing. That reasoning is right for an activity and wrong for a configuration:
// a relay's watch set is not in flight, it is simply current, and it stays
// current across either peer's restart. So this registry is equally
// non-persistent, but the relay re-states its engine state on every connect —
// which means a restarted feeder's view refills as soon as its relays reconnect,
// rather than waiting for a pack to be installed.
//
// The registry itself therefore holds only what it was told, and the honest
// answer for a relay that has not reported is ABSENCE, not a zeroed row.
package enginestate

import (
	"sort"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// State is one relay's reported engine state, with the relay it came from and
// when this app peer received it.
//
// ReportedAtMs is stamped by the APP from its own clock, not carried on the
// wire. The relay's clock is not trusted for app-side ordering (Clock trust,
// relay/1), and the fact an operator actually needs — "how long ago did this
// box hear this" — is a property of the receiving peer anyway.
type State struct {
	RelayID             string `json:"relay_id"`
	SSDPLane            bool   `json:"ssdp_lane"`
	MDNSLane            bool   `json:"mdns_lane"`
	SSDPWatches         int    `json:"ssdp_watches"`
	MDNSWatches         int    `json:"mdns_watches"`
	PackPatterns        int    `json:"pack_patterns"`
	MDNSUndeliverable   int    `json:"mdns_undeliverable"`
	MacOUIUnimplemented int    `json:"mac_oui_unimplemented"`
	Malformed           int    `json:"malformed"`
	// WatchingNothing is the derived judgement, published rather than left for
	// each caller to recompute: a relay with no live watch in any lane discovers
	// nothing passively however healthy the rest of its signals look.
	WatchingNothing bool  `json:"watching_nothing"`
	ReportedAtMs    int64 `json:"reported_at_ms"`
}

// Registry is the latest reported engine state per relay.
type Registry struct {
	mu  sync.RWMutex
	by  map[string]State
	now func() int64
}

// New returns an empty registry stamping receipts from nowMs.
//
// The clock is injected rather than read from time.Now inside Apply so a test
// asserts a fixed instant instead of a window — the same discipline the rest of
// the app's clock-dependent code follows.
func New(nowMs func() int64) *Registry {
	if nowMs == nil {
		return nil
	}
	return &Registry{by: map[string]State{}, now: nowMs}
}

// ApplyDiscoveryEngineState records relayID's latest report, replacing any
// previous one. It satisfies feeder/relayconn.DiscoveryEngineStateSink.
//
// An empty relayID is ignored rather than filed under "": the caller passes the
// AUTHENTICATED connection identity, so an empty one is a wiring defect, and
// inventing a row for it would put an engine state on the console attributable
// to no relay.
func (r *Registry) ApplyDiscoveryEngineState(relayID string, body wire.DiscoveryEngineStateBody) {
	if r == nil || relayID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.by[relayID] = State{
		RelayID:             relayID,
		SSDPLane:            body.SSDPLane,
		MDNSLane:            body.MDNSLane,
		SSDPWatches:         body.SSDPWatches,
		MDNSWatches:         body.MDNSWatches,
		PackPatterns:        body.PackPatterns,
		MDNSUndeliverable:   body.MDNSUndeliverable,
		MacOUIUnimplemented: body.MacOUIUnimplemented,
		Malformed:           body.Malformed,
		WatchingNothing:     body.WatchingNothing(),
		ReportedAtMs:        r.now(),
	}
}

// States returns every reported engine state in relay-id order — a stable order,
// so a console rendering the list does not reshuffle it between polls.
//
// A relay that has never reported is ABSENT rather than present-with-zeroes.
// That distinction is the whole point of the frame: zeroes mean "watching for
// nothing", which is a real and alarming condition, and minting them for a relay
// that simply has not spoken would raise a false alarm on every fresh feeder.
func (r *Registry) States() []State {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]State, 0, len(r.by))
	for _, s := range r.by {
		out = append(out, s)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].RelayID < out[j].RelayID })
	return out
}
