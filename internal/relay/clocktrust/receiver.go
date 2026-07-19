package clocktrust

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// maxHintBody bounds a clock.hint request body — the message is a tiny fixed
// envelope, so anything larger is rejected before decode.
const maxHintBody = 1 << 16

// HintReceiver is the relay-runtime dispatcher for the app peer's wire
// `clock.hint` message (REL-133): it decodes an actual clock.hint envelope
// arriving on an established connection and applies it to a RuntimeClock via
// AcceptHint, bounded by the relay's own certificate not_after plus a grace
// period. It is the connection-layer entry point that makes the RuntimeClock
// reachable end-to-end — a real `clock.hint` on the wire reaches AcceptHint
// through Receive/ServeHTTP, never a direct unit-test call alone.
//
// A hint adjusts the runtime clock ONLY; the receiver holds no reference to
// the identity store, so it structurally cannot advance the persisted floor
// (REL-132).
type HintReceiver struct {
	clock          *RuntimeClock
	certNotAfterMs int64
	boundedGraceMs int64
}

// NewHintReceiver binds a receiver to clock, bounding accepted hints to
// certNotAfterMs + boundedGraceMs (REL-133) — an app peer MUST NOT be able to
// use a hint alone to push the relay's runtime clock past its own credential's
// expiry.
func NewHintReceiver(clock *RuntimeClock, certNotAfterMs, boundedGraceMs int64) *HintReceiver {
	return &HintReceiver{clock: clock, certNotAfterMs: certNotAfterMs, boundedGraceMs: boundedGraceMs}
}

// Receive decodes a wire clock.hint envelope and applies it to the runtime
// clock (REL-133), reading the monotonic instant from the clock itself so the
// caller need not thread it. A malformed envelope or a non-`clock.hint` type
// is a decode error; a well-formed but out-of-bound hint is a clean
// accepted=false with a nil error (rejected, not an error). On accept the
// runtime clock is adjusted; the persisted floor is never touched.
func (r *HintReceiver) Receive(raw []byte) (accepted bool, err error) {
	var h ClockHint
	if err := json.Unmarshal(raw, &h); err != nil {
		return false, fmt.Errorf("clocktrust: decode clock.hint: %w", err)
	}
	if h.Type != "clock.hint" {
		return false, fmt.Errorf("clocktrust: unexpected message type %q, want clock.hint", h.Type)
	}
	return r.clock.AcceptHintNow(h, r.certNotAfterMs, r.boundedGraceMs), nil
}

// ServeHTTP mounts the receiver as the relay's `clock.hint` endpoint on its own
// listener: the app peer POSTs a clock.hint on the established connection
// (REL-133) and the relay applies it to its runtime clock only, answering with
// whether it was accepted and the resulting runtime wall time. A rejected
// (out-of-bound) hint is still a 200 with accepted=false — it is a valid
// message the relay declined to act on, not a protocol error.
func (r *HintReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxHintBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	accepted, err := r.Receive(body)
	if err != nil {
		http.Error(w, "malformed clock.hint", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accepted":        accepted,
		"runtime_wall_ms": r.clock.WallMillis(),
	})
}
