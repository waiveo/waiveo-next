package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// datamodel_corpus_test.go is the second data-model/1 corpus driver, and the
// only one that can exist for the cases it drives.
//
// The first — internal/datamodel/corpus_test.go — replays every case against
// this platform's reference ENGINE, which is right for a contract whose
// requirements are almost all pure functions over rows. The referential-integrity
// rules are not: they are refusals a REQUEST must receive, and the reference
// engine has no write path to refuse one with. It has no delete operation for
// DAT-020/021/022, and no store for DAT-002's and DAT-006's create-side refusals,
// each of which is a question about a REFERENCE into the stored tree rather than
// about the row in hand. So those cases have to be driven on the HTTP surface,
// against the mounted handler over a real store — the same discipline the api/1
// driver adopted after an audit found it certifying the convention libraries
// rather than the shipped surface.
//
// It lives here, in the api package's own external test package, because that is
// the only place the mount and the corpus can be in one compilation unit:
// internal/datamodel cannot import internal/app/api (which imports it), and
// conformance/cmd/driven-manifest cannot import a _test.go file. That is the
// same constraint every other hand-maintained entry in
// conformance/driven-manifest.json is under, and the same answer they gave.
//
// # Which cases this drives, and how the two drivers agree on the split
//
// Neither driver holds a list of case ids. Both classify from the case's own
// input block: a case whose `input` carries a PRESENT, NON-NULL `request` member
// is an HTTP case (this file drives it); anything else is an engine case
// (corpus_test.go drives it). One property, read from the frozen data by both
// sides, so the two cannot drift into driving the same case twice or neither
// driving it at all — the api/1 driver's own "dispatch purely from input
// structure, never from hard-coded case-id knowledge" rule, applied across a
// package boundary.
//
// "Non-null" is part of the rule, not pedantry. This driver's `Request` field is a
// pointer, and encoding/json leaves a pointer nil for an explicit JSON null, so a
// `"request": null` case is not owned here — and the engine side has to agree
// exactly, or such a case is skipped by both. Each side pins its own half of the
// rule: TestHTTPDriverOwnershipIsPresentAndNonNull here,
// TestRequestShapedMatchesTheHTTPDriversDecode there.

// corpusDirDataModel1 resolves the frozen data-model/1 corpus from THIS source
// file's location, so it is found regardless of the working directory a test run
// happens to use.
func corpusDirDataModel1() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "conformance", "corpora", "data-model-1")
}

// dmCase is the corpus envelope this driver reads, plus the two blocks it drives
// from. Fields it does not drive are left off deliberately: a driver that decoded
// everything would silently accept a case whose expectations it never checks.
type dmCase struct {
	CaseID string `json:"case_id"`
	Input  struct {
		Seed struct {
			ScopeNodes []json.RawMessage `json:"scope_nodes"`
			Rows       []struct {
				Kind string          `json:"kind"`
				Body json.RawMessage `json:"body"`
			} `json:"rows"`
		} `json:"seed"`
		Request *struct {
			Method  string            `json:"method"`
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
			// Body is the create-shaped cases' request payload. It has to be
			// read here rather than left out, or a case's body is silently
			// dropped: the request would still be issued, against a handler that
			// never saw what the case said to send, and the diff would blame the
			// handler.
			Body json.RawMessage `json:"body"`
		} `json:"request"`
	} `json:"input"`
	Expected struct {
		Status      int    `json:"status"`
		ContentType string `json:"content_type"`
		// DeleteExecuted / CreateExecuted are the halves a status code cannot
		// carry: that the refused request changed nothing. Each is a pointer so
		// "absent" is distinguishable from "false" — a case that does not pin the
		// side effect is not silently asserted to have none.
		DeleteExecuted *bool          `json:"delete_executed"`
		CreateExecuted *bool          `json:"create_executed"`
		Body           map[string]any `json:"body"`
	} `json:"expected"`
}

// loadDataModel1Corpus reads every frozen data-model/1 case.
func loadDataModel1Corpus(t *testing.T) []dmCase {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(corpusDirDataModel1(), "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob data-model-1 corpus: %v (%d file(s))", err, len(files))
	}
	sort.Strings(files)
	out := make([]dmCase, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var c dmCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("decode %s: %v", f, err)
		}
		if c.CaseID != stem(f) {
			t.Fatalf("%s: case_id %q does not match its filename", f, c.CaseID)
		}
		out = append(out, c)
	}
	return out
}

func stem(path string) string {
	base := filepath.Base(path)
	return base[:len(base)-len(filepath.Ext(base))]
}

// httpDrivenDataModel1Cases are the frozen cases carrying a `request` block —
// the ones this driver owns. See the file doc for why that is the split.
func httpDrivenDataModel1Cases(t *testing.T) []dmCase {
	t.Helper()
	var out []dmCase
	for _, c := range loadDataModel1Corpus(t) {
		if c.Input.Request != nil {
			out = append(out, c)
		}
	}
	return out
}

