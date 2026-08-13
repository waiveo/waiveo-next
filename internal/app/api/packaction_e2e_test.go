package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// packaction_e2e_test.go is the whole RUL-231 round trip: an operator authors a
// rule carrying a `pack_action`, runs it, and the named pack finds real work in
// its queue.
//
// The defect this closes is the silent one. `pack_action` fell through the
// evaluator's default arm, so a rule containing one ran, reported
// `disposition: "ran"`, and asked the pack for nothing. Every assertion here is
// therefore made against the PACK'S QUEUE — the place the effect either exists
// or does not — and only then against the run report, which is the thing that
// used to lie.

// automationPackManifest is an action pack that also exposes its actions to
// automation (MAN-091), which the bare actionPackManifest deliberately does not:
// the two halves are separate declarations and this file's whole subject is what
// happens when they disagree.
func automationPackManifest(id string) map[string]any {
	m := actionPackManifest(id)
	m["actions"] = []any{
		map[string]any{
			"name": "run-backup", "paramsSchema": map[string]any{},
			"capabilityScope": "storage.write", "auditClass": "write",
			"idempotencyClass": "safe-to-retry", "automationCallable": true,
		},
		// Declared automation-facing but as a RELAY command, which RUL-232 routes
		// somewhere this host does not yet reach.
		map[string]any{
			"name": "flash-lights", "paramsSchema": map[string]any{},
			"capabilityScope": "storage.write", "auditClass": "write",
			"idempotencyClass": "safe-to-retry", "automationCallable": true,
		},
		// Declared automation-facing but NOT automationCallable — the manifest
		// contradicting itself, which nothing else in the platform checks.
		map[string]any{
			"name": "charge-card", "paramsSchema": map[string]any{},
			"capabilityScope": "storage.write", "auditClass": "write",
			"idempotencyClass": "not-idempotent",
		},
		// Callable, but never exposed to automation at all (absent from
		// contributes.automation.actions below).
		map[string]any{
			"name": "internal-only", "paramsSchema": map[string]any{},
			"capabilityScope": "storage.write", "auditClass": "write",
			"idempotencyClass": "safe-to-retry", "automationCallable": true,
		},
	}
	m["contributes"] = map[string]any{
		"automation": map[string]any{
			"actions": []any{
				map[string]any{"name": "run-backup", "fieldsSchema": map[string]any{}, "execution": "app-service"},
				map[string]any{"name": "flash-lights", "fieldsSchema": map[string]any{}, "execution": "relay-command"},
				map[string]any{"name": "charge-card", "fieldsSchema": map[string]any{}, "execution": "app-service"},
			},
		},
	}
	return m
}

func (e *testEnv) installAutomationPack(t *testing.T, id string) {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", packBundle(t, automationPackManifest(id)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install automation pack %s: %d %s", id, resp.StatusCode, raw)
	}
}

// mintPackActionAutomation authors a rule whose only action is one pack_action.
func mintPackActionAutomation(t *testing.T, e *testEnv, node string, action map[string]any) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
		"name":       "Pack Action Automation",
		"scope_node": node,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "time", "at": "09:00:00"}},
		"actions":    []any{action},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint pack_action automation: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

func runPackAutomation(t *testing.T, e *testEnv, id string, body string) map[string]any {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+id+"/run", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run automation: %d %s", resp.StatusCode, raw)
	}
	var rep map[string]any
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("decode run report: %v", err)
	}
	return rep
}

// firstPackActionOutcome pulls the single reported pack_action outcome.
func firstPackActionOutcome(t *testing.T, rep map[string]any) map[string]any {
	t.Helper()
	arr, ok := rep["pack_actions"].([]any)
	if !ok {
		t.Fatalf("the run report carries no pack_actions array: %+v", rep)
	}
	if len(arr) != 1 {
		t.Fatalf("want exactly one pack_action outcome, got %d: %+v", len(arr), arr)
	}
	out, _ := arr[0].(map[string]any)
	if out == nil {
		t.Fatalf("outcome is not an object: %+v", arr[0])
	}
	return out
}

// --- the headline -------------------------------------------------------------

