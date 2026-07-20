// Package api1 is the executable api/1 sync-convention conformance driver: the
// §10 differential oracle for the four cross-cutting request conventions
// every management-door operation in api/openapi.yaml follows —
// optimistic concurrency (ETag/If-Match, API-020-025), keyset pagination
// (opaque cursor + limit, API-030-036), the label-selector grammar
// (API-040-046), and the client-assignable external_id convention
// (API-100-104). It replays every conformance/corpora/api-1 case against the
// LIVE internal/shared/apihttp and internal/shared/apiselector
// implementations and diffs the actual behavior against each case's own
// declared `expected` block.
//
// A corpus case is not named to a helper by hard-coded case-id knowledge:
// classifyShape inspects the case's own `input` block and dispatches to the
// matching family purely from its structure (does it carry
// current_resource_state? a requests array paired with collection_state? a
// singular request whose query carries a selector? a body carrying
// external_id alongside collection_state?) — the same shape distinctions a
// real router would use to decide which convention governs a given request.
//
// This driver's remit is the synchronous half of api/1's conventions only.
// The async half — Idempotency-Key replay (API-050-056), the Job resource
// (API-110-117), and data-subject export/delete (API-120-124) — needs an
// injectable-clock durable store this driver does not have; those corpus
// cases (API-052/053/111/121), plus API-010/013 (api/1's error-shape
// convention, already driven by its own contract), are recorded PENDING with
// an explicit reason rather than silently skipped (§10 "no silent caps").
package api1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

const contract = "api/1"

// Run loads the frozen api-1 corpus from disk and drives every sync-
// convention case against the live implementation.
func Run() report.Report {
	rep := report.Report{Driver: "api1", Target: "internal/shared/apihttp, internal/shared/apiselector"}

	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load api-1 corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against an explicit,
// caller-supplied case set. It is the seam TestAPI1DriverHasTeeth uses: load
// the real corpus, corrupt one case's `expected` block in memory, and confirm
// the SAME comparison logic (never a re-implementation of it) reports FAIL
// against the corrupted expectation.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "api1", Target: "internal/shared/apihttp, internal/shared/apiselector"}
	driveCases(&rep, cases)
	return rep
}

// syncConventionCaseIDs are the six sync-convention corpus cases this driver
// exercises: optimistic concurrency (API-022/023), keyset pagination
// (API-032), the label-selector grammar (API-044/045), and client-assignable
// external_id (API-102).
var syncConventionCaseIDs = []string{
	"API-022", "API-023", "API-032", "API-044", "API-045", "API-102",
}

// pendingCaseIDs are every other case frozen under conformance/corpora/api-1:
// the async-half cases (Idempotency-Key replay, the Job resource, data-
// subject export/delete) this driver deliberately does not drive, plus
// API-010/013 (api/1's error-shape convention — already covered by that
// convention's own corpus cases, out of this sync-convention driver's scope).
var pendingCaseIDs = map[string]string{
	"API-010": "api/1's Problem error shape (API-010-016) is a separate convention with its own corpus coverage — out of this sync-convention driver's scope.",
	"API-013": "api/1's Problem error shape (API-010-016) is a separate convention with its own corpus coverage — out of this sync-convention driver's scope.",
	"API-052": "Idempotency-Key replay (API-050-056) is the async half's concern — needs an injectable-clock durable store; deferred to the async-half follow-up plan.",
	"API-053": "Idempotency-Key replay (API-050-056) is the async half's concern — needs an injectable-clock durable store; deferred to the async-half follow-up plan.",
	"API-111": "the Job resource + bulk-mutating state machine (API-110-117) is the async half's concern — deferred to the async-half follow-up plan.",
	"API-121": "data-subject export/delete (API-120-124) is the async half's concern — the archive/1 container + SEC-121 key-destruction realization are their own contracts; deferred to the async-half follow-up plan.",
}

