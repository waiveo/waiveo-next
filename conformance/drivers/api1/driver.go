// Package api1 is the executable api/1 conformance driver: the §10
// differential oracle for the cross-cutting request conventions every
// management-door operation in api/openapi.yaml follows. It covers both
// halves of the contract's conventions:
//
//   - the synchronous conventions — optimistic concurrency (ETag/If-Match,
//     API-020-025), keyset pagination (opaque cursor + limit, API-030-036),
//     the label-selector grammar (API-040-046), and the client-assignable
//     external_id convention (API-100-104);
//   - the asynchronous conventions — Idempotency-Key replay/reuse/in-progress
//     (API-050-056) and the 202 + Job resource (API-110-117, API-120-123);
//   - the Problem error shape itself (API-010-016).
//
// It replays every conformance/corpora/api-1 case against the LIVE,
// HTTP-mounted internal/app/api handler (api.New, over a real
// internal/app/store.Store and internal/shared/apihttp.IdempotencyStore) and
// diffs the actual HTTP behavior against each case's own declared `expected`
// block.
//
// This driver deliberately mounts the same *http.Handler production wires
// (api.New) rather than calling the convention helpers (apihttp/apiselector/
// apijob) directly: a prior version of this driver did the latter, and a
// 2026-07-26 audit found it certified the convention LIBRARIES, not the
// shipped /api/v1 surface — no conformance driver in the repo imported a
// single internal/app/ package. Driving the real handler is not cosmetic: it
// surfaced several genuine divergences between the frozen corpus and the
// shipped code that the old, library-level driver could not see (see the
// per-case doc comments below and pendingCaseIDs/knownDivergent).
//
// A corpus case is not named to a helper by hard-coded case-id knowledge:
// classifyShape inspects the case's own `input` block and dispatches to the
// matching family purely from its structure — the same shape distinctions a
// real router would use to decide which convention governs a given request.
package api1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

const contract = "api/1"

// conformanceEntity is the fixture entity ULID every seeded automation's state
// trigger/device_command action names (the same value internal/app/api's own
// automations_test.go fixtures use) so a seeded automation body actually
// compiles (rules/1 RUL-282: the trigger subject and the command target are
// the same entity).
const conformanceEntity = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"

// Run loads the frozen api-1 corpus from disk and drives every convention
// case against the live, HTTP-mounted api.New handler.
func Run() report.Report {
	rep := report.Report{Driver: "api1", Target: "internal/app/api (api.New), internal/app/store"}

	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load api-1 corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against an explicit,
// caller-supplied case set. It is the seam the teeth-tests use: load the real
// corpus, corrupt one case's `expected` block in memory, and confirm the SAME
// comparison logic (never a re-implementation of it) reports FAIL against the
// corrupted expectation.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "api1", Target: "internal/app/api (api.New), internal/app/store"}
	driveCases(&rep, cases)
	return rep
}

// drivenCaseIDs are every api-1 corpus case this driver drives against the
// live HTTP handler — every case in the frozen corpus except API-121, which
// has no mounted route to drive (see pendingCaseIDs).
var drivenCaseIDs = []string{
	"API-010", "API-013",
	"API-022", "API-023", "API-032", "API-044", "API-045", "API-102",
	"API-052", "API-053", "API-111",
}

// pendingCaseIDs are api/1 cases frozen under conformance/corpora/api-1 that
// no mounted route exists to drive yet.
var pendingCaseIDs = map[string]string{
	"API-121": "api.New's mux (internal/app/api/api.go) mounts no /api/v1/workspace/export route — the 2026-07-26 reconciliation audit confirms it is one of several openapi-declared paths (/jobs/{job_id}, /workspace/export, /workspace/delete, /auth/login, /principals, /devices, /entities, /casts, /system/health, /packs/{pack}/actions/{name}) with no handler in the tree. Deferred until that route is built (plan G8).",
}

