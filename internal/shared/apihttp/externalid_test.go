package apihttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// externalIDConflictCase is the slice of the api-1 API-102 corpus case this
// file's uniqueness check drives: a create request body (kind/name/parent_id/
// external_id) plus the existing collection_state it collides against, and
// the `expected` {status, write_executed, body} the case pins. The frozen
// corpus JSON is the oracle — the test reads its expectations from the file,
// never from values re-typed here.
type externalIDConflictCase struct {
	CaseID string `json:"case_id"`
	Input  struct {
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    struct {
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			ParentID   string `json:"parent_id"`
			ExternalID string `json:"external_id"`
		} `json:"body"`
		CollectionState []struct {
			ID         string `json:"id"`
			Kind       string `json:"kind"`
			ParentID   string `json:"parent_id"`
			ExternalID string `json:"external_id"`
		} `json:"collection_state"`
	} `json:"input"`
	Expected struct {
		Status        int            `json:"status"`
		ContentType   string         `json:"content_type"`
		WriteExecuted bool           `json:"write_executed"`
		Body          map[string]any `json:"body"`
	} `json:"expected"`
}

func loadExternalIDConflictCase(t *testing.T, name string) externalIDConflictCase {
	t.Helper()
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "api-1", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus case %s: %v", name, err)
	}
	var c externalIDConflictCase
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("decode corpus case %s: %v", name, err)
	}
	return c
}

// scopeNodeResourceType is the api/1 resourceType family tag api.go's
// scopeNodesConfig actually scopes external_id uniqueness by — "scope-nodes",
// shared by every scope-node kind alike (API-101). The corpus's own
// collection_state/body rows carry each row's `kind` (e.g. "screen"), which is
// NOT what production matches on; refsFromCollectionState and this file's
// calls into CheckExternalIDUnique tag every row with this family constant
// instead, so the test drives the SAME matching key the live handler does.
const scopeNodeResourceType = "scope-nodes"

// scopeNodeDisplayName is the human noun api.go's scopeNodesConfig.displayName
// carries, and the wording CheckExternalIDUnique's rejection Detail names.
const scopeNodeDisplayName = "scope node"

// refsFromCollectionState projects a corpus case's collection_state rows into
// the []ExternalRef CheckExternalIDUnique/ResolveRef consume. Every row is
// tagged scopeNodeResourceType — never the row's own `kind` — matching the
// live api.go handler's family-scoped, kind-agnostic matching key (API-101).
func refsFromCollectionState(c externalIDConflictCase) []ExternalRef {
	refs := make([]ExternalRef, 0, len(c.Input.CollectionState))
	for _, row := range c.Input.CollectionState {
		refs = append(refs, ExternalRef{
			ID:           row.ID,
			ExternalID:   row.ExternalID,
			ResourceType: scopeNodeResourceType,
			ScopeNode:    row.ParentID,
		})
	}
	return refs
}

// TestCheckExternalIDUniqueConflictCorpus drives API-101/102: a create whose
// external_id already names another resource of the SAME resource type under
// the SAME parent scope node is rejected 400 / EXTERNAL_ID_CONFLICT before the
// write executes, with the pinned detail reproduced verbatim.
func TestCheckExternalIDUniqueConflictCorpus(t *testing.T) {
	c := loadExternalIDConflictCase(t, "API-102-invalid-external-id-conflict.json")
	existing := refsFromCollectionState(c)

	// selfID is empty: this is a create, so no existing record can be "this
	// same resource" and the check can never suppress itself.
	prob := CheckExternalIDUnique(existing, scopeNodeResourceType, scopeNodeDisplayName, c.Input.Body.ParentID, c.Input.Body.ExternalID, "")
	if prob == nil {
		t.Fatal("CheckExternalIDUnique returned nil (no conflict), want EXTERNAL_ID_CONFLICT (API-102)")
	}

	writeExecuted, gotBody, gotStatus, gotCT := runExternalIDOutcome(c, prob)
	if writeExecuted != c.Expected.WriteExecuted {
		t.Errorf("write_executed = %v, want %v (no write on a rejected external_id check, API-102)", writeExecuted, c.Expected.WriteExecuted)
	}
	if gotStatus != c.Expected.Status {
		t.Errorf("status = %d, want %d", gotStatus, c.Expected.Status)
	}
	if gotCT != c.Expected.ContentType {
		t.Errorf("Content-Type = %q, want %q", gotCT, c.Expected.ContentType)
	}
	assertBodyMatchesCorpus(t, gotBody, c.Expected.Body)
}

