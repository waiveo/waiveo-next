package webhookdeliver_test

// webhookdeliver_test.go drives the delivery loop against REAL in-process
// receiving servers: every assertion about a signature is made by the receiver
// recomputing the HMAC over the bytes it actually received, never by inspecting
// the request the sender built. A test that asserted "a call happened" would
// pass against a sender that signed with the wrong secret, the wrong material,
// or not at all.
//
// Every timing assertion moves an injected clock. There is no sleep anywhere in
// this file, and there is no outbound network: each receiver is an
// httptest.Server and the Deliverer is handed that server's own client.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/webhookdeliver"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/secretseal"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

const (
	orgNodeID    = "01J8Z3W1H0RG00000000000001"
	siteNodeID   = "01J8Z3W1H0S1TE0000000000A2"
	otherSiteID  = "01J8Z3W1H0S1TE0000000000B3"
	endpointAID  = "01J8Z3W1H0EP0000000000000A"
	endpointBID  = "01J8Z3W1H0EP0000000000000B"
	firstSecret  = "whsec_test_first_5f6e7a1b2c3d4e5f"
	secondSecret = "whsec_test_second_9a8b7c6d5e4f3a2b"

	// startMs is a fixed epoch-ms the injected clock starts at. Every deadline
	// this file asserts is an offset from it.
	startMs = int64(1752537600000)
)

// --- the receiver ----------------------------------------------------------

// receipt is one delivery a receiver actually accepted, recorded from the bytes
// on the wire.
type receipt struct {
	DeliveryID string
	EventID    string
	Body       []byte
}

// receiver is an in-process webhook receiver that VERIFIES rather than records.
//
// It holds the secret(s) an operator configured on its side and accepts a
// delivery only when one of them reproduces a signature the request carries —
// the same check a real receiver performs. accept and reject are counted
// separately so a test can tell "not called" from "called and refused", which
// is exactly the distinction the rotation cases turn on.
type receiver struct {
	mu       sync.Mutex
	secrets  []string
	status   int // the status code to answer an ACCEPTED delivery with
	accepted []receipt
	rejected int
	srv      *httptest.Server
}

func newReceiver(t *testing.T, status int, secrets ...string) *receiver {
	t.Helper()
	r := &receiver{secrets: secrets, status: status}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) serve(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ts := req.Header.Get(events.HeaderTimestamp)

	// A receiver accepts a delivery whose EITHER signature header reproduces
	// under a secret it holds: the ordinary one, or — while a rotation overlaps
	// — the prior-secret one.
	offered := []string{req.Header.Get(events.HeaderSignature)}
	if prior := req.Header.Get(events.HeaderPriorSignature); prior != "" {
		offered = append(offered, prior)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, secret := range r.secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "." + string(body)))
		want := hex.EncodeToString(mac.Sum(nil))
		for _, got := range offered {
			if got != "" && hmac.Equal([]byte(got), []byte(want)) {
				var env struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(body, &env)
				r.accepted = append(r.accepted, receipt{
					DeliveryID: req.Header.Get(events.HeaderDeliveryID),
					EventID:    env.ID,
					Body:       body,
				})
				w.WriteHeader(r.status)
				return
			}
		}
	}
	r.rejected++
	w.WriteHeader(http.StatusUnauthorized)
}

func (r *receiver) snapshot() ([]receipt, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receipt(nil), r.accepted...), r.rejected
}

func (r *receiver) acceptedIDs() []string {
	got, _ := r.snapshot()
	out := make([]string, 0, len(got))
	for _, rec := range got {
		out = append(out, rec.EventID)
	}
	return out
}

// --- the environment -------------------------------------------------------

type clock struct {
	mu sync.Mutex
	ms int64
}

func newClock() *clock { return &clock{ms: startMs} }
func (c *clock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}
func (c *clock) advance(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += ms
}

type env struct {
	t       *testing.T
	dir     string
	store   *store.Store
	log     *store.EventLog
	clock   *clock
	secrets *webhookdeliver.Secrets
	nextID  func() string
	errs    []string
	disable []string
}

// testSealer is the REAL sealing construction over a fixed key — never a stub,
// which would let every secret assertion here pass against an implementation
// that sealed nothing.
func testSealer(t *testing.T) *secretseal.Sealer {
	t.Helper()
	key := make([]byte, secretseal.KeySize)
	for i := range key {
		key[i] = byte(i*11 + 3)
	}
	s, err := secretseal.New(key)
	if err != nil {
		t.Fatalf("secretseal.New: %v", err)
	}
	return s
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	return openEnv(t, dir, newClock())
}