// TestHTTPDriverOwnershipIsPresentAndNonNull pins THIS driver's half of the split
// to the written rule: a case is owned here when its `input.request` member is
// present and not JSON null — no more, no less.
//
// The rule has to be pinned on both sides independently, because neither side can
// see the other's predicate (each lives in a _test.go file in a package the other
// cannot import). The engine side pins its own half against the same sentence in
// TestRequestShapedMatchesTheHTTPDriversDecode. Without this half, changing the
// `Request` field's shape above — a non-pointer with a presence flag, say, or a
// decode that treated null as an empty request — would move this driver's
// ownership silently, and the engine side would go on skipping exactly the cases
// it used to skip. The result is a case driven by nobody, which is precisely what
// a `"request": null` case used to be.
func TestHTTPDriverOwnershipIsPresentAndNonNull(t *testing.T) {
	for _, c := range loadDataModel1Corpus(t) {
		// The raw member, read without this file's own struct, so the comparison is
		// against the RULE rather than against the same decode restated.
		var raw struct {
			Input map[string]json.RawMessage `json:"input"`
		}
		full := readCorpusFile(t, c.CaseID)
		if err := json.Unmarshal(full, &raw); err != nil {
			t.Fatalf("re-read %s: %v", c.CaseID, err)
		}
		member, present := raw.Input["request"]
		wantOwned := present && string(bytes.TrimSpace(member)) != "null"
		if owned := c.Input.Request != nil; owned != wantOwned {
			t.Errorf("%s: this driver owns it = %v, but the rule (a present, non-null `request` member) says %v — "+
				"the two drivers' shares no longer partition the corpus", c.CaseID, owned, wantOwned)
		}
	}
}

// readCorpusFile returns one frozen case's bytes by case id (which IS its
// filename — loadDataModel1Corpus asserts as much).
func readCorpusFile(t *testing.T, caseID string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusDirDataModel1(), caseID+".json"))
	if err != nil {
		t.Fatalf("read case %s: %v", caseID, err)
	}
	return b
}

// TestDataModel1HTTPCorpus replays every request-shaped data-model/1 case
// against the live handler and diffs the response against the case's own frozen
// expectation.
func TestDataModel1HTTPCorpus(t *testing.T) {
	cases := httpDrivenDataModel1Cases(t)
	if len(cases) == 0 {
		t.Fatal("no request-shaped data-model/1 case found — this driver would pass by driving nothing")
	}
	for _, c := range cases {
		t.Run(c.CaseID, func(t *testing.T) { driveDataModel1Case(t, c) })
	}
}

func driveDataModel1Case(t *testing.T, c dmCase) {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()

	// The tree and the rows placed in it are fixture setup, not the operation
	// under test, so they are seeded THROUGH THE STORE with the ids the frozen
	// case names — the same seed-via-store/drive-via-handler split the api/1
	// driver uses. A create through the API would mint server-assigned ids, and a
	// frozen case cannot name one of those.
	for _, node := range c.Input.Seed.ScopeNodes {
		if _, err := e.store.Create(ctx, store.KindScopeNode, node); err != nil {
			t.Fatalf("seed scope node %s: %v", node, err)
		}
	}
	for _, row := range c.Input.Seed.Rows {
		kind, ok := seedableKind(row.Kind)
		if !ok {
			t.Fatalf("case seeds kind %q, which this driver does not know how to write", row.Kind)
		}
		if _, err := e.store.Create(ctx, kind, row.Body); err != nil {
			t.Fatalf("seed %s row: %v", row.Kind, err)
		}
	}

	req := c.Input.Request
	var reqBody []byte
	if len(req.Body) > 0 {
		reqBody = req.Body
	}
	// The collection's size BEFORE the request, for a case pinning
	// `create_executed: false`. Read here rather than after the fact because "the
	// collection is empty" is not the claim — the claim is that this request added
	// nothing to whatever was already there.
	var itemsBefore int
	if c.Expected.CreateExecuted != nil {
		itemsBefore = collectionSize(t, e, req.Path)
	}
	resp, raw := e.do(t, req.Method, req.Path, reqBody, req.Headers)

	if resp.StatusCode != c.Expected.Status {
		t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, c.Expected.Status, raw)
	}
	if want := c.Expected.ContentType; want != "" {
		if got := resp.Header.Get("Content-Type"); got != want {
			t.Fatalf("Content-Type = %q, want %q", got, want)
		}
	}

	// Every member the case pins is compared; members it does not name are not
	// invented here. The frozen block is the expectation — a driver that also
	// asserted fields of its own would be asserting something nobody reviewed.
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response body: %v (body %s)", err, raw)
	}
	for _, k := range sortedMapKeys(c.Expected.Body) {
		want := c.Expected.Body[k]
		got, present := body[k]
		if !present {
			t.Fatalf("response body has no %q member (body %s)", k, raw)
		}
		if !reflect.DeepEqual(normalizeJSON(got), normalizeJSON(want)) {
			t.Fatalf("response body %q = %#v, want %#v", k, got, want)
		}
	}

	// `delete_executed: false` is the half a status code cannot carry: the row the
	// refused request named must still be there, read back through the same
	// surface that refused to remove it.
	if c.Expected.DeleteExecuted != nil && !*c.Expected.DeleteExecuted {
		if resp, raw := e.do(t, http.MethodGet, req.Path, nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("after a refused delete, GET %s = %d, want 200 — the refusal must have executed nothing (body %s)",
				req.Path, resp.StatusCode, raw)
		}
	}
	// `create_executed: false` is the same half for a POST, and it is the one that
	// matters most for a refusal whose whole subject is a row that must not exist:
	// a 422 with the row stored anyway would be the dangling reference plus a
	// misleading status. The collection is counted through the same surface that
	// refused the write.
	if c.Expected.CreateExecuted != nil && !*c.Expected.CreateExecuted {
		if after := collectionSize(t, e, req.Path); after != itemsBefore {
			t.Fatalf("after a refused create, %s holds %d row(s), want the %d it held before — "+
				"the refusal must have executed nothing", req.Path, after, itemsBefore)
		}
	}
}