// A rule's pack_action must put REAL WORK in the pack's queue — observed by
// leasing it as the pack, not by reading the run report.
func TestPackActionFromARuleReachesThePacksQueue(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installAutomationPack(t, "acme/menu-board")
	token := packSession(t, e, "acme/menu-board")

	// The queue starts empty, so a lease below cannot be satisfied by work some
	// other part of the harness left behind.
	if resp, _ := asPack(t, e, token, http.MethodGet, "/api/v1/pack-invocations/pending", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("precondition: the queue must start empty; lease = %d", resp.StatusCode)
	}

	id := mintPackActionAutomation(t, e, e.orgRoot(t), map[string]any{
		"type":   "pack_action",
		"action": "acme/menu-board.run-backup",
		"params": map[string]any{"full": true},
	})
	rep := runPackAutomation(t, e, id, `{}`)
	if rep["disposition"] != "ran" {
		t.Fatalf("the rule must run; got %v (%+v)", rep["disposition"], rep)
	}

	resp, leased := asPack(t, e, token, http.MethodGet, "/api/v1/pack-invocations/pending", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("RUL-231: the rule's pack_action MUST queue work for the pack; lease = %d. "+
			"This is the silent no-op the action used to be", resp.StatusCode)
	}
	if leased["action"] != "run-backup" {
		t.Fatalf("the pack was handed %v, want the pack-LOCAL action name run-backup", leased["action"])
	}
	// The params the rule declared reached the pack. A route that dropped them
	// would pass every other assertion here.
	params, _ := leased["params"].(map[string]any)
	if params["full"] != true {
		t.Fatalf("params = %v, want the rule's declared params", leased["params"])
	}
}

// The run REPORT names the routing decision, so an operator reading the response
// can tell an invocation that was queued from one that was refused.
func TestPackActionIsReportedInTheRunResult(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installAutomationPack(t, "acme/menu-board")

	id := mintPackActionAutomation(t, e, e.orgRoot(t), map[string]any{
		"type": "pack_action", "action": "acme/menu-board.run-backup",
	})
	out := firstPackActionOutcome(t, runPackAutomation(t, e, id, `{}`))

	if out["action"] != "pack_action" || out["name"] != "acme/menu-board.run-backup" {
		t.Errorf("outcome = %+v, want it to name the action it performed", out)
	}
	if out["ok"] != true {
		t.Errorf("outcome = %+v, want ok", out)
	}
	if out["error"] != nil {
		t.Errorf("a successful routing must report no error; got %v", out["error"])
	}
}

// --- routing: the refusals that matter ----------------------------------------

// RUL-232's whole point. A `relay-command` action MUST NOT be dispatched to the
// pack's handler — and the failure mode this guards is invisible, because
// queueing it anyway is one line and looks like it worked. So the assertion is
// against the QUEUE: nothing must arrive there.
func TestARelayCommandPackActionIsNeverQueuedToThePackHandler_RUL232(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installAutomationPack(t, "acme/menu-board")
	token := packSession(t, e, "acme/menu-board")

	id := mintPackActionAutomation(t, e, e.orgRoot(t), map[string]any{
		"type": "pack_action", "action": "acme/menu-board.flash-lights",
	})
	rep := runPackAutomation(t, e, id, `{}`)

	resp, leased := asPack(t, e, token, http.MethodGet, "/api/v1/pack-invocations/pending", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("RUL-232: an execution: relay-command action MUST NOT reach the pack's handler; "+
			"the pack leased %+v", leased)
	}
	// And it is REPORTED, not dropped: an operator whose rule does nothing must
	// be told why rather than reading a successful run.
	out := firstPackActionOutcome(t, rep)
	if out["ok"] != false {
		t.Fatalf("an unroutable action must not report ok; got %+v", out)
	}
	if !strings.Contains(str(out["error"]), "relay-command") {
		t.Errorf("error = %v, want it to name the execution class that could not be routed", out["error"])
	}
}

// Each of these is a rule pointing at something the pack did not agree to. In
// every case the queue must stay empty AND the report must say which of the
// several possible reasons applied — "nothing happened" alone would leave an
// operator guessing between a typo, an uninstalled pack, and a manifest flag.
func TestPackActionRefusalsAreNamedAndQueueNothing(t *testing.T) {
	cases := []struct {
		name      string
		action    string
		wantError string
	}{
		{"pack not installed", "acme/never-installed.run-backup", "no pack `acme/never-installed` is installed"},
		{"action the pack never declared", "acme/menu-board.no-such-action", "declares no action `no-such-action`"},
		{"declared but not exposed to automation", "acme/menu-board.internal-only", "does not expose"},
		{"exposed but not automationCallable", "acme/menu-board.charge-card", "not automationCallable"},
		{"unqualified name", "run-backup", "publisher-qualified"},
		{"pack id with no publisher", "menuboard.run-backup", "publisher-qualified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
			e.installAutomationPack(t, "acme/menu-board")
			token := packSession(t, e, "acme/menu-board")

			id := mintPackActionAutomation(t, e, e.orgRoot(t), map[string]any{
				"type": "pack_action", "action": tc.action,
			})
			rep := runPackAutomation(t, e, id, `{}`)

			if resp, leased := asPack(t, e, token, http.MethodGet, "/api/v1/pack-invocations/pending", nil); resp.StatusCode != http.StatusNoContent {
				t.Fatalf("a refused pack_action MUST queue nothing; the pack leased %+v", leased)
			}
			out := firstPackActionOutcome(t, rep)
			if out["ok"] != false {
				t.Fatalf("outcome = %+v, want a refusal", out)
			}
			if !strings.Contains(str(out["error"]), tc.wantError) {
				t.Errorf("error = %v, want it to contain %q", out["error"], tc.wantError)
			}
		})
	}
}

