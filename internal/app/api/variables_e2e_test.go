package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/events"
)

// variables_e2e_test.go drives the WHOLE variables loop over real HTTP against
// the real handler, real store and real rules evaluator.
//
// The defect this closes is not a missing feature; it is a shipped control that
// silently did nothing. The automation builder offered a `variable_write` action
// and a `variable` condition, the compiler accepted both, the store persisted
// both, and at run time the action fell through RunActions' `default:` arm while
// the condition was evaluated against a hardcoded empty map. So the two
// assertions that matter most here are not about status codes:
//
//   - TestVariableWriteActionActuallyWritesTheVariable — the action WRITES.
//   - TestVariableConditionReadsTheStoredValue — the condition READS.
//
// Everything else supports those two.

// --- helpers -----------------------------------------------------------------

// varEnv is a testEnv plus the event log its variable.changed records land in.
type varEnv struct {
	*testEnv
	log *events.EventLog
}

func newVarEnv(t *testing.T) *varEnv {
	t.Helper()
	log := events.NewEventLog(0)
	return &varEnv{testEnv: newEnvWithOptions(t, api.WithVariableEvents(log)), log: log}
}

// createVariable POSTs one variable and returns the decoded response body.
func (e *varEnv) createVariable(t *testing.T, node, name string, value any) map[string]any {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/variables", mustJSON(t, map[string]any{
		"name": name, "value": value, "scope_node": node,
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create variable %s: %d %s", name, resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode created variable: %v", err)
	}
	return body
}