func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	for _, short := range syncConventionCaseIDs {
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
// recognize is a driver FAIL, not a silent skip — every one of the six sync-
// convention cases MUST resolve to exactly one of the four known families.
func driveByShape(rep *report.Report, cases map[string]corpus.Case, short string) {
	c, ok := corpus.ByID(cases, short)
	if !ok {
		rep.Fail(short, contract, "case not found in frozen corpus")
		return
	}

	switch classifyShape(c.Input) {
	case shapeConcurrency:
		driveConcurrency(rep, c)
	case shapePagination:
		drivePagination(rep, c)
	case shapeSelector:
		driveSelector(rep, c)
	case shapeExternalID:
		driveExternalID(rep, c)
	default:
		rep.Fail(c.CaseID, contract, "input block did not match any known sync-convention shape (concurrency/pagination/selector/external_id)")
	}
}

// shape names the sync-convention family a corpus case's `input` block
// structurally belongs to.
type shape int

const (
	shapeUnknown shape = iota
	shapeConcurrency
	shapePagination
	shapeSelector
	shapeExternalID
)

// classifyShape inspects a case's `input` block and reports which sync-
// convention family it belongs to, purely from its structure:
//
//   - `current_resource_state` present → a conditional write against a
//     mutable resource's revision: optimistic concurrency (API-020-025).
//   - a `requests` array paired with `collection_state` → a sequence of list
//     requests against a fixed collection: keyset pagination (API-030-036).
//     (A `requests` array with NO `collection_state` — e.g. an Idempotency-Key
//     replay pair — is a different, async-half shape this driver does not
//     claim.)
//   - a singular `request` whose `query` carries a `selector` → the label-
//     selector grammar (API-040-046), whether or not the case also supplies
//     `collection_state` (a malformed-selector case needs none).
//   - a `body` carrying an `external_id` key alongside `collection_state` →
//     the client-assignable external_id convention (API-100-104).
//
// Anything else classifies shapeUnknown.
func classifyShape(input map[string]any) shape {
	if _, ok := input["current_resource_state"]; ok {
		return shapeConcurrency
	}
	if _, ok := input["requests"].([]any); ok {
		if _, ok := input["collection_state"]; ok {
			return shapePagination
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
	return shapeUnknown
}

// --- concurrency (API-020-025) -------------------------------------------

type concurrencyInput struct {
	Path                 string            `json:"path"`
	Headers              map[string]string `json:"headers"`
	CurrentResourceState struct {
		Revision int64 `json:"revision"`
	} `json:"current_resource_state"`
}

// driveConcurrency drives a conditional-write corpus case (API-022/023)
// against apihttp.CheckIfMatch: a missing If-Match rejects 428/
// IF_MATCH_REQUIRED before any write (API-022); an If-Match that no longer
// matches the resource's current ETag rejects 412/REVISION_CONFLICT carrying
// current_revision, again before any write (API-023). One function drives
// both cases identically — the shape and the write-discipline invariant (no
// write on a non-OK outcome) are the same for either rejection.
func driveConcurrency(rep *report.Report, c corpus.Case) {
	var in concurrencyInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	ifMatch, present := in.Headers["If-Match"]
	rev := in.CurrentResourceState.Revision
	out := apihttp.CheckIfMatch(ifMatch, present, rev)

	// Model the write path a handler follows: a non-OK outcome MUST NOT
	// perform the write (API-022's "no unconditional write path"); an OK
	// outcome would.
	writeExecuted := out.OK
	gotStatus := http.StatusOK
	gotCT := ""
	var gotBody map[string]any

	if !out.OK {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPatch, in.Path, nil)
		var extra map[string]any
		if out.CurrentRevision != nil {
			extra = map[string]any{"current_revision": *out.CurrentRevision}
		}
		apihttp.WriteProblemExt(w, r, in.Headers["Trace-Id"], out.Status, out.Code, out.Title, out.Detail, extra)
		gotStatus = w.Code
		gotCT = w.Header().Get("Content-Type")
		gotBody = decodeJSONBody(w)
	}

	var diffs []report.Diff
	if want, ok := expectBool(c, "write_executed"); ok && writeExecuted != want {
		diffs = append(diffs, report.Diff{Field: "write_executed", Expected: want, Actual: writeExecuted})
	}
	if want, ok := expectInt(c, "status"); ok && gotStatus != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: gotStatus})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" && gotCT != want {
		diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: gotCT})
	}
	diffs = append(diffs, bodyDiffs(c, gotBody)...)

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

