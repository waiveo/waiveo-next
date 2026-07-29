package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Automation fixtures for the api-layer CRUD/run/bulk-enable tests (fixture-ULID
// convention; no secrets). The trigger subject and the device_command target are
// the SAME entity, so the edge rule never trips the RUL-282 cross-entity
// restriction — exactly the shape of the demo rule. An automation's own id is
// now exclusively server-assigned (rejectClientSuppliedID) like every other
// kind, so every body-building helper below takes an empty id and callers
// capture the server-minted one from each create response instead of pinning
// it as a constant.
const (
	autoScreenEntity = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	autoScopeNode    = "01J8Z0B0000000000000000000"
)

// edgeAutomationBody is the management-API resource envelope around a well-formed
// edge rule: a state trigger on autoScreenEntity rising to "on" firing a
// device_command launch on that same entity (RUL-002 edge). The rules/1 members
// (mode/triggers/conditions/actions) sit alongside the resource envelope
// (name/scope_node/labels/enabled) exactly as the openapi Automation schema
// defines; the rule compiler reads only its own vocabulary and ignores the
// envelope fields.
func edgeAutomationBody(id, scopeNode string, labels map[string]string) []byte {
	return automationBody(id, scopeNode, labels, `{"type":"device_command","entity_id":"`+autoScreenEntity+`","command":"launch","params":{"channel":"dev"}}`)
}

// disabledEdgeAutomationBody is edgeAutomationBody with the resource-envelope
// `enabled` flag flipped to false: a compile-clean, edge-classified rule the author
// has switched OFF. It is stored and validated exactly like its enabled sibling —
// the rule compiler ignores the envelope flag — but must never ride edge_rules and
// so must never fire. It reuses edgeAutomationBody so the two differ only by the
// flag under test.
func disabledEdgeAutomationBody(id, scopeNode string) []byte {
	m := map[string]any{}
	_ = json.Unmarshal(edgeAutomationBody(id, scopeNode, nil), &m)
	m["enabled"] = false
	b, _ := json.Marshal(m)
	return b
}

// appAutomationBody compiles but classifies APP (a notify action is app-class
// unconditionally, RUL-210) — stored + validated, but never carried to the relay.
func appAutomationBody(id, scopeNode string, labels map[string]string) []byte {
	return automationBody(id, scopeNode, labels, `{"type":"notify","message":"hello"}`)
}

func automationBody(id, scopeNode string, labels map[string]string, actionJSON string) []byte {
	m := map[string]any{
		"name":       "Demo Automation",
		"scope_node": scopeNode,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "state", "entity_id": autoScreenEntity, "to": []string{"on"}}},
		"conditions": []any{},
		"actions":    []json.RawMessage{json.RawMessage(actionJSON)},
	}
	if labels != nil {
		m["labels"] = labels
	}
	// A create body carries an id only when a case is deliberately exercising
	// API-105's refusal of one: `id` is not a member AutomationCreate declares,
	// and a create body is checked against that declared schema.
	if id != "" {
		m["id"] = id
	}
	b, _ := json.Marshal(m)
	return b
}

// nonCompilingAutomationBody carries a trigger type outside the closed vocabulary,
// so compile.Compile rejects it (UNKNOWN_VOCABULARY_MEMBER, RUL-001) — the create
// must surface 422 / VALIDATION_FAILED with the compiler's message and store
// nothing.
func nonCompilingAutomationBody(id, scopeNode string) []byte {
	m := map[string]any{
		"name":       "Bad Automation",
		"scope_node": scopeNode,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "teleport", "entity_id": autoScreenEntity}},
		"conditions": []any{},
		"actions":    []any{map[string]any{"type": "device_command", "entity_id": autoScreenEntity, "command": "launch"}},
	}
	if id != "" {
		m["id"] = id
	}
	b, _ := json.Marshal(m)
	return b
}