// variableValueAt reads the stored value of `name` placed at `node`, or fails.
// It reads through the LIST API rather than the store handle, so what it
// observes is what a client observes.
func (e *varEnv) variableValueAt(t *testing.T, node, name string) (any, bool) {
	t.Helper()
	_, raw := e.do(t, http.MethodGet, "/api/v1/variables", nil, nil)
	var page struct {
		Items []struct {
			Name      string `json:"name"`
			Value     any    `json:"value"`
			ScopeNode string `json:"scope_node"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode variables page: %v", err)
	}
	for _, it := range page.Items {
		if it.Name == name && it.ScopeNode == node {
			return it.Value, true
		}
	}
	return nil, false
}

// variableChanges returns every variable.changed payload the log holds, in
// order.
func (e *varEnv) variableChanges(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, env := range e.log.After("") {
		if env.Schema != events.SchemaVariableChanged {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("decode variable.changed payload: %v", err)
		}
		payload["__scope_node"] = env.ScopeNode
		out = append(out, payload)
	}
	return out
}

// mintVariableAutomation creates one automation at node with the given
// conditions and actions, and returns its id.
func mintVariableAutomation(t *testing.T, e *varEnv, node string, conditions, actions []any) string {
	t.Helper()
	body := map[string]any{
		"name":       "Variable Automation",
		"scope_node": node,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "time", "at": "09:00:00"}},
		"actions":    actions,
	}
	if conditions != nil {
		body["conditions"] = conditions
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, body), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint automation: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// runAutomation fires one automation by hand and returns the decoded run report.
func runAutomation(t *testing.T, e *varEnv, id string) map[string]any {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+id+"/run", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run automation: %d %s", resp.StatusCode, raw)
	}
	var rep map[string]any
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("decode run report: %v", err)
	}
	return rep
}

// --- PRIORITY 1: the action WRITES -------------------------------------------

// The headline. A rule carrying a `variable_write` action, run for real, must
// change the variable's stored value — observed through the read API, not
// through the run report, because the run report is what used to lie.
func TestVariableWriteActionActuallyWritesTheVariable(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	e.createVariable(t, node, "guest_mode", false)

	if got, _ := e.variableValueAt(t, node, "guest_mode"); got != false {
		t.Fatalf("precondition: guest_mode must start false; got %v", got)
	}

	id := mintVariableAutomation(t, e, node, nil, []any{
		map[string]any{"type": "variable_write", "variable": "guest_mode", "value": true},
	})
	rep := runAutomation(t, e, id)

	if rep["disposition"] != "ran" {
		t.Fatalf("the rule must run; got %v", rep["disposition"])
	}

	got, found := e.variableValueAt(t, node, "guest_mode")
	if !found {
		t.Fatal("the variable disappeared")
	}
	if got != true {
		t.Fatalf("RUL-220: the variable_write action MUST change the stored value; guest_mode is still %v. This is the silent no-op the whole track exists to remove", got)
	}
}

// A write to a name that does not exist yet CREATES the row. An operator does
// not have to pre-declare every variable a rule might set.
func TestVariableWriteActionCreatesAnUndeclaredVariable(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)

	id := mintVariableAutomation(t, e, node, nil, []any{
		map[string]any{"type": "variable_write", "variable": "newly_declared", "value": "hello"},
	})
	runAutomation(t, e, id)

	got, found := e.variableValueAt(t, node, "newly_declared")
	if !found {
		t.Fatal("a write to an undeclared name must create the row")
	}
	if got != "hello" {
		t.Fatalf("value = %v, want hello", got)
	}
}

// The run REPORT names what the write did, so an operator reading the response
// can tell a write that landed from one that did not.
func TestVariableWriteActionIsReportedInTheRunResult(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)

	id := mintVariableAutomation(t, e, node, nil, []any{
		map[string]any{"type": "variable_write", "variable": "reported", "value": 7},
	})
	rep := runAutomation(t, e, id)

	vars, _ := rep["variables"].([]any)
	if len(vars) != 1 {
		t.Fatalf("the run report must carry one variable outcome; got %v", rep["variables"])
	}
	got, _ := vars[0].(map[string]any)
	if got["variable"] != "reported" || got["ok"] != true {
		t.Fatalf("outcome = %v, want reported/ok", got)
	}
	if got["action"] != "variable_write" {
		t.Fatalf("outcome must name the action; got %v", got["action"])
	}
}

// A rule whose write the data model refuses reports the refusal AND writes
// nothing. `Store Open` violates DAT-131a's grammar, and an author can only
// discover that from the run report.
func TestVariableWriteActionRefusalIsReportedAndWritesNothing(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)

	id := mintVariableAutomation(t, e, node, nil, []any{
		map[string]any{"type": "variable_write", "variable": "Store Open", "value": true},
	})
	rep := runAutomation(t, e, id)

	vars, _ := rep["variables"].([]any)
	if len(vars) != 1 {
		t.Fatalf("the refusal must be reported; got %v", rep["variables"])
	}
	got, _ := vars[0].(map[string]any)
	if got["ok"] != false {
		t.Fatalf("a name violating DAT-131a must be refused; got %v", got)
	}
	if msg, _ := got["error"].(string); msg == "" {
		t.Fatalf("the refusal must carry a reason; got %v", got)
	}
	if _, found := e.variableValueAt(t, node, "Store Open"); found {
		t.Fatal("a refused write must store nothing")
	}
}

// A dry run resolves the write and withholds it — the same contract the signage
// sink honours.
func TestVariableWriteActionUnderDryRunWithholdsTheWrite(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	e.createVariable(t, node, "guest_mode", false)

	id := mintVariableAutomation(t, e, node, nil, []any{
		map[string]any{"type": "variable_write", "variable": "guest_mode", "value": true},
	})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+id+"/run",
		[]byte(`{"dry_run":true}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dry run: %d %s", resp.StatusCode, raw)
	}
	if got, _ := e.variableValueAt(t, node, "guest_mode"); got != false {
		t.Fatalf("a dry run must withhold the write; guest_mode is now %v", got)
	}
}

// --- PRIORITY 1: the condition READS -----------------------------------------

// The other half. A rule guarded by a `variable` condition must actually
// consult the stored value: it runs when the value matches and skips when it
// does not.
//
// Both directions are asserted in one test on purpose. A test that only checked
// the `skipped` case would pass against the OLD implementation, where the
// environment was hardcoded empty and every variable condition failed closed.
func TestVariableConditionReadsTheStoredValue(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	e.createVariable(t, node, "guest_mode", true)

	id := mintVariableAutomation(t, e, node,
		[]any{map[string]any{"type": "variable", "variable": "guest_mode", "equals": true}},
		[]any{map[string]any{"type": "variable_write", "variable": "witness", "value": "ran"}})

	// Matching: the rule runs.
	if rep := runAutomation(t, e, id); rep["disposition"] != "ran" {
		t.Fatalf("RUL-150: the condition must SEE guest_mode=true and let the rule run; got %v. An empty variable environment gives exactly this failure", rep["disposition"])
	}
	if got, _ := e.variableValueAt(t, node, "witness"); got != "ran" {
		t.Fatalf("the guarded action must have run; witness = %v", got)
	}

	// Not matching: the rule skips. Flip the variable and re-run.
	e.setVariable(t, node, "guest_mode", false)
	if rep := runAutomation(t, e, id); rep["disposition"] != "skipped" {
		t.Fatalf("with guest_mode=false the rule must skip; got %v", rep["disposition"])
	}
}

// A condition reads through DAT-134's ancestor walk: a variable declared at the
// org resolves for a rule placed at a descendant node.
func TestVariableConditionResolvesThroughAnAncestor_DAT134(t *testing.T) {
	e := newVarEnv(t)
	org := e.orgRoot(t)
	child := e.placementNode(t)
	e.createVariable(t, org, "store_open", true)

	id := mintVariableAutomation(t, e, child,
		[]any{map[string]any{"type": "variable", "variable": "store_open", "equals": true}},
		[]any{map[string]any{"type": "variable_write", "variable": "witness", "value": "ran"}})

	if rep := runAutomation(t, e, id); rep["disposition"] != "ran" {
		t.Fatalf("DAT-134: a rule at a descendant must resolve the org's store_open; got %v", rep["disposition"])
	}
}

// A name nothing declares fails the condition closed (RUL-150) — the rule
// skips rather than erroring.
func TestVariableConditionFailsClosedOnAnUndeclaredName_RUL150(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)

	id := mintVariableAutomation(t, e, node,
		[]any{map[string]any{"type": "variable", "variable": "never_declared", "equals": true}},
		[]any{map[string]any{"type": "variable_write", "variable": "witness", "value": "ran"}})

	if rep := runAutomation(t, e, id); rep["disposition"] != "skipped" {
		t.Fatalf("an undeclared variable must fail the condition CLOSED; got %v", rep["disposition"])
	}
	if _, found := e.variableValueAt(t, node, "witness"); found {
		t.Fatal("a skipped rule must run no actions")
	}
}

