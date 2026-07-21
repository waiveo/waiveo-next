package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
)

// Automation fixtures (fixture-ULID convention; no secrets). The trigger subject
// and the device_command target are the SAME entity, so an edge rule never trips
// the RUL-282 cross-entity restriction — exactly the shape of the demo rule.
const (
	autoRuleEntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	autoEdgeRuleID   = "01J8ZA0EDGE0RULE0FIXTURE01"
	autoAppRuleID    = "01J8ZA0APP00RULE0FIXTURE01"
	autoBadRuleID    = "01J8ZA0BAD00RULE0FIXTURE01"
)

// edgeAutomation is a well-formed edge rule: a state trigger on autoRuleEntityID
// rising to "on" firing a device_command on that same entity (RUL-002 edge).
func edgeAutomation(id string) json.RawMessage {
	return json.RawMessage(`{"id":"` + id + `","mode":"single",` +
		`"triggers":[{"type":"state","entity_id":"` + autoRuleEntityID + `","to":["on"]}],` +
		`"conditions":[],` +
		`"actions":[{"type":"device_command","entity_id":"` + autoRuleEntityID + `","command":"launch","params":{"channel":"dev"}}]}`)
}

// appAutomation is a well-formed rule that compiles but classifies APP: the
// notify action is app-class unconditionally (RUL-210), so the whole rule is app.
func appAutomation(id string) json.RawMessage {
	return json.RawMessage(`{"id":"` + id + `","mode":"single",` +
		`"triggers":[{"type":"state","entity_id":"` + autoRuleEntityID + `","to":["on"]}],` +
		`"conditions":[],` +
		`"actions":[{"type":"notify","message":"hello"}]}`)
}

// unknownTriggerAutomation carries a trigger type outside the closed vocabulary,
// so compile.Compile rejects it (UNKNOWN_VOCABULARY_MEMBER, RUL-001) — the store
// must never persist it.
func unknownTriggerAutomation(id string) json.RawMessage {
	return json.RawMessage(`{"id":"` + id + `","mode":"single",` +
		`"triggers":[{"type":"teleport","entity_id":"` + autoRuleEntityID + `"}],` +
		`"conditions":[],` +
		`"actions":[{"type":"device_command","entity_id":"` + autoRuleEntityID + `","command":"launch"}]}`)
}

func ruleIDOf(t *testing.T, body json.RawMessage) string {
	t.Helper()
	var top struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal edge rule body: %v", err)
	}
	return top.ID
}

