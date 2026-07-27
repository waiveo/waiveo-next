package eventsse

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
)

// scope_filter_test.go drives events/1's scope-node filtering (EVT-120–124) and
// the two query parameters that carry it on the SSE binding (EVT-101), through
// the REAL handler over real HTTP, with a real scope-node tree and two really-
// differently-bound principals.
//
// The fixture is deliberately two-sided. A test that only proved "alice receives
// her own events" would pass against a handler that filters nothing at all, so
// the oracle here is always that BOB'S events are provably ABSENT from alice's
// stream — while bob's own connection simultaneously proves those same events
// were genuinely deliverable, so every absence is about authorization and never
// about a lost append.
//
// # How the negative assertions avoid a timing guess
//
// "This event never arrives" needs an endpoint, or it degenerates into a sleep.
// Two endpoints are used, both real signals:
//
//   - LIVE: a trailing SENTINEL event placed where the reading principal CAN see
//     it. The log delivers in id order (EVT-011) and the sentinel is appended
//     last, so once it arrives every event that was ever going to precede it
//     already has. A suppressed event is an absence at a definite point.
//   - BACKLOG: Hub.Close() BEFORE the connection is dialed. The handler writes
//     the whole resolved backlog and flushes it before it ever reaches the live
//     select, and that select then returns immediately on the closed done
//     channel — so the response body is exactly the filtered backlog followed by
//     EOF. Nothing is timed; the stream ends on its own.

// The fixture tree: one org with two sibling sites, a screen under each. A site
// carries all three geo columns (DAT-031); an org carries none (DAT-032).
const (
	orgNode     = "01J8Z5A0B1C2D3E4F5G6H7Z0RG"
	siteANode   = "01J8Z5A0B1C2D3E4F5G6H7Z5A0"
	siteBNode   = "01J8Z5A0B1C2D3E4F5G6H7Z5B0"
	screenANode = "01J8Z5A0B1C2D3E4F5G6H7ZCA0"
	screenBNode = "01J8Z5A0B1C2D3E4F5G6H7ZCB0"
	// absentNode is a syntactically valid ULID naming a scope node the tree does
	// NOT contain — a selector term that must read exactly like a real but
	// out-of-reach node (EVT-122), never like an error that would confirm the
	// difference.
	absentNode = "01J8Z5A0B1C2D3E4F5G6H7ZAB0"
)

// Envelope ids used across this file. seedID sorts below every other id, so it
// is a usable resume_from anchor whose own backlog is "everything else".
var (
	seedID     = idPrefix + "Y1"
	sentinelID = idPrefix + "Z9"
)

func scopeFixtureNodes() []datamodel.ScopeNode {
	str := func(s string) *string { return &s }
	f := func(v float64) *float64 { return &v }
	return []datamodel.ScopeNode{
		{ID: orgNode, Kind: "org", Name: "org", Revision: 1},
		{ID: siteANode, Kind: "site", ParentID: str(orgNode), Name: "site A", TZ: str("UTC"), Lat: f(1), Long: f(2), Revision: 1},
		{ID: siteBNode, Kind: "site", ParentID: str(orgNode), Name: "site B", TZ: str("UTC"), Lat: f(3), Long: f(4), Revision: 1},
		{ID: screenANode, Kind: "screen", ParentID: str(siteANode), Name: "screen A", Revision: 1},
		{ID: screenBNode, Kind: "screen", ParentID: str(siteBNode), Name: "screen B", Revision: 1},
	}
}

// scopedEnv is an automation.run envelope placed at an explicit scope node — the
// only envelope field scope-node filtering consults (EVT-012 sets it at recording
// time to the subject resource's own placement; EVT-010 calls it "the sole input
// to scope-node filtering").
func scopedEnv(id, scopeNode string) events.Envelope {
	env := autoEnv(id)
	env.ScopeNode = scopeNode
	return env
}

// scopedEnvSchema is scopedEnv with an explicit schema, for the EVT-124 cases.
func scopedEnvSchema(id, scopeNode, schema string) events.Envelope {
	env := scopedEnv(id, scopeNode)
	env.Schema = schema
	return env
}

