package main

// webhookdelivery_test.go proves the feeder actually DELIVERS to a webhook
// endpoint an operator registered, by driving the wiring main itself installs
// (startWebhookDelivery) rather than by calling the deliverer's Tick.
//
// That distinction is the whole point of the file. internal/app/webhookdeliver
// has thorough tests of the delivery mechanics, and every one of them passed
// while nothing in this binary ever called Tick — so on a real box an operator
// could register an endpoint, install its signing secret, watch the API accept
// both, and never receive a single POST. A test that called Tick could not have
// caught that; only one that goes through the same startWebhookDelivery main
// calls can.
//
// Both sides are real: a genuine api/1 handler with a real authenticator, the
// real audit path (an ordinary mutating request emits an events/1 audit.event
// through the same hub main wires), the real store and durable event log, the
// production HTTP client, and an in-process receiver that VERIFIES the HMAC over
// the bytes it received rather than recording that a call happened. There is no
// sleep and no outbound network: the receiver is an httptest.Server on loopback,
// every clock is injected, and every wait is on a real signal with a failure
// deadline.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/eventsse"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/webhookdeliver"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// webhookE2ESecret is the signing secret the operator installs on the endpoint
// AND configures on the receiver — one value in two places, which is exactly the
// shape the registration surface takes an operator-supplied secret for. It is at
// the surface's own 32-character floor.
const webhookE2ESecret = "whsec_feeder_e2e_0123456789abcdef"

// webhookE2ENowMs is the injected instant this whole test runs at. Nothing here
// reads a wall clock: the api handler, the idempotency store, the auth fixture,
// the event log and the delivery loop are all handed this function.
const webhookE2ENowMs = int64(1752537600000)

// verifyingReceiver is an in-process webhook receiver that accepts a delivery
// only when the secret it holds reproduces the signature the request carries —
// the same check a real receiver performs. Recording "a request arrived" would
// pass against a feeder that POSTed unsigned bodies, or signed with the wrong
// secret, or signed the wrong material.
type verifyingReceiver struct {
	secret   string
	accepted chan webhookReceipt
	srv      *httptest.Server
}

// webhookReceipt is one verified delivery, read from the bytes on the wire.
type webhookReceipt struct {
	DeliveryID string
	Envelope   events.Envelope
}