// --- dry run --------------------------------------------------------------------

// A dry run reports the routing decision and gives the pack NO work. An
// operator checking what a rule would do must not thereby make it happen.
func TestADryRunRoutesButQueuesNothing(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installAutomationPack(t, "acme/menu-board")
	token := packSession(t, e, "acme/menu-board")

	id := mintPackActionAutomation(t, e, e.orgRoot(t), map[string]any{
		"type": "pack_action", "action": "acme/menu-board.run-backup",
	})
	rep := runPackAutomation(t, e, id, `{"dry_run":true}`)

	if rep["dry_run"] != true {
		t.Fatalf("the run must report itself as a dry run; got %+v", rep["dry_run"])
	}
	if resp, leased := asPack(t, e, token, http.MethodGet, "/api/v1/pack-invocations/pending", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a DRY run MUST queue nothing; the pack leased %+v", leased)
	}
	// Still reported, and reported as routable: the point of a dry run is to be
	// told what would happen, and reporting nothing would make a rule that works
	// look identical to one pointing at an uninstalled pack.
	out := firstPackActionOutcome(t, rep)
	if out["ok"] != true {
		t.Fatalf("a dry run must still report the routing decision it reached; got %+v", out)
	}
}

// A dry run must still REFUSE what a real run would refuse — otherwise the
// preview an operator trusts is exactly wrong about the case they need it for.
func TestADryRunStillReportsARefusal(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installAutomationPack(t, "acme/menu-board")

	id := mintPackActionAutomation(t, e, e.orgRoot(t), map[string]any{
		"type": "pack_action", "action": "acme/menu-board.flash-lights",
	})
	out := firstPackActionOutcome(t, runPackAutomation(t, e, id, `{"dry_run":true}`))

	if out["ok"] != false || !strings.Contains(str(out["error"]), "relay-command") {
		t.Fatalf("a dry run must preview the refusal a real run would give; got %+v", out)
	}
}

// --- idempotency ------------------------------------------------------------------

// MAN-103's class is copied from the manifest AT ENQUEUE, exactly as the
// management route does, so a rule-fired invocation and an operator-fired one
// carry the same retry promise (CTX-111 says so in those words). Observed
// through the leased row rather than the store, so it is the value the pack
// actually receives.
func TestARuleFiredInvocationCarriesTheManifestsIdempotencyClass(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installAutomationPack(t, "acme/menu-board")
	token := packSession(t, e, "acme/menu-board")

	id := mintPackActionAutomation(t, e, e.orgRoot(t), map[string]any{
		"type": "pack_action", "action": "acme/menu-board.run-backup",
	})
	runPackAutomation(t, e, id, `{}`)

	resp, leased := asPack(t, e, token, http.MethodGet, "/api/v1/pack-invocations/pending", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lease = %d, want the queued invocation", resp.StatusCode)
	}
	if leased["idempotency"] != "safe-to-retry" {
		t.Errorf("idempotency = %v, want safe-to-retry from the manifest", leased["idempotency"])
	}
}

// --- isolation ----------------------------------------------------------------------

// A rule naming one pack must not put work in ANOTHER pack's queue. The
// qualified name is the only thing that decides which queue, so a split that
// went wrong would land the work somewhere plausible rather than nowhere.
func TestARulesPackActionOnlyReachesTheNamedPack(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installAutomationPack(t, "acme/menu-board")
	e.installAutomationPack(t, "acme/other-pack")
	other := packSession(t, e, "acme/other-pack")

	id := mintPackActionAutomation(t, e, e.orgRoot(t), map[string]any{
		"type": "pack_action", "action": "acme/menu-board.run-backup",
	})
	runPackAutomation(t, e, id, `{}`)

	if resp, leased := asPack(t, e, other, http.MethodGet, "/api/v1/pack-invocations/pending", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the other pack MUST see nothing; it leased %+v", leased)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
