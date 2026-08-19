package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/eventsse"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// The device-plane fixture the adopt cases below drive. Its ids are distinct
// from every other fixture's so a stray row from one cannot satisfy another.
const (
	auditDeviceID   = "01J8Z3AD1TDEV1CE0000000AA2"
	auditRelayID    = "relay-audit-fixture"
	auditDeviceSite = "01J8Z3AD1TS1TE00000000000A"
)

// audit_e2e_test.go drives the api/1 surface's audit trail end to end, through
// the SAME two doors a browser uses: the real authenticated api/1 mux, and a
// real /events/v1 SSE subscriber reading the events off the live stream.
//
// The stream is the oracle throughout. Not "the middleware called Emit" — that
// would prove a function was reached; these cases assert that a mutation made as
// a named principal SURFACES on the event stream, attributed to that principal,
// filed at the scope node of the resource it touched, and consequently visible
// to exactly the subscribers entitled to see that resource.

// auditEnv is a testEnv whose auth fixture publishes into a live event log, plus
// the /events/v1 SSE server reading from it. The api handler and the SSE server
// are two listeners over ONE hub — the same wiring cmd/waiveo-feeder builds.
type auditEnv struct {
	*testEnv
	hub    *eventsse.Hub
	events *httptest.Server
}

// newAuditEnv builds the whole loop. It deliberately does NOT reuse newEnv: that
// harness seeds an auth fixture with a nil sink (its auditor is silent), and a
// silent auditor is exactly what these cases must not be handed.
func newAuditEnv(t *testing.T, opts ...api.Option) *auditEnv {
	t.Helper()

	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hub := eventsse.NewHub(events.NewEventLog(0))
	t.Cleanup(hub.Close)

	clock := func() int64 { return fixedNowMs }
	fixture, err := authtest.New(authtest.Config{NowMs: clock, Sink: hub})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)

	jobs := api.NewJobRunner()
	content := origin.New()
	apiTS := httptest.NewServer(api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock,
		ulid.Monotonic(), content, testContentBase, fixture.Auth, append([]api.Option{api.WithJobRunner(jobs)}, opts...)...))
	t.Cleanup(apiTS.Close)

	// The SSE server resolves each subscriber's visible set against the SAME
	// scope tree the api handler authorizes writes against (EVT-120), so a
	// principal's reach over the stream and over the resources is one answer.
	eventsTS := httptest.NewServer(eventsse.New(hub, fixture.Auth, st.ScopeNodes))
	t.Cleanup(eventsTS.Close)

	return &auditEnv{
		testEnv: &testEnv{ts: apiTS, store: st, content: content, contentBase: testContentBase,
			auth: fixture, jobs: jobs},
		hub:    hub,
		events: eventsTS,
	}
}

// auditFrameWait bounds how long a case waits for an event to arrive on the
// stream. It is a ceiling on a real signal — the read completes as soon as the
// frame lands — never a sleep the case pauses for.
const auditFrameWait = 5 * time.Second

// subscribe opens an /events/v1 SSE connection as who, restricted to
// `audit.event` so the case reads its own subject rather than filtering the
// whole stream by hand. A fresh connection delivers from-now (EVT-132), so it is
// opened BEFORE the mutation it is meant to observe.
func (e *auditEnv) subscribe(t *testing.T, who authtest.Credential) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.events.URL+"/events/v1?schemas=audit.event", nil)
	if err != nil {
		cancel()
		t.Fatalf("building SSE request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	who.Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("dialing /events/v1: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		resp.Body.Close()
		t.Fatalf("/events/v1 must answer 200 for an authenticated subscriber; got %d", resp.StatusCode)
	}
	return bufio.NewReader(resp.Body), func() { cancel(); resp.Body.Close() }
}