// openEnv opens a store over dir — a FILE-backed store, so a case can close it
// and open it again over the same bytes, which is what "survives a restart"
// has to mean.
func openEnv(t *testing.T, dir string, clk *clock) *env {
	t.Helper()
	st, err := store.Open(dir + "/workspace.sqlite")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log, err := store.OpenEventLog(st, events.DefaultRetentionPolicy(), clk.now, func(err error) {
		t.Errorf("event log: %v", err)
	})
	if err != nil {
		t.Fatalf("store.OpenEventLog: %v", err)
	}

	return &env{
		t: t, dir: dir, store: st, log: log, clock: clk,
		secrets: webhookdeliver.NewSecrets(testSealer(t)),
		nextID:  ulid.Monotonic(),
	}
}

func (e *env) seedTree() {
	e.t.Helper()
	ctx := e.t.Context()
	mk := func(body string) {
		if _, err := e.store.Create(ctx, store.KindScopeNode, []byte(body)); err != nil {
			e.t.Fatalf("create scope node: %v", err)
		}
	}
	mk(`{"id":"` + orgNodeID + `","kind":"org","name":"Root Org"}`)
	mk(`{"id":"` + siteNodeID + `","kind":"site","name":"Site A","parent_id":"` + orgNodeID + `","tz":"UTC","lat":47.6,"long":-122.3}`)
	mk(`{"id":"` + otherSiteID + `","kind":"site","name":"Site B","parent_id":"` + orgNodeID + `","tz":"UTC","lat":47.7,"long":-122.4}`)
}

// registerEndpoint creates the endpoint row and installs its first signing
// secret, which is what makes it eligible for delivery.
func (e *env) registerEndpoint(id, url, scopeNode, secret string, schemas ...string) {
	e.t.Helper()
	ctx := e.t.Context()
	body, err := json.Marshal(map[string]any{
		"id": id, "name": "Endpoint " + id, "scope_node": scopeNode,
		"url": url, "schemas": schemas,
	})
	if err != nil {
		e.t.Fatalf("marshal endpoint body: %v", err)
	}
	if _, err := e.store.Create(ctx, store.KindWebhookEndpoint, body); err != nil {
		e.t.Fatalf("create endpoint: %v", err)
	}
	e.installSecret(id, secret, "")
}

// installSecret seals secret as the endpoint's current signing secret. prior,
// when non-empty, is the secret being superseded — which the caller re-seals
// under the prior context, exactly as the api layer's rotate path does.
func (e *env) installSecret(id, secret, priorSealedCurrent string) {
	e.t.Helper()
	sealed, err := e.secrets.Seal(id, []byte(secret))
	if err != nil {
		e.t.Fatalf("seal secret: %v", err)
	}
	prior := ""
	if priorSealedCurrent != "" {
		prior, err = e.secrets.Reseal(id, priorSealedCurrent)
		if err != nil {
			e.t.Fatalf("reseal superseded secret: %v", err)
		}
	}
	if err := e.store.RotateWebhookSecret(e.t.Context(), id, sealed, prior, e.clock.now()); err != nil {
		e.t.Fatalf("RotateWebhookSecret: %v", err)
	}
}

// rotate performs the real two-step rotation: read the current sealed blob,
// re-seal it into the prior slot, install the new one.
func (e *env) rotate(id, newSecret string) {
	e.t.Helper()
	st, err := e.store.WebhookDeliveryStateFor(e.t.Context(), id)
	if err != nil {
		e.t.Fatalf("read delivery state before rotation: %v", err)
	}
	e.installSecret(id, newSecret, st.SealedSecret)
}

func (e *env) appendEvent(id, scopeNode, schema string) {
	e.t.Helper()
	e.log.Append(events.Envelope{
		ID: id, Schema: schema, TS: e.clock.now(), ScopeNode: scopeNode,
		TraceID: "01J8Z3W1H0TRACE00000000001", CostClass: "cheap",
		RetentionClass: "operational", Origin: "internal",
		Payload: json.RawMessage(`{}`),
	})
}

