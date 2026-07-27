package api1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// resourcelifecycle.go drives the RESOURCE-LIFECYCLE case shape: a case that
// seeds only a scope-node tree and then AUTHORS, through the create verb itself,
// the rows it goes on to enumerate.
//
// It exists because the pagination shape cannot express this. There,
// `collection_state` is seeded behind the API — deliberately, since the question
// under test is how a list pages a collection that already exists. Here the
// question is whether a resource FAMILY is addressable end to end: whether the
// same create that mints a row produces one the ordinary selector and the
// ordinary opaque cursor can then find. A family can pass every pagination case
// in the corpus with rows a driver inserted straight into SQLite and still have
// no working create at all.
//
// # Why rows are matched by external_id
//
// Every expectation below identifies a row by its `external_id`, never by `id`.
// API-105 makes a resource's id server-assigned, so a corpus that pinned one
// would be asserting the behaviour of an id generator — an implementation
// detail this contract deliberately does not fix — rather than a convention.
// API-100's client-assignable external_id is the identity a client actually
// controls, which makes it the only stable name a frozen case can use.
//
// The ids are not left unexamined, though: `server_assigned_ids_are_canonical_ulids`
// asserts every id the server minted is a canonical ULID, which is what makes
// the keyset order (API-034) a real order rather than a coincidence of insertion.

type lifecycleRequest struct {
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query"`
	Body   json.RawMessage   `json:"body"`
}

type lifecycleInput struct {
	Seed struct {
		ScopeNodes []map[string]any `json:"scope_nodes"`
	} `json:"seed"`
	Requests []lifecycleRequest `json:"requests"`
}

type lifecycleExpected struct {
	Responses []struct {
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	} `json:"responses"`
	ServerAssignedIDsAreCanonicalULIDs bool     `json:"server_assigned_ids_are_canonical_ulids"`
	PagedCoverExactlyOnce              []string `json:"paged_screens_cover_collection_exactly_once"`
}