// auditRecord is the decoded payload of one `audit.event`, paired with the
// envelope placement that decides who may see it.
type auditRecord struct {
	scopeNode string
	Actor     string `json:"actor_principal"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Result    string `json:"result"`
}

// readAudit reads the next `audit.event` off the stream, failing the case if
// none arrives within auditFrameWait. The wait is on the read itself, so a
// missing emission is a hard failure rather than a hang.
func readAudit(t *testing.T, br *bufio.Reader) auditRecord {
	t.Helper()
	type result struct {
		data string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var data string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- result{"", err}
				return
			}
			if line == "\n" {
				if data != "" {
					ch <- result{data, nil}
					return
				}
				continue
			}
			if s := strings.TrimPrefix(strings.TrimRight(line, "\n"), "data: "); s != strings.TrimRight(line, "\n") {
				data = s
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("reading an audit.event off /events/v1: %v", r.err)
		}
		var env events.Envelope
		if err := json.Unmarshal([]byte(r.data), &env); err != nil {
			t.Fatalf("decoding the SSE envelope: %v", err)
		}
		if env.Schema != events.SchemaAuditEvent {
			t.Fatalf("the schemas filter must deliver only audit.event; got %q", env.Schema)
		}
		// EVT-013's gate is what a subscriber's own consumer relies on: a record
		// that would not validate is a producer bug, and asserting it here means
		// a malformed emission fails loudly instead of streaming quietly.
		if err := events.Validate(env); err != nil {
			t.Fatalf("an emitted audit.event must satisfy the EVT-013 delivery gate: %v", err)
		}
		var rec auditRecord
		if err := json.Unmarshal(env.Payload, &rec); err != nil {
			t.Fatalf("decoding the audit.event payload: %v", err)
		}
		rec.scopeNode = env.ScopeNode
		return rec
	case <-time.After(auditFrameWait):
		t.Fatalf("no audit.event reached /events/v1 within %s", auditFrameWait)
		return auditRecord{}
	}
}

// TestAudit_MutationStreamsWithRealPrincipalAttribution is the core case: a
// mutating api/1 request made as principal A produces an observable audit.event
// on the live stream, attributed to A — not to a fixed deployment identity, and
// not to whoever happened to seed the tree.
//
// It drives a PATCH rather than a create so the assertion covers the ordinary
// update path, and it makes the request as a principal that is NOT the env's
// root-bound seeder, so "the actor is A" cannot be satisfied by accident.
func TestAudit_MutationStreamsWithRealPrincipalAttribution(t *testing.T) {
	e := newAuditEnv(t)
	tr := seedScopedTree(t, e.testEnv)
	alice := e.principalAt(t, tr.siteA)

	screen := tr.screensA[0]
	etag := e.etagOf(t, alice, "/api/v1/scope-nodes/"+screen)

	// Subscribed as the ROOT-bound fixture principal, which can read every
	// placement — so this case observes attribution alone, with scope filtering
	// held out of it (that is the next case's subject).
	br, closeConn := e.subscribe(t, e.auth.Credential())
	defer closeConn()

	resp, raw := e.as(t, alice, http.MethodPatch, "/api/v1/scope-nodes/"+screen,
		[]byte(`{"name":"Renamed By Alice"}`), map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH scope-node = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	rec := readAudit(t, br)
	if rec.Actor != alice.PrincipalID {
		t.Fatalf("audit.event actor_principal must be the REAL authenticated caller\n got %q\nwant %q",
			rec.Actor, alice.PrincipalID)
	}
	if rec.Result != events.AuditResultSuccess {
		t.Fatalf("a mutation that returned 200 must record result=success; got %q", rec.Result)
	}
	if want := "scope-nodes:" + screen; rec.Target != want {
		t.Fatalf("audit.event target must name the affected resource (EVT-080)\n got %q\nwant %q",
			rec.Target, want)
	}
	if rec.Action != "scope-nodes.update" {
		t.Fatalf("audit.event action must name the act performed; got %q", rec.Action)
	}
	// EVT-012: the record is filed at the SUBJECT's own placement. A scope node
	// is placed at itself.
	if rec.scopeNode != screen {
		t.Fatalf("audit.event scope_node must be the subject resource's own placement (EVT-012)\n got %q\nwant %q",
			rec.scopeNode, screen)
	}
}

// TestAudit_SubjectPlacementDrivesStreamVisibility is the placement case, and it
// is the reason EVT-012 matters here rather than being a formality: because the
// stream filters per event against each subscriber's visible set (EVT-120/123),
// a wrongly-placed audit record is either invisible to the operator responsible
// for the resource or visible to one with no authority over it.
//
// Two subscribers, bound at two sibling sites, watch the same stream while a
// mutation lands under Site A. The Site A subscriber must receive it; the Site B
// subscriber must not — and "must not" is proven by a SECOND mutation under Site
// B that B does receive, so the negative is a filtering decision rather than a
// stream that was simply empty or slow.
func TestAudit_SubjectPlacementDrivesStreamVisibility(t *testing.T) {
	e := newAuditEnv(t)
	tr := seedScopedTree(t, e.testEnv)
	alice := e.principalAt(t, tr.siteA)
	bob := e.principalAt(t, tr.siteB)

	aliceStream, closeA := e.subscribe(t, alice)
	defer closeA()
	bobStream, closeB := e.subscribe(t, bob)
	defer closeB()

	// A mutation under Site A, made by Alice.
	screenA := tr.screensA[0]
	etagA := e.etagOf(t, alice, "/api/v1/scope-nodes/"+screenA)
	resp, raw := e.as(t, alice, http.MethodPatch, "/api/v1/scope-nodes/"+screenA,
		[]byte(`{"name":"Alice Renamed A"}`), map[string]string{"If-Match": etagA})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH under Site A = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	// A mutation under Site B, made by Bob.
	screenB := tr.screensB[0]
	etagB := e.etagOf(t, bob, "/api/v1/scope-nodes/"+screenB)
	resp, raw = e.as(t, bob, http.MethodPatch, "/api/v1/scope-nodes/"+screenB,
		[]byte(`{"name":"Bob Renamed B"}`), map[string]string{"If-Match": etagB})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH under Site B = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	// Alice's first delivered audit record must be her own Site A mutation: the
	// Site B record was appended to the same log and filtered away for her.
	got := readAudit(t, aliceStream)
	if got.scopeNode != screenA {
		t.Fatalf("a Site A subscriber's first audit.event must be the Site A mutation; got one at %q", got.scopeNode)
	}
	if got.Actor != alice.PrincipalID {
		t.Fatalf("Site A record actor = %q, want %q", got.Actor, alice.PrincipalID)
	}

	// Bob's first delivered audit record must be the Site B mutation — proving
	// the Site A record was WITHHELD from him rather than merely not yet
	// delivered: it was appended first, so anything unfiltered would arrive
	// first.
	got = readAudit(t, bobStream)
	if got.scopeNode != screenB {
		t.Fatalf("a Site B subscriber must not receive the Site A audit record (EVT-120/123); got one at %q",
			got.scopeNode)
	}
	if got.Actor != bob.PrincipalID {
		t.Fatalf("Site B record actor = %q, want %q", got.Actor, bob.PrincipalID)
	}
}

// TestAudit_RefusedMutationIsRecordedInFull drives EVT-083 directly: a mutation
// REFUSED at authorization is as auditable as one that succeeded, and carries
// every field a success does — never elided for having failed.
//
// This is the case the store's post-commit hook could not have produced at all:
// the write is turned away before any transaction opens, so nothing downstream
// of the commit ever learns it was attempted. An audit trail recording only
// successful mutations would show this probe as silence.
func TestAudit_RefusedMutationIsRecordedInFull(t *testing.T) {
	e := newAuditEnv(t)
	tr := seedScopedTree(t, e.testEnv)
	// Bound at Site B only — so a write against Site A's subtree is refused.
	bob := e.principalAt(t, tr.siteB)

	br, closeConn := e.subscribe(t, e.auth.Credential())
	defer closeConn()

	// Bob cannot even READ the Site A screen, so this is answered 404 rather than
	// 403 (scopeview.go) — the refusal an unaddressable row draws. Either way it
	// is a mutation attempt that must be recorded.
	screenA := tr.screensA[0]
	resp, _ := e.as(t, bob, http.MethodPatch, "/api/v1/scope-nodes/"+screenA,
		[]byte(`{"name":"Bob Should Not"}`), nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a write against another site's subtree must be refused; got 200")
	}

	rec := readAudit(t, br)
	if rec.Result != events.AuditResultFailure {
		t.Fatalf("a refused mutation must record result=failure (EVT-083); got %q", rec.Result)
	}
	if rec.Actor != bob.PrincipalID {
		t.Fatalf("a refused mutation must still name the principal that attempted it\n got %q\nwant %q",
			rec.Actor, bob.PrincipalID)
	}
	// EVT-083: target is still required on a failure — a failed action is never
	// elided of the resource it was aimed at.
	if want := "scope-nodes:" + screenA; rec.Target != want {
		t.Fatalf("a failure must carry the target it was aimed at (EVT-083)\n got %q\nwant %q",
			rec.Target, want)
	}
	if rec.Action != "scope-nodes.update" {
		t.Fatalf("a refused mutation must name the act attempted; got %q", rec.Action)
	}
}

// TestAudit_CreateRecordsServerMintedIdAndPlacement covers the create path,
// whose subject does not exist when the request arrives: the id is minted by the
// server mid-handler, and the placement only becomes readable once the row
// lands. A record naming an empty target here would satisfy the schema's shape
// while being useless to an operator.
func TestAudit_CreateRecordsServerMintedIdAndPlacement(t *testing.T) {
	e := newAuditEnv(t)
	tr := seedScopedTree(t, e.testEnv)
	alice := e.principalAt(t, tr.siteA)
	screen := tr.screensA[0]

	br, closeConn := e.subscribe(t, e.auth.Credential())
	defer closeConn()

	body, err := json.Marshal(map[string]any{
		"scope_node": screen,
		"name":       "Audited Playlist",
		"items":      []any{},
	})
	if err != nil {
		t.Fatalf("marshal playlist: %v", err)
	}
	resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/playlists", body, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST playlist = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decoding the created playlist: %v", err)
	}
	if created.ID == "" {
		t.Fatal("the create response carried no server-minted id")
	}

	rec := readAudit(t, br)
	if want := "playlists:" + created.ID; rec.Target != want {
		t.Fatalf("a create must record the SERVER-MINTED id (EVT-080)\n got %q\nwant %q", rec.Target, want)
	}
	if rec.Action != "playlists.create" {
		t.Fatalf("audit.event action = %q, want %q", rec.Action, "playlists.create")
	}
	// A scheduling row is placed at its own scope_node — the screen, not the
	// deployment node.
	if rec.scopeNode != screen {
		t.Fatalf("a created row's audit record must be filed at the row's placement (EVT-012)\n got %q\nwant %q",
			rec.scopeNode, screen)
	}
	if rec.Actor != alice.PrincipalID {
		t.Fatalf("create record actor = %q, want %q", rec.Actor, alice.PrincipalID)
	}
}

// TestAudit_ActionStylePostIsAudited covers the mutating POSTs that are not
// CRUD: a per-row action (automations/{id}/run) and a collection-wide one
// (automations/bulk-enable). Both change fleet state without being a create,
// update or delete, and both are exactly the kind of route a per-handler audit
// call gets forgotten on.
func TestAudit_ActionStylePostIsAudited(t *testing.T) {
	e := newAuditEnv(t)
	tr := seedScopedTree(t, e.testEnv)
	alice := e.principalAt(t, tr.siteA)
	screen := tr.screensA[0]

	automationID := e.createAutomation(t, alice, screen, map[string]string{"fleet": "audited"})

	br, closeConn := e.subscribe(t, e.auth.Credential())
	defer closeConn()

	resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/automations/"+automationID+"/run", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST automations/{id}/run = %d (body %s)", resp.StatusCode, raw)
	}

	rec := readAudit(t, br)
	if rec.Action != "automations.run" {
		t.Fatalf("a per-row action POST must record its own act; got %q", rec.Action)
	}
	if want := "automations:" + automationID; rec.Target != want {
		t.Fatalf("run record target = %q, want %q", rec.Target, want)
	}
	if rec.scopeNode != screen {
		t.Fatalf("a run record must be filed at the automation's placement; got %q, want %q", rec.scopeNode, screen)
	}
	if rec.Actor != alice.PrincipalID {
		t.Fatalf("run record actor = %q, want %q", rec.Actor, alice.PrincipalID)
	}

	// The collection-wide action: no single row is its subject, so it records the
	// bare resource type as its target rather than a fabricated id.
	_, raw = e.bulkEnable(t, alice, "fleet=audited", false)
	rec = readAudit(t, br)
	if rec.Action != "automations.bulk-enable" {
		t.Fatalf("a collection-wide action POST must record its own act; got %q (bulk body %s)", rec.Action, raw)
	}
	if rec.Target != "automations" {
		t.Fatalf("a fleet-wide action names the collection as its target; got %q", rec.Target)
	}
}

// TestAudit_AuthFlowRecordsStillEmit is the regression guard on the work this
// builds beside: the auth package's own SEC-150 records must keep emitting, from
// their own handlers, and must NOT be duplicated by the api middleware. A logout
// is one act and belongs in the trail once.
func TestAudit_AuthFlowRecordsStillEmit(t *testing.T) {
	e := newAuditEnv(t)
	tr := seedScopedTree(t, e.testEnv)
	alice := e.principalAt(t, tr.siteA)

	br, closeConn := e.subscribe(t, e.auth.Credential())
	defer closeConn()

	resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/auth/logout", nil, nil)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST auth/logout = %d (body %s)", resp.StatusCode, raw)
	}

	rec := readAudit(t, br)
	if rec.Action != auth.ActionSessionRevoked {
		t.Fatalf("a logout must record the auth package's own session.revoked (SEC-150); got %q", rec.Action)
	}
	if rec.Actor != alice.PrincipalID {
		t.Fatalf("logout record actor = %q, want %q", rec.Actor, alice.PrincipalID)
	}

	// The next record on the stream must NOT be a second, middleware-generated
	// record for the same logout. Driving an unrelated mutation and asserting IT
	// arrives next proves the logout produced exactly one record — a duplicate
	// would be delivered ahead of it.
	seeder := e.auth.Credential()
	node := e.createNode(t, screenNode("", tr.siteA, ""))
	next := readAudit(t, br)
	if next.Action != "scope-nodes.create" {
		t.Fatalf("a logout must produce exactly ONE audit record; the next event was %q, not the following mutation",
			next.Action)
	}
	if want := "scope-nodes:" + node; next.Target != want {
		t.Fatalf("follow-up record target = %q, want %q", next.Target, want)
	}
	if next.Actor != seeder.PrincipalID {
		t.Fatalf("follow-up record actor = %q, want the seeding principal %q", next.Actor, seeder.PrincipalID)
	}
}

// TestAudit_AdoptRecordsTheDeviceAndItsPlacement is the audit half of the one
// operation that puts a physical device under this platform's control.
//
// It failed on all three counts before the route was classified: `devices` is
// not a mounted CRUD family, so the request fell to the generic fallback, whose
// id was the LAST path segment. Every adoption of every device therefore
// recorded as `devices.create` against the literal target `devices:adopt`, with
// no scope-node placement at all — so the record could not say what was
// adopted, called it a create, and filed at the deployment fallback rather than
// at the device's own node, which is what EVT-012/EVT-120 stream visibility is
// decided by. The subscriber below is the site's own operator, and that is the
// assertion that would have caught the placement half: a record about her
// fleet that she cannot see is not an audit trail.
func TestAudit_AdoptRecordsTheDeviceAndItsPlacement(t *testing.T) {
	registry := devices.New(auditDeviceSite, func() int64 { return 0 })
	e := newAuditEnv(t, api.WithDevicePlane(registry, nil))
	tr := seedScopedTree(t, e.testEnv)
	alice := e.principalAt(t, tr.siteA)

	node := tr.screensA[0]
	mustPutDevice(t, registry, devices.Device{
		ID: auditDeviceID, RelayID: auditRelayID, DeviceClass: "media-player",
		Name: "Audited TV", ScopeNode: node, Labels: map[string]string{},
		Address: "192.168.50.31:8060",
	})
	if _, err := e.store.ReplaceDiscoveredDevices(context.Background(), auditRelayID, []store.DiscoveredDevice{{
		DeviceID: auditDeviceID, RelayID: auditRelayID, ScopeNode: node,
		Driver: "roku-ecp", NativeID: "uuid:roku:ecp:AUDIT1", DeviceClass: "media-player",
		Name: "Audited TV", Address: "192.168.50.31:8060", FirstSeen: 1000, LastSeen: 2000,
		Entities: []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}}); err != nil {
		t.Fatalf("mirror the discovered device: %v", err)
	}

	// Subscribed as the operator whose site the device sits at, not as the
	// unrestricted fixture: the placement is only observable through someone
	// whose visible set it has to fall inside.
	br, closeConn := e.subscribe(t, alice)
	defer closeConn()

	resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/devices/"+auditDeviceID+"/adopt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST adopt = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	rec := readAudit(t, br)
	if rec.Action != "devices.adopt" {
		t.Errorf("action = %q, want \"devices.adopt\" — an adoption is not a create", rec.Action)
	}
	if want := "devices:" + auditDeviceID; rec.Target != want {
		t.Errorf("target = %q, want %q — the record must name WHICH device was adopted", rec.Target, want)
	}
	if rec.scopeNode != node {
		t.Errorf("record filed at %q, want the device's own placement %q — EVT-012 placement is what decides which "+
			"operators can see the record (EVT-120/123)", rec.scopeNode, node)
	}
	if rec.Actor != alice.PrincipalID {
		t.Errorf("actor = %q, want %q", rec.Actor, alice.PrincipalID)
	}
	if rec.Result != events.AuditResultSuccess {
		t.Errorf("result = %q, want success", rec.Result)
	}
}

// TestAudit_ARefusedAdoptIsRecordedAgainstTheSameSubject: EVT-083 makes a
// refusal exactly as auditable as a success, and an adoption someone was not
// allowed to make is the more interesting of the two. The subject must still be
// the device — a refusal recorded against `devices:adopt` tells an investigator
// nothing about what was attempted.
func TestAudit_ARefusedAdoptIsRecordedAgainstTheSameSubject(t *testing.T) {
	registry := devices.New(auditDeviceSite, func() int64 { return 0 })
	e := newAuditEnv(t, api.WithDevicePlane(registry, nil))
	tr := seedScopedTree(t, e.testEnv)

	node := tr.screensA[0]
	mustPutDevice(t, registry, devices.Device{
		ID: auditDeviceID, RelayID: auditRelayID, DeviceClass: "media-player",
		Name: "Audited TV", ScopeNode: node, Labels: map[string]string{},
	})
	// Viewer where the device is, operator elsewhere: clears the method's role
	// floor, refused by the per-node write check.
	mixed := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer}, roleAt{tr.siteB, auth.RoleOperator})

	br, closeConn := e.subscribe(t, e.auth.Credential())
	defer closeConn()

	resp, raw := e.as(t, mixed, http.MethodPost, "/api/v1/devices/"+auditDeviceID+"/adopt", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST adopt as a read-only principal = %d, want 403 (body %s)", resp.StatusCode, raw)
	}

	rec := readAudit(t, br)
	if rec.Action != "devices.adopt" {
		t.Errorf("action = %q, want \"devices.adopt\"", rec.Action)
	}
	if want := "devices:" + auditDeviceID; rec.Target != want {
		t.Errorf("target = %q, want %q", rec.Target, want)
	}
	if rec.Result != events.AuditResultFailure {
		t.Errorf("result = %q, want failure — a refused mutation is recorded in full (EVT-083)", rec.Result)
	}
}

// TestAudit_RetiringAFirstSeenNamesTheActAndTheDevice is the audit half of the
// only operation on this surface that DESTROYS a durable value.
//
// `first_seen` is planted once and never moves; retiring it is the single
// sanctioned way one leaves, and the ledger keeps no copy of what went — so the
// audit record is the entire remaining account of the act. It shipped with none.
// `devices` is not a mounted CRUD family and no arm recognised a three-segment
// DELETE, so the request fell to the generic default, which spells
// `<family>.<verb>` and takes the LAST segment as the subject. Measured on the
// real router before the fix:
//
//	action="devices.delete" id="first-seen" registry=""
//
// Three separate falsehoods in one record: it names an act this surface does not
// have (no device was deleted), it names a path segment where a device id
// belongs, and with no registry the placement cannot be resolved, so it files at
// the deployment fallback — invisible to the very operator whose fleet it
// changed. The operation carries `mcp:act`, so an agent can run it across a whole
// inventory; sixty-four byte-identical records naming no device is the worst
// possible trail for the one act that re-dates a fleet as new.
//
// The subscriber is deliberately the SITE's operator rather than the
// unrestricted fixture, because that is the assertion the placement half turns
// on: a record about her fleet that she cannot see is not an audit trail
// (EVT-012/120/123).
func TestAudit_RetiringAFirstSeenNamesTheActAndTheDevice(t *testing.T) {
	registry := devices.New(auditDeviceSite, func() int64 { return 0 })
	e := newAuditEnv(t, api.WithDevicePlane(registry, nil))
	tr := seedScopedTree(t, e.testEnv)
	alice := e.principalAt(t, tr.siteA)

	node := tr.screensA[0]
	mustPutDevice(t, registry, devices.Device{
		ID: auditDeviceID, RelayID: auditRelayID, DeviceClass: "media-player",
		Name: "Audited TV", ScopeNode: node, Labels: map[string]string{},
		Address: "192.168.50.31:8060",
	})
	if _, err := e.store.ReplaceDiscoveredDevices(context.Background(), auditRelayID, []store.DiscoveredDevice{{
		DeviceID: auditDeviceID, RelayID: auditRelayID, ScopeNode: node,
		Driver: "roku-ecp", NativeID: "uuid:roku:ecp:AUDIT1", DeviceClass: "media-player",
		Name: "Audited TV", Address: "192.168.50.31:8060", LastSeen: 2000,
		Entities: []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}}); err != nil {
		t.Fatalf("mirror the discovered device: %v", err)
	}

	br, closeConn := e.subscribe(t, alice)
	defer closeConn()

	resp, raw := e.as(t, alice, http.MethodDelete, "/api/v1/devices/"+auditDeviceID+"/first-seen", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE first-seen = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	rec := readAudit(t, br)
	if rec.Action == "devices.delete" {
		t.Fatalf("retiring an age records `devices.delete`, which describes DELETING THE DEVICE — an act this " +
			"surface does not have")
	}
	if rec.Action != "devices.retire-first-seen" {
		t.Errorf("action = %q, want \"devices.retire-first-seen\"", rec.Action)
	}
	if rec.Target == "devices:first-seen" {
		t.Fatalf("the record's subject is the literal path segment, so the only account of a destroyed durable " +
			"value cannot say WHICH device lost it")
	}
	if want := "devices:" + auditDeviceID; rec.Target != want {
		t.Errorf("target = %q, want %q", rec.Target, want)
	}
	if rec.scopeNode != node {
		t.Errorf("record filed at %q, want the device's own placement %q — filed at the deployment fallback it is "+
			"invisible to the operator whose fleet it changed (EVT-012/120/123)", rec.scopeNode, node)
	}
	if rec.Actor != alice.PrincipalID {
		t.Errorf("actor = %q, want %q", rec.Actor, alice.PrincipalID)
	}
	if rec.Result != events.AuditResultSuccess {
		t.Errorf("result = %q, want success", rec.Result)
	}

	// The sibling DELETE on this family had the identical defect and the identical
	// fix; asserted here so the two cannot drift apart again.
	resp, raw = e.as(t, alice, http.MethodDelete, "/api/v1/devices/"+auditDeviceID+"/ignore", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE ignore = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	unignore := readAudit(t, br)
	if unignore.Action != "devices.unignore" {
		t.Errorf("un-ignoring records %q, want \"devices.unignore\"", unignore.Action)
	}
	if want := "devices:" + auditDeviceID; unignore.Target != want {
		t.Errorf("un-ignore target = %q, want %q", unignore.Target, want)
	}
	if unignore.scopeNode != node {
		t.Errorf("un-ignore filed at %q, want the device's placement %q", unignore.scopeNode, node)
	}
}