// TestCreateAndGetAutomation: a compile-clean edge rule is created (201 + ETag +
// Location), then GET returns it with its ETag — the same ETag/identity
// conventions scope-nodes honor.
func TestCreateAndGetAutomation(t *testing.T) {
	e := newEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("create ETag = %q, want \"1\"", etag)
	}
	automationAID := decodeID(t, raw)
	if loc := resp.Header.Get("Location"); loc != "/api/v1/automations/"+automationAID {
		t.Fatalf("create Location = %q, want /api/v1/automations/%s", loc, automationAID)
	}

	resp, raw = e.do(t, http.MethodGet, "/api/v1/automations/"+automationAID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("get ETag = %q, want \"1\"", etag)
	}
	if got := decodeID(t, raw); got != automationAID {
		t.Fatalf("got id = %q, want %q", got, automationAID)
	}

	// A missing id is 404 / NOT_FOUND.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/automations/01J8Z0Z0000000000000000000", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-missing status = %d, want 404", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
}

// TestCreateNonCompilingAutomationRejected: a rule that fails compile.Compile is
// rejected 422 / VALIDATION_FAILED carrying the compiler's message + field, and
// nothing is stored (the compile-gate at write, never a bad rule to the relay).
func TestCreateNonCompilingAutomationRejected(t *testing.T) {
	e := newEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", nonCompilingAutomationBody("", autoScopeNode), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("non-compiling create status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	// The compiler's error rides the api/1 `errors` extension (API-013), naming the
	// offending member's field path and the UNKNOWN_VOCABULARY_MEMBER code.
	errsAny, _ := p["errors"].([]any)
	if len(errsAny) == 0 {
		t.Fatalf("non-compiling create carried no errors extension (body %s)", raw)
	}
	first, _ := errsAny[0].(map[string]any)
	if first["field"] != "triggers[0].type" {
		t.Fatalf("compile error field = %v, want triggers[0].type (body %s)", first["field"], raw)
	}
	if first["code"] != "UNKNOWN_VOCABULARY_MEMBER" {
		t.Fatalf("compile error code = %v, want UNKNOWN_VOCABULARY_MEMBER", first["code"])
	}

	// Nothing stored.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/automations", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list automations status = %d, body %s", resp.StatusCode, raw)
	}
	var listed struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Items) != 0 {
		t.Fatalf("rejected automation was stored: %d items, want 0", len(listed.Items))
	}
}