// scopeEnv is one scope-filtering test's world: a real auth store holding two
// principals bound at two DIFFERENT sites, the shared event log, and the live
// handler mounted over the fixture tree.
type scopeEnv struct {
	hub   *Hub
	srv   *httptest.Server
	alice authtest.Credential
	bob   authtest.Credential
}

func newScopeEnv(t *testing.T) *scopeEnv {
	t.Helper()
	// alice is the fixture's built-in principal, bound at site A; bob is a second
	// principal at site B. Both are `viewer` — the weakest role that reads at all
	// — so nothing here passes by virtue of an over-broad role.
	fixture, err := authtest.New(authtest.Config{Role: auth.RoleViewer, ScopeNode: siteANode})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	t.Cleanup(fixture.Close)
	bob, err := fixture.AddPrincipal(authtest.Config{Role: auth.RoleViewer, ScopeNode: siteBNode})
	if err != nil {
		t.Fatalf("authtest.AddPrincipal: %v", err)
	}

	nodes := scopeFixtureNodes()
	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(New(hub, fixture.Auth, func(context.Context) ([]datamodel.ScopeNode, error) {
		return nodes, nil
	}))
	t.Cleanup(srv.Close)

	return &scopeEnv{hub: hub, srv: srv, alice: fixture.Credential(), bob: bob}
}

// request builds an authenticated SSE GET for cred with the given raw query.
func (e *scopeEnv) request(t *testing.T, cred authtest.Credential, query string) *http.Request {
	t.Helper()
	target := e.srv.URL + "/events/v1"
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	// EVT-111: a browser's native EventSource cannot set custom headers, so the
	// SSE binding authenticates by the session cookie — which is what this
	// presents. EVT-112: never a query-string credential.
	cred.Authorize(req)
	return req
}

// dialAs opens a LIVE SSE stream, asserting the 200 / text/event-stream
// handshake. The caller MUST call the returned closer.
func (e *scopeEnv) dialAs(t *testing.T, cred authtest.Credential, query string) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := e.request(t, cred, query).WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("dialing SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		resp.Body.Close()
		t.Fatalf("the stream must open 200 (EVT-100); got %d", resp.StatusCode)
	}
	return bufio.NewReader(resp.Body), func() { cancel(); resp.Body.Close() }
}

// backlogIDs is the deterministic negative harness. The caller has already
// appended the world; this closes the Hub, dials a RESUMED connection (so the
// whole retained tail is written as the resolved backlog), and reads the body to
// EOF — returning every envelope id the handler actually delivered.
//
// Closing the Hub first is what removes every timing question: the handler
// writes and flushes the resolved backlog BEFORE it reaches the live select, and
// that select then returns at once on the already-closed done channel. The body
// is therefore exactly "the filtered backlog, then EOF". No timer decides when
// the stream is finished — the stream finishes.
func (e *scopeEnv) backlogIDs(t *testing.T, cred authtest.Credential, query string) []string {
	t.Helper()
	e.hub.Close()

	q := "resume_from=" + seedID
	if query != "" {
		q += "&" + query
	}
	resp, err := http.DefaultClient.Do(e.request(t, cred, q))
	if err != nil {
		t.Fatalf("dialing SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("the stream must open 200 — an out-of-reach selector term MUST NOT be surfaced as an error (EVT-122); got %d %s",
			resp.StatusCode, body)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the backlog stream to EOF: %v", err)
	}
	var ids []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimPrefix(line, "id: "))
		}
	}
	return ids
}

// appendWorld records the shared interleaved A/B world: a resume anchor alice can
// read, then four events alternating between the two subtrees, then a sentinel in
// alice's own subtree.
func (e *scopeEnv) appendWorld() {
	e.hub.Append(scopedEnv(seedID, screenANode))
	e.hub.Append(scopedEnv(idA, screenANode)) // alice
	e.hub.Append(scopedEnv(idB, screenBNode)) // bob
	e.hub.Append(scopedEnv(idC, siteANode))   // alice
	e.hub.Append(scopedEnv(idD, siteBNode))   // bob
	e.hub.Append(scopedEnv(sentinelID, screenANode))
}