// TestCheckExternalIDUniqueDifferentTypeNotCollision asserts the same
// external_id used by a resource of a DIFFERENT type under the same scope
// node is not a collision (API-101) — placement AND type together form the
// uniqueness scope, not the bare string alone.
func TestCheckExternalIDUniqueDifferentTypeNotCollision(t *testing.T) {
	c := loadExternalIDConflictCase(t, "API-102-invalid-external-id-conflict.json")
	existing := refsFromCollectionState(c)

	prob := CheckExternalIDUnique(existing, "device", "device", c.Input.Body.ParentID, c.Input.Body.ExternalID, "")
	if prob != nil {
		t.Errorf("CheckExternalIDUnique for a different resourceType = %+v, want nil (a different type is not a collision, API-101)", prob)
	}
}

// TestCheckExternalIDUniqueDifferentScopeNotCollision asserts the same
// external_id used by a resource of the same type under a DIFFERENT scope
// node is not a collision (API-101).
func TestCheckExternalIDUniqueDifferentScopeNotCollision(t *testing.T) {
	c := loadExternalIDConflictCase(t, "API-102-invalid-external-id-conflict.json")
	existing := refsFromCollectionState(c)

	prob := CheckExternalIDUnique(existing, scopeNodeResourceType, scopeNodeDisplayName, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z9", c.Input.Body.ExternalID, "")
	if prob != nil {
		t.Errorf("CheckExternalIDUnique under a different scopeNode = %+v, want nil (a different scope node is not a collision, API-101)", prob)
	}
}

// TestCheckExternalIDUniqueEmptyNeverCollides asserts an empty external_id
// never collides, however crowded the existing collection under that same
// (type, scope node) — setting external_id is optional (API-100).
func TestCheckExternalIDUniqueEmptyNeverCollides(t *testing.T) {
	c := loadExternalIDConflictCase(t, "API-102-invalid-external-id-conflict.json")
	existing := refsFromCollectionState(c)

	prob := CheckExternalIDUnique(existing, scopeNodeResourceType, scopeNodeDisplayName, c.Input.Body.ParentID, "", "")
	if prob != nil {
		t.Errorf("CheckExternalIDUnique(\"\") = %+v, want nil (an empty external_id never collides)", prob)
	}
}

// TestCheckExternalIDUniqueSelfUpdateNotCollision asserts an update that
// leaves its own external_id unchanged is never mistaken for a collision
// with itself: selfID equal to the existing record's own ID is excluded.
func TestCheckExternalIDUniqueSelfUpdateNotCollision(t *testing.T) {
	c := loadExternalIDConflictCase(t, "API-102-invalid-external-id-conflict.json")
	existing := refsFromCollectionState(c)
	self := c.Input.CollectionState[0]

	prob := CheckExternalIDUnique(existing, scopeNodeResourceType, scopeNodeDisplayName, self.ParentID, self.ExternalID, self.ID)
	if prob != nil {
		t.Errorf("CheckExternalIDUnique with selfID = the colliding record's own ID = %+v, want nil (not a DIFFERENT resource)", prob)
	}
}

// TestCheckExternalIDUniqueOtherResourceStillCollides asserts selfID only
// excuses the record it names — a THIRD resource attempting to reuse a value
// already claimed by a DIFFERENT existing record still collides even when
// selfID is supplied (an update to some other resource entirely).
func TestCheckExternalIDUniqueOtherResourceStillCollides(t *testing.T) {
	c := loadExternalIDConflictCase(t, "API-102-invalid-external-id-conflict.json")
	existing := refsFromCollectionState(c)
	self := c.Input.CollectionState[0]

	prob := CheckExternalIDUnique(existing, scopeNodeResourceType, scopeNodeDisplayName, self.ParentID, self.ExternalID, "01J8Z3K4N5P6Q7R8S9T0V1W2Y2")
	if prob == nil {
		t.Fatal("CheckExternalIDUnique with a selfID that does not name the colliding record = nil, want EXTERNAL_ID_CONFLICT")
	}
	if prob.Status != 400 || prob.Code != "EXTERNAL_ID_CONFLICT" {
		t.Errorf("status/code = %d/%q, want 400/EXTERNAL_ID_CONFLICT", prob.Status, prob.Code)
	}
}

// runExternalIDOutcome models the write path a handler follows: on a non-nil
// *ExternalIDError it emits the Problem and NEVER performs the write; a nil
// error means the write proceeds. It returns whether the write ran plus the
// emitted response so the test can diff it against the corpus.
func runExternalIDOutcome(c externalIDConflictCase, prob *ExternalIDError) (writeExecuted bool, body map[string]any, status int, contentType string) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, c.Input.Path, nil)
	traceID := c.Input.Headers["Trace-Id"]

	if prob == nil {
		writeExecuted = true // a real handler performs the create/update here
		w.WriteHeader(http.StatusOK)
		return writeExecuted, nil, http.StatusOK, ""
	}

	WriteProblemExt(w, r, traceID, prob.Status, prob.Code, prob.Title, prob.Detail, nil)

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		return writeExecuted, nil, w.Code, w.Header().Get("Content-Type")
	}
	return writeExecuted, got, w.Code, w.Header().Get("Content-Type")
}

