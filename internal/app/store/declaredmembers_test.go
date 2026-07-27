package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// declaredmembers_test.go pins the store's representation completion: a write
// that leaves a declared-required member absent is stored — and therefore
// served — with the member materialized at its stated default.
//
// These are the unit-level companions to the api layer's live response-schema
// conformance check (internal/app/api/response_schema_test.go). That check
// proves the SERVED bytes carry every declared member; these pin WHICH value
// each default is, and — for `enabled` — that the value the representation
// reports is the value the carry path acts on.

// dmOrgNodeID / dmChildNodeID / dmAutomationID are this file's fixture ULIDs,
// distinct from every other fixture in the package so a case here can never be
// satisfied (or broken) by a row another case wrote.
const (
	dmOrgNodeID     = "01J8ZDECM0RG0N0DEF1XTVRE01"
	dmChildNodeID   = "01J8ZDECM0CH11DF1XTVRE0001"
	dmAutomationID  = "01J8ZDECM0AT00F1XTVRE00001"
	dmAutomationID2 = "01J8ZDECM0AT00F1XTVRE00002"
)

// bodyOf decodes a stored row body into its top-level members.
func bodyOf(t *testing.T, res store.Resource) map[string]json.RawMessage {
	t.Helper()
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(res.Body, &m); err != nil {
		t.Fatalf("decode stored body: %v", err)
	}
	return m
}

// assertMember fails unless the body carries key with exactly the wanted JSON
// literal. Presence and value are asserted together on purpose: a member present
// as `null` where the schema declares an object is no more usable to a generated
// client than an absent one.
func assertMember(t *testing.T, m map[string]json.RawMessage, key, want string) {
	t.Helper()
	raw, present := m[key]
	if !present {
		t.Fatalf("stored body omits %q, want it materialized as %s", key, want)
	}
	if got := string(raw); got != want {
		t.Fatalf("stored body %q = %s, want %s", key, got, want)
	}
}

// TestScopeNodeCreateMaterializesParentIDAndLabels: a root org node authored
// with nothing but its kind and name is stored with `parent_id: null` and
// `labels: {}` — the two members the openapi ScopeNode schema declares required
// and a minimal create never mentions.
func TestScopeNodeCreateMaterializesParentIDAndLabels(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	res, err := s.Create(ctx, store.KindScopeNode,
		json.RawMessage(`{"id":"`+dmOrgNodeID+`","kind":"org","name":"Root Org"}`))
	if err != nil {
		t.Fatalf("create org scope node: %v", err)
	}
	m := bodyOf(t, res)
	assertMember(t, m, "parent_id", "null")
	assertMember(t, m, "labels", "{}")
}

// TestScopeNodeCreateKeepsAuthoredMembers: completion fills gaps, it never
// overwrites. A create that DID state parent_id and labels keeps exactly what it
// stated.
func TestScopeNodeCreateKeepsAuthoredMembers(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, store.KindScopeNode,
		json.RawMessage(`{"id":"`+dmOrgNodeID+`","kind":"org","name":"Root Org"}`)); err != nil {
		t.Fatalf("create org scope node: %v", err)
	}
	res, err := s.Create(ctx, store.KindScopeNode,
		json.RawMessage(`{"id":"`+dmChildNodeID+`","kind":"site","parent_id":"`+dmOrgNodeID+
			`","name":"Site","labels":{"env":"prod"},"tz":"`+siteTZ+`","lat":41.8781,"long":-87.6298}`))
	if err != nil {
		t.Fatalf("create site scope node: %v", err)
	}
	m := bodyOf(t, res)
	assertMember(t, m, "parent_id", `"`+dmOrgNodeID+`"`)
	assertMember(t, m, "labels", `{"env":"prod"}`)
}

// TestAutomationCreateWithoutEnabledIsStoredDisabled is the decision itself: an
// automation authored without saying whether it is on is stored OFF, and the
// remaining declared members it did not mention are materialized alongside it.
//
// The second half is what makes the first half more than a representation
// detail: the row is NOT carried to the relay's edge_rules, so the automation
// the API reports as disabled is an automation that genuinely does not fire.
func TestAutomationCreateWithoutEnabledIsStoredDisabled(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	res, err := s.Create(ctx, store.KindAutomation, json.RawMessage(
		`{"id":"`+dmAutomationID+`","mode":"single",`+
			`"triggers":[{"type":"state","entity_id":"`+autoRuleEntityID+`","to":["on"]}],`+
			`"actions":[{"type":"device_command","entity_id":"`+autoRuleEntityID+`","command":"launch"}]}`))
	if err != nil {
		t.Fatalf("create automation without enabled: %v", err)
	}
	m := bodyOf(t, res)
	assertMember(t, m, "enabled", "false")
	assertMember(t, m, "max", "null")
	assertMember(t, m, "conditions", "[]")
	assertMember(t, m, "labels", "{}")

	bodies, _, _, err := s.EdgeRuleBodies(ctx)
	if err != nil {
		t.Fatalf("EdgeRuleBodies: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("an automation created without `enabled` rode edge_rules (%d bodies), want 0 — "+
			"the representation says disabled, so the carry path must agree", len(bodies))
	}

	// The same rule authored with an explicit enabled:true DOES ride, so the
	// exclusion above is the flag's doing and not a broken carry path.
	if _, err := s.Create(ctx, store.KindAutomation, edgeAutomationEnabled(dmAutomationID2, true)); err != nil {
		t.Fatalf("create enabled automation: %v", err)
	}
	bodies, _, _, err = s.EdgeRuleBodies(ctx)
	if err != nil {
		t.Fatalf("EdgeRuleBodies (after enabled sibling): %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("EdgeRuleBodies = %d bodies, want 1 (the explicitly enabled rule only)", len(bodies))
	}
}

// TestUpdateNormalizesANulledRequiredMember: a patch that sets a required member
// whose declared type does not admit null TO null is normalized back to the
// stated default rather than persisted — `{"labels": null}` is as unusable to a
// client whose generated type is a map as an absent member is.
func TestUpdateNormalizesANulledRequiredMember(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	res, err := s.Create(ctx, store.KindScopeNode,
		json.RawMessage(`{"id":"`+dmOrgNodeID+`","kind":"org","name":"Root Org","labels":{"env":"prod"}}`))
	if err != nil {
		t.Fatalf("create org scope node: %v", err)
	}
	upd, err := s.Update(ctx, store.KindScopeNode, dmOrgNodeID, res.Revision, json.RawMessage(`{"labels":null}`))
	if err != nil {
		t.Fatalf("patch labels:null: %v", err)
	}
	assertMember(t, bodyOf(t, upd), "labels", "{}")
}