// collectIDs reads frames from a LIVE stream until it has seen sentinel,
// returning every envelope id delivered up to and including it. See this file's
// header for why the sentinel makes that deterministic; the duration is a hang
// guard, not a timing assumption.
func collectIDs(t *testing.T, br *bufio.Reader, sentinel string) []string {
	t.Helper()
	var ids []string
	for {
		f := readFrameWithin(t, br, 3*time.Second)
		if f.event != "event" {
			t.Fatalf("expected only event frames on this stream; got event=%q data=%s", f.event, f.data)
		}
		ids = append(ids, f.id)
		if f.id == sentinel {
			return ids
		}
	}
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestSSE_LiveStreamCarriesOnlyTheVisibleScopeNodes is EVT-120/123's oracle on
// the live path: two principals watch the SAME Hub across the SAME appends, and
// each receives exactly its own subtree. The event alice must not see is, at the
// same instant, proven deliverable by bob receiving it.
func TestSSE_LiveStreamCarriesOnlyTheVisibleScopeNodes(t *testing.T) {
	e := newScopeEnv(t)

	aliceBR, closeAlice := e.dialAs(t, e.alice, "")
	defer closeAlice()
	bobBR, closeBob := e.dialAs(t, e.bob, "")
	defer closeBob()

	// Interleaved, so neither stream can pass by delivering a contiguous prefix.
	e.hub.Append(scopedEnv(idA, screenANode)) // alice only
	e.hub.Append(scopedEnv(idB, screenBNode)) // bob only
	e.hub.Append(scopedEnv(idC, siteANode))   // alice only
	e.hub.Append(scopedEnv(idD, siteBNode))   // bob only
	// The org node is ABOVE both bindings. SEC-010 inherits downward, never
	// upward, so this one is invisible to everybody.
	e.hub.Append(scopedEnv(idPrefix+"Z0", orgNode))
	// A per-side sentinel, each in its own subtree.
	e.hub.Append(scopedEnv(idPrefix+"ZA", screenANode))
	e.hub.Append(scopedEnv(idPrefix+"ZB", screenBNode))

	aliceIDs := collectIDs(t, aliceBR, idPrefix+"ZA")
	bobIDs := collectIDs(t, bobBR, idPrefix+"ZB")

	if want := []string{idA, idC, idPrefix + "ZA"}; !reflect.DeepEqual(aliceIDs, want) {
		t.Fatalf("the site-A principal must receive exactly its own subtree's events (EVT-120/123)\n got %v\nwant %v", aliceIDs, want)
	}
	if want := []string{idB, idD, idPrefix + "ZB"}; !reflect.DeepEqual(bobIDs, want) {
		t.Fatalf("the site-B principal must receive exactly its own subtree's events (EVT-120/123)\n got %v\nwant %v", bobIDs, want)
	}
	for _, hidden := range []string{idB, idD, idPrefix + "Z0"} {
		if contains(aliceIDs, hidden) {
			t.Fatalf("event %s is outside the site-A principal's readable set and MUST NOT be delivered (EVT-120); stream was %v", hidden, aliceIDs)
		}
	}
	if contains(bobIDs, idPrefix+"Z0") {
		t.Fatalf("an event placed at an ANCESTOR of the principal's binding must not be delivered (SEC-010 inherits downward); stream was %v", bobIDs)
	}
}

// TestSSE_ReplayedBacklogIsScopeFilteredToo pins the same boundary on the
// RESUMED path. EVT-120 says "an event whose scope_node falls outside that set
// MUST NOT be delivered" without qualifying live vs replayed, and a leaking
// backlog would be the more dangerous half: a subscriber could name a
// resume_from and pull down history it was never allowed to watch.
func TestSSE_ReplayedBacklogIsScopeFilteredToo(t *testing.T) {
	e := newScopeEnv(t)
	e.appendWorld()

	ids := e.backlogIDs(t, e.alice, "")
	if want := []string{idA, idC, sentinelID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("a resumed backlog must carry only the visible-set events after the cursor (EVT-120)\n got %v\nwant %v", ids, want)
	}

	// And bob's resume of the same log proves the suppressed events were in the
	// retained backlog all along.
	e2 := newScopeEnv(t)
	e2.appendWorld()
	bobIDs := e2.backlogIDs(t, e2.bob, "")
	if want := []string{idB, idD}; !reflect.DeepEqual(bobIDs, want) {
		t.Fatalf("the site-B principal's resumed backlog must carry its own subtree's events\n got %v\nwant %v", bobIDs, want)
	}
}

// TestSSE_SelectorNarrowsWithinReach drives EVT-101's `selector` parameter doing
// real work: a subtree term inside the principal's own reach delivers the named
// node and its descendants (API-044) and excludes a sibling placement the
// principal could otherwise see.
func TestSSE_SelectorNarrowsWithinReach(t *testing.T) {
	e := newScopeEnv(t)
	e.appendWorld()

	ids := e.backlogIDs(t, e.alice, "selector="+url.QueryEscape("scope_node subtree "+screenANode))
	// idA and sentinelID are at screen A; idC is at site A — visible to alice,
	// but ABOVE the named subtree, so the selector genuinely removed it.
	if want := []string{idA, sentinelID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("a subtree term must deliver the named node's events and exclude a placement above it (API-044/EVT-121)\n got %v\nwant %v", ids, want)
	}
}

// TestSSE_SelectorCannotWidenTheVisibleSet is EVT-121's rule: a selector naming a
// node ABOVE the principal's own binding selects the whole tree on its own terms,
// yet delivers only the part the principal could already see — the intersection,
// never the union.
func TestSSE_SelectorCannotWidenTheVisibleSet(t *testing.T) {
	e := newScopeEnv(t)
	e.appendWorld()

	ids := e.backlogIDs(t, e.alice, "selector="+url.QueryEscape("scope_node subtree "+orgNode))
	if want := []string{idA, idC, sentinelID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("a subtree term naming an ancestor ABOVE the caller's binding must narrow, never widen (EVT-121)\n got %v\nwant %v", ids, want)
	}
	for _, hidden := range []string{idB, idD} {
		if contains(ids, hidden) {
			t.Fatalf("selecting the org must not deliver %s from the other subtree (EVT-121)", hidden)
		}
	}
}

// TestSSE_OutOfReachSelectorTermMatchesNothingAndNeverErrors is EVT-122 verbatim:
// a term resolving outside the readable set "MUST simply match nothing for that
// term, exactly as an ordinary empty-result filter would — it MUST NOT be
// surfaced as an error", because an error would let a selector probe for the
// existence of scope nodes the principal cannot read.
//
// Both halves are asserted, and both matter. backlogIDs fails the test on any
// status other than 200, so a 400 or 403 (or a 404, or any Problem at all) is
// caught — including the tempting "that node does not exist" answer, which is the
// probe. And the delivered set must be EMPTY, so a widening implementation is
// caught too. A REAL out-of-reach node and a NEVER-MINTED one must be
// indistinguishable, which is why both appear in the table.
func TestSSE_OutOfReachSelectorTermMatchesNothingAndNeverErrors(t *testing.T) {
	for name, selector := range map[string]string{
		"other subtree, subtree form": "scope_node subtree " + siteBNode,
		"other subtree, exact form":   "scope_node=" + screenBNode,
		"ancestor above the binding":  "scope_node=" + orgNode,
		"never-minted node, subtree":  "scope_node subtree " + absentNode,
		"never-minted node, exact":    "scope_node=" + absentNode,
	} {
		t.Run(name, func(t *testing.T) {
			e := newScopeEnv(t)
			e.appendWorld()
			if ids := e.backlogIDs(t, e.alice, "selector="+url.QueryEscape(selector)); len(ids) != 0 {
				t.Fatalf("selector %q resolves outside the readable set: it must match nothing (EVT-122); delivered %v", selector, ids)
			}
		})
	}
}

// TestSSE_SchemasParameterFilters drives EVT-101's `schemas` parameter and
// EVT-124: it restricts delivery to the listed schemas, ALONGSIDE scope-node
// filtering rather than in place of it. Both accepted spellings of the list are
// driven, plus a list carrying an unrecognized name (a member no event matches,
// which is an empty result rather than an error).
func TestSSE_SchemasParameterFilters(t *testing.T) {
	for name, query := range map[string]string{
		"single value":    "schemas=" + url.QueryEscape(events.SchemaAutomationRun),
		"comma-separated": "schemas=" + url.QueryEscape(events.SchemaAutomationRun+","+events.SchemaBoxVitals),
		"repeated param":  "schemas=" + url.QueryEscape(events.SchemaAutomationRun) + "&schemas=" + url.QueryEscape(events.SchemaBoxVitals),
		"unknown member":  "schemas=" + url.QueryEscape(events.SchemaAutomationRun+",no.such_schema"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newScopeEnv(t)
			e.hub.Append(scopedEnvSchema(seedID, screenANode, events.SchemaAutomationRun))
			// Visible to alice, but a schema outside every list above.
			e.hub.Append(scopedEnvSchema(idA, screenANode, events.SchemaContentPlayed))
			// Visible AND in every list.
			e.hub.Append(scopedEnvSchema(idB, screenANode, events.SchemaAutomationRun))
			// In the list for the multi-schema cases, but placed in bob's
			// subtree: EVT-124's "alongside, never in place of" — matching the
			// schemas filter buys no visibility.
			e.hub.Append(scopedEnvSchema(idC, screenBNode, events.SchemaBoxVitals))

			ids := e.backlogIDs(t, e.alice, query)
			if want := []string{idB}; !reflect.DeepEqual(ids, want) {
				t.Fatalf("%s: schemas must restrict to the listed schemas AND keep scope-node filtering (EVT-124)\n got %v\nwant %v", name, ids, want)
			}
		})
	}
}