// --- ResolveRef (API-103) ---

// TestResolveRefByID asserts a value that names an existing record's own id
// resolves to that id directly — the id-first step of API-103.
func TestResolveRefByID(t *testing.T) {
	existing := []ExternalRef{
		{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y0", ExternalID: "lobby-screen-1", ResourceType: "screen", ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"},
	}
	id, prob := ResolveRef("01J8Z3K4N5P6Q7R8S9T0V1W2Y0", existing, "screen", "screen", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5")
	if prob != nil {
		t.Fatalf("ResolveRef by id = error %+v, want a resolved id", prob)
	}
	if id != "01J8Z3K4N5P6Q7R8S9T0V1W2Y0" {
		t.Errorf("ResolveRef by id = %q, want the matched record's own id", id)
	}
}

// TestResolveRefByExternalIDFallback asserts a value that does not name any
// record's id, but does name a record's external_id within the same
// (resourceType, scopeNode), resolves to that record's id — the fallback
// step of API-103.
func TestResolveRefByExternalIDFallback(t *testing.T) {
	existing := []ExternalRef{
		{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y0", ExternalID: "lobby-screen-1", ResourceType: "screen", ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"},
	}
	id, prob := ResolveRef("lobby-screen-1", existing, "screen", "screen", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5")
	if prob != nil {
		t.Fatalf("ResolveRef by external_id = error %+v, want a resolved id", prob)
	}
	if id != "01J8Z3K4N5P6Q7R8S9T0V1W2Y0" {
		t.Errorf("ResolveRef by external_id = %q, want the matched record's own id", id)
	}
}

// TestResolveRefIDTakesPrecedenceOverExternalID asserts the id check runs
// FIRST: a value that happens to equal one record's id is resolved to that
// record even when it would ALSO match a different record's external_id.
func TestResolveRefIDTakesPrecedenceOverExternalID(t *testing.T) {
	const ambiguous = "01J8Z3K4N5P6Q7R8S9T0V1W2Y0"
	existing := []ExternalRef{
		{ID: ambiguous, ExternalID: "lobby-screen-1", ResourceType: "screen", ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"},
		{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y2", ExternalID: ambiguous, ResourceType: "screen", ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"},
	}
	id, prob := ResolveRef(ambiguous, existing, "screen", "screen", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5")
	if prob != nil {
		t.Fatalf("ResolveRef(%q) = error %+v, want a resolved id", ambiguous, prob)
	}
	if id != ambiguous {
		t.Errorf("ResolveRef(%q) = %q, want %q (id resolution takes precedence over external_id, API-103)", ambiguous, id, ambiguous)
	}
}

// TestResolveRefNotFound asserts a value resolving to neither an id nor an
// external_id within scope is rejected with code: NOT_FOUND (API-103) — the
// same code an unresolvable id produces elsewhere in this contract.
func TestResolveRefNotFound(t *testing.T) {
	existing := []ExternalRef{
		{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y0", ExternalID: "lobby-screen-1", ResourceType: "screen", ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"},
	}
	id, prob := ResolveRef("no-such-value", existing, "screen", "screen", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5")
	if prob == nil {
		t.Fatalf("ResolveRef(\"no-such-value\") = (%q, nil), want NOT_FOUND", id)
	}
	if id != "" {
		t.Errorf("unresolved ResolveRef returned id %q, want empty", id)
	}
	if prob.Status != 404 {
		t.Errorf("status = %d, want 404", prob.Status)
	}
	if prob.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", prob.Code)
	}
}

// TestResolveRefExternalIDNotFoundOutsideScope asserts an external_id that
// matches a record of the same value but a DIFFERENT scope node (or a
// different resourceType) does not resolve — API-103's fallback is scoped
// the same way API-101's uniqueness is: (resourceType, scopeNode) together,
// not the bare string alone.
func TestResolveRefExternalIDNotFoundOutsideScope(t *testing.T) {
	existing := []ExternalRef{
		{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y0", ExternalID: "lobby-screen-1", ResourceType: "screen", ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"},
	}

	// Same external_id value, but querying a different scope node.
	if _, prob := ResolveRef("lobby-screen-1", existing, "screen", "screen", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z9"); prob == nil {
		t.Error("ResolveRef matched an external_id under a different scope node, want NOT_FOUND")
	}

	// Same external_id value, but querying a different resourceType.
	if _, prob := ResolveRef("lobby-screen-1", existing, "device", "device", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"); prob == nil {
		t.Error("ResolveRef matched an external_id under a different resourceType, want NOT_FOUND")
	}
}