func newVerifyingReceiver(t *testing.T, secret string) *verifyingReceiver {
	t.Helper()
	// Buffered well past what this test reads, so a receiver goroutine never
	// blocks holding the delivery loop's only pass open.
	r := &verifyingReceiver{secret: secret, accepted: make(chan webhookReceipt, 64)}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *verifyingReceiver) serve(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ts := req.Header.Get(events.HeaderTimestamp)
	mac := hmac.New(sha256.New, []byte(r.secret))
	mac.Write([]byte(ts + "." + string(body)))
	want := hex.EncodeToString(mac.Sum(nil))
	got := req.Header.Get(events.HeaderSignature)
	if got == "" || !hmac.Equal([]byte(got), []byte(want)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var env events.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	select {
	case r.accepted <- webhookReceipt{DeliveryID: req.Header.Get(events.HeaderDeliveryID), Envelope: env}:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}

// webhookE2EBacklog is how many audited writes are made BEFORE the endpoint is
// given a signing secret — a backlog it owes the moment it is armed, since a
// fresh endpoint's cursor is empty and it owes the whole retained log.
//
// It is sized to make the drain assertion sharp rather than incidental. A pass
// makes at most one attempt per endpoint, so a scheduler that only ticked would
// need webhookE2EBacklog × webhookdeliver.DefaultInterval (40s) to hand these
// over, and the deadline below is 10s. Producing them before the endpoint can
// receive anything is what stops each one's own hub wake from doing the
// scheduler's job for it: at the moment they are appended the endpoint has no
// secret, so every pass they prompt delivers nothing.
const webhookE2EBacklog = 8

// TestFeederDeliversToARegisteredWebhookEndpoint is the missing-caller
// regression: register an endpoint and install its signing secret through the
// real api/1 surface, make one ordinary authoring write, and assert signed POSTs
// actually reach the receiver — with nothing in the test calling Tick.
//
// It asserts two things a broken scheduler fails differently. That a delivery
// happens AT ALL is the hole this loop was added to close. That the owed backlog
// drains on one wake rather than one event per tick is the hole a scheduler
// naively built out of a ticker would leave: a box that had been recording
// events for a month would hand a newly registered endpoint its first month of
// history at one event per interval, while every read of its delivery state
// looked perfectly healthy.
func TestFeederDeliversToARegisteredWebhookEndpoint(t *testing.T) {
	ctx := context.Background()
	clock := func() int64 { return webhookE2ENowMs }

	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// The durable event log and the live-transport hub over it, wired as main
	// wires them: ONE log, and every append goes through the hub.
	eventLog, err := st.EventLog(events.DefaultRetentionPolicy(), clock, func(err error) {
		t.Errorf("event log failure: %v", err)
	})
	if err != nil {
		t.Fatalf("EventLog: %v", err)
	}
	hub := eventsse.NewHub(eventLog)

	// The real workspace key and the real sealing construction — the same
	// Secrets instance feeds the api operation that INSTALLS the secret and the
	// delivery loop that OPENS it, exactly as main shares one.
	wsKey, err := workspacekey.LoadOrCreate(t.TempDir(), ulid.New)
	if err != nil {
		t.Fatalf("workspacekey.LoadOrCreate: %v", err)
	}
	sealer, err := wsKey.SecretSealer()
	if err != nil {
		t.Fatalf("SecretSealer: %v", err)
	}
	secrets := webhookdeliver.NewSecrets(sealer)

	// A real authenticator whose auditor publishes into that hub — the seam main
	// wires as auth.NewAuditor(eventHub, …). It is what makes the events this
	// test delivers REAL platform records produced by the request path, rather
	// than envelopes the test appended for itself.
	fixture, err := authtest.New(authtest.Config{NowMs: clock, Sink: hub, Sealer: sealer})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)
	feederAPIAuth = fixture

	apiTS := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.New,
		origin.New(), "https://192.0.2.12:7420", fixture.Auth,
		api.WithWebhookSecrets(secrets, webhookRotationOverlapMs)))
	t.Cleanup(apiTS.Close)

	receiver := newVerifyingReceiver(t, webhookE2ESecret)

	// THE WIRING UNDER TEST — the identical call main makes, hub hook and all.
	loop, err := startWebhookDelivery(st, eventLog, hub, secrets, clock)
	if err != nil {
		t.Fatalf("startWebhookDelivery: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := loop.Shutdown(shutdownCtx); err != nil {
			t.Errorf("loop.Shutdown: %v", err)
		}
	})

	// --- what an operator does ------------------------------------------
	orgID := createFeederOrgNode(t, apiTS)
	endpointID := registerFeederWebhookEndpoint(t, apiTS, orgID, receiver.srv.URL)

	// The history the endpoint will owe. Ordinary authoring writes, each one
	// audited and filed at the endpoint's own scope node — recorded while the
	// endpoint has no secret, so none of them can be delivered yet.
	for i := 0; i < webhookE2EBacklog; i++ {
		renameFeederWebhookEndpoint(t, apiTS, endpointID, "Backlog Rename")
	}

	// Installing the signing secret is what arms the endpoint: until it lands
	// there is nothing to sign with and no delivery is defined.
	if resp, raw := doFeederReq(t, apiTS, http.MethodPost,
		"/api/v1/webhook-endpoints/"+endpointID+"/signing-secret",
		mustFeederJSON(t, map[string]any{"secret": webhookE2ESecret}), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("install signing secret: %d %s", resp.StatusCode, raw)
	}

	// One more ordinary authoring write — the single wake the whole backlog has
	// to drain on.
	renameFeederWebhookEndpoint(t, apiTS, endpointID, "Renamed Endpoint")

	// --- what must happen without anything else being called -------------
	var receipts []webhookReceipt
	deadline := time.After(10 * time.Second)
	for len(receipts) < webhookE2EBacklog {
		select {
		case r := <-receiver.accepted:
			receipts = append(receipts, r)
		case <-deadline:
			if len(receipts) == 0 {
				t.Fatal("no signed delivery reached the receiver: the feeder registered the endpoint, sealed its secret, and delivered nothing")
			}
			t.Fatalf("only %d of %d owed deliveries reached the receiver before the deadline; the backlog is trickling one per tick instead of draining",
				len(receipts), webhookE2EBacklog)
		}
	}

	seen := map[string]bool{}
	for i, r := range receipts {
		if r.DeliveryID == "" {
			t.Errorf("delivery %d carried no %s header", i, events.HeaderDeliveryID)
		}
		if r.Envelope.Schema != events.SchemaAuditEvent {
			t.Errorf("delivery %d schema = %q, want %q — the delivered body should be a real platform event",
				i, r.Envelope.Schema, events.SchemaAuditEvent)
		}
		if r.Envelope.ScopeNode != orgID {
			t.Errorf("delivery %d scope_node = %q, want the endpoint's own node %q (EVT-123 scope filtering)",
				i, r.Envelope.ScopeNode, orgID)
		}
		if seen[r.Envelope.ID] {
			t.Errorf("delivery %d redelivered event %s inside one drain — the cursor did not advance", i, r.Envelope.ID)
		}
		seen[r.Envelope.ID] = true
	}

	// The cursor is durable, not in-flight state: a restart must resume here.
	state, err := st.WebhookDeliveryStateFor(ctx, endpointID)
	if err != nil {
		t.Fatalf("WebhookDeliveryStateFor: %v", err)
	}
	if state.LastDeliveredID == "" {
		t.Fatal("the endpoint's persisted last_delivered_id is empty after two accepted deliveries; a restart would replay from nothing")
	}
	if !seen[state.LastDeliveredID] {
		// Deliveries continue draining behind this assertion, so the cursor may
		// legitimately have moved past the two receipts read above — but it must
		// never sit BELOW them.
		if state.LastDeliveredID < receipts[len(receipts)-1].Envelope.ID {
			t.Errorf("persisted last_delivered_id = %q, which sorts below the last accepted delivery %q",
				state.LastDeliveredID, receipts[len(receipts)-1].Envelope.ID)
		}
	}
}