func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	for _, short := range drivenCaseIDs {
		driveByShape(rep, cases, short)
	}
	for _, short := range sortedKeys(pendingCaseIDs) {
		c, ok := corpus.ByID(cases, short)
		if !ok {
			rep.Fail(short, contract, "case not found in frozen corpus")
			continue
		}
		rep.Pending(c.CaseID, contract, pendingCaseIDs[short])
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// driveByShape looks up the named case, classifies its `input` block's shape,
// and dispatches to the matching helper family. A shape the driver does not
// recognize is a driver FAIL, not a silent skip.
func driveByShape(rep *report.Report, cases map[string]corpus.Case, short string) {
	c, ok := corpus.ByID(cases, short)
	if !ok {
		rep.Fail(short, contract, "case not found in frozen corpus")
		return
	}

	switch classifyShape(c.CaseID, c.Input) {
	case shapeProblemNotFound:
		driveProblemNotFound(rep, c)
	case shapeProblemValidation:
		driveProblemValidation(rep, c)
	case shapeConcurrency:
		driveConcurrency(rep, c)
	case shapePagination:
		drivePagination(rep, c)
	case shapeSelector:
		driveSelector(rep, c)
	case shapeExternalID:
		driveExternalID(rep, c)
	case shapeIdempotency:
		driveIdempotency(rep, c)
	case shapeJob:
		driveJob(rep, c)
	default:
		rep.Fail(c.CaseID, contract, "input block did not match any known api/1 convention shape")
	}
}

// shape names the convention family a corpus case's `input` block structurally
// belongs to.
type shape int

const (
	shapeUnknown shape = iota
	shapeProblemNotFound
	shapeProblemValidation
	shapeConcurrency
	shapePagination
	shapeSelector
	shapeExternalID
	shapeIdempotency
	shapeJob
)

// classifyShape inspects a case's own id and `input` block and reports which
// convention family it belongs to. API-010/013 are named explicitly (their
// input shape — a bare method+path+headers, or a bare method+path+body — is
// not otherwise distinguishable from a job-mutation shape without the case's
// own req_ids); every other case is classified purely from its input's
// structure, as a real router would dispatch on request shape.
func classifyShape(caseID string, input map[string]any) shape {
	switch caseID {
	case "API-010-valid-simple-problem":
		return shapeProblemNotFound
	case "API-013-valid-multi-field-validation-problem":
		return shapeProblemValidation
	}
	if _, ok := input["current_resource_state"]; ok {
		return shapeConcurrency
	}
	if reqs, ok := input["requests"].([]any); ok {
		if _, ok := input["collection_state"]; ok {
			return shapePagination
		}
		if requestsCarryIdempotencyKey(reqs) {
			return shapeIdempotency
		}
		return shapeUnknown
	}
	if req, ok := input["request"].(map[string]any); ok {
		if q, ok := req["query"].(map[string]any); ok {
			if _, ok := q["selector"]; ok {
				return shapeSelector
			}
		}
		return shapeUnknown
	}
	if body, ok := input["body"].(map[string]any); ok {
		if _, ok := body["external_id"]; ok {
			if _, ok := input["collection_state"]; ok {
				return shapeExternalID
			}
		}
	}
	if _, ok := input["method"].(string); ok {
		if _, ok := input["path"].(string); ok {
			return shapeJob
		}
	}
	return shapeUnknown
}

func requestsCarryIdempotencyKey(reqs []any) bool {
	if len(reqs) == 0 {
		return false
	}
	for _, r := range reqs {
		m, ok := r.(map[string]any)
		if !ok {
			return false
		}
		h, ok := m["headers"].(map[string]any)
		if !ok {
			return false
		}
		if _, ok := h["Idempotency-Key"]; !ok {
			return false
		}
	}
	return true
}

// --- the live harness -------------------------------------------------------

// harness mounts the SAME http.Handler production wires (api.New) over a real,
// in-memory internal/app/store.Store and a real apihttp.IdempotencyStore under
// an injected clock. Every case drives requests through h.do — the exact
// dispatch a real client's request takes, never a hand-built http.HandlerFunc.
type harness struct {
	store *store.Store
	h     http.Handler
}

// newHarness opens a fresh in-memory store and mounts api.New over it. nowMs
// is the fixed clock every Idempotency-Key / Job timestamp in this harness's
// lifetime is stamped with — never the wall clock — so a case's outcome is
// reproducible.
func newHarness(nowMs int64) (*harness, error) {
	st, err := store.Open(":memory:")
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	clock := func() int64 { return nowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	h := api.New(st, idem, clock, origin.New(), "https://origin.example")
	return &harness{store: st, h: h}, nil
}

func (h *harness) close() { _ = h.store.Close() }

// result is the decoded outcome of one request driven through the live
// handler.
type result struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

// do drives one HTTP request through the live handler — no network, but the
// exact http.Handler a real listener would dispatch to.
func (h *harness) do(method, path string, body []byte, headers map[string]string) result {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return result{status: rec.Code, header: rec.Header(), body: decoded, raw: rec.Body.Bytes()}
}

// seedScopeNode writes a scope-node row directly into the store (fixture
// setup, not the operation under test — the same pattern internal/app/api's
// own e2e tests use: seed via the store, drive the case under test through the
// live handler). fields is marshaled as-is as the row body.
func (h *harness) seedScopeNode(fields map[string]any) error {
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = h.store.Create(context.Background(), store.KindScopeNode, b)
	return err
}

// seedAutomation writes a compile-clean edge-automation row directly into the
// store (fixture setup). Every seeded automation shares the same trigger/
// action shape (a state trigger rising to "on" firing a device_command on
// conformanceEntity) so it clears the rules/1 compile-gate every write path
// runs — the corpus's own collection_state fixtures for the automations kind
// carry only the api/1-relevant fields (id/scope_node/labels/name), never a
// full rule, since they were designed against the convention libraries
// directly; this driver supplies the rest of a valid rule so the SAME
// api/1-relevant fields can be seeded through the real, compile-gated store
// write path.
func (h *harness) seedAutomation(id, scopeNode string, labels map[string]string) error {
	m := map[string]any{
		"id":         id,
		"name":       "Conformance Fixture Automation",
		"scope_node": scopeNode,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "state", "entity_id": conformanceEntity, "to": []string{"on"}}},
		"conditions": []any{},
		"actions":    []any{map[string]any{"type": "device_command", "entity_id": conformanceEntity, "command": "launch"}},
	}
	if labels != nil {
		m["labels"] = labels
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = h.store.Create(context.Background(), store.KindAutomation, b)
	return err
}

// --- api/1's own Problem error shape (API-010-016) --------------------------

// driveProblemNotFound drives API-010: GET on a scope-node id that does not
// exist. The prior version of this driver recorded this case PENDING,
// reasoning "a scope-node NOT_FOUND GET route ... does not exist yet" — a
// 2026-07-26 audit found that reasoning was itself wrong: the route
// (GET /api/v1/scope-nodes/{id}) has existed since it was built (api.go's
// generic resource GET, `rs.notFound`) and this driver was simply never wired
// to call it. Driven for real, the corpus's pinned `detail` — "No scope node
// exists with this identifier." — diverges from the live handler, which emits
// the resource-kind-agnostic generic detail every kind's 404 shares ("No
// resource exists at this identifier.", api.go's rs.notFound): a genuine,
// pre-existing corpus-vs-code mismatch this driver surfaces rather than
// silently reproducing.
func driveProblemNotFound(rep *report.Report, c corpus.Case) {
	var in struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	}
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	res := h.do(in.Method, in.Path, nil, in.Headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	if want, ok := c.Expected["headers"].(map[string]any); ok {
		if wantTrace, ok := want["Trace-Id"].(string); ok {
			if got := res.header.Get("Trace-Id"); got != wantTrace {
				diffs = append(diffs, report.Diff{Field: "headers.Trace-Id", Expected: wantTrace, Actual: got})
			}
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "api/1 Problem (NOT_FOUND) diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// driveProblemValidation drives API-013: creating a scope node with two
// independently-invalid fields. The prior version of this driver recorded
// this PENDING, reasoning "a multi-field VALIDATION_FAILED `errors`-array
// aggregator ... does not exist yet" — that too was wrong: api.go's
// writeStoreError already renders a store.ValidationError's per-field list as
// the `errors` extension (validationExtra). Driven for real, THIS case
// diverges from the live handler in more than one way, and both are reported
// rather than papered over:
//   - status: the corpus pins 400; every VALIDATION_FAILED the live code
//     emits (api.go's writeValidationFailed / writeStoreError) is 422, per
//     the later scheduling-core plan that settled on 422 uniformly.
//   - codes: the corpus pins ENUM_MISMATCH/TOO_SHORT; the live scope-node
//     validator (internal/datamodel.BuildScopeTree) has no such codes — an
//     invalid kind is SCOPE_NODE_KIND_INVALID, and there is no `name` length
//     validation on a scope node at all today, so the corpus's second
//     pinned field failure has no live counterpart to reproduce.
//
// This is a corpus-vs-implementation reconciliation question (which side is
// stale), not a driver bug, so it is left FAILING with the concrete diffs
// rather than "fixed" by loosening the assertion or renaming a live error
// code to match.
func driveProblemValidation(rep *report.Report, c corpus.Case) {
	var in struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    map[string]any    `json:"body"`
	}
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	bodyBytes, err := json.Marshal(in.Body)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
		return
	}
	res := h.do(in.Method, in.Path, bodyBytes, in.Headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "api/1 Problem (VALIDATION_FAILED) diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- concurrency (API-020-025) -------------------------------------------

const fixedNowMs = int64(1_700_000_000_000)

type concurrencyInput struct {
	Path                 string            `json:"path"`
	Headers              map[string]string `json:"headers"`
	Body                 map[string]any    `json:"body"`
	CurrentResourceState struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	} `json:"current_resource_state"`
}

// driveConcurrency drives a conditional-write corpus case (API-022/023)
// against the live PATCH /api/v1/scope-nodes/{id} handler: a missing If-Match
// rejects 428/IF_MATCH_REQUIRED before any write (API-022); an If-Match that
// no longer matches the resource's current ETag rejects 412/REVISION_CONFLICT
// carrying current_revision (API-023). The target row is seeded, then bumped
// to the case's own current_resource_state.revision via two real writes
// (store.Update), so its live ETag genuinely is the corpus-pinned revision —
// never asserted by construction.
func driveConcurrency(rep *report.Report, c corpus.Case) {
	var in concurrencyInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	id := in.CurrentResourceState.ID
	// A screen (not a site) needs no geo columns (DAT-031 only binds a site),
	// so seeding needs no fields beyond what the concurrency convention itself
	// cares about: identity and revision.
	if err := h.seedScopeNode(map[string]any{"id": id, "kind": "screen", "parent_id": "01J8Z0PLACEHOLDERPARENT01", "name": "Original Site"}); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed scope node: %v", err))
		return
	}
	cur, _, err := h.store.Get(context.Background(), store.KindScopeNode, id)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("get seeded scope node: %v", err))
		return
	}
	for cur.Revision < in.CurrentResourceState.Revision {
		cur, err = h.store.Update(context.Background(), store.KindScopeNode, id, cur.Revision, []byte(`{"name":"Original Site"}`))
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("bump seeded scope node to revision %d: %v", in.CurrentResourceState.Revision, err))
			return
		}
	}
	if cur.Revision != in.CurrentResourceState.Revision {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seeded scope node landed at revision %d, want the corpus's pinned %d", cur.Revision, in.CurrentResourceState.Revision))
		return
	}

	bodyBytes, err := json.Marshal(in.Body)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
		return
	}
	res := h.do("PATCH", in.Path, bodyBytes, in.Headers)

	afterRev, _, _ := h.store.Get(context.Background(), store.KindScopeNode, id)
	writeExecuted := afterRev.Revision != cur.Revision

	var diffs []report.Diff
	if want, ok := expectBool(c, "write_executed"); ok && writeExecuted != want {
		diffs = append(diffs, report.Diff{Field: "write_executed", Expected: want, Actual: writeExecuted})
	}
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "optimistic-concurrency outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- keyset pagination (API-030-036) --------------------------------------