// TestPatchAutomationRecompileGated: PATCH honors If-Match and re-runs the compile
// gate — a patch that breaks compilation is rejected 422 and nothing changes.
func TestPatchAutomationRecompileGated(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	automationAID := decodeID(t, raw)

	rename := mustJSON(t, map[string]string{"name": "Renamed Automation"})

	// No If-Match → 428 / IF_MATCH_REQUIRED.
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/automations/"+automationAID, rename, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("patch-no-ifmatch status = %d, want 428", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "IF_MATCH_REQUIRED")

	// A patch that breaks compilation is rejected 422, revision unchanged.
	badPatch := mustJSON(t, map[string]any{
		"triggers": []any{map[string]any{"type": "teleport", "entity_id": autoScreenEntity}},
	})
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/automations/"+automationAID, badPatch, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("compile-breaking patch status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")

	// Correct If-Match with a benign patch → 200 + ETag "2".
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/automations/"+automationAID, rename, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch-ok status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"2"` {
		t.Fatalf("patch-ok ETag = %q, want \"2\"", etag)
	}

	// DELETE requires If-Match too.
	resp, _ = e.do(t, http.MethodDelete, "/api/v1/automations/"+automationAID, nil, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("delete-no-ifmatch status = %d, want 428", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodDelete, "/api/v1/automations/"+automationAID, nil, map[string]string{"If-Match": `"2"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
}

// TestListAutomationsPaginationAndSelector: the automations list honors keyset
// pagination and the label selector, exactly as the scope-nodes list does.
func TestListAutomationsPaginationAndSelector(t *testing.T) {
	e := newEnv(t)
	create := func(labels map[string]string) string {
		resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, labels), nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
		}
		return decodeID(t, raw)
	}
	automationAID := create(map[string]string{"env": "prod"})
	create(map[string]string{"env": "staging"})
	automationCID := create(map[string]string{"env": "prod"})

	type page struct {
		Items  []json.RawMessage `json:"items"`
		Cursor *string           `json:"cursor"`
	}

	// Page 1: limit=1 → one item + a continuation cursor.
	resp, raw := e.do(t, http.MethodGet, "/api/v1/automations?limit=1", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list p1 status = %d, body %s", resp.StatusCode, raw)
	}
	var p1 page
	if err := json.Unmarshal(raw, &p1); err != nil {
		t.Fatalf("decode p1: %v (body %s)", err, raw)
	}
	if len(p1.Items) != 1 || p1.Cursor == nil {
		t.Fatalf("page 1 items=%d cursor=%v, want 1 item + a cursor", len(p1.Items), p1.Cursor)
	}

	// selector=env=prod → the two prod automations (A and C), not the staging one.
	q := url.Values{"selector": {"env=prod"}}
	resp, raw = e.do(t, http.MethodGet, "/api/v1/automations?"+q.Encode(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("selector status = %d, body %s", resp.StatusCode, raw)
	}
	var sel page
	if err := json.Unmarshal(raw, &sel); err != nil {
		t.Fatalf("decode selector page: %v", err)
	}
	if len(sel.Items) != 2 {
		t.Fatalf("selector env=prod returned %d items, want 2", len(sel.Items))
	}
	for _, it := range sel.Items {
		if id := decodeID(t, it); id != automationAID && id != automationCID {
			t.Fatalf("selector returned unexpected id %q", id)
		}
	}

	// A malformed selector is SELECTOR_INVALID.
	q = url.Values{"selector": {"env = prod"}}
	resp, raw = e.do(t, http.MethodGet, "/api/v1/automations?"+q.Encode(), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-selector status = %d, want 400", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "SELECTOR_INVALID")
}

// TestCreateAutomationExternalIDAndIdempotency: external_id uniqueness (API-101)
// and Idempotency-Key replay (API-050/052) are honored by the same conventions
// scope-nodes uses — the automations handler reuses them, never re-derives them.
func TestCreateAutomationExternalIDAndIdempotency(t *testing.T) {
	e := newEnv(t)

	body := edgeAutomationBody("", autoScopeNode, nil)
	withExt := func(ext string) []byte {
		m := map[string]json.RawMessage{}
		_ = json.Unmarshal(edgeAutomationBody("", autoScopeNode, nil), &m)
		m["external_id"], _ = json.Marshal(ext)
		out, _ := json.Marshal(m)
		return out
	}

	// Idempotency-Key replay: the same key + body yields the identical stored
	// response, not a second row.
	hdr := map[string]string{"Idempotency-Key": "auto-key-1"}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", body, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, body %s", resp.StatusCode, raw)
	}
	resp2, raw2 := e.do(t, http.MethodPost, "/api/v1/automations", body, hdr)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("idempotent replay status = %d, want 201", resp2.StatusCode)
	}
	if string(raw) != string(raw2) {
		t.Fatalf("idempotent replay body differs:\n%s\n%s", raw, raw2)
	}

	// external_id uniqueness within the scope node: a second automation claiming the
	// same external_id under the same scope is EXTERNAL_ID_CONFLICT.
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations", withExt("shared-ext"), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ext create B status = %d, body %s", resp.StatusCode, raw)
	}
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations", withExt("shared-ext"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate external_id status = %d, want 400 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "EXTERNAL_ID_CONFLICT")
}

// TestRunAutomationReturnsDisposition: POST /automations/{id}/run performs a
// synchronous basic invoke, driving the authored edge rule's state trigger and
// returning its mode-evaluation disposition (RUL-246). An edge rule that fires and
// starts a run reports `ran`.
func TestRunAutomationReturnsDisposition(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}
	automationAID := decodeID(t, raw)

	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/"+automationAID+"/run", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d, body %s", resp.StatusCode, raw)
	}
	var out struct {
		RunID       string `json:"run_id"`
		Disposition string `json:"disposition"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode run result: %v (body %s)", err, raw)
	}
	if out.Disposition != "ran" {
		t.Fatalf("run disposition = %q, want ran (body %s)", out.Disposition, raw)
	}
	if out.RunID == "" {
		t.Fatalf("run result missing run_id (body %s)", raw)
	}

	// Running a missing automation is 404 / NOT_FOUND.
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/01J8Z0Z0000000000000000000/run", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("run-missing status = %d, want 404", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
}

// TestRunAppClassAutomationRefused: POST /automations/{id}/run REFUSES an
// app-classified automation. Only edge rules ride live execution (REL-062); an
// app-class rule is stored + validated but its execution is deferred, so a
// synchronous run must not carry it into the engine.Observe/dispatch path a live
// edge rule uses (the safety property: an app-classified rule must not leak into
// live execution). appAutomationBody carries a well-formed state trigger
// identical to the edge fixture's — it is only its notify action that classifies
// it APP — so the refusal is the class guard's doing, not a malformed trigger.
//
// The run is refused 400 / VALIDATION_FAILED naming the app-class reason. The
// detail is asserted deliberately: engine.Load also defensively rejects a
// non-edge rule, so a bare status+code check would still pass with the API-layer
// class guard deleted (the run would 400 with the engine's message instead). The
// detail substring is what pins the API guard specifically — so a future edit
// that drops or inverts it (e.g. the documented app-side-execution fast-follow,
// once engine.Load is relaxed to run app rules app-side) is caught here.
func TestRunAppClassAutomationRefused(t *testing.T) {
	e := newEnv(t)

	// An app-classified automation (a notify action is app-class unconditionally,
	// RUL-210) is stored + validated exactly like an edge rule — the compile gate
	// accepts it; classification, not compilation, is what gates the run.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", appAutomationBody("", autoScopeNode, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app-class automation status = %d, body %s", resp.StatusCode, raw)
	}
	automationAID := decodeID(t, raw)

	// The synchronous run is refused before the rule reaches the engine.
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/"+automationAID+"/run", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("run app-class status = %d, want 400 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if detail, _ := p["detail"].(string); !strings.Contains(detail, "app-class") {
		t.Fatalf("app-class refusal detail = %q, want it to name the app-class reason (body %s)", detail, raw)
	}
}