// collectionSize lists a resource collection through the mounted handler and
// returns how many rows it holds. The limit is above every frozen case's row
// count, so a short page can never read as an unchanged collection.
func collectionSize(t *testing.T, e *testEnv, path string) int {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path+"?limit=200", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list %s = %d, want 200 (body %s)", path, resp.StatusCode, raw)
	}
	var page struct {
		Items  []json.RawMessage `json:"items"`
		Cursor *string           `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode %s page: %v (body %s)", path, err, raw)
	}
	if page.Cursor != nil {
		t.Fatalf("list %s returned a continuation cursor: the collection is larger than this count can see", path)
	}
	return len(page.Items)
}

// seedableKind maps a case's `kind` string onto the store Kind it names. It is an
// allowlist rather than a store.Kind(s) conversion so a typo in a frozen case
// fails the run instead of reaching the store as an unknown table.
func seedableKind(name string) (store.Kind, bool) {
	for _, k := range []store.Kind{
		store.KindScopeNode, store.KindPlaylist, store.KindSchedule, store.KindDaypart,
		store.KindValidityWindow, store.KindFallback, store.KindPresetBatch,
		store.KindAutomation, store.KindWebhookEndpoint, store.KindScreen, store.KindAdoptedDevice,
	} {
		if string(k) == name {
			return k, true
		}
	}
	return "", false
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normalizeJSON re-marshals a decoded value so a frozen expectation and an
// observed response are compared as JSON rather than as Go types (a JSON number
// decodes to float64 on both sides, but nested maps compare more predictably
// through one round trip).
func normalizeJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// TestDataModel1HTTPCorpusIsInTheDrivenManifest keeps this driver's half of
// conformance/driven-manifest.json honest from this side, as
// TestDataModel1DrivenManifestMatchesRuntime does from the engine's.
//
// The claim it pins is the whole entry, not just this driver's share: data-model/1
// drives its full frozen corpus with nothing pending, so the manifest's driven
// list must be exactly the case files on disk. Together with the engine side's
// "driven == my handlers ∪ the request-shaped cases", that leaves no room for an
// id in the manifest that nobody executes.
func TestDataModel1HTTPCorpusIsInTheDrivenManifest(t *testing.T) {
	var all []string
	for _, c := range loadDataModel1Corpus(t) {
		all = append(all, c.CaseID)
	}
	sort.Strings(all)

	entry := loadDrivenManifestEntry(t, "data-model/1")
	if !reflect.DeepEqual(entry.Driven, all) {
		t.Errorf("conformance/driven-manifest.json[%q].driven = %v, want %v (every frozen case) — data-model/1 drives "+
			"its full corpus between the engine driver and this one", "data-model/1", entry.Driven, all)
	}
	if len(entry.Pending) != 0 {
		t.Errorf("conformance/driven-manifest.json[%q].pending = %v, want none", "data-model/1", entry.Pending)
	}

	// And every case this driver owns is in it — the half the equality above
	// would still satisfy if this driver quietly drove nothing.
	driven := map[string]bool{}
	for _, id := range entry.Driven {
		driven[id] = true
	}
	for _, c := range httpDrivenDataModel1Cases(t) {
		if !driven[c.CaseID] {
			t.Errorf("%s is driven here but missing from the manifest's data-model/1 driven list", c.CaseID)
		}
	}
}

// drivenManifestEntry mirrors one contract's record in
// conformance/driven-manifest.json (conformance/cmd/driven-manifest's own Entry
// type) — duplicated rather than imported because that command is `package main`.
type drivenManifestEntry struct {
	Driver  string   `json:"driver"`
	Driven  []string `json:"driven"`
	Pending []string `json:"pending"`
}

func loadDrivenManifestEntry(t *testing.T, contract string) drivenManifestEntry {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(self), "..", "..", "..", "conformance", "driven-manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var manifest map[string]drivenManifestEntry
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	entry, ok := manifest[contract]
	if !ok {
		t.Fatalf("%s has no entry for %q", path, contract)
	}
	return entry
}
