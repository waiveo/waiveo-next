package apihttp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

// cursorTokenGrammar is api/1 API-036's opaque-cursor grammar, recompiled here
// independently so the test asserts EncodeCursor's output against the frozen
// grammar rather than against the implementation's own regexp.
var cursorTokenGrammar = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// paginationCase is the slice of an api-1 corpus case this package's keyset-
// pagination helpers drive: a stable, id-ordered collection and a sequence of
// list requests carrying limit/cursor, plus the per-request `expected`
// {items, cursor} envelope the case pins (API-032). The frozen corpus JSON is
// the oracle — the test reads its expectations from the file, never from
// values re-typed here.
type paginationCase struct {
	CaseID string `json:"case_id"`
	Input  struct {
		CollectionState []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			CreatedAt string `json:"created_at"`
		} `json:"collection_state"`
		Requests []struct {
			Method string            `json:"method"`
			Path   string            `json:"path"`
			Query  map[string]string `json:"query"`
		} `json:"requests"`
	} `json:"input"`
	Expected struct {
		Responses []struct {
			Status int            `json:"status"`
			Body   map[string]any `json:"body"`
		} `json:"responses"`
		CombinedItemsCoverCollectionExactlyOnce bool `json:"combined_items_cover_collection_exactly_once"`
	} `json:"expected"`
}

func loadPaginationCase(t *testing.T, name string) paginationCase {
	t.Helper()
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "api-1", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus case %s: %v", name, err)
	}
	var c paginationCase
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("decode corpus case %s: %v", name, err)
	}
	return c
}

// listItem is the response projection a list endpoint emits per row: only the
// opaque id (API-032's response bodies pin `items:[{id:...}]`, never the full
// stored row).
type listItem struct {
	ID string `json:"id"`
}

// TestPaginationRoundtripCorpus drives API-032: over a two-row collection,
// limit=1 yields page 1 with a non-null opaque cursor equal to the last
// returned id, and re-requesting with that exact cursor yields page 2 with the
// remaining row and cursor:null. The two pages enumerate the collection exactly
// once, in stable id order, and the page-2 cursor is only ever the last
// returned id passed back verbatim (never constructed).
func TestPaginationRoundtripCorpus(t *testing.T) {
	c := loadPaginationCase(t, "API-032-valid-pagination-roundtrip.json")

	// The collection sorted into the stable keyset order (ascending id, which
	// equals creation order for ULIDs). A real handler queries this order.
	sorted := make([]listItem, 0, len(c.Input.CollectionState))
	for _, row := range c.Input.CollectionState {
		sorted = append(sorted, listItem{ID: row.ID})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	if len(c.Input.Requests) != len(c.Expected.Responses) {
		t.Fatalf("corpus %s has %d requests but %d expected responses", c.CaseID, len(c.Input.Requests), len(c.Expected.Responses))
	}

	seen := map[string]int{}
	for i, req := range c.Input.Requests {
		gotStatus, gotBody := servePage(t, sorted, req.Query["cursor"], req.Query["limit"])

		wantResp := c.Expected.Responses[i]
		if gotStatus != wantResp.Status {
			t.Errorf("request %d: status = %d, want %d", i, gotStatus, wantResp.Status)
		}
		if !reflect.DeepEqual(gotBody, wantResp.Body) {
			t.Errorf("request %d: body = %#v, want %#v (corpus-pinned)", i, gotBody, wantResp.Body)
		}

		// Tally the ids this page returned for the exactly-once check.
		if items, ok := gotBody["items"].([]any); ok {
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					if id, ok := m["id"].(string); ok {
						seen[id]++
					}
				}
			}
		}
	}

	if c.Expected.CombinedItemsCoverCollectionExactlyOnce {
		if len(seen) != len(c.Input.CollectionState) {
			t.Errorf("pages returned %d distinct ids, want %d (every row exactly once)", len(seen), len(c.Input.CollectionState))
		}
		for _, row := range c.Input.CollectionState {
			if seen[row.ID] != 1 {
				t.Errorf("row %s was returned %d times across the pages, want exactly once", row.ID, seen[row.ID])
			}
		}
	}
}

