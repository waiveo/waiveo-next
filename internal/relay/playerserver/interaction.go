package playerserver

import (
	"encoding/json"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// interaction.go is the RETURN PATH: `POST /player/v1/interaction`, the one
// route on this server a player uses to tell the platform something rather than
// to be told something (interactive slide layers, parity milestones 1.5/3.7).
//
// A slide layer may carry a `ping_name` (wire.LayerIsInteractive). The player
// draws such a layer as a focusable region, the viewer moves the remote's D-pad
// onto it and presses OK, and the player POSTs that press here. This server
// resolves WHICH SCREEN pressed it from the presented channel token — never from
// the body — binds the press to a Lease it actually issued to that screen, and
// records it into the durable telemetry buffer as an events/1
// `screen.interaction` (EVT-055), from where the ordinary telemetry channel
// (REL-090/092/097) carries it to the app peer's event log and any automation
// with a matching `event` trigger (rules/1 RUL-080/081).
//
// # Why it goes up the telemetry channel rather than down a new one
//
// Every fact this relay observes and the app must eventually know already
// travels one durable, acked, loss-marked path. A second path for presses would
// need its own buffering, its own retry, and its own answer to "what happens to
// a press made while the box is unreachable" — and would get a different answer
// than the one REL-093 already gives. Recording it as a telemetry entry means a
// press made during an app-peer outage is retained and delivered when the peer
// returns, exactly like a playback record, with no code here that knows anything
// about outages.
//
// # What is validated, and why each check exists
//
//   - the channel token authorizes, and its screen_id is what the event carries.
//     A body-supplied screen id would let one paired screen manufacture another
//     screen's interactions — the cross-screen presentation PLY-070 forbids,
//     arriving through the one route that writes durable state on a human's
//     behalf.
//   - the lease_id names a Lease THIS relay issued to THAT screen (leaseIssuedTo,
//     the identical binding lease/ack and render/start apply, PLY-114). Without
//     it a caller can assert a press against content the screen never showed,
//     and the event's whole value is that it is attributable to what was on
//     screen.
//   - the interaction name is a well-formed ping name (wire.ValidPingName — the
//     SAME grammar the authoring gate applies, not a second copy). This is the
//     value an automation matches on, so a name this relay would accept but no
//     author could ever have written is a press that can never fire anything.
type interactionRequest struct {
	LeaseID     string `json:"lease_id"`
	Interaction string `json:"interaction"`
	SlideID     string `json:"slide_id,omitempty"`
}

// screenInteractionPayload is the events/1 EVT-055 `screen.interaction` payload
// this relay records. It is declared here, at the emission point, for the reason
// telemetry.Entry's own doc gives about payloads: the telemetry package carries a
// schema's fields unmodified and never redefines them, so the shape lives with
// the producer that fills it.
//
// SlideID is `omitempty`: a slide projected from a cast carries a cast-local id
// and a generated alert slide does not, and EVT-055 types the field optional for
// exactly that reason.
type screenInteractionPayload struct {
	ScreenID    string `json:"screen_id"`
	Interaction string `json:"interaction"`
	LeaseID     string `json:"lease_id"`
	SlideID     string `json:"slide_id,omitempty"`
	At          int64  `json:"at"`
}

// InteractionRecorder is the seam this server records an accepted press through:
// the relay's durable telemetry buffer (internal/relay/telemetry.Buffer.Record),
// wired by the binary (cmd/waiveo-relay) which owns that buffer.
//
// It is a func rather than a concrete *telemetry.Buffer so this package does not
// depend on the telemetry package for one call, and so a test can assert exactly
// what would have been buffered without standing a buffer up. It is NOT optional
// in effect: with no recorder installed, a press is refused rather than accepted
// and dropped — see handleInteraction. A route that returns 200 for work nothing
// will perform is the defect this whole track exists to close, and it would be
// especially invisible here, where the only evidence is a person pressing a
// button and nothing happening.
type InteractionRecorder func(schema string, payload json.RawMessage, subject string, atMs int64, traceID string)

// SetInteractionRecorder installs the durable sink accepted presses are recorded
// into. Callers install it once, at construction, before serving traffic — the
// same discipline SetSigningKey and EnablePersistence document, and for the same
// reason: an assignment racing an in-flight request is a request served without
// it.
func (s *Server) SetInteractionRecorder(rec InteractionRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interactionRecorder = rec
}

// interactionRecorderLocked reads the installed recorder under mu.
func (s *Server) interactionRecorderFn() InteractionRecorder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interactionRecorder
}

// handleInteraction implements POST /player/v1/interaction.
func (s *Server) handleInteraction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	traceID := apihttp.TraceID(r)

	screenID, _, ok := s.authorizeChannelToken(w, r, traceID)
	if !ok {
		return
	}

	var req interactionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	if req.LeaseID == "" || !wire.ValidPingName(req.Interaction) {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed")
		return
	}
	if !s.leaseIssuedTo(screenID, req.LeaseID) {
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, "LEASE_UNKNOWN", "Lease Unknown")
		return
	}

	rec := s.interactionRecorderFn()
	if rec == nil {
		// No durable sink wired: refuse, loudly and honestly. Answering 200 here
		// would make a viewer's press vanish with every layer of the stack
		// reporting success — the player logs a delivered press, the relay logs a
		// request served, and the automation never runs.
		apihttp.WriteProblem(w, r, traceID, http.StatusServiceUnavailable, "UNAVAILABLE", "Unavailable")
		return
	}

	at := s.nowMs()
	payload, err := json.Marshal(screenInteractionPayload{
		ScreenID:    screenID,
		Interaction: req.Interaction,
		LeaseID:     req.LeaseID,
		SlideID:     req.SlideID,
		At:          at,
	})
	if err != nil {
		apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "INTERNAL", "Internal Error")
		return
	}
	// Subject is the screen: it is the latest-only supersession key the buffer
	// would use IF this schema were latest-only. It is not (EVT-056 forbids
	// coalescing two presses), so the subject is carried for uniformity with
	// every other producer rather than to enable a discard.
	rec(eventSchemaScreenInteraction, payload, screenID, at, traceID)

	// A press is liveness evidence of the strongest kind available: the screen
	// is not merely reachable, a human is standing in front of it interacting
	// with what it is showing.
	s.noteLeaseAck(screenID)

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// eventSchemaScreenInteraction is events/1 EVT-055's schema name, spelled here
// rather than imported from internal/events because this package is RELAY-side
// and importing the app's event catalog for one string would couple the relay
// binary to it. internal/relay/telemetry carries the same constant for the same
// reason (SchemaScreenInteraction), and TestScreenInteractionSchemaNamesAgree
// pins the two to events.SchemaScreenInteraction so the three cannot drift.
const eventSchemaScreenInteraction = "screen.interaction"
