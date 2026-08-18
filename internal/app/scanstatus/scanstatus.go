// Package scanstatus holds the latest scan-engine state each relay has reported
// (`discovery.scan_status`), so the console can tell "still sweeping" from
// "finished" from "never scanned".
//
// It is a deliberately tiny latest-wins registry rather than a store table, for
// the same reason the discovered-device mirror is not a resource Kind: nothing
// authors a scan state, it carries no revision, it is replaced wholesale by the
// next report, and none of it is signed or shipped anywhere. It is also NOT
// persisted — a scan is an in-flight activity, and a feeder that restarts has
// genuinely lost track of whatever a relay was doing. Reporting "unknown" after
// a restart is honest; resurrecting a stale "scanning" from disk would not be.
package scanstatus

import (
	"sort"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Status is one relay's reported scan state, with the relay it came from.
type Status struct {
	RelayID    string `json:"relay_id"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	ScanID     string `json:"scan_id,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Candidates int    `json:"candidates"`
	Error      string `json:"error,omitempty"`
}

// Registry is the latest reported status per relay.
type Registry struct {
	mu sync.RWMutex
	by map[string]Status
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{by: map[string]Status{}}
}

// ApplyDiscoveryScanStatus records relayID's latest report, replacing any
// previous one. It satisfies feeder/relayconn.DiscoveryScanStatusSink.
//
// An empty relayID is ignored rather than filed under "": the caller passes the
// AUTHENTICATED connection identity, so an empty one is a wiring defect, and
// inventing a row for it would put a status on the console attributable to no
// relay.
func (r *Registry) ApplyDiscoveryScanStatus(relayID string, body wire.DiscoveryScanStatusBody) {
	if relayID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.by[relayID] = Status{
		RelayID:    relayID,
		State:      body.State,
		Reason:     body.Reason,
		ScanID:     body.ScanID,
		StartedAt:  body.StartedAt,
		FinishedAt: body.FinishedAt,
		Candidates: body.Candidates,
		Error:      body.Error,
	}
}

// Statuses returns every reported status in relay-id order — a stable order, so
// a console rendering the list does not reshuffle it between polls.
//
// A relay that has never reported is absent rather than present-with-defaults:
// "this relay has told us nothing about scanning" and "this relay is idle" are
// different facts, and only the relay can assert the second.
func (r *Registry) Statuses() []Status {
	r.mu.RLock()
	out := make([]Status, 0, len(r.by))
	for _, s := range r.by {
		out = append(out, s)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].RelayID < out[j].RelayID })
	return out
}