// --- placement: a rule writes an OVERRIDE at its own node --------------------

// A rule placed at a descendant writing a name the org declares must create an
// override AT ITS OWN NODE, leaving the org's row untouched. Writing the
// ancestor instead would let a rule authored at one group retarget every other
// group's rules — a widening no placement check would catch.
func TestVariableWriteLandsAtTheRulesOwnNodeNotTheAncestors(t *testing.T) {
	e := newVarEnv(t)
	org := e.orgRoot(t)
	child := e.placementNode(t)
	e.createVariable(t, org, "store_open", "org-value")

	id := mintVariableAutomation(t, e, child, nil, []any{
		map[string]any{"type": "variable_write", "variable": "store_open", "value": "child-value"},
	})
	runAutomation(t, e, id)

	orgValue, orgFound := e.variableValueAt(t, org, "store_open")
	if !orgFound || orgValue != "org-value" {
		t.Fatalf("the ANCESTOR's row must be untouched; org store_open = %v (found=%v)", orgValue, orgFound)
	}
	childValue, childFound := e.variableValueAt(t, child, "store_open")
	if !childFound {
		t.Fatal("the rule's own node must carry the new override row")
	}
	if childValue != "child-value" {
		t.Fatalf("the override's value = %v, want child-value", childValue)
	}
}

// --- DAT-137 / EVT-084: the event ---------------------------------------------

