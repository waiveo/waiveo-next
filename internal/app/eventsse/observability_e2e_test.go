package eventsse

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/relay/telemetryhttp"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// The observability e2e wires the WHOLE loop the increment lights up, in one
// process, over the real HTTP transports — no fakes at the seams:
//
//	relay automationhost (fires an authored edge rule on an observation)
//	  -> telemetry.Buffer -> telemetry.Channel -> telemetryhttp (POST /telemetry/v1/push)
//	    -> app eventingest (reconstructs an events/1 Envelope, appends to the shared log)
//	      -> eventsse.Hub wakes a connected /events/v1 SSE subscriber
//	        -> the subscriber RECEIVES the automation.run live.
//
// THE LIVE STREAM IS THE ORACLE: the assertion is not that the firing was
// buffered, but that its automation.run reaches a subscriber watching
// /events/v1 — matching the fired rule's id, mode_disposition: ran, and
// origin: relay (REL-090/092/097, EVT-010/013/042/100).
const (
	// obsRuleID is a VALID ULID (Crockford base32, no I/L/O/U) — automation.run's
	// rule_id is contractually a ULID (EVT-040), so the fired rule's id must
	// satisfy events.Validate at ingest or the event is dropped and never streams.
	obsRuleID   = "01J8Z3K4N5P6Q7R8S9T0V1W2YC"
	obsEntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	obsDeviceID = "01J8Z3K4N5P6Q7R8S9T0V1DEVA"
	obsRelayID  = "01J8Z3K4N5P6Q7R8S9T0V1RELY"
)

// obsEdgeRule is the authored edge rule the e2e drives: when the screen rises to
// "on", launch the dev channel on it — a state trigger firing a device_command,
// classified edge-class (RUL-002), the same shape the feeder signs into
// desired-state (REL-062).
var obsEdgeRule = json.RawMessage(`{"id":"` + obsRuleID + `","mode":"single",` +
	`"triggers":[{"type":"state","entity_id":"` + obsEntityID + `","to":["on"]}],` +
	`"conditions":[],` +
	`"actions":[{"type":"device_command","entity_id":"` + obsEntityID + `","command":"launch","params":{"channel":"dev"}}]}`)

// nopController is the e2e's stand-in physical device: it accepts every dispatch
// and never fails, so a fired rule's device_command always completes and the
// firing records its automation.run.
type nopController struct{}

func (nopController) Dispatch(entityID, command string, params map[string]any) error { return nil }

// obsResolver maps the rule's screen entity to a media-player device so the
// launch command resolves against the canonical registry vocabulary (REL-112/113).
func obsResolver(entityID string) (deviceID, deviceClass string, ok bool) {
	return obsDeviceID, "media-player", true
}

func obsEnt(st string) state.Entity {
	return state.Entity{ID: obsEntityID, DeviceClass: "media-player", State: st}
}

func TestObservability_FiredRuleStreamsToSSESubscriber(t *testing.T) {
	// --- App: one shared event log, one Hub, ingest (writer) + SSE (reader)
	// over real HTTP. The ingest and the SSE server share the SAME Hub — the
	// seam that makes an ingested telemetry event push live (EVT-100). ---
	log := events.NewEventLog(0)
	hub := NewHub(log)
	// The subscriber's listener. The INGEST gets its own listener because it is
	// mutually authenticated: the relay presents its enrollment-issued client
	// certificate on the push (relay/1 REL-003/041) and the app peer verifies it
	// against the enrollment CA, while a browser-shaped /events/v1 subscriber
	// carries a session cookie and no certificate at all. Two listeners, one Hub
	// — the Hub is the seam this test is about, and it is unchanged.
	app := httptest.NewServer(newTestServer(hub))
	defer app.Close()
	ingestSrv, ingestClient := newIngestServer(t, hub)
	defer ingestSrv.Close()

	// --- Relay: the automation host over a fresh operational store, loaded with
	// the authored edge rule, and the telemetry channel pushing its durable
	// buffer to the app peer over the REAL telemetryhttp transport (REL-090). ---
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	defer store.Close()
	host, err := automationhost.New(store, deviceclass.Builtin(), nopController{}, obsResolver, obsRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	if err := host.ApplyEdgeRules([]json.RawMessage{obsEdgeRule}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}
	// The REAL relay-side transport, over the REAL mutually authenticated client
	// — the same telemetryhttp.Upstream the relay binary constructs, differing
	// only in which certificate the client presents.
	channel := telemetry.NewChannel(host.TelemetryBuffer(), telemetryhttp.New(ingestSrv.URL, ingestClient), nil)

	// --- A fresh SSE subscriber connects BEFORE the rule fires, so it must
	// receive the firing as a live push (EVT-132: fresh delivers from-now). ---
	br, closeConn := dialSSE(t, app, "", nil)
	defer closeConn()

	// --- Fire the authored rule: a durable "off" baseline (no transition, no
	// firing) then the "off -> on" rising edge that fires it (RUL-300/330). ---
	reg := registry.FromDeviceClass(deviceclass.Builtin(), registry.Overlay{})
	src := automationhost.NewSyntheticSource(
		state.NewObservation(reg, obsEnt("off"), obsEnt("off")),
		state.NewObservation(reg, obsEnt("off"), obsEnt("on")),
	)
	if err := host.Run(context.Background(), src); err != nil {
		t.Fatalf("host.Run (drive synthetic screen-on): %v", err)
	}

	// --- Push the buffered automation.run upstream: the channel POSTs the batch
	// to the app ingest, which reconstructs the envelope, appends it to the shared
	// log, and wakes the SSE subscriber (REL-090/092 -> EVT-010/100). ---
	if _, err := channel.Flush(); err != nil {
		t.Fatalf("channel.Flush (push telemetry to app peer): %v", err)
	}

	// --- The oracle: the subscriber RECEIVES the automation.run live. ---
	f := readFrameWithin(t, br, 3*time.Second)
	if f.event != "event" {
		t.Fatalf("the fired rule's automation.run must push live as an SSE event frame (EVT-100); got event=%q", f.event)
	}
	var env events.Envelope
	if err := json.Unmarshal([]byte(f.data), &env); err != nil {
		t.Fatalf("frame data must be the reconstructed events/1 envelope JSON (EVT-103); got %v data=%s", err, f.data)
	}
	if env.Origin != "relay" {
		t.Fatalf("the streamed event must carry origin: relay (EVT-042); got %q", env.Origin)
	}
	if env.Schema != events.SchemaAutomationRun {
		t.Fatalf("the streamed event must be an automation.run; got %q", env.Schema)
	}
	if env.ScopeNode != siteScope {
		t.Fatalf("the streamed event must carry the site scope node the ingest stamps; got %q", env.ScopeNode)
	}
	var p struct {
		RuleID          string `json:"rule_id"`
		ModeDisposition string `json:"mode_disposition"`
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("automation.run payload must be JSON; got %v (%s)", err, env.Payload)
	}
	if p.RuleID != obsRuleID {
		t.Fatalf("the streamed automation.run must name the FIRED rule; want rule_id %q got %q", obsRuleID, p.RuleID)
	}
	if p.ModeDisposition != "ran" {
		t.Fatalf("the fired rule ran, so mode_disposition must be \"ran\" (EVT-041); got %q", p.ModeDisposition)
	}
}