type paginationInput struct {
	CollectionState []struct {
		ID string `json:"id"`
	} `json:"collection_state"`
	Requests []struct {
		Query map[string]string `json:"query"`
	} `json:"requests"`
}

type paginationExpected struct {
	Responses []struct {
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	} `json:"responses"`
	CombinedItemsCoverCollectionExactlyOnce bool `json:"combined_items_cover_collection_exactly_once"`
}

// drivePagination drives the keyset-pagination roundtrip case (API-032)
// against the live GET /api/v1/automations handler: the corpus's
// collection_state rows are seeded as compile-clean automations (see
// seedAutomation), then each request in sequence is replayed through the
// live handler and diffed against that request's own pinned expected
// response.
//
// The live automations list scopes its cursor by resourceType ("automations",
// api.go's automationsConfig + apihttp.EncodeCursor), whereas this corpus
// case pins the UNSCOPED bare-ULID cursor form ("the automations list is
// pinned to the unscoped bare-ULID cursor form", the prior driver's own
// comment). No unscoped list route exists in the shipped code — every
// resource's cursor is scoped uniformly — so this is a genuine, confirmed
// corpus-vs-code divergence (not a driver bug) that this case now correctly
// FAILS on, where the old library-level driver could not see it at all (it
// called apihttp.Page("", ...) directly, choosing the unscoped form itself).
func drivePagination(rep *report.Report, c corpus.Case) {
	var in paginationInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var exp paginationExpected
	if err := decodeField(c.Expected, &exp); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	const scopeNode = "01J8Z0PAGINATIONSCOPENODE1"
	for _, row := range in.CollectionState {
		if err := h.seedAutomation(row.ID, scopeNode, nil); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed automation %s: %v", row.ID, err))
			return
		}
	}

	if len(in.Requests) != len(exp.Responses) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus has %d requests but %d expected responses", len(in.Requests), len(exp.Responses)))
		return
	}

	var diffs []report.Diff
	seen := map[string]int{}
	for i, req := range in.Requests {
		path := "/api/v1/automations?" + encodeQuery(req.Query)
		res := h.do("GET", path, nil, nil)
		want := exp.Responses[i]
		if res.status != want.Status {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].status", i), Expected: want.Status, Actual: res.status})
		}
		for _, d := range listBodyDiffsRaw(fmt.Sprintf("responses[%d].body", i), want.Body, res.body) {
			diffs = append(diffs, d)
		}
		if items, ok := res.body["items"].([]any); ok {
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					if id, ok := m["id"].(string); ok {
						seen[id]++
					}
				}
			}
		}
	}

	if exp.CombinedItemsCoverCollectionExactlyOnce {
		if len(seen) != len(in.CollectionState) {
			diffs = append(diffs, report.Diff{Field: "combined_items_cover_collection_exactly_once", Expected: len(in.CollectionState), Actual: len(seen)})
		}
		for _, row := range in.CollectionState {
			if seen[row.ID] != 1 {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("row %s occurrence count", row.ID), Expected: 1, Actual: seen[row.ID]})
			}
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "keyset-pagination roundtrip diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// encodeQuery builds a URL query string from a plain string map.
func encodeQuery(q map[string]string) string {
	v := url.Values{}
	for k, val := range q {
		v.Set(k, val)
	}
	return v.Encode()
}