func (e *env) deliverer(t *testing.T, client *http.Client, cfg webhookdeliver.Config) *webhookdeliver.Deliverer {
	t.Helper()
	cfg.Store = e.store
	cfg.Log = e.log
	cfg.HTTP = client
	cfg.NowMs = e.clock.now
	cfg.NewID = e.nextID
	cfg.Secrets = e.secrets
	if cfg.OnError == nil {
		cfg.OnError = func(id string, err error) { e.errs = append(e.errs, id+": "+err.Error()) }
	}
	if cfg.OnDisabled == nil {
		cfg.OnDisabled = func(id, url string) { e.disable = append(e.disable, id) }
	}
	d, err := webhookdeliver.New(cfg)
	if err != nil {
		t.Fatalf("webhookdeliver.New: %v", err)
	}
	return d
}

func tick(t *testing.T, d *webhookdeliver.Deliverer) {
	t.Helper()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func eventID(n int) string {
	// Ascending, grammar-valid ULIDs: id order is delivery order (EVT-157).
	return "01J8Z3W1H0EV000000000000" + string(rune('A'+n/10)) + string(rune('0'+n%10))
}

// --- the cases -------------------------------------------------------------

// TestRegisteredEndpointReceivesSignedDeliveries is the end-to-end shape:
// registering an endpoint results in observed HTTP POSTs at a real receiving
// server, each carrying an EVT-151 signature the RECEIVER itself verifies,
// delivered in id order (EVT-157), one per pass.
func TestRegisteredEndpointReceivesSignedDeliveries(t *testing.T) {
	e := newEnv(t)
	e.seedTree()
	rcv := newReceiver(t, http.StatusOK, firstSecret)
	e.registerEndpoint(endpointAID, rcv.srv.URL, orgNodeID, firstSecret)

	for i := 0; i < 3; i++ {
		e.appendEvent(eventID(i), siteNodeID, events.SchemaAutomationRun)
	}

	d := e.deliverer(t, rcv.srv.Client(), webhookdeliver.Config{})
	for i := 0; i < 3; i++ {
		tick(t, d)
	}

	accepted, rejected := rcv.snapshot()
	if rejected != 0 {
		t.Fatalf("the receiver REJECTED %d deliveries; every delivery must verify under the endpoint's own signing secret (EVT-151)", rejected)
	}
	want := []string{eventID(0), eventID(1), eventID(2)}
	if got := rcv.acceptedIDs(); !equal(got, want) {
		t.Fatalf("accepted event ids = %v; want %v in id order (EVT-157)", got, want)
	}
	// Each logical delivery carries its own id (EVT-151), and the body the
	// receiver verified is the envelope itself.
	seen := map[string]bool{}
	for _, rec := range accepted {
		if rec.DeliveryID == "" {
			t.Fatal("a delivery arrived with no X-Waiveo-Delivery-Id")
		}
		if seen[rec.DeliveryID] {
			t.Fatalf("two distinct events shared one delivery id %q", rec.DeliveryID)
		}
		seen[rec.DeliveryID] = true
	}

	// The cursor is where delivery got to, so a further pass with nothing new
	// delivers nothing rather than replaying.
	tick(t, d)
	if got := rcv.acceptedIDs(); len(got) != 3 {
		t.Fatalf("a pass with an empty backlog delivered again: %v", got)
	}
}

// TestBackoffPacesRetriesAndDisablesAfterExhaustedDeliveries (EVT-153/154): a
// refusing receiver is retried on a capped exponential schedule — never on the
// pass immediately after a failure — and the endpoint is disabled once enough
// deliveries have exhausted their whole retry budget. Every boundary here is
// crossed by moving the injected clock.
func TestBackoffPacesRetriesAndDisablesAfterExhaustedDeliveries(t *testing.T) {
	e := newEnv(t)
	e.seedTree()
	// Answering 500 to a delivery it VERIFIED: the failure under test is the
	// receiver's, not a signature mismatch.
	rcv := newReceiver(t, http.StatusInternalServerError, firstSecret)
	e.registerEndpoint(endpointAID, rcv.srv.URL, orgNodeID, firstSecret)
	e.appendEvent(eventID(0), siteNodeID, events.SchemaAutomationRun)

	const base, cap = int64(1000), int64(4000)
	d := e.deliverer(t, rcv.srv.Client(), webhookdeliver.Config{
		MaxAttempts: 2,
		Endpoint: events.EndpointConfig{
			MaxConsecutiveFailures: 2,
			BackoffBaseMs:          base,
			BackoffCapMs:           cap,
			RotationOverlapMs:      events.DefaultRotationOverlapMs,
		},
	})

	attempts := func() int {
		got, _ := rcv.snapshot()
		return len(got)
	}
	// The receiver VERIFIED each attempt before answering 500, so its accepted
	// count is the attempt count.
	tick(t, d)
	if attempts() != 1 {
		t.Fatalf("first pass made %d attempts; want 1", attempts())
	}

	// Immediately after a failure the endpoint is behind its backoff gate: a
	// pass costs it nothing.
	tick(t, d)
	tick(t, d)
	if attempts() != 1 {
		t.Fatalf("a passing tick retried inside the backoff window: %d attempts; want 1 (EVT-153)", attempts())
	}

	// Crossing the first backoff releases exactly one more attempt, which
	// exhausts this logical delivery's budget of 2.
	e.clock.advance(base)
	tick(t, d)
	if attempts() != 2 {
		t.Fatalf("crossing the backoff made %d attempts; want 2", attempts())
	}
	st := mustState(t, e, endpointAID)
	if st.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d after ONE exhausted delivery; want 1 — the count is per exhausted delivery, not per attempt (EVT-154)", st.ConsecutiveFailures)
	}
	if st.Status != events.EndpointActive {
		t.Fatalf("status = %q after one exhausted delivery; want still active (bound is 2)", st.Status)
	}

	// The second delivery's two attempts trip the bound.
	e.clock.advance(cap)
	tick(t, d)
	e.clock.advance(cap)
	tick(t, d)
	st = mustState(t, e, endpointAID)
	if st.Status != events.EndpointDisabled {
		t.Fatalf("status = %q after 2 exhausted deliveries; want disabled (EVT-154). attempts=%d failures=%d", st.Status, attempts(), st.ConsecutiveFailures)
	}
	if len(e.disable) != 1 || e.disable[0] != endpointAID {
		t.Fatalf("operator-facing disable signal = %v; want exactly one for %s (EVT-154)", e.disable, endpointAID)
	}

	// A disabled endpoint receives no further attempts, however far the clock
	// moves (EVT-154).
	before := attempts()
	e.clock.advance(cap * 100)
	tick(t, d)
	tick(t, d)
	if attempts() != before {
		t.Fatalf("a disabled endpoint was still attempted: %d -> %d attempts", before, attempts())
	}

	// Nothing was ever delivered, so the cursor never moved: re-enabling
	// resumes at the same event rather than skipping it (EVT-155).
	if st.LastDeliveredID != "" {
		t.Fatalf("last_delivered_id = %q after only failures; want empty — a failed attempt must not advance the cursor", st.LastDeliveredID)
	}
}

