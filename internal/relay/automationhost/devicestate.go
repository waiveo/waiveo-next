package automationhost

import (
	"context"

	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/rules/engine"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// DeviceStateSource is the seam the relay's device-state input feeds the engine
// through: Next returns the next entity observation (RUL-330) and ok=false when
// the source is exhausted. The real source (polling/ECP against live devices) is
// hardware-gated and lands later; SyntheticSource drives it in tests and the
// dev-stack demo, proving the wired automation stack fires without hardware.
type DeviceStateSource interface {
	Next() (state.Observation, bool)
}

// SyntheticSource is a DeviceStateSource that replays a fixed observation
// sequence — the injectable stand-in for real device-state input. It hands out
// each observation once, in order, then reports exhausted.
type SyntheticSource struct {
	obs []state.Observation
	i   int
}

// NewSyntheticSource returns a SyntheticSource that will replay obs in order.
func NewSyntheticSource(obs ...state.Observation) *SyntheticSource {
	return &SyntheticSource{obs: obs}
}

// Next returns the next observation, or ok=false once every one has been handed
// out.
func (s *SyntheticSource) Next() (state.Observation, bool) {
	if s.i >= len(s.obs) {
		return state.Observation{}, false
	}
	o := s.obs[s.i]
	s.i++
	return o, true
}

// Observe advances the engine by one device observation and emits an
// automation.run into the durable telemetry queue for every firing it produces
// (REL-090/093, EVT-040/041). A firing's RunDisposition is mapped to the
// events/1 automation.run payload by telemetry.AutomationRunEntry and recorded
// into the durable buffer, which write-throughs to the operational store so a
// committed automation.run survives a power-pull. It returns the firings'
// dispositions (for observation/e2e assertions) and the first durable
// write-through error the record hit, if any (telemetry.Buffer.StoreErr) — a
// durable store that has stopped accepting writes is surfaced, never swallowed.
//
// engine.Observe only ever yields ran/skipped/restarted dispositions (RUL-246);
// the generation-swap Canceled outcome arises solely from ApplyGeneration, never
// from an observation, so AutomationRunEntry's EVT-041 enum is never fed an
// out-of-band value here.
//
// Each recorded entry leaves here carrying a correlation id (relay/1 REL-006),
// assigned by Buffer.Record at the same moment it assigns the REL-091 seq, and
// it rides the telemetry wire into the delivered events/1 envelope's trace_id
// (EVT-010) — which is what makes api/1 API-063's cross-component correlation
// hold for an edge firing. Record rather than RecordTraced is the right call
// here: an edge-evaluated rule fires from a device observation the relay made
// itself, so this IS where the operation originates and there is no upstream
// trace to propagate. A firing that DOES trace to an app-dispatched operation
// (the engine does not yet carry one down from a device.command) would record
// through Buffer.RecordTraced with that operation's own trace id instead.
func (h *Host) Observe(obs state.Observation) ([]engine.RunDisposition, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Record the observed state before advancing the engine, so EntityState
	// reflects what the relay has SEEN even for an entity no loaded rule
	// subjects (see the field's doc). It is recorded unconditionally, not only
	// when obs.StateChanged: the poller's first snapshot per entity arrives as a
	// self-transition seed with StateChanged false, and that seed is precisely
	// the reading that first populates a slide's entity widget after boot.
	//
	// Under stateMu, held for this assignment alone and released before the
	// engine advances: everything below this line can dispatch a device command
	// and block on a wedged TV, and a Lease being issued on another goroutine
	// must never wait behind that (stateMu's doc). Doing it FIRST also means the
	// reading is visible to a reader for the whole duration of the dispatch it
	// may have caused, rather than only after it completes.
	h.stateMu.Lock()
	h.entityStates[obs.Entity.ID] = obs.Entity.State
	h.stateMu.Unlock()

	disps := h.engine.Observe(obs)
	atMs := h.clk.WallMillis()
	for _, d := range disps {
		schema, payload, subject := telemetry.AutomationRunEntry(d, atMs)
		h.buf.Record(schema, payload, subject, atMs)
	}
	return disps, h.buf.StoreErr()
}

// Run pulls observations from src and feeds each through Observe until the source
// is exhausted or ctx is canceled (RUL-330). It is the bounded drive loop the
// synthetic source uses in the dev-stack demo; the real polling/ECP source will
// drive the same loop on hardware. It returns ctx's error on cancellation, the
// first durable write-through error an Observe surfaces, or nil once the source
// is exhausted.
func (h *Host) Run(ctx context.Context, src DeviceStateSource) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		obs, ok := src.Next()
		if !ok {
			return nil
		}
		if _, err := h.Observe(obs); err != nil {
			return err
		}
	}
}