// --- label-selector grammar (API-040-046) ---------------------------------

type selectorInput struct {
	CollectionState []struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		ParentID string `json:"parent_id"`
	} `json:"collection_state"`
	Headers map[string]string `json:"headers"`
	Request struct {
		Method string            `json:"method"`
		Path   string            `json:"path"`
		Query  map[string]string `json:"query"`
	} `json:"request"`
}

// siteGeo is synthetic (non-corpus) geo data this driver supplies for every
// seeded site-kind scope node — DAT-031 requires a site to declare non-null
// tz/lat/long, a rule orthogonal to what these selector/pagination/
// external_id cases assert (kind + placement + label matching), so seeding
// omits nothing the corpus itself pins; it only satisfies a live-store
// invariant the corpus's collection_state fixtures (written against the
// convention libraries directly) never had to satisfy.
func siteGeo(m map[string]any) {
	m["tz"] = "America/Chicago"
	m["lat"] = 41.8781
	m["long"] = -87.6298
}

// driveSelector drives a label-selector corpus case against the live
// GET /api/v1/scope-nodes handler. When the selector parses (API-044): seed
// the collection as real scope-node rows (site rows get synthetic geo, see
// siteGeo) and diff the selected + paginated ids against the pinned
// {items, cursor} envelope — the subtree containment comes from the REAL
// scope-node tree the handler reads (store.DesiredStateRows +
// datamodel.BuildScopeTree), not a driver-modeled parent map. When it fails to
// parse (API-045): diff the genuine response the live handler emits.
func driveSelector(rep *report.Report, c corpus.Case) {
	var in selectorInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	for _, row := range in.CollectionState {
		m := map[string]any{"id": row.ID, "kind": row.Kind, "parent_id": row.ParentID}
		if row.Kind == "site" {
			siteGeo(m)
		}
		if err := h.seedScopeNode(m); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed scope node %s: %v", row.ID, err))
			return
		}
	}

	method := in.Request.Method
	if method == "" {
		method = http.MethodGet
	}
	path := in.Request.Path + "?" + encodeQuery(in.Request.Query)
	headers := in.Headers
	res := h.do(method, path, nil, headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, listBodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "selector outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- client-assignable external_id (API-100-104) --------------------------

type externalIDInput struct {
	Path            string            `json:"path"`
	Headers         map[string]string `json:"headers"`
	Body            map[string]any    `json:"body"`
	CollectionState []struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		ParentID   string `json:"parent_id"`
		ExternalID string `json:"external_id"`
	} `json:"collection_state"`
}