// A create, an update and a delete each emit exactly one variable.changed, with
// null on the side that did not exist.
func TestVariableWritesEmitVariableChanged_DAT137(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)

	created := e.createVariable(t, node, "guest_mode", false)
	id, _ := created["id"].(string)

	e.setVariable(t, node, "guest_mode", true)

	etag := e.etagOf(t, "/api/v1/variables/"+id)
	resp, raw := e.do(t, http.MethodDelete, "/api/v1/variables/"+id, nil,
		map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, raw)
	}

	changes := e.variableChanges(t)
	if len(changes) != 3 {
		t.Fatalf("DAT-137: one event per committed write — create, update, delete; got %d: %+v", len(changes), changes)
	}

	// Create: old_value null (unset beforehand), new_value the created value.
	if changes[0]["old_value"] != nil {
		t.Errorf("create: old_value must be null (EVT-084 'unset beforehand'); got %v", changes[0]["old_value"])
	}
	if changes[0]["new_value"] != false {
		t.Errorf("create: new_value = %v, want false", changes[0]["new_value"])
	}
	// Update: both sides present.
	if changes[1]["old_value"] != false || changes[1]["new_value"] != true {
		t.Errorf("update: want old=false new=true; got old=%v new=%v", changes[1]["old_value"], changes[1]["new_value"])
	}
	// Delete: new_value null (unset by this change).
	if changes[2]["old_value"] != true {
		t.Errorf("delete: old_value must be the value it held; got %v", changes[2]["old_value"])
	}
	if changes[2]["new_value"] != nil {
		t.Errorf("delete: new_value must be null (EVT-084 'unset by this change'); got %v", changes[2]["new_value"])
	}

	for i, c := range changes {
		if c["variable"] != "guest_mode" {
			t.Errorf("change[%d]: variable = %v, want guest_mode", i, c["variable"])
		}
		if c["__scope_node"] != node {
			t.Errorf("change[%d]: DAT-137 files the event at the WRITTEN ROW's placement; got %v want %v", i, c["__scope_node"], node)
		}
	}
}

// A RULE's write emits the event too. The api-layer afterCommit hook never sees
// it — the sink writes straight to the store — so an emitter that lived only in
// the handler would leave exactly the automatic writes unpublished, and with
// them the `event`-kind trigger that fires on a variable change (EVT-085).
func TestARulesVariableWriteAlsoEmitsVariableChanged_DAT137(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	e.createVariable(t, node, "guest_mode", false)

	before := len(e.variableChanges(t))

	id := mintVariableAutomation(t, e, node, nil, []any{
		map[string]any{"type": "variable_write", "variable": "guest_mode", "value": true},
	})
	runAutomation(t, e, id)

	changes := e.variableChanges(t)
	if len(changes) != before+1 {
		t.Fatalf("a rule's committed write must emit exactly one variable.changed; had %d, now %d", before, len(changes))
	}
	last := changes[len(changes)-1]
	if last["old_value"] != false || last["new_value"] != true {
		t.Fatalf("the rule's write must publish both sides; got old=%v new=%v", last["old_value"], last["new_value"])
	}
}

// A REFUSED write publishes nothing. The event says a value changed, and one
// that did not change must not say so.
func TestARefusedVariableWriteEmitsNothing(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	before := len(e.variableChanges(t))

	id := mintVariableAutomation(t, e, node, nil, []any{
		map[string]any{"type": "variable_write", "variable": "Not A Name", "value": true},
	})
	runAutomation(t, e, id)

	if after := len(e.variableChanges(t)); after != before {
		t.Fatalf("a refused write must publish nothing; had %d, now %d", before, after)
	}
}

// --- the field-level refusals ------------------------------------------------