// servePage models the read path a list handler follows for one request:
// parse the page params, decode a supplied cursor to its keyset position,
// select the rows after that position, and cut the page with Page(). It
// returns the HTTP status and the JSON-decoded {items, cursor} envelope so the
// test can diff it against the corpus.
func servePage(t *testing.T, sorted []listItem, rawCursor, rawLimit string) (status int, body map[string]any) {
	t.Helper()

	cursor, limit, perr := ParsePageParams(rawCursor, rawLimit)
	if perr != nil {
		t.Fatalf("ParsePageParams(%q, %q) rejected a valid corpus request: %+v", rawCursor, rawLimit, perr)
	}

	lastID := ""
	if cursor != "" {
		id, derr := DecodeCursor(cursor)
		if derr != nil {
			t.Fatalf("DecodeCursor(%q) rejected a valid corpus cursor: %+v", cursor, derr)
		}
		lastID = id
	}

	// The keyset window: every row strictly after the cursor position, then at
	// most limit+1 of them (the extra row is how Page learns a next page
	// exists — the standard fetch-one-more keyset technique).
	after := make([]listItem, 0, len(sorted))
	for _, row := range sorted {
		if row.ID > lastID {
			after = append(after, row)
		}
	}
	if len(after) > limit+1 {
		after = after[:limit+1]
	}

	env := Page(after, limit, func(it listItem) string { return it.ID })

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal page envelope: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode page envelope: %v", err)
	}
	return 200, decoded
}

// TestParsePageParamsDefaultLimit confirms an absent limit resolves to the
// default of 50 and an absent cursor passes through empty (API-030/031).
func TestParsePageParamsDefaultLimit(t *testing.T) {
	cursor, limit, perr := ParsePageParams("", "")
	if perr != nil {
		t.Fatalf("ParsePageParams(\"\", \"\") = error %+v, want the default page", perr)
	}
	if limit != 50 {
		t.Errorf("default limit = %d, want 50", limit)
	}
	if cursor != "" {
		t.Errorf("absent cursor = %q, want empty", cursor)
	}
}

// TestParsePageParamsAcceptsBounds confirms the inclusive [1,200] bounds parse
// unchanged — never clamped, never defaulted (API-031).
func TestParsePageParamsAcceptsBounds(t *testing.T) {
	for raw, want := range map[string]int{"1": 1, "50": 50, "199": 199, "200": 200} {
		_, limit, perr := ParsePageParams("", raw)
		if perr != nil {
			t.Errorf("ParsePageParams(\"\", %q) = error %+v, want limit %d", raw, perr, want)
			continue
		}
		if limit != want {
			t.Errorf("ParsePageParams(\"\", %q) limit = %d, want %d", raw, limit, want)
		}
	}
}

// TestParsePageParamsRejectsOutOfRange confirms a limit outside [1,200] or a
// non-integer limit is rejected 400 / VALIDATION_FAILED — never silently
// clamped to a bound (API-031).
func TestParsePageParamsRejectsOutOfRange(t *testing.T) {
	for _, raw := range []string{"0", "-1", "201", "1000", "abc", "1.5", " 1", "10 ", ""} {
		if raw == "" {
			continue // an absent limit is the default, covered separately
		}
		_, _, perr := ParsePageParams("", raw)
		if perr == nil {
			t.Errorf("ParsePageParams(\"\", %q) accepted an out-of-range/non-integer limit, want VALIDATION_FAILED (never clamped)", raw)
			continue
		}
		if perr.Status != 400 {
			t.Errorf("limit %q: status = %d, want 400", raw, perr.Status)
		}
		if perr.Code != "VALIDATION_FAILED" {
			t.Errorf("limit %q: code = %q, want VALIDATION_FAILED", raw, perr.Code)
		}
	}
}

// TestEncodeCursorIsOpaqueToken confirms EncodeCursor emits the last row's id
// verbatim as the continuation token (the ULID keyset makes the position self-
// describing) and that the token satisfies API-036's opaque-cursor grammar.
func TestEncodeCursorIsOpaqueToken(t *testing.T) {
	lastID := "01J8Z3K4N5P6Q7R8S9T0V1W2Z1"
	tok := EncodeCursor(lastID)
	if tok != lastID {
		t.Errorf("EncodeCursor(%q) = %q, want the id itself (the keyset token IS the last id)", lastID, tok)
	}
	if !cursorTokenGrammar.MatchString(tok) {
		t.Errorf("EncodeCursor(%q) = %q, does not satisfy ^[A-Za-z0-9_-]+$ (API-036)", lastID, tok)
	}
}