// driveExternalID drives the external_id-conflict corpus case (API-102)
// against the live POST /api/v1/scope-nodes handler: a create whose
// external_id already names another resource of the same kind under the same
// parent scope node is rejected before the write executes.
//
// The live handler scopes the uniqueness check by resourceConfig.resourceType
// (api.go's refsFrom), which is the generic tag "scope-nodes" for every
// scope-node kind — not the row's own `kind` ("screen") the corpus's pinned
// detail names ("already in use by another screen under this parent."). This
// is a genuine wording divergence this case now correctly surfaces.
func driveExternalID(rep *report.Report, c corpus.Case) {
	var in externalIDInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	for _, row := range in.CollectionState {
		m := map[string]any{"id": row.ID, "kind": row.Kind, "parent_id": row.ParentID, "external_id": row.ExternalID}
		if err := h.seedScopeNode(m); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed scope node %s: %v", row.ID, err))
			return
		}
	}

	before, err := h.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("list before create: %v", err))
		return
	}

	bodyBytes, err := json.Marshal(in.Body)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
		return
	}
	res := h.do("POST", in.Path, bodyBytes, in.Headers)

	after, err := h.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("list after create: %v", err))
		return
	}
	writeExecuted := len(after) != len(before)

	var diffs []report.Diff
	if want, ok := expectBool(c, "write_executed"); ok && writeExecuted != want {
		diffs = append(diffs, report.Diff{Field: "write_executed", Expected: want, Actual: writeExecuted})
	}
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "external_id uniqueness outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- Idempotency-Key replay/reuse (API-050-056) ---------------------------