// A create violating DAT-131a's name grammar or DAT-132/133's value rule is
// REFUSED, naming the offending field, and stores nothing.
//
// It asserts the refusal and the field, NOT the code — and that is a deliberate,
// recorded limitation rather than a weak assertion. `VariableCreate` declares
// `name` with the DAT-131a `pattern` and `value` as a three-scalar `oneOf`, so
// the request-body schema refuses both before the row validator runs; the
// response is a 422 VALIDATION_FAILED whose `detail` names the field. That is
// exactly the shape conformance/traceability/data-model-1.md already records for
// DEVICE_IDENTITY_INCOMPLETE, and it is kept for the reason recorded there:
// making the code reachable over HTTP would mean weakening a declared schema so
// a second refusal could fire, which costs a caller the clearer answer and a
// generated client its scalar union.
//
// The published codes are not thereby unreachable promises — they have a LIVE
// runtime raiser, which is where the difference from DEVICE_IDENTITY_INCOMPLETE
// lies. See TestVariableWriteRefusalsCarryTheirPublishedCodes below.
func TestVariableCreateRefusals(t *testing.T) {
	cases := []struct {
		name      string
		body      map[string]any
		wantField string
	}{
		{"name outside the grammar", map[string]any{"name": "Store Open", "value": true}, "name"},
		{"name with a hyphen", map[string]any{"name": "store-open", "value": true}, "name"},
		{"object value", map[string]any{"name": "ok_name", "value": map[string]any{"k": 1}}, "value"},
		{"array value", map[string]any{"name": "ok_name", "value": []any{1}}, "value"},
		{"null value", map[string]any{"name": "ok_name", "value": nil}, "value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newVarEnv(t)
			body := map[string]any{"scope_node": e.orgRoot(t)}
			for k, v := range c.body {
				body[k] = v
			}
			resp, raw := e.do(t, http.MethodPost, "/api/v1/variables", mustJSON(t, body), nil)
			if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("want a 4xx refusal; got %d %s", resp.StatusCode, raw)
			}
			if !refusalNamesField(t, raw, c.wantField) {
				t.Fatalf("the refusal must name the offending field %q; got %s", c.wantField, raw)
			}
			// …and nothing was stored. A refusal that still wrote the row would be
			// the worst of both answers.
			name, _ := c.body["name"].(string)
			if _, found := e.variableValueAt(t, e.orgRoot(t), name); found {
				t.Fatalf("a refused create must store nothing; %q exists", name)
			}
		})
	}
}

// The published field-level codes DAT-131a and DAT-132/133 name are raised by
// the row validator, and they reach an operator through the RUN REPORT of a
// rule whose `variable_write` names a bad row — the path a request body never
// passes through, and a live one rather than a seed or a migration.
//
// This is the half that makes the codes real. Without it they would be
// published, allowlisted-as-unimplemented forever, and the register would be
// promising a refusal nothing can produce.
func TestVariableWriteRefusalsCarryTheirPublishedCodes(t *testing.T) {
	cases := []struct {
		name     string
		variable string
		value    any
		wantCode string
	}{
		{"name outside the grammar", "Store Open", true, "VARIABLE_NAME_INVALID"},
		{"name with a hyphen", "store-open", true, "VARIABLE_NAME_INVALID"},
		{"name over 64 characters", "a234567890123456789012345678901234567890123456789012345678901234x", true, "VARIABLE_NAME_INVALID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newVarEnv(t)
			node := e.orgRoot(t)
			id := mintVariableAutomation(t, e, node, nil, []any{
				map[string]any{"type": "variable_write", "variable": c.variable, "value": c.value},
			})
			rep := runAutomation(t, e, id)

			vars, _ := rep["variables"].([]any)
			if len(vars) != 1 {
				t.Fatalf("the refusal must be reported; got %v", rep["variables"])
			}
			got, _ := vars[0].(map[string]any)
			if got["ok"] != false {
				t.Fatalf("want a refusal; got %v", got)
			}
			msg, _ := got["error"].(string)
			if !strings.Contains(msg, c.wantCode) {
				t.Fatalf("the refusal must carry the published code %s; got %q", c.wantCode, msg)
			}
		})
	}
}

// The duplicate-name refusal DOES reach an api/1 response with its published
// code, because the uniqueness rule is decided inside the write transaction —
// past every schema check. It is the one of the three that a client can branch
// on over HTTP, and that difference is worth an assertion of its own.
func TestVariableDuplicateRefusalCarriesItsPublishedCodeOverHTTP(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	e.createVariable(t, node, "guest_mode", false)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/variables", mustJSON(t, map[string]any{
		"name": "guest_mode", "value": true, "scope_node": node,
	}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422; got %d %s", resp.StatusCode, raw)
	}
	if !bodyCarriesFieldCode(t, raw, "VARIABLE_NAME_DUPLICATE") {
		t.Fatalf("want VARIABLE_NAME_DUPLICATE in errors[]; got %s", raw)
	}
}