// listItem is the response projection a list endpoint emits per row (API-032:
// `items:[{id:...}]`, never the full stored row).
type listItem struct {
	ID string `json:"id"`
}

// drivePagination drives the keyset-pagination roundtrip case (API-032): over
// a fixed collection, replay each request in sequence through
// apihttp.ParsePageParams / DecodeCursor / Page and diff the resulting
// {items, cursor} envelope against that request's own pinned expected
// response — then confirm every row across all pages was returned exactly
// once (API-034).
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

	sorted := make([]listItem, 0, len(in.CollectionState))
	for _, row := range in.CollectionState {
		sorted = append(sorted, listItem{ID: row.ID})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	if len(in.Requests) != len(exp.Responses) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus has %d requests but %d expected responses", len(in.Requests), len(exp.Responses)))
		return
	}

	var diffs []report.Diff
	seen := map[string]int{}
	for i, req := range in.Requests {
		status, body, err := servePage(sorted, req.Query["cursor"], req.Query["limit"])
		if err != nil {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d]", i), Expected: "a servable page", Actual: err.Error()})
			continue
		}
		want := exp.Responses[i]
		if status != want.Status {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].status", i), Expected: want.Status, Actual: status})
		}
		if !reflect.DeepEqual(body, want.Body) {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].body", i), Expected: want.Body, Actual: body})
		}

		if items, ok := body["items"].([]any); ok {
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

// servePage models the read path a list handler follows for one request:
// parse the page params, decode a supplied cursor to its keyset position,
// select the rows after that position, and cut the page with apihttp.Page.
// The automations list is pinned to the unscoped bare-ULID cursor form
// (API-032), so scope is always "".
func servePage(sorted []listItem, rawCursor, rawLimit string) (status int, body map[string]any, err error) {
	cursor, limit, perr := apihttp.ParsePageParams(rawCursor, rawLimit)
	if perr != nil {
		return 0, nil, fmt.Errorf("ParsePageParams(%q, %q) rejected a valid corpus request: %+v", rawCursor, rawLimit, perr)
	}

	lastID := ""
	if cursor != "" {
		id, derr := apihttp.DecodeCursor("", cursor)
		if derr != nil {
			return 0, nil, fmt.Errorf("DecodeCursor(%q) rejected a valid corpus cursor: %+v", cursor, derr)
		}
		lastID = id
	}

	after := make([]listItem, 0, len(sorted))
	for _, row := range sorted {
		if row.ID > lastID {
			after = append(after, row)
		}
	}
	if len(after) > limit+1 {
		after = after[:limit+1]
	}

	env := apihttp.Page("", after, limit, func(it listItem) string { return it.ID })
	raw, merr := json.Marshal(env)
	if merr != nil {
		return 0, nil, fmt.Errorf("marshal page envelope: %w", merr)
	}
	var decoded map[string]any
	if uerr := json.Unmarshal(raw, &decoded); uerr != nil {
		return 0, nil, fmt.Errorf("decode page envelope: %w", uerr)
	}
	return http.StatusOK, decoded, nil
}

// --- label-selector grammar (API-040-046) ---------------------------------

type selectorInput struct {
	CollectionState []struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		ParentID string `json:"parent_id"`
	} `json:"collection_state"`
	Request struct {
		Query map[string]string `json:"query"`
	} `json:"request"`
}

// driveSelector drives a label-selector corpus case against
// apiselector.Parse. When the selector parses (API-044): evaluate it over the
// collection (placement modeled as an id→parent_id tree, `kind` carried as the
// resource's sole label) and diff the selected + paginated ids against the
// pinned {items, cursor} envelope. When it fails to parse (API-045): diff the
// resulting *ParseError's Status/Code/Title/Detail against the pinned Problem
// body — the offending term named verbatim.
func driveSelector(rep *report.Report, c corpus.Case) {
	var in selectorInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	sel, perr := apiselector.Parse(in.Request.Query["selector"])
	if perr != nil {
		driveSelectorParseError(rep, c, perr)
		return
	}
	driveSelectorMatch(rep, c, in, sel)
}

