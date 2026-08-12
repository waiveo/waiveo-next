package playerserver

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// POST /player/v1/interaction — the return path (interactive slide layers,
// parity milestones 1.5/3.7). These drive the LIVE mounted handler over HTTP
// with a real channel token, so what is asserted is the route a player actually
// reaches, not a method call.

// recordedInteraction is one call the installed recorder saw.
type recordedInteraction struct {
	schema  string
	payload json.RawMessage
	subject string
	traceID string
}

// interactionRecorderSpy is a thread-safe InteractionRecorder that keeps what it
// was handed.
type interactionRecorderSpy struct {
	mu   sync.Mutex
	seen []recordedInteraction
}

func (s *interactionRecorderSpy) record(schema string, payload json.RawMessage, subject string, _ int64, traceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, recordedInteraction{schema: schema, payload: payload, subject: subject, traceID: traceID})
}

func (s *interactionRecorderSpy) all() []recordedInteraction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedInteraction(nil), s.seen...)
}

// interactionTestServer stands a paired server up, pulls one program so a lease
// exists to bind a press to, and returns everything a press needs.
func interactionTestServer(t *testing.T) (srv *Server, spy *interactionRecorderSpy, token, leaseID string) {
	t.Helper()
	srv, _, token = programTestServer(t)
	spy = &interactionRecorderSpy{}
	srv.SetInteractionRecorder(spy.record)

	resp, raw := doProgram(t, srv, token, []string{"image", "video", "slide"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("program status = %d, want 200", resp.StatusCode)
	}
	var lease LeaseResponse
	remarshal(t, raw, &lease)
	if lease.LeaseID == "" {
		t.Fatal("program did not yield a lease id")
	}
	return srv, spy, token, lease.LeaseID
}

func postInteraction(t *testing.T, srv *Server, token string, body map[string]any) *http.Response {
	t.Helper()
	ts := newPairingTestServer(t, srv)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return postPlayerJSON(t, ts, token, "/player/v1/interaction", raw)
}

// TestInteractionRecordsADurableScreenInteraction is the end-to-end shape of the
// whole return path: a press posted with a real channel token and a lease this
// relay issued produces exactly one events/1 `screen.interaction` entry, whose
// payload VALIDATES against the registered schema (so the app peer's EVT-013
// gate will append it rather than drop it) and whose screen_id is the one the
// TOKEN resolves to.
func TestInteractionRecordsADurableScreenInteraction(t *testing.T) {
	srv, spy, token, leaseID := interactionTestServer(t)

	resp := postInteraction(t, srv, token, map[string]any{
		"lease_id":    leaseID,
		"interaction": "call_service",
		"slide_id":    "intro",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("interaction status = %d, want 200", resp.StatusCode)
	}

	got := spy.all()
	if len(got) != 1 {
		t.Fatalf("recorded %d entries, want exactly 1", len(got))
	}
	if got[0].schema != events.SchemaScreenInteraction {
		t.Errorf("schema = %q, want %q", got[0].schema, events.SchemaScreenInteraction)
	}
	if got[0].subject != testScreenIDA {
		t.Errorf("subject = %q, want the pressing screen %q", got[0].subject, testScreenIDA)
	}

	var payload struct {
		ScreenID    string `json:"screen_id"`
		Interaction string `json:"interaction"`
		LeaseID     string `json:"lease_id"`
		SlideID     string `json:"slide_id"`
		At          int64  `json:"at"`
	}
	if err := json.Unmarshal(got[0].payload, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload.ScreenID != testScreenIDA {
		t.Errorf("payload.screen_id = %q, want the token's own screen %q — never a body-supplied one", payload.ScreenID, testScreenIDA)
	}
	if payload.Interaction != "call_service" {
		t.Errorf("payload.interaction = %q, want the authored name verbatim (EVT-057)", payload.Interaction)
	}
	if payload.LeaseID != leaseID {
		t.Errorf("payload.lease_id = %q, want %q", payload.LeaseID, leaseID)
	}
	if payload.SlideID != "intro" {
		t.Errorf("payload.slide_id = %q, want the slide that carried the pressed element", payload.SlideID)
	}
	if payload.At <= 0 {
		t.Error("payload.at must carry the relay's observation instant")
	}

	// The recorded payload must pass the app peer's own delivery gate, or the
	// press is buffered, pushed, and then DROPPED on arrival with the console
	// showing nothing. Validating it here — against the real events/1 validator,
	// with the envelope fields an ingest supplies — is what proves the producer
	// and the catalog agree.
	env := events.Envelope{
		ID:             "01J8Z3K4N5P6Q7R8S9T0V1W2Z5",
		Schema:         events.SchemaScreenInteraction,
		TS:             1,
		ScopeNode:      "01J8Z3K4N5P6Q7R8S9T0V1W2Z3",
		TraceID:        "01J8Z3K4N5P6Q7R8S9T0V1W2Z4",
		Origin:         "relay",
		CostClass:      "telemetry",
		RetentionClass: "telemetry-standard",
		Payload:        got[0].payload,
	}
	if err := events.Validate(env); err != nil {
		t.Fatalf("the recorded payload must satisfy the events/1 delivery gate, got %v", err)
	}
}

// TestInteractionRequiresALeaseIssuedToThisScreen pins the binding that makes an
// interaction attributable to what was actually on screen (the same rule
// lease/ack and render/start apply, PLY-114). Without it a caller can assert a
// press against content the screen never showed.
func TestInteractionRequiresALeaseIssuedToThisScreen(t *testing.T) {
	srv, spy, token, _ := interactionTestServer(t)

	resp := postInteraction(t, srv, token, map[string]any{
		"lease_id":    "lease-that-was-never-issued",
		"interaction": "call_service",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown lease", resp.StatusCode)
	}
	if n := len(spy.all()); n != 0 {
		t.Fatalf("recorded %d entries for a refused press, want 0", n)
	}
}

// TestInteractionRejectsAMalformedName: the name is what a rules/1 `event`
// trigger matches on, so a name this relay would accept but no author could ever
// have written is a press that can never fire anything. The grammar is the
// AUTHORING one (wire.ValidPingName), not a second copy.
func TestInteractionRejectsAMalformedName(t *testing.T) {
	srv, spy, token, leaseID := interactionTestServer(t)

	for _, bad := range []string{"", "Front Desk", "UPPER"} {
		resp := postInteraction(t, srv, token, map[string]any{"lease_id": leaseID, "interaction": bad})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("interaction %q: status = %d, want 400", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if n := len(spy.all()); n != 0 {
		t.Fatalf("recorded %d entries for refused presses, want 0", n)
	}
}

// TestInteractionRefusesWithNoRecorder is the anti-dead-end guard, and it is the
// specific defect this whole track exists to close.
//
// With no durable sink installed there is nowhere for a press to go. Answering
// 200 would make it vanish with every layer reporting success — the player logs
// a delivered press, the relay logs a request served, and the automation never
// runs, with the only evidence being a person pressing a button and nothing
// happening. The route must refuse instead.
func TestInteractionRefusesWithNoRecorder(t *testing.T) {
	srv, _, token := programTestServer(t)
	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("program status = %d", resp.StatusCode)
	}
	var lease LeaseResponse
	remarshal(t, raw, &lease)

	// Deliberately NO SetInteractionRecorder.
	press := postInteraction(t, srv, token, map[string]any{"lease_id": lease.LeaseID, "interaction": "call_service"})
	defer press.Body.Close()
	if press.StatusCode == http.StatusOK {
		t.Fatal("a press with no durable sink must NOT be answered 200 — that is a surface accepting work it never performs")
	}
	if press.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", press.StatusCode)
	}
}

// TestInteractionRejectsAnUnauthenticatedPress: the screen identity comes from
// the channel token, so a press with no token has no screen to attribute and
// must never reach the recorder.
func TestInteractionRejectsAnUnauthenticatedPress(t *testing.T) {
	srv, spy, _, leaseID := interactionTestServer(t)
	resp := postInteraction(t, srv, "", map[string]any{"lease_id": leaseID, "interaction": "call_service"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if n := len(spy.all()); n != 0 {
		t.Fatalf("recorded %d entries for an unauthenticated press, want 0", n)
	}
}

// TestScreenInteractionSchemaNamesAgree pins the three independent spellings of
// one schema name — this package's emission constant, the relay telemetry
// channel's class-table constant, and events/1's own catalog — so a rename in
// one place cannot leave a producer emitting a schema the buffer will not class
// and the app peer will not accept.
func TestScreenInteractionSchemaNamesAgree(t *testing.T) {
	if eventSchemaScreenInteraction != events.SchemaScreenInteraction {
		t.Errorf("playerserver emits %q, events/1 registers %q", eventSchemaScreenInteraction, events.SchemaScreenInteraction)
	}
}

// A viewer's press must not clear the CONTENT-transfer failure count.
//
// This is not a question of how long a screen reads `fetching`, which is how the
// conflation was first booked. unackedPulls is the only discriminator in
// screens.reachabilityOf that survives a screen which keeps talking: `rejected`
// is tested first and is driven by this counter, and the clause below it grades
// `live` on the freshest of pull-or-ack age — which a screen looping on a failed
// fetch keeps fresh by pulling. Zeroing the counter on a press therefore did not
// delay `rejected` on an interactive wall, it made it unreachable, and a wall
// refusing every program it was handed read `live` for as long as people kept
// touching it.
//
// So the assertion is the one that matters operationally: drive the count PAST
// the bound the read model grades on, press, and require the count to survive.
func TestAPressDoesNotClearTheContentTransferFailureCount(t *testing.T) {
	srv, _, token, _ := interactionTestServer(t)

	// Pull content-bearing programs until the count is past the bound — the
	// state of a screen that is abandoning Lease after Lease. Bounded so a
	// regression in the counter fails the test instead of hanging it.
	leaseID := ""
	for i := 0; int64(statusOf(t, srv, testScreenIDA).UnackedPulls) <= wire.ScreenFetchingMaxUnackedPulls; i++ {
		if i > 20 {
			t.Fatalf("unacked_pulls never passed %d over %d content-bearing pulls; this test can no longer reach the state it exists to check",
				wire.ScreenFetchingMaxUnackedPulls, i)
		}
		_, raw := doProgram(t, srv, token, []string{"image", "video", "slide"})
		var lease LeaseResponse
		remarshal(t, raw, &lease)
		leaseID = lease.LeaseID
	}
	before := statusOf(t, srv, testScreenIDA).UnackedPulls

	// The press names the lease most recently issued, because the relay only
	// accepts one it recently handed out (PLY-114) — an earlier id would be
	// refused and the test would pass for the wrong reason.
	resp := postInteraction(t, srv, token, map[string]any{
		"lease_id":    leaseID,
		"interaction": "call_service",
		"slide_id":    "intro",
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("interaction status = %d, want 200 — the press must be ACCEPTED for this test to be about what an accepted press does", resp.StatusCode)
	}

	after := statusOf(t, srv, testScreenIDA)
	if after.UnackedPulls != before {
		t.Errorf("unacked_pulls = %d after a viewer press, want %d unchanged.\n"+
			"A press is liveness evidence, not content-transfer evidence. Clearing this count is what let an interactive wall that was refusing every program read `live` indefinitely: screens.reachabilityOf tests `rejected` FIRST and derives it from this counter.",
			after.UnackedPulls, before)
	}
	if int64(after.UnackedPulls) <= wire.ScreenFetchingMaxUnackedPulls {
		t.Errorf("unacked_pulls = %d is back inside the %d the read model tolerates, so the screen would grade reachable again on a press alone",
			after.UnackedPulls, wire.ScreenFetchingMaxUnackedPulls)
	}
}