// TestWebhookLoopShutdownDoesNotWaitOnAStalledReceiver pins the other half of
// the lifecycle: a receiver that accepts the connection and then never answers
// must cost the shutdown its deadline and not the process's ability to exit.
//
// The endpoint's per-attempt timeout is EVT-153's ten seconds, so a Shutdown
// that waited for the pass to finish on its own would block for that long. The
// assertion is that Shutdown returns its own context's error promptly instead —
// and the stalled handler is released by the delivery's request context being
// cancelled, which is the abort path under test, not by a timer.
func TestWebhookLoopShutdownDoesNotWaitOnAStalledReceiver(t *testing.T) {
	ctx := context.Background()
	clock := func() int64 { return webhookE2ENowMs }

	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eventLog, err := st.EventLog(events.DefaultRetentionPolicy(), clock, func(err error) {
		t.Errorf("event log failure: %v", err)
	})
	if err != nil {
		t.Fatalf("EventLog: %v", err)
	}
	hub := eventsse.NewHub(eventLog)

	wsKey, err := workspacekey.LoadOrCreate(t.TempDir(), ulid.New)
	if err != nil {
		t.Fatalf("workspacekey.LoadOrCreate: %v", err)
	}
	sealer, err := wsKey.SecretSealer()
	if err != nil {
		t.Fatalf("SecretSealer: %v", err)
	}
	secrets := webhookdeliver.NewSecrets(sealer)

	fixture, err := authtest.New(authtest.Config{NowMs: clock, Sink: hub, Sealer: sealer})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)
	feederAPIAuth = fixture

	apiTS := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, ulid.New,
		origin.New(), "https://192.0.2.12:7420", fixture.Auth,
		api.WithWebhookSecrets(secrets, webhookRotationOverlapMs)))
	t.Cleanup(apiTS.Close)

	// A receiver that reads the request and then answers only when its own
	// request context is cancelled. It signals that it is holding a delivery, so
	// the test shuts down at the one instant that actually exercises the case
	// rather than at a guessed one.
	stalled := make(chan struct{}, 1)
	stallTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		select {
		case stalled <- struct{}{}:
		default:
		}
		<-req.Context().Done()
	}))
	t.Cleanup(stallTS.Close)

	loop, err := startWebhookDelivery(st, eventLog, hub, secrets, clock)
	if err != nil {
		t.Fatalf("startWebhookDelivery: %v", err)
	}

	orgID := createFeederOrgNode(t, apiTS)
	endpointID := registerFeederWebhookEndpoint(t, apiTS, orgID, stallTS.URL)
	if resp, raw := doFeederReq(t, apiTS, http.MethodPost,
		"/api/v1/webhook-endpoints/"+endpointID+"/signing-secret",
		mustFeederJSON(t, map[string]any{"secret": webhookE2ESecret}), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("install signing secret: %d %s", resp.StatusCode, raw)
	}

	select {
	case <-stalled:
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery ever reached the stalling receiver")
	}

	// A zero-length budget: the pass is in flight and cannot finish, so this is
	// the deadline-expired branch by construction.
	shutdownCtx, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- loop.Shutdown(shutdownCtx) }()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("Shutdown reported a clean drain while a delivery was still stalled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked on a stalled receiver — a slow third party must not hold the feeder open")
	}
}