func driveSelectorParseError(rep *report.Report, c corpus.Case, perr *apiselector.ParseError) {
	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && perr.Status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: perr.Status})
	}
	body, _ := c.Expected["body"].(map[string]any)
	if want, ok := body["code"].(string); ok && perr.Code != want {
		diffs = append(diffs, report.Diff{Field: "body.code", Expected: want, Actual: perr.Code})
	}
	if want, ok := body["title"].(string); ok && perr.Title != want {
		diffs = append(diffs, report.Diff{Field: "body.title", Expected: want, Actual: perr.Title})
	}
	if want, ok := body["detail"].(string); ok && perr.Detail != want {
		diffs = append(diffs, report.Diff{Field: "body.detail", Expected: want, Actual: perr.Detail})
	}
	if want, ok := body["type"].(string); ok && want != "about:blank" {
		diffs = append(diffs, report.Diff{Field: "body.type", Expected: want, Actual: "about:blank"})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "selector parse-error Problem diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

func driveSelectorMatch(rep *report.Report, c corpus.Case, in selectorInput, sel apiselector.Selector) {
	parent := map[string]string{}
	for _, row := range in.CollectionState {
		parent[row.ID] = row.ParentID
	}
	inSubtree := func(ancestor, node string) bool {
		for cur := node; ; {
			p, ok := parent[cur]
			if !ok {
				return false
			}
			if p == ancestor {
				return true
			}
			cur = p
		}
	}

	var matched []listItem
	for _, row := range in.CollectionState {
		if sel.Matches(map[string]string{"kind": row.Kind}, row.ID, inSubtree) {
			matched = append(matched, listItem{ID: row.ID})
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })

	env := apihttp.Page("", matched, apihttp.DefaultPageLimit, func(it listItem) string { return it.ID })

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && want != http.StatusOK {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: http.StatusOK})
	}
	body, _ := c.Expected["body"].(map[string]any)
	wantItems, _ := body["items"].([]any)
	wantIDs := make([]string, 0, len(wantItems))
	for _, it := range wantItems {
		if m, ok := it.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				wantIDs = append(wantIDs, id)
			}
		}
	}
	sort.Strings(wantIDs)
	gotIDs := make([]string, 0, len(env.Items))
	for _, it := range env.Items {
		gotIDs = append(gotIDs, it.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		diffs = append(diffs, report.Diff{Field: "body.items[].id", Expected: wantIDs, Actual: gotIDs})
	}
	if cursorRaw, present := body["cursor"]; present {
		if cursorRaw == nil {
			if env.Cursor != nil {
				diffs = append(diffs, report.Diff{Field: "body.cursor", Expected: nil, Actual: *env.Cursor})
			}
		} else if s, ok := cursorRaw.(string); ok {
			if env.Cursor == nil || *env.Cursor != s {
				diffs = append(diffs, report.Diff{Field: "body.cursor", Expected: s, Actual: env.Cursor})
			}
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "selector evaluation diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- client-assignable external_id (API-100-104) --------------------------

type externalIDInput struct {
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    struct {
		Kind       string `json:"kind"`
		ParentID   string `json:"parent_id"`
		ExternalID string `json:"external_id"`
	} `json:"body"`
	CollectionState []struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		ParentID   string `json:"parent_id"`
		ExternalID string `json:"external_id"`
	} `json:"collection_state"`
}

// driveExternalID drives the external_id-conflict corpus case (API-102)
// against apihttp.CheckExternalIDUnique: a create whose external_id already
// names another resource of the same type under the same scope node rejects
// 400/EXTERNAL_ID_CONFLICT before the write executes.
func driveExternalID(rep *report.Report, c corpus.Case) {
	var in externalIDInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	existing := make([]apihttp.ExternalRef, 0, len(in.CollectionState))
	for _, row := range in.CollectionState {
		existing = append(existing, apihttp.ExternalRef{
			ID: row.ID, ExternalID: row.ExternalID, ResourceType: row.Kind, ScopeNode: row.ParentID,
		})
	}

	// selfID is empty: this is a create, so no existing record can be "this
	// same resource" and the check can never suppress itself (API-102).
	prob := apihttp.CheckExternalIDUnique(existing, in.Body.Kind, in.Body.ParentID, in.Body.ExternalID, "")

	writeExecuted := prob == nil
	gotStatus := http.StatusOK
	gotCT := ""
	var gotBody map[string]any
	if prob != nil {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, in.Path, nil)
		apihttp.WriteProblemExt(w, r, in.Headers["Trace-Id"], prob.Status, prob.Code, prob.Title, prob.Detail, nil)
		gotStatus = w.Code
		gotCT = w.Header().Get("Content-Type")
		gotBody = decodeJSONBody(w)
	}

	var diffs []report.Diff
	if want, ok := expectBool(c, "write_executed"); ok && writeExecuted != want {
		diffs = append(diffs, report.Diff{Field: "write_executed", Expected: want, Actual: writeExecuted})
	}
	if want, ok := expectInt(c, "status"); ok && gotStatus != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: gotStatus})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" && gotCT != want {
		diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: gotCT})
	}
	diffs = append(diffs, bodyDiffs(c, gotBody)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "external_id uniqueness outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- shared helpers ---------------------------------------------------

// decodeField re-marshals a corpus case's generic map[string]any input/expected
// (or any sub-object of it) into a concrete typed value — the driver's own
// fields are JSON-tagged to match the frozen corpus's field names exactly, so
// this is a lossless re-decode, not a schema translation.
func decodeField(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal corpus field: %w", err)
	}
	return json.Unmarshal(b, v)
}