// TestRotationOverlapIsAcceptedByEitherSecret (EVT-158): rotating an endpoint's
// signing secret does not interrupt delivery. A receiver that has NOT yet
// adopted the new secret keeps accepting for the length of the overlap window;
// a receiver that HAS adopted it accepts immediately; and once the window
// closes, the old secret stops working.
func TestRotationOverlapIsAcceptedByEitherSecret(t *testing.T) {
	e := newEnv(t)
	e.seedTree()

	const overlapMs = int64(60 * 60 * 1000)

	// oldRcv is the receiver still configured with ONLY the original secret —
	// the operator has not gotten to it yet. newRcv has already adopted the
	// rotated one. Two endpoints so both postures are exercised on the same
	// rotation timeline.
	oldRcv := newReceiver(t, http.StatusOK, firstSecret)
	newRcv := newReceiver(t, http.StatusOK, secondSecret)
	e.registerEndpoint(endpointAID, oldRcv.srv.URL, orgNodeID, firstSecret)
	e.registerEndpoint(endpointBID, newRcv.srv.URL, orgNodeID, firstSecret)

	d := e.deliverer(t, oldRcv.srv.Client(), webhookdeliver.Config{
		Endpoint: events.EndpointConfig{
			MaxConsecutiveFailures: events.DefaultMaxConsecutiveFailures,
			BackoffBaseMs:          1000,
			BackoffCapMs:           1000,
			RotationOverlapMs:      overlapMs,
		},
	})

	// Before the rotation the old secret is the only one, and only the receiver
	// holding it accepts.
	e.appendEvent(eventID(0), siteNodeID, events.SchemaAutomationRun)
	tick(t, d)
	if got := oldRcv.acceptedIDs(); !equal(got, []string{eventID(0)}) {
		t.Fatalf("pre-rotation delivery to the old-secret receiver = %v; want %v", got, []string{eventID(0)})
	}
	if _, rejected := newRcv.snapshot(); rejected != 1 {
		t.Fatalf("the not-yet-rotated secret was accepted by a receiver that does not hold it; rejected = %d", rejected)
	}

	e.rotate(endpointAID, secondSecret)
	e.rotate(endpointBID, secondSecret)

	// INSIDE the overlap: BOTH receivers accept the same rotation's deliveries.
	// The one still on the old secret verifies the prior-secret header; the one
	// that adopted the new secret verifies the ordinary EVT-151 header.
	e.clock.advance(overlapMs / 2)
	e.appendEvent(eventID(1), siteNodeID, events.SchemaAutomationRun)
	// Two passes: the failing/backed-off endpoint B needs its own gate cleared.
	e.clock.advance(1000)
	tick(t, d)
	tick(t, d)

	if got := oldRcv.acceptedIDs(); !equal(got, []string{eventID(0), eventID(1)}) {
		t.Fatalf("a receiver still holding only the PRIOR secret stopped accepting inside the overlap window: %v (EVT-158 requires rotation without a delivery gap)", got)
	}
	// Endpoint B's first delivery was REFUSED before the rotation (its receiver
	// never held the original secret), and a refused delivery does not advance a
	// cursor — so the rotation is what finally lets that same event through,
	// under the new secret, ahead of the newer one. That is EVT-157 holding
	// across a rotation, not a replay.
	if got := newRcv.acceptedIDs(); !equal(got, []string{eventID(0), eventID(1)}) {
		t.Fatalf("a receiver holding the ROTATED secret did not accept after the rotation: %v", got)
	}

	// PAST the overlap: the prior-secret header is no longer emitted, so the
	// receiver still holding only the old secret starts refusing — the old
	// secret has genuinely stopped working, not merely gone unused.
	e.clock.advance(overlapMs)
	e.appendEvent(eventID(2), siteNodeID, events.SchemaAutomationRun)
	_, beforeRejected := oldRcv.snapshot()
	tick(t, d)
	tick(t, d)

	if got := oldRcv.acceptedIDs(); !equal(got, []string{eventID(0), eventID(1)}) {
		t.Fatalf("the prior secret was still accepted past the overlap window: %v (EVT-158)", got)
	}
	if _, r := oldRcv.snapshot(); r <= beforeRejected {
		t.Fatal("past the overlap the old-secret receiver neither accepted nor rejected anything — the case proves nothing unless a delivery was actually attempted")
	}
	if got := newRcv.acceptedIDs(); !equal(got, []string{eventID(0), eventID(1), eventID(2)}) {
		t.Fatalf("the rotated secret stopped working past the overlap window: %v — the CURRENT secret always verifies", got)
	}
}