// DAT-131: the same name twice at one node is refused, atomically, with the
// published code — not a 500, and not two rows.
func TestVariableDuplicateNameAtOneNodeIsRefused_DAT131(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	e.createVariable(t, node, "guest_mode", false)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/variables", mustJSON(t, map[string]any{
		"name": "guest_mode", "value": true, "scope_node": node,
	}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a duplicate name must be a 422, not %d: %s", resp.StatusCode, raw)
	}
	if !bodyCarriesFieldCode(t, raw, "VARIABLE_NAME_DUPLICATE") {
		t.Fatalf("want VARIABLE_NAME_DUPLICATE; got %s", raw)
	}
}

// …and the same name at a DIFFERENT node is fine — that is what an override IS
// (DAT-134). A uniqueness check written over the whole table rather than per
// scope node would refuse this, making overrides impossible.
func TestVariableSameNameAtADifferentNodeIsAllowed_DAT134(t *testing.T) {
	e := newVarEnv(t)
	org := e.orgRoot(t)
	child := e.placementNode(t)
	e.createVariable(t, org, "store_open", "org-value")
	e.createVariable(t, child, "store_open", "child-value")

	if got, _ := e.variableValueAt(t, org, "store_open"); got != "org-value" {
		t.Errorf("org row = %v", got)
	}
	if got, _ := e.variableValueAt(t, child, "store_open"); got != "child-value" {
		t.Errorf("child row = %v", got)
	}
}

// A patch that changes only the value must NOT collide with the row's own name.
func TestVariablePatchingTheValueDoesNotSelfCollide(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	created := e.createVariable(t, node, "guest_mode", false)
	id, _ := created["id"].(string)

	resp, raw := e.do(t, http.MethodPatch, "/api/v1/variables/"+id,
		mustJSON(t, map[string]any{"value": true}),
		map[string]string{"If-Match": e.etagOf(t, "/api/v1/variables/"+id)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patching the value must succeed; got %d %s", resp.StatusCode, raw)
	}
	if got, _ := e.variableValueAt(t, node, "guest_mode"); got != true {
		t.Fatalf("value = %v, want true", got)
	}
}

// A patch that MOVES the name onto one already taken at the node is refused.
func TestVariablePatchOntoATakenNameIsRefused_DAT131(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	e.createVariable(t, node, "taken", 1)
	other := e.createVariable(t, node, "free", 2)
	id, _ := other["id"].(string)

	resp, raw := e.do(t, http.MethodPatch, "/api/v1/variables/"+id,
		mustJSON(t, map[string]any{"name": "taken"}),
		map[string]string{"If-Match": e.etagOf(t, "/api/v1/variables/"+id)})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("renaming onto a taken name must be a 422; got %d %s", resp.StatusCode, raw)
	}
	if !bodyCarriesFieldCode(t, raw, "VARIABLE_NAME_DUPLICATE") {
		t.Fatalf("want VARIABLE_NAME_DUPLICATE; got %s", raw)
	}
}

// A patch that makes the value non-scalar is refused too — the rules are applied
// to the EFFECTIVE post-merge body, not only to a create.
func TestVariablePatchToANonScalarValueIsRefused_DAT132(t *testing.T) {
	e := newVarEnv(t)
	node := e.orgRoot(t)
	created := e.createVariable(t, node, "guest_mode", false)
	id, _ := created["id"].(string)

	resp, raw := e.do(t, http.MethodPatch, "/api/v1/variables/"+id,
		mustJSON(t, map[string]any{"value": map[string]any{"k": 1}}),
		map[string]string{"If-Match": e.etagOf(t, "/api/v1/variables/"+id)})
	if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a non-scalar patch must be refused; got %d %s", resp.StatusCode, raw)
	}
	if got, _ := e.variableValueAt(t, node, "guest_mode"); got != false {
		t.Fatalf("a refused patch must change nothing; got %v", got)
	}
}