// TestCreateEdgeAutomationStoredAsEdge: a compile-clean edge rule is stored,
// records execution_class "edge", bumps the generation, and rides EdgeRuleBodies.
func TestCreateEdgeAutomationStoredAsEdge(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	res, err := s.Create(ctx, store.KindAutomation, edgeAutomation(autoEdgeRuleID))
	if err != nil {
		t.Fatalf("create edge automation: %v", err)
	}
	if res.Revision != 1 {
		t.Fatalf("created revision = %d, want 1", res.Revision)
	}
	if res.ID != autoEdgeRuleID {
		t.Fatalf("created id = %q, want %q", res.ID, autoEdgeRuleID)
	}
	if res.ExecutionClass != "edge" {
		t.Fatalf("created execution_class = %q, want \"edge\"", res.ExecutionClass)
	}
	if g := gen(t, s); g != 1 {
		t.Fatalf("generation after one automation create = %d, want 1", g)
	}

	bodies, minor, generation, err := s.EdgeRuleBodies(ctx)
	if err != nil {
		t.Fatalf("EdgeRuleBodies: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("EdgeRuleBodies returned %d bodies, want 1", len(bodies))
	}
	if got := ruleIDOf(t, bodies[0]); got != autoEdgeRuleID {
		t.Fatalf("EdgeRuleBodies[0] rule id = %q, want %q", got, autoEdgeRuleID)
	}
	if minor != "1.0" {
		t.Fatalf("EdgeRuleBodies rules_minor_version = %q, want \"1.0\"", minor)
	}
	if generation != 1 {
		t.Fatalf("EdgeRuleBodies generation = %d, want 1", generation)
	}
}

// TestCreateUnknownTriggerRejectedNothingStored: a rule that fails compile.Compile
// is rejected with the compiler's typed error, and NOTHING is written — the row is
// absent, the generation never advanced, and EdgeRuleBodies stays empty.
func TestCreateUnknownTriggerRejectedNothingStored(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	_, err := s.Create(ctx, store.KindAutomation, unknownTriggerAutomation(autoBadRuleID))
	var ce *compile.CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("unknown-trigger create error = %v, want *compile.CompileError", err)
	}
	if ce.Field != "triggers[0].type" {
		t.Fatalf("compile error field = %q, want \"triggers[0].type\"", ce.Field)
	}

	if g := gen(t, s); g != 0 {
		t.Fatalf("generation advanced on rejected non-compiling create: want 0, got %d", g)
	}
	if _, ok, err := s.Get(ctx, store.KindAutomation, autoBadRuleID); err != nil || ok {
		t.Fatalf("Get rejected rule: ok=%v err=%v, want not-found and no error", ok, err)
	}
	bodies, _, _, err := s.EdgeRuleBodies(ctx)
	if err != nil {
		t.Fatalf("EdgeRuleBodies: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("EdgeRuleBodies after a rejected write = %d bodies, want 0", len(bodies))
	}
}

// TestCreateAppAutomationStoredAsApp: an app-classified rule (a notify action) is
// stored and validated but records execution_class "app" — and is NOT carried by
// EdgeRuleBodies (only edge rules ride edge_rules, REL-062).
func TestCreateAppAutomationStoredAsApp(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	res, err := s.Create(ctx, store.KindAutomation, appAutomation(autoAppRuleID))
	if err != nil {
		t.Fatalf("create app automation: %v", err)
	}
	if res.ExecutionClass != "app" {
		t.Fatalf("created execution_class = %q, want \"app\"", res.ExecutionClass)
	}
	if g := gen(t, s); g != 1 {
		t.Fatalf("generation after one app automation create = %d, want 1", g)
	}

	bodies, _, generation, err := s.EdgeRuleBodies(ctx)
	if err != nil {
		t.Fatalf("EdgeRuleBodies: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("EdgeRuleBodies carried %d bodies, want 0 (an app rule must not ride edge_rules)", len(bodies))
	}
	if generation != 1 {
		t.Fatalf("EdgeRuleBodies generation = %d, want 1", generation)
	}
}

// TestEdgeRuleBodiesReturnsEdgeOnly: with one edge and one app rule stored,
// EdgeRuleBodies returns exactly the edge rule's body, at the store generation.
func TestEdgeRuleBodiesReturnsEdgeOnly(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, store.KindAutomation, edgeAutomation(autoEdgeRuleID)); err != nil {
		t.Fatalf("create edge automation: %v", err)
	}
	if _, err := s.Create(ctx, store.KindAutomation, appAutomation(autoAppRuleID)); err != nil {
		t.Fatalf("create app automation: %v", err)
	}

	bodies, _, generation, err := s.EdgeRuleBodies(ctx)
	if err != nil {
		t.Fatalf("EdgeRuleBodies: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("EdgeRuleBodies returned %d bodies, want 1 (edge only)", len(bodies))
	}
	if got := ruleIDOf(t, bodies[0]); got != autoEdgeRuleID {
		t.Fatalf("EdgeRuleBodies[0] rule id = %q, want the edge rule %q", got, autoEdgeRuleID)
	}
	if generation != 2 {
		t.Fatalf("EdgeRuleBodies generation = %d, want 2 (two creates)", generation)
	}
}

// TestGetAndListPreserveExecutionClass: the compiler's edge/app classification is a
// persisted, row-level property, so a subsequent Get or List of an automation MUST
// report the same execution_class the compile-gated Create recorded — not the empty
// default. Regression: scanResource read only the shared baseline columns, so every
// read path but the one-shot Create/Update return silently dropped execution_class.
func TestGetAndListPreserveExecutionClass(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, store.KindAutomation, edgeAutomation(autoEdgeRuleID)); err != nil {
		t.Fatalf("create edge automation: %v", err)
	}
	if _, err := s.Create(ctx, store.KindAutomation, appAutomation(autoAppRuleID)); err != nil {
		t.Fatalf("create app automation: %v", err)
	}

	// A Get after the write must still carry the persisted classification.
	gotEdge, ok, err := s.Get(ctx, store.KindAutomation, autoEdgeRuleID)
	if err != nil || !ok {
		t.Fatalf("Get edge rule: ok=%v err=%v", ok, err)
	}
	if gotEdge.ExecutionClass != "edge" {
		t.Fatalf("Get edge rule execution_class = %q, want \"edge\"", gotEdge.ExecutionClass)
	}
	gotApp, ok, err := s.Get(ctx, store.KindAutomation, autoAppRuleID)
	if err != nil || !ok {
		t.Fatalf("Get app rule: ok=%v err=%v", ok, err)
	}
	if gotApp.ExecutionClass != "app" {
		t.Fatalf("Get app rule execution_class = %q, want \"app\"", gotApp.ExecutionClass)
	}

	// A List must carry it for every returned row too.
	list, err := s.List(ctx, store.KindAutomation, store.ListFilter{})
	if err != nil {
		t.Fatalf("List automations: %v", err)
	}
	byID := map[string]string{}
	for _, r := range list {
		byID[r.ID] = r.ExecutionClass
	}
	if byID[autoEdgeRuleID] != "edge" {
		t.Fatalf("List edge rule execution_class = %q, want \"edge\"", byID[autoEdgeRuleID])
	}
	if byID[autoAppRuleID] != "app" {
		t.Fatalf("List app rule execution_class = %q, want \"app\"", byID[autoAppRuleID])
	}
}

// TestUpdateRecompilesAndReclassifies: an Update re-runs the compile gate. A patch
// that breaks compilation is rejected (revision/generation unchanged, class kept);
// a patch that turns the rule app-class flips execution_class and drops it from
// EdgeRuleBodies.
func TestUpdateRecompilesAndReclassifies(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	res, err := s.Create(ctx, store.KindAutomation, edgeAutomation(autoEdgeRuleID))
	if err != nil {
		t.Fatalf("create edge automation: %v", err)
	}
	genAfterCreate := gen(t, s)

	// A patch that makes the rule non-compiling is rejected; nothing changes.
	badPatch := json.RawMessage(`{"triggers":[{"type":"teleport","entity_id":"` + autoRuleEntityID + `"}]}`)
	_, err = s.Update(ctx, store.KindAutomation, autoEdgeRuleID, res.Revision, badPatch)
	var ce *compile.CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("non-compiling update error = %v, want *compile.CompileError", err)
	}
	if g := gen(t, s); g != genAfterCreate {
		t.Fatalf("generation advanced on rejected update: %d -> %d", genAfterCreate, g)
	}
	if bodies, _, _, _ := s.EdgeRuleBodies(ctx); len(bodies) != 1 {
		t.Fatalf("edge rule dropped after a rejected update: got %d bodies, want 1", len(bodies))
	}

	// A patch swapping the action for an app-class notify flips the rule to app.
	appPatch := json.RawMessage(`{"actions":[{"type":"notify","message":"hi"}]}`)
	upd, err := s.Update(ctx, store.KindAutomation, autoEdgeRuleID, res.Revision, appPatch)
	if err != nil {
		t.Fatalf("app-flipping update: %v", err)
	}
	if upd.ExecutionClass != "app" {
		t.Fatalf("updated execution_class = %q, want \"app\"", upd.ExecutionClass)
	}
	if bodies, _, _, _ := s.EdgeRuleBodies(ctx); len(bodies) != 0 {
		t.Fatalf("app-reclassified rule still rides edge_rules: got %d bodies, want 0", len(bodies))
	}
}