// TestAFailingEndpointDoesNotStallAHealthyOne: one endpoint refusing every
// delivery must not hold up the endpoint beside it. They share a pass, so a
// sender that retried the failing one inline — or that let a backoff block the
// loop — would starve the healthy one.
func TestAFailingEndpointDoesNotStallAHealthyOne(t *testing.T) {
	e := newEnv(t)
	e.seedTree()

	broken := newReceiver(t, http.StatusInternalServerError, firstSecret)
	healthy := newReceiver(t, http.StatusOK, secondSecret)
	// The BROKEN endpoint sorts first, so it takes its turn before the healthy
	// one on every pass — if a failure stalled the pass, the healthy endpoint
	// would be the one to starve.
	e.registerEndpoint(endpointAID, broken.srv.URL, orgNodeID, firstSecret)
	e.registerEndpoint(endpointBID, healthy.srv.URL, orgNodeID, secondSecret)

	for i := 0; i < 4; i++ {
		e.appendEvent(eventID(i), siteNodeID, events.SchemaAutomationRun)
	}

	d := e.deliverer(t, broken.srv.Client(), webhookdeliver.Config{
		MaxAttempts: 50,
		Endpoint: events.EndpointConfig{
			MaxConsecutiveFailures: 50,
			BackoffBaseMs:          1000,
			BackoffCapMs:           1000,
			RotationOverlapMs:      events.DefaultRotationOverlapMs,
		},
	})
	for i := 0; i < 4; i++ {
		tick(t, d)
		e.clock.advance(1000) // release the failing endpoint's backoff gate too
	}

	want := []string{eventID(0), eventID(1), eventID(2), eventID(3)}
	if got := healthy.acceptedIDs(); !equal(got, want) {
		t.Fatalf("the healthy endpoint received %v; want %v — a failing endpoint sharing the pass must not stall it", got, want)
	}
	// The failing endpoint is still stuck on its first event: a failed attempt
	// never advances the cursor (EVT-157).
	brokenSt := mustState(t, e, endpointAID)
	if brokenSt.LastDeliveredID != "" {
		t.Fatalf("the failing endpoint's cursor advanced to %q despite delivering nothing", brokenSt.LastDeliveredID)
	}
	if got, _ := broken.snapshot(); len(got) < 2 {
		t.Fatalf("the failing endpoint was attempted only %d times; the case proves nothing unless it was genuinely retried alongside", len(got))
	}
	for _, rec := range mustReceipts(broken) {
		if rec.EventID != eventID(0) {
			t.Fatalf("the failing endpoint was offered %s ahead of its unacknowledged head %s (EVT-157)", rec.EventID, eventID(0))
		}
	}
}