// --- helpers ---------------------------------------------------------------

// createFeederOrgNode mints the org node (the row that IS the workspace) an
// endpoint is registered under, over the real api/1 surface.
func createFeederOrgNode(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, raw := doFeederReq(t, ts, http.MethodPost, "/api/v1/scope-nodes",
		mustFeederJSON(t, map[string]any{"kind": "org", "name": "Webhook E2E Org", "account_state": "active"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint org node: %d %s", resp.StatusCode, raw)
	}
	return decodeFeederID(t, raw)
}

// registerFeederWebhookEndpoint registers an endpoint at scopeNode pointing at
// url, with no schemas restriction — "deliver me every event in my scope", the
// registration an operator makes when they want the audit trail.
func registerFeederWebhookEndpoint(t *testing.T, ts *httptest.Server, scopeNode, url string) string {
	t.Helper()
	resp, raw := doFeederReq(t, ts, http.MethodPost, "/api/v1/webhook-endpoints",
		mustFeederJSON(t, map[string]any{
			"name":       "E2E Receiver",
			"scope_node": scopeNode,
			"url":        url,
		}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register webhook endpoint: %d %s", resp.StatusCode, raw)
	}
	return decodeFeederID(t, raw)
}

// renameFeederWebhookEndpoint makes one ordinary conditional authoring write
// against the endpoint. Each call produces exactly one events/1 audit.event,
// filed at the endpoint's own scope node — an event that endpoint is entitled to
// receive. The GET is unaudited (only mutations are), so the count of events
// this produces is exactly the count of calls.
func renameFeederWebhookEndpoint(t *testing.T, ts *httptest.Server, endpointID, name string) {
	t.Helper()
	getResp, _ := doFeederReq(t, ts, http.MethodGet, "/api/v1/webhook-endpoints/"+endpointID, nil, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET endpoint for If-Match: %d", getResp.StatusCode)
	}
	resp, raw := doFeederReq(t, ts, http.MethodPatch, "/api/v1/webhook-endpoints/"+endpointID,
		mustFeederJSON(t, map[string]any{"name": name}),
		map[string]string{"If-Match": getResp.Header.Get("ETag")})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH endpoint: %d %s", resp.StatusCode, raw)
	}
}

func mustFeederJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func decodeFeederID(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode id from %s: %v", raw, err)
	}
	if body.ID == "" {
		t.Fatalf("response carried no id: %s", raw)
	}
	return body.ID
}