// TestDecodeCursorValid confirms a well-formed cursor (a canonical ULID keyset
// position) decodes back to that position with no error.
func TestDecodeCursorValid(t *testing.T) {
	cursor := "01J8Z3K4N5P6Q7R8S9T0V1W2Z1"
	lastID, perr := DecodeCursor(cursor)
	if perr != nil {
		t.Fatalf("DecodeCursor(%q) = error %+v, want a valid keyset position", cursor, perr)
	}
	if lastID != cursor {
		t.Errorf("DecodeCursor(%q) = %q, want %q", cursor, lastID, cursor)
	}
}

// TestDecodeCursorMalformed confirms a cursor failing the opaque grammar is
// rejected 400 / CURSOR_INVALID and yields no keyset position — never a silent
// "start from the beginning" (API-035).
func TestDecodeCursorMalformed(t *testing.T) {
	lastID, perr := DecodeCursor("not a cursor!!")
	if perr == nil {
		t.Fatal("DecodeCursor(\"not a cursor!!\") accepted a malformed cursor, want CURSOR_INVALID (API-035)")
	}
	if lastID != "" {
		t.Errorf("rejected cursor yielded keyset position %q, want empty (no start-from-beginning)", lastID)
	}
	if perr.Status != 400 {
		t.Errorf("status = %d, want 400", perr.Status)
	}
	if perr.Code != "CURSOR_INVALID" {
		t.Errorf("code = %q, want CURSOR_INVALID", perr.Code)
	}
}

// TestDecodeCursorGrammarOKButNotAKeysetPosition confirms a token that
// satisfies the opaque grammar but does not decode to a valid keyset position
// (a valid ULID) is still rejected CURSOR_INVALID — the grammar is necessary,
// not sufficient, and a bad cursor never silently restarts from the beginning
// (API-035).
func TestDecodeCursorGrammarOKButNotAKeysetPosition(t *testing.T) {
	for _, cursor := range []string{"abc-def_123", "0", "01J8Z3K4N5P6Q7R8S9T0V1W2Z"} {
		if !cursorTokenGrammar.MatchString(cursor) {
			t.Fatalf("test bug: %q should satisfy the opaque grammar", cursor)
		}
		lastID, perr := DecodeCursor(cursor)
		if perr == nil {
			t.Errorf("DecodeCursor(%q) accepted a grammar-valid non-ULID, want CURSOR_INVALID", cursor)
			continue
		}
		if lastID != "" {
			t.Errorf("DecodeCursor(%q) yielded keyset position %q, want empty", cursor, lastID)
		}
		if perr.Code != "CURSOR_INVALID" {
			t.Errorf("DecodeCursor(%q) code = %q, want CURSOR_INVALID", cursor, perr.Code)
		}
	}
}

// TestPageLastPageCursorNull confirms Page emits a null next cursor when no
// further row remains (the fetched window did not exceed limit).
func TestPageLastPageCursorNull(t *testing.T) {
	rows := []listItem{{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z8"}}
	env := Page(rows, 1, func(it listItem) string { return it.ID })
	if env.Cursor != nil {
		t.Errorf("Page cursor = %v, want nil on the last page", *env.Cursor)
	}
	if len(env.Items) != 1 || env.Items[0].ID != rows[0].ID {
		t.Errorf("Page items = %#v, want the single remaining row", env.Items)
	}
}

// TestPageMoreRowsSetsCursor confirms Page truncates to limit and sets the next
// cursor to the last returned id when the window carried an extra row.
func TestPageMoreRowsSetsCursor(t *testing.T) {
	rows := []listItem{
		{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z1"},
		{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z8"},
	}
	env := Page(rows, 1, func(it listItem) string { return it.ID })
	if len(env.Items) != 1 || env.Items[0].ID != rows[0].ID {
		t.Fatalf("Page items = %#v, want just the first row", env.Items)
	}
	if env.Cursor == nil {
		t.Fatal("Page cursor = nil, want the first row's id as the next cursor")
	}
	if *env.Cursor != rows[0].ID {
		t.Errorf("Page cursor = %q, want %q (EncodeCursor of the last returned id)", *env.Cursor, rows[0].ID)
	}
}