// TestSSE_SchemasFiltersTheLiveTailToo: EVT-123 applies the restriction per event
// at delivery time, so the live path is filtered exactly as the backlog is.
func TestSSE_SchemasFiltersTheLiveTailToo(t *testing.T) {
	e := newScopeEnv(t)
	br, closeConn := e.dialAs(t, e.alice, "schemas="+url.QueryEscape(events.SchemaAutomationRun))
	defer closeConn()

	e.hub.Append(scopedEnvSchema(idA, screenANode, events.SchemaContentPlayed))
	e.hub.Append(scopedEnvSchema(idB, screenBNode, events.SchemaAutomationRun))
	e.hub.Append(scopedEnvSchema(sentinelID, screenANode, events.SchemaAutomationRun))

	if ids := collectIDs(t, br, sentinelID); !reflect.DeepEqual(ids, []string{sentinelID}) {
		t.Fatalf("the live tail must apply both restrictions per event (EVT-123/124); got %v", ids)
	}
}

// TestSSE_SchemasListMatchingNothingIsEmptyNotAnError: a list naming only
// unrecognized schemas yields an empty stream. EVT-124 defines membership, not a
// registry check, and answering with an error would be the same existence oracle
// EVT-122 forbids in the sibling parameter.
func TestSSE_SchemasListMatchingNothingIsEmptyNotAnError(t *testing.T) {
	e := newScopeEnv(t)
	e.appendWorld()
	if ids := e.backlogIDs(t, e.alice, "schemas=no.such_schema"); len(ids) != 0 {
		t.Fatalf("a schemas list matching nothing must yield an empty stream; delivered %v", ids)
	}
}