// TestDeliveryProgressSurvivesARestart: the log is durable, and so is the
// cursor. A process that delivered three events and died must not redeliver
// them all — nor skip what it had not reached.
func TestDeliveryProgressSurvivesARestart(t *testing.T) {
	clk := newClock()
	dir := t.TempDir()
	e := openEnv(t, dir, clk)
	e.seedTree()

	rcv := newReceiver(t, http.StatusOK, firstSecret)
	e.registerEndpoint(endpointAID, rcv.srv.URL, orgNodeID, firstSecret)
	for i := 0; i < 4; i++ {
		e.appendEvent(eventID(i), siteNodeID, events.SchemaAutomationRun)
	}

	d := e.deliverer(t, rcv.srv.Client(), webhookdeliver.Config{})
	tick(t, d)
	tick(t, d)
	if got := rcv.acceptedIDs(); !equal(got, []string{eventID(0), eventID(1)}) {
		t.Fatalf("pre-restart deliveries = %v; want the first two", got)
	}

	// The process goes away. A brand-new store handle, a brand-new event log
	// and a brand-new Deliverer come up over the same bytes.
	if err := e.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	restarted := openEnv(t, dir, clk)
	d2 := restarted.deliverer(t, rcv.srv.Client(), webhookdeliver.Config{})
	tick(t, d2)
	tick(t, d2)

	want := []string{eventID(0), eventID(1), eventID(2), eventID(3)}
	if got := rcv.acceptedIDs(); !equal(got, want) {
		t.Fatalf("post-restart deliveries = %v; want %v — the cursor must survive the process, replaying nothing and skipping nothing", got, want)
	}
}

// TestScopeFilteringKeepsOutOfScopeEventsOffTheWire (EVT-120/123/124): an
// endpoint placed at a site receives that site's events and not its sibling's,
// and a schemas restriction narrows further. Filtering is per event at delivery
// time, so it holds whatever the cursor resolution found.
func TestScopeFilteringKeepsOutOfScopeEventsOffTheWire(t *testing.T) {
	e := newEnv(t)
	e.seedTree()
	rcv := newReceiver(t, http.StatusOK, firstSecret)
	e.registerEndpoint(endpointAID, rcv.srv.URL, siteNodeID, firstSecret, events.SchemaAutomationRun)

	e.appendEvent(eventID(0), otherSiteID, events.SchemaAutomationRun) // wrong scope
	e.appendEvent(eventID(1), siteNodeID, events.SchemaContentPlayed)  // wrong schema
	e.appendEvent(eventID(2), siteNodeID, events.SchemaAutomationRun)  // delivered
	e.appendEvent(eventID(3), orgNodeID, events.SchemaAutomationRun)   // ABOVE the endpoint
	e.appendEvent(eventID(4), siteNodeID, events.SchemaAutomationRun)  // delivered

	d := e.deliverer(t, rcv.srv.Client(), webhookdeliver.Config{})
	for i := 0; i < 5; i++ {
		tick(t, d)
	}

	want := []string{eventID(2), eventID(4)}
	if got := rcv.acceptedIDs(); !equal(got, want) {
		t.Fatalf("delivered %v; want %v — an endpoint receives its own subtree, narrowed by its schemas list, and nothing else", got, want)
	}
	if _, rejected := rcv.snapshot(); rejected != 0 {
		t.Fatalf("%d deliveries failed verification", rejected)
	}
}