// TestBulkEnableReturnsJobOverMatchedIDs: POST /automations/bulk-enable is a
// fleet-mutating operation — it returns 202 + an api/1 Job whose targets are the
// selector-matched automation ids, each pending (API-110/111).
func TestBulkEnableReturnsJobOverMatchedIDs(t *testing.T) {
	e := newEnv(t)
	create := func(labels map[string]string) string {
		resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, labels), nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
		}
		return decodeID(t, raw)
	}
	automationAID := create(map[string]string{"env": "prod"})
	create(map[string]string{"env": "staging"})
	automationCID := create(map[string]string{"env": "prod"})

	req := mustJSON(t, map[string]any{"selector": "env=prod", "enabled": false})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", req, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bulk-enable status = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	var job struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Targets []struct {
			TargetID string `json:"target_id"`
			State    string `json:"state"`
		} `json:"targets"`
		CreatedBy string `json:"created_by"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("decode job: %v (body %s)", err, raw)
	}
	if job.ID == "" || job.CreatedBy == "" {
		t.Fatalf("job missing id/created_by (body %s)", raw)
	}
	if job.State != "pending" {
		t.Fatalf("job state = %q, want pending", job.State)
	}
	if len(job.Targets) != 2 {
		t.Fatalf("job targets = %d, want 2 (env=prod matched)", len(job.Targets))
	}
	got := map[string]bool{}
	for _, tg := range job.Targets {
		if tg.State != "pending" {
			t.Fatalf("target %s state = %q, want pending", tg.TargetID, tg.State)
		}
		got[tg.TargetID] = true
	}
	if !got[automationAID] || !got[automationCID] {
		t.Fatalf("job targets = %v, want {A,C}", got)
	}

	// A malformed selector is SELECTOR_INVALID.
	bad := mustJSON(t, map[string]any{"selector": "env = prod", "enabled": true})
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", bad, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-selector bulk-enable status = %d, want 400", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "SELECTOR_INVALID")
}

// TestBulkEnableMalformedBodyIs422: API-013a binds VALIDATION_FAILED to 422 when
// the failure is in the request body — an unparseable JSON body is exactly that
// (not a query-parameter failure), and POST /scope-nodes already returns 422 for
// the identical failure class (parseFields absorbs the parse error into
// zero-valued fields, which then fail datamodel validation). bulk-enable MUST
// agree, not carry a second, 400 status for the same VALIDATION_FAILED code.
func TestBulkEnableMalformedBodyIs422(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", []byte("{not json"), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("malformed-body bulk-enable status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
}

// TestBulkEnableRequiresSelector: `selector` is required (openapi
// AutomationBulkEnableRequest.required). An absent OR empty/whitespace selector
// is rejected 422 / VALIDATION_FAILED rather than silently matching every stored
// automation — the schema marks it required precisely so a fleet-mutating request
// cannot omit its target predicate and touch the whole fleet by accident.
func TestBulkEnableRequiresSelector(t *testing.T) {
	e := newEnv(t)
	// A stored automation the fleet-wide default WOULD have matched, so a missing
	// guard is observable as a Job over it rather than a 422.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}

	// selector omitted entirely.
	omitted := mustJSON(t, map[string]any{"enabled": false})
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", omitted, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("omitted-selector status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")

	// selector present but empty — apiselector treats "" as matching everything,
	// so this MUST be rejected too, not accepted as a fleet-wide target.
	empty := mustJSON(t, map[string]any{"selector": "", "enabled": false})
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", empty, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty-selector status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")

	// whitespace-only selector is likewise empty after trimming.
	blank := mustJSON(t, map[string]any{"selector": "   ", "enabled": false})
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", blank, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("blank-selector status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
}

// TestRunAutomationIdempotency: POST /automations/{id}/run is a mutating POST
// tagged mcp:act, so it MUST honor Idempotency-Key (API-050/072) — a retry with
// the same key replays the retained response verbatim instead of firing a second
// run, while a different key executes a genuinely fresh run.
func TestRunAutomationIdempotency(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}
	automationAID := decodeID(t, raw)
	runID := func(raw []byte) string {
		t.Helper()
		var out struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode run result: %v (body %s)", err, raw)
		}
		if out.RunID == "" {
			t.Fatalf("run result missing run_id (body %s)", raw)
		}
		return out.RunID
	}

	path := "/api/v1/automations/" + automationAID + "/run"
	keyed := map[string]string{"Idempotency-Key": "run-key-1"}

	resp, raw1 := e.do(t, http.MethodPost, path, nil, keyed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first run status = %d, body %s", resp.StatusCode, raw1)
	}
	resp, raw2 := e.do(t, http.MethodPost, path, nil, keyed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent run replay status = %d, want 200", resp.StatusCode)
	}
	// A replay returns the identical retained body — the SAME run_id, not a second run.
	if string(raw1) != string(raw2) {
		t.Fatalf("idempotent run replay body differs (second run fired):\n%s\n%s", raw1, raw2)
	}

	// A different key is a fresh request: it executes a new run with a new run_id.
	resp, raw3 := e.do(t, http.MethodPost, path, nil, map[string]string{"Idempotency-Key": "run-key-2"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second-key run status = %d, body %s", resp.StatusCode, raw3)
	}
	if runID(raw1) == runID(raw3) {
		t.Fatalf("different Idempotency-Key replayed the first run_id %q instead of a fresh run", runID(raw1))
	}
}

// TestBulkEnableIdempotency: POST /automations/bulk-enable is a mutating mcp:act
// POST, so it MUST honor Idempotency-Key (API-050/072). A retry with the same
// key+body replays the original 202 Job verbatim (same job id) rather than
// minting a second Job; the same key with a DIFFERENT body is a reuse conflict
// (409 / IDEMPOTENCY_KEY_REUSED).
func TestBulkEnableIdempotency(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, map[string]string{"env": "prod"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}

	body := mustJSON(t, map[string]any{"selector": "env=prod", "enabled": false})
	keyed := map[string]string{"Idempotency-Key": "bulk-key-1"}

	resp, raw1 := e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", body, keyed)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first bulk-enable status = %d, body %s", resp.StatusCode, raw1)
	}
	resp, raw2 := e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", body, keyed)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("idempotent bulk-enable replay status = %d, want 202", resp.StatusCode)
	}
	// A replay returns the identical Job — same id — not a second, distinct job.
	if string(raw1) != string(raw2) {
		t.Fatalf("idempotent bulk-enable replay body differs (second Job minted):\n%s\n%s", raw1, raw2)
	}

	// Same key, different body → reuse conflict (409).
	other := mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true})
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", other, keyed)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("reuse-with-different-body status = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "IDEMPOTENCY_KEY_REUSED")
}