// TestSSE_EmptySchemasParameterImposesNoRestriction: `schemas=` present but
// carrying no name is treated as absent. The alternative reading — "restrict to
// the empty set" — turns a trailing `&schemas=` in a hand-built URL into a
// silently dead stream, the worse failure for a parameter whose whole job is
// optional narrowing.
func TestSSE_EmptySchemasParameterImposesNoRestriction(t *testing.T) {
	e := newScopeEnv(t)
	e.appendWorld()
	if ids := e.backlogIDs(t, e.alice, "schemas="); !reflect.DeepEqual(ids, []string{idA, idC, sentinelID}) {
		t.Fatalf("an empty schemas parameter must impose no restriction; got %v", ids)
	}
}

// TestSSE_UnparseableSelectorIsRejectedBeforeTheStream: a selector that does not
// PARSE is 400 / SELECTOR_INVALID (API-045), written as an api/1 Problem before
// any stream begins. The distinction from EVT-122 is the whole point — a syntax
// error is a statement about the REQUEST and reveals nothing about which scope
// nodes exist, so it may be an error where an out-of-reach node may not.
//
// The status is 400, not 422: API-013a puts a body-validation failure at 422 and
// a QUERY-parameter failure at 400, and a selector is a query parameter.
func TestSSE_UnparseableSelectorIsRejectedBeforeTheStream(t *testing.T) {
	e := newScopeEnv(t)

	// value is the OFFENDING TERM the detail must name — which for a trailing
	// comma is the empty term the split produced, not the whole selector string.
	for name, tc := range map[string]struct{ selector, wantTerm string }{
		"whitespace around equality": {"kind = screen", "kind = screen"},
		"unterminated set":           {"kind in (screen", "kind in (screen"},
		"scope_node not a ULID":      {"scope_node=not-a-ulid", "scope_node=not-a-ulid"},
		"empty trailing term":        {"kind=screen,", `""`},
	} {
		selector := tc.selector
		resp, err := http.DefaultClient.Do(e.request(t, e.alice, "selector="+url.QueryEscape(selector)))
		if err != nil {
			t.Fatalf("%s: request: %v", name, err)
		}
		var problem map[string]any
		decodeErr := json.NewDecoder(resp.Body).Decode(&problem)
		resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: an unparseable selector is a query-parameter failure = 400 (API-013a/API-045); got %d", name, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("%s: the refusal must be written BEFORE the stream opens, never as an SSE frame; Content-Type %q", name, ct)
		}
		if decodeErr != nil {
			t.Fatalf("%s: the refusal must be an api/1 Problem document: %v", name, decodeErr)
		}
		if problem["code"] != "SELECTOR_INVALID" {
			t.Fatalf("%s: Problem code = %v, want SELECTOR_INVALID (API-045)", name, problem["code"])
		}
		// API-045 requires the offending TERM be identified in detail.
		if detail, _ := problem["detail"].(string); !strings.Contains(detail, tc.wantTerm) {
			t.Fatalf("%s: API-045 requires the Problem detail name the offending term %q; got %q", name, tc.wantTerm, detail)
		}
	}
}