// driveResourceLifecycle seeds the case's scope-node tree, replays every request
// through the live handler (chaining any "$responses[N].field" cursor marker
// against the response actually observed, exactly as the pagination shape does),
// and diffs each outcome against that request's own pinned expectation.
func driveResourceLifecycle(rep *report.Report, c corpus.Case) {
	var in lifecycleInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var exp lifecycleExpected
	if err := decodeField(c.Expected, &exp); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}
	if len(in.Requests) != len(exp.Responses) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus has %d requests but %d expected responses", len(in.Requests), len(exp.Responses)))
		return
	}

	h, err := newHarness(fixedNowMs, deterministicIDs())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	// The scope-node tree is fixture setup, not the operation under test, so it
	// is seeded through the store — the same seed-via-store/drive-via-handler
	// split every other case in this driver uses. The rows the case is ABOUT are
	// authored through the live create verb below.
	for _, node := range in.Seed.ScopeNodes {
		if err := h.seedScopeNode(node); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed scope node %v: %v", node["id"], err))
			return
		}
	}

	var diffs []report.Diff
	var priorBodies []map[string]any
	// externalIDOf maps a server-minted id back to the external_id its create
	// carried, so a later list — which serves ids, not the client's own names —
	// can be compared against the corpus's stable identities.
	externalIDOf := map[string]string{}
	var mintedIDs []string

	for i, req := range in.Requests {
		query, qdiffs := resolvePaginationQuery(i, req.Query, priorBodies)
		diffs = append(diffs, qdiffs...)

		method := req.Method
		if method == "" {
			method = http.MethodGet
		}
		path := req.Path
		if len(query) > 0 {
			path += "?" + encodeQuery(query)
		}
		var body []byte
		if len(req.Body) > 0 {
			body = req.Body
		}
		res := h.do(method, path, body, nil)
		priorBodies = append(priorBodies, res.body)

		want := exp.Responses[i]
		if res.status != want.Status {
			diffs = append(diffs, report.Diff{
				Field:    fmt.Sprintf("responses[%d].status", i),
				Expected: want.Status,
				Actual:   fmt.Sprintf("%d (body %s)", res.status, res.raw),
			})
			continue
		}
		// A create's response is the row itself: record the id↔external_id
		// binding it established, so every later list can be read in the
		// corpus's own vocabulary.
		if method == http.MethodPost {
			if id, ok := res.body["id"].(string); ok {
				mintedIDs = append(mintedIDs, id)
				if ext, ok := res.body["external_id"].(string); ok {
					externalIDOf[id] = ext
				}
			}
		}
		diffs = append(diffs, lifecycleBodyDiffs(fmt.Sprintf("responses[%d].body", i), want.Body, res.body, externalIDOf)...)
	}

	if exp.ServerAssignedIDsAreCanonicalULIDs {
		if len(mintedIDs) == 0 {
			diffs = append(diffs, report.Diff{
				Field:    "server_assigned_ids_are_canonical_ulids",
				Expected: "at least one server-minted id to check",
				Actual:   "no create returned an id",
			})
		}
		for _, id := range mintedIDs {
			if !ulid.Valid(id) {
				diffs = append(diffs, report.Diff{
					Field:    "server_assigned_ids_are_canonical_ulids",
					Expected: "a canonical ULID (data-model/1 DAT-005a)",
					Actual:   id,
				})
			}
		}
	}

	// The keyset walk's own completeness claim: the pages together name every
	// row of the collection exactly once, in the corpus's external_id vocabulary.
	if len(exp.PagedCoverExactlyOnce) > 0 {
		seen := map[string]int{}
		for _, ext := range externalIDsSeenInPagedRequests(in.Requests, exp.Responses, priorBodies, externalIDOf) {
			seen[ext]++
		}
		for _, ext := range exp.PagedCoverExactlyOnce {
			if seen[ext] != 1 {
				diffs = append(diffs, report.Diff{
					Field:    fmt.Sprintf("paged coverage of %q", ext),
					Expected: 1,
					Actual:   seen[ext],
				})
			}
		}
		if len(seen) != len(exp.PagedCoverExactlyOnce) {
			diffs = append(diffs, report.Diff{
				Field:    "paged_screens_cover_collection_exactly_once",
				Expected: exp.PagedCoverExactlyOnce,
				Actual:   sortedSeen(seen),
			})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "resource-lifecycle outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// lifecycleBodyDiffs compares one response body against its expectation. An
// `items` member is compared by the ordered external_id projection (see this
// file's header); every other named member is compared exactly, and members the
// expectation does not name are ignored — extra members are as permitted here as
// they are everywhere else in this driver.
func lifecycleBodyDiffs(prefix string, want, got map[string]any, externalIDOf map[string]string) []report.Diff {
	if len(want) == 0 {
		return nil
	}
	if got == nil {
		return []report.Diff{{Field: prefix, Expected: want, Actual: nil}}
	}
	var diffs []report.Diff
	for k, wv := range want {
		if k == "items" {
			wantExt := expectedExternalIDs(wv)
			gotExt := servedExternalIDs(got["items"], externalIDOf)
			if !reflect.DeepEqual(wantExt, gotExt) {
				diffs = append(diffs, report.Diff{Field: prefix + ".items[].external_id", Expected: wantExt, Actual: gotExt})
			}
			continue
		}
		gv, present := got[k]
		if !present {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: "<absent>"})
			continue
		}
		// A JSON number decodes to float64 on both sides, so an integer written
		// in the corpus compares equal to the integer the server served.
		if wn, ok := wv.(float64); ok {
			if gn, ok := gv.(float64); !ok || gn != wn {
				diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: gv})
			}
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: gv})
		}
	}
	return diffs
}

// expectedExternalIDs projects a corpus `items` expectation onto its ordered
// external_id list.
func expectedExternalIDs(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if ext, ok := m["external_id"].(string); ok {
				out = append(out, ext)
			}
		}
	}
	return out
}

// servedExternalIDs projects a SERVED `items` array onto the same list, reading
// each row's own external_id and falling back to the create-time binding when a
// served row does not carry one.
func servedExternalIDs(v any, externalIDOf map[string]string) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if ext, ok := m["external_id"].(string); ok && ext != "" {
			out = append(out, ext)
			continue
		}
		if id, ok := m["id"].(string); ok {
			out = append(out, externalIDOf[id])
		}
	}
	return out
}

// externalIDsSeenInPagedRequests collects the external_ids served across every
// request the corpus paged with an explicit `limit` — the keyset walk itself,
// as distinct from the selector queries beside it, which narrow on purpose and
// would make a coverage claim meaningless.
func externalIDsSeenInPagedRequests(
	reqs []lifecycleRequest,
	expected []struct {
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	},
	bodies []map[string]any,
	externalIDOf map[string]string,
) []string {
	var out []string
	for i, req := range reqs {
		if _, paged := req.Query["limit"]; !paged {
			continue
		}
		if i >= len(bodies) || i >= len(expected) {
			continue
		}
		out = append(out, servedExternalIDs(bodies[i]["items"], externalIDOf)...)
	}
	return out
}

func sortedSeen(seen map[string]int) []string {
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