// decodeJSONBody decodes an httptest.ResponseRecorder's body as a generic
// JSON object; a decode failure yields a nil map (surfaced downstream as a
// missing-body diff, never a panic).
func decodeJSONBody(w *httptest.ResponseRecorder) map[string]any {
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body
}

// expectInt reads an expected top-level field as an int (JSON numbers decode
// to float64 in a map[string]any).
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

// expectBool reads an expected top-level field as a bool, reporting whether
// it was present (distinct from corpus.Case.ExpectBool, which defaults absent
// to false rather than reporting absence).
func expectBool(c corpus.Case, key string) (bool, bool) {
	v, ok := c.Expect(key)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// expectString reads an expected top-level field as a string, reporting
// whether it was present.
func expectString(c corpus.Case, key string) (string, bool) {
	v, ok := c.Expect(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// bodyDiffs diffs every member the corpus `expected.body` pins against the
// emitted body. Extra members in the emission (e.g. `instance`, API-015) are
// permitted; every pinned member MUST be reproduced exactly (the corpus is
// the oracle — a pinned `detail` string or `current_revision` value is
// matched verbatim, never "improved").
func bodyDiffs(c corpus.Case, got map[string]any) []report.Diff {
	want, ok := c.Expected["body"].(map[string]any)
	if !ok {
		return nil
	}
	if got == nil {
		return []report.Diff{{Field: "body", Expected: want, Actual: nil}}
	}
	var diffs []report.Diff
	for k, wv := range want {
		gv, present := got[k]
		if !present {
			diffs = append(diffs, report.Diff{Field: "body." + k, Expected: wv, Actual: "<absent>"})
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			diffs = append(diffs, report.Diff{Field: "body." + k, Expected: wv, Actual: gv})
		}
	}
	return diffs
}

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "api-1")
}

// LoadCorpus loads every frozen api-1 corpus case, keyed by case_id — the
// exact set Run itself reads. Exported so the driver's own tests can
// independently verify every case_id in the corpus DIRECTORY is accounted for
// (driven or pending), and so the teeth-test can load the real corpus and
// hand RunCases a deliberately-corrupted copy of one case.
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