// TestSSE_UnresolvableVisibleSetRefusesRatherThanStreamsEverything is SEC-005's
// never-default-permit rule at this seam: when the scope tree cannot be read the
// handler must refuse, not fall back to an unfiltered stream. The failure mode
// this guards is the tempting one — "the tree read failed, so skip filtering".
func TestSSE_UnresolvableVisibleSetRefusesRatherThanStreamsEverything(t *testing.T) {
	fixture, err := authtest.New(authtest.Config{Role: auth.RoleViewer, ScopeNode: siteANode})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	defer fixture.Close()

	hub := NewHub(events.NewEventLog(0))
	hub.Append(scopedEnv(idA, screenANode))
	srv := httptest.NewServer(New(hub, fixture.Auth, func(context.Context) ([]datamodel.ScopeNode, error) {
		return nil, errScopeTreeUnavailable{}
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/events/v1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	fixture.Credential().Authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("an unresolvable visible set must refuse the connection, never stream unfiltered (SEC-005); got %d", resp.StatusCode)
	}
	if strings.Contains(string(body), "event: event") {
		t.Fatalf("the refusal must not deliver a single event; body was %s", body)
	}
}

// errScopeTreeUnavailable is the injected tree-read failure the SEC-005 case
// drives.
type errScopeTreeUnavailable struct{}

func (errScopeTreeUnavailable) Error() string { return "scope tree unavailable" }