// --- DAT-136: deleting an override re-exposes the ancestor, over real HTTP ---

func TestDeletingAVariableOverrideReExposesTheAncestor_DAT136(t *testing.T) {
	e := newVarEnv(t)
	org := e.orgRoot(t)
	child := e.placementNode(t)
	e.createVariable(t, org, "store_open", "org-value")
	override := e.createVariable(t, child, "store_open", "child-value")
	overrideID, _ := override["id"].(string)

	// A rule at the child sees the override…
	id := mintVariableAutomation(t, e, child,
		[]any{map[string]any{"type": "variable", "variable": "store_open", "equals": "child-value"}},
		[]any{map[string]any{"type": "log", "message": "guarded"}})
	if rep := runAutomation(t, e, id); rep["disposition"] != "ran" {
		t.Fatalf("precondition: the override must be effective at the child; got %v", rep["disposition"])
	}

	// …and after deleting it, the ANCESTOR's value is what resolves there.
	resp, raw := e.do(t, http.MethodDelete, "/api/v1/variables/"+overrideID, nil,
		map[string]string{"If-Match": e.etagOf(t, "/api/v1/variables/"+overrideID)})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete override: %d %s", resp.StatusCode, raw)
	}

	orgGuarded := mintVariableAutomation(t, e, child,
		[]any{map[string]any{"type": "variable", "variable": "store_open", "equals": "org-value"}},
		[]any{map[string]any{"type": "log", "message": "guarded"}})
	if rep := runAutomation(t, e, orgGuarded); rep["disposition"] != "ran" {
		t.Fatalf("DAT-136: deleting the override must RE-EXPOSE the ancestor's value, not unset the name; got %v", rep["disposition"])
	}
}

// --- helpers on testEnv -------------------------------------------------------

// setVariable patches the named variable at node to value, through the real
// PATCH with a real If-Match.
func (e *varEnv) setVariable(t *testing.T, node, name string, value any) {
	t.Helper()
	_, raw := e.do(t, http.MethodGet, "/api/v1/variables", nil, nil)
	var page struct {
		Items []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			ScopeNode string `json:"scope_node"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode variables page: %v", err)
	}
	for _, it := range page.Items {
		if it.Name != name || it.ScopeNode != node {
			continue
		}
		resp, body := e.do(t, http.MethodPatch, "/api/v1/variables/"+it.ID,
			mustJSON(t, map[string]any{"value": value}),
			map[string]string{"If-Match": e.etagOf(t, "/api/v1/variables/"+it.ID)})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set variable %s: %d %s", name, resp.StatusCode, body)
		}
		return
	}
	t.Fatalf("no variable named %s at %s to set", name, node)
}

// etagOf reads a resource's current ETag.
func (e *varEnv) etagOf(t *testing.T, path string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read etag of %s: %d %s", path, resp.StatusCode, raw)
	}
	return resp.Header.Get("ETag")
}

// refusalNamesField reports whether a Problem body attributes its refusal to
// the named field — either through the `errors` array (the row validator's
// shape) or through `detail`, which is where the request-body schema check
// writes `"<field>: <reason>"`. Both are real refusal shapes on this surface,
// and a helper that recognized only one would make a passing test depend on
// which layer happened to answer first.
func refusalNamesField(t *testing.T, raw []byte, field string) bool {
	t.Helper()
	var problem struct {
		Detail string `json:"detail"`
		Errors []struct {
			Field string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &problem); err != nil {
		return false
	}
	for _, e := range problem.Errors {
		if e.Field == field {
			return true
		}
	}
	return strings.HasPrefix(problem.Detail, field+":")
}

// bodyCarriesFieldCode reports whether a Problem body's `errors` array carries
// the given field-level code. It asserts on the CODE, never on prose.
func bodyCarriesFieldCode(t *testing.T, raw []byte, code string) bool {
	t.Helper()
	var problem struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &problem); err != nil {
		return false
	}
	for _, e := range problem.Errors {
		if e.Code == code {
			return true
		}
	}
	return false
}