// TestDeletedEndpointStateIsPruned: deleting an endpoint goes through the
// generic resource handler, which knows nothing about the private state table.
// The pass reconciles, so a deleted endpoint's sealed signing secret does not
// outlive it.
func TestDeletedEndpointStateIsPruned(t *testing.T) {
	e := newEnv(t)
	e.seedTree()
	rcv := newReceiver(t, http.StatusOK, firstSecret)
	e.registerEndpoint(endpointAID, rcv.srv.URL, orgNodeID, firstSecret)

	d := e.deliverer(t, rcv.srv.Client(), webhookdeliver.Config{})
	tick(t, d)
	if _, err := e.store.WebhookDeliveryStateFor(t.Context(), endpointAID); err != nil {
		t.Fatalf("test setup invalid — no state to prune: %v", err)
	}

	res, ok, err := e.store.Get(t.Context(), store.KindWebhookEndpoint, endpointAID)
	if err != nil || !ok {
		t.Fatalf("read endpoint before delete: %v (ok=%v)", err, ok)
	}
	if err := e.store.Delete(t.Context(), store.KindWebhookEndpoint, endpointAID, res.Revision); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}

	tick(t, d)
	if _, err := e.store.WebhookDeliveryStateFor(t.Context(), endpointAID); err == nil {
		t.Fatal("a deleted endpoint's delivery state — and its sealed signing secret — survived")
	}
}

// TestAnEndpointWithNoSecretIsNeverDeliveredTo: a registered endpoint that has
// never been given a signing secret waits. An unsigned POST is not a delivery
// this contract defines (EVT-151), and sending one would be strictly worse than
// sending nothing.
func TestAnEndpointWithNoSecretIsNeverDeliveredTo(t *testing.T) {
	e := newEnv(t)
	e.seedTree()
	rcv := newReceiver(t, http.StatusOK, firstSecret)

	body, err := json.Marshal(map[string]any{
		"id": endpointAID, "name": "Unsecreted", "scope_node": orgNodeID,
		"url": rcv.srv.URL, "schemas": []string{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := e.store.Create(t.Context(), store.KindWebhookEndpoint, body); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	e.appendEvent(eventID(0), siteNodeID, events.SchemaAutomationRun)

	d := e.deliverer(t, rcv.srv.Client(), webhookdeliver.Config{})
	tick(t, d)
	tick(t, d)

	got, rejected := rcv.snapshot()
	if len(got) != 0 || rejected != 0 {
		t.Fatalf("an endpoint with no signing secret was POSTed to: %d accepted, %d rejected", len(got), rejected)
	}
}

// TestNewRefusesAWiringWithNoClock pins that the injected clock is required,
// not defaulted: a Deliverer that silently fell back to the wall clock would
// make every timing case in this file untestable and would do it quietly.
func TestNewRefusesAWiringWithNoClock(t *testing.T) {
	e := newEnv(t)
	if _, err := webhookdeliver.New(webhookdeliver.Config{
		Store: e.store, Log: e.log, HTTP: http.DefaultClient,
		NewID: ulid.Monotonic(), Secrets: e.secrets,
	}); err == nil {
		t.Fatal("New accepted a config with no clock; the wall clock must never be a silent default")
	}
	if _, err := webhookdeliver.New(webhookdeliver.Config{
		Store: e.store, Log: e.log, HTTP: http.DefaultClient,
		NowMs: e.clock.now, NewID: ulid.Monotonic(),
	}); err == nil {
		t.Fatal("New accepted a config with no secret opener; an endpoint must never be delivered to unsigned")
	}
}

// --- helpers ---------------------------------------------------------------

func mustState(t *testing.T, e *env, id string) store.WebhookDeliveryState {
	t.Helper()
	st, err := e.store.WebhookDeliveryStateFor(t.Context(), id)
	if err != nil {
		t.Fatalf("WebhookDeliveryStateFor(%s): %v", id, err)
	}
	return st
}

func mustReceipts(r *receiver) []receipt {
	got, _ := r.snapshot()
	return got
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