type idempotencyDriverInput struct {
	Requests []struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    map[string]any    `json:"body"`
	} `json:"requests"`
}

type idempotencyDriverExpected struct {
	Responses []struct {
		Status      int            `json:"status"`
		ContentType string         `json:"content_type"`
		Body        map[string]any `json:"body"`
		Replayed    bool           `json:"replayed"`
	} `json:"responses"`
	ResourcesCreated int `json:"resources_created"`
}

// driveIdempotency drives an Idempotency-Key corpus case (API-052/053)
// against the live POST /api/v1/scope-nodes handler under a fixed injected
// clock: each request in the case is replayed in sequence and diffed against
// its own pinned expectation, plus the total create side effects.
//
// Both frozen API-052/053 fixtures create a `kind: site` scope node carrying
// no tz/lat/long. DAT-031 requires a site to declare all three — a rule this
// driver's own harness enforces for real, since it is the live store — so the
// very first request in each case is rejected 422/VALIDATION_FAILED by the
// live handler instead of succeeding 201, and the whole case fails from
// there. This is a genuine, confirmed corpus-vs-code divergence: the
// Idempotency-Key fixtures were written before (or independent of) DAT-031's
// site-geo-required rule and have never been reconciled with it.
func driveIdempotency(rep *report.Report, c corpus.Case) {
	var in idempotencyDriverInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var exp idempotencyDriverExpected
	if err := decodeField(c.Expected, &exp); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}
	if len(in.Requests) != len(exp.Responses) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus has %d requests but %d expected responses", len(in.Requests), len(exp.Responses)))
		return
	}

	h, err := newHarness(fixedNowMs)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	before, err := h.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("list before: %v", err))
		return
	}

	var diffs []report.Diff
	for i, req := range in.Requests {
		bodyBytes, err := json.Marshal(req.Body)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal requests[%d].body: %v", i, err))
			return
		}
		res := h.do(req.Method, req.Path, bodyBytes, req.Headers)

		want := exp.Responses[i]
		if res.status != want.Status {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].status", i), Expected: want.Status, Actual: res.status})
		}
		if want.ContentType != "" {
			if got := res.header.Get("Content-Type"); got != want.ContentType {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].content_type", i), Expected: want.ContentType, Actual: got})
			}
		}
		diffs = append(diffs, memberDiffs(fmt.Sprintf("responses[%d].body", i), want.Body, res.body)...)
	}

	after, err := h.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("list after: %v", err))
		return
	}
	creates := len(after) - len(before)
	if creates != exp.ResourcesCreated {
		diffs = append(diffs, report.Diff{Field: "resources_created", Expected: exp.ResourcesCreated, Actual: creates})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "Idempotency-Key replay/reuse outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- 202 + Job resource (API-110-117, API-120-123) ------------------------

type jobDriverInput struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    struct {
		Selector string `json:"selector"`
		Enabled  bool   `json:"enabled"`
	} `json:"body"`
	CollectionState []struct {
		ID        string `json:"id"`
		ScopeNode string `json:"scope_node"`
		Labels    []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"labels"`
	} `json:"collection_state"`
}

// driveJob drives the 202 + Job corpus case (API-111 bulk-enable) against the
// live POST /api/v1/automations/bulk-enable handler. The selector's
// `scope_node subtree` term is resolved against a REAL, seeded scope-node
// tree (a site plus the screen the fixture automations are placed under) —
// the containment comes from the handler's own inSubtreeFn
// (store.DesiredStateRows + datamodel.BuildScopeTree.AncestorChain), not a
// driver-modeled membership set.
//
// The harness clock is set to the case's own pinned created_at, so that part
// of the Job resource is reproduced exactly. Two fields never can be: the
// live handler mints the Job id via ulid.New() (automations.go's
// bulkEnableExec) with no injection seam, and stamps created_by from the
// fixed pocPrincipal constant (auth is deferred, api.go's own doc comment) —
// neither can equal the corpus's arbitrary pinned id/created_by. Both are
// left asserted (not loosened), so this case FAILS on exactly those two
// fields — a confirmed, reportable divergence, not a driver defect.
func driveJob(rep *report.Report, c corpus.Case) {
	var in jobDriverInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var expBody struct {
		ID        string `json:"id"`
		CreatedBy string `json:"created_by"`
		CreatedAt string `json:"created_at"`
		Targets   []struct {
			TargetID string `json:"target_id"`
		} `json:"targets"`
	}
	if raw, ok := c.Expected["body"].(map[string]any); ok {
		if err := decodeField(raw, &expBody); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected body: %v", err))
			return
		}
	}

	createdAt, err := time.Parse(time.RFC3339, expBody.CreatedAt)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("parse created_at %q: %v", expBody.CreatedAt, err))
		return
	}

	h, err := newHarness(createdAt.UnixMilli())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	// Seed a real scope-node subtree: a site plus one screen under it, so the
	// selector's `scope_node subtree` term resolves against the SAME tree the
	// live inSubtreeFn reads, not a driver-modeled membership set.
	const siteNode = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	siteBody := map[string]any{"id": siteNode, "kind": "site", "parent_id": "01J8Z0JOBPLACEHOLDERORG01"}
	siteGeo(siteBody)
	if err := h.seedScopeNode(siteBody); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed site scope node: %v", err))
		return
	}
	screenNodes := map[string]bool{}
	for _, row := range in.CollectionState {
		screenNodes[row.ScopeNode] = true
	}
	for screenID := range screenNodes {
		if err := h.seedScopeNode(map[string]any{"id": screenID, "kind": "screen", "parent_id": siteNode}); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed screen scope node %s: %v", screenID, err))
			return
		}
	}
	for _, row := range in.CollectionState {
		labels := make(map[string]string, len(row.Labels))
		for _, l := range row.Labels {
			labels[l.Key] = l.Value
		}
		if err := h.seedAutomation(row.ID, row.ScopeNode, labels); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed automation %s: %v", row.ID, err))
			return
		}
	}

	bodyBytes, err := json.Marshal(map[string]any{"selector": in.Body.Selector, "enabled": in.Body.Enabled})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
		return
	}
	res := h.do(in.Method, in.Path, bodyBytes, in.Headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	if want, ok := expectString(c, "headers.Trace-Id"); ok && want != "" {
		if got := res.header.Get("Trace-Id"); got != want {
			diffs = append(diffs, report.Diff{Field: "headers.Trace-Id", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "202 Job response diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- shared helpers ---------------------------------------------------

func decodeField(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal corpus field: %w", err)
	}
	return json.Unmarshal(b, v)
}

func expectInt(c corpus.Case, key string) (int, bool) {
	v, ok := c.Expect(key)
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func expectBool(c corpus.Case, key string) (bool, bool) {
	v, ok := c.Expect(key)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func expectString(c corpus.Case, key string) (string, bool) {
	v, ok := c.Expect(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// bodyDiffs diffs every member the corpus `expected.body` pins against the
// emitted body. Extra members in the emission are permitted; every pinned
// member MUST be reproduced exactly (the corpus is the oracle).
func bodyDiffs(c corpus.Case, got map[string]any) []report.Diff {
	want, ok := c.Expected["body"].(map[string]any)
	if !ok {
		return nil
	}
	return memberDiffs("body", want, got)
}

// listBodyDiffs is bodyDiffs for a list-endpoint response whose corpus
// `expected.body.items` pins only the id-projection shape (`{id: ...}`) a
// convention-library-level driver would have produced, while the live
// resource-list handlers (api.go's generic `list`) return each item as the
// FULL stored row body (every column, not an id-only projection) — extra
// per-item members are exactly as permitted as extra top-level members
// (bodyDiffs's own rule), so `items` is compared by its ids only, in order;
// every other top-level member (cursor, status) is still compared exactly.
func listBodyDiffs(c corpus.Case, got map[string]any) []report.Diff {
	want, ok := c.Expected["body"].(map[string]any)
	if !ok {
		return nil
	}
	return listBodyDiffsRaw("body", want, got)
}

// listBodyDiffsRaw is listBodyDiffs over explicit want/got maps and an
// explicit field prefix, for a caller (like drivePagination) diffing a
// response nested under `responses[i].body` rather than a case's top-level
// `expected.body`.
func listBodyDiffsRaw(prefix string, want, got map[string]any) []report.Diff {
	if len(want) == 0 {
		return nil
	}
	if got == nil {
		return []report.Diff{{Field: prefix, Expected: want, Actual: nil}}
	}
	var diffs []report.Diff
	for k, wv := range want {
		if k == "items" {
			wantIDs := itemIDs(wv)
			gotIDs := itemIDs(got["items"])
			if !reflect.DeepEqual(wantIDs, gotIDs) {
				diffs = append(diffs, report.Diff{Field: prefix + ".items[].id", Expected: wantIDs, Actual: gotIDs})
			}
			continue
		}
		gv, present := got[k]
		if !present {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: "<absent>"})
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: gv})
		}
	}
	return diffs
}

// itemIDs extracts the ordered `id` projection from a decoded JSON `items`
// array (each element a map that carries at least an `id` string).
func itemIDs(v any) []string {
	items, _ := v.([]any)
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func memberDiffs(prefix string, want, got map[string]any) []report.Diff {
	if len(want) == 0 {
		return nil
	}
	if got == nil {
		return []report.Diff{{Field: prefix, Expected: want, Actual: nil}}
	}
	var diffs []report.Diff
	for k, wv := range want {
		gv, present := got[k]
		if !present {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: "<absent>"})
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: gv})
		}
	}
	return diffs
}

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "api-1")
}

// LoadCorpus loads every frozen api-1 corpus case, keyed by case_id.
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
