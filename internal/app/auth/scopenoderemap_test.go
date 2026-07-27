package auth

import (
	"context"
	"testing"
)

// The scope-node ids this file uses. legacyScreen is the shape a scope node had
// before the authoring store required every row id to be a canonical ULID;
// currentScreen is what that node became. otherNode never moves.
const (
	legacyScreenNode  = "01J8Z4DEMOSCREENFIRSTPHOTN"
	currentScreenNode = "01J8Z4DEM0SCREENF1RSTPH0TN"
	otherNode         = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
)

// TestRemapScopeNodesFollowsARenamedNode: when the authoring store canonicalizes
// a scope node's id, the bindings and grants that name it follow. A binding left
// behind would authorize a node that no longer exists, so the principal would
// quietly lose the subtree they were granted.
func TestRemapScopeNodesFollowsARenamedNode(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	p, err := st.CreatePrincipal(ctx, KindUser, "operator")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := st.PutRoleBinding(ctx, p.PrincipalID, legacyScreenNode, RoleAdmin); err != nil {
		t.Fatalf("PutRoleBinding: %v", err)
	}
	minted, err := st.MintGrant(ctx, MintGrantOptions{
		Purpose:                PurposeSetup,
		ResultingPrincipalKind: KindUser,
		ScopeNode:              legacyScreenNode,
		Role:                   RoleOwner,
		TTLMs:                  DefaultSetupGrantTTLMs,
	})
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}

	n, err := st.RemapScopeNodes(ctx, map[string]string{legacyScreenNode: currentScreenNode})
	if err != nil {
		t.Fatalf("RemapScopeNodes: %v", err)
	}
	if n != 2 {
		t.Fatalf("RemapScopeNodes changed %d row(s), want 2 (one binding, one grant)", n)
	}

	bindings, err := st.RoleBindings(ctx, p.PrincipalID)
	if err != nil {
		t.Fatalf("RoleBindings: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(bindings))
	}
	if bindings[0].ScopeNode != currentScreenNode {
		t.Errorf("binding scope_node = %q, want %q", bindings[0].ScopeNode, currentScreenNode)
	}
	// The role is the principal's authority and must be exactly what it was —
	// following a renamed node is not an occasion to change what anyone may do.
	if bindings[0].Role != RoleAdmin {
		t.Errorf("binding role = %q, want %q", bindings[0].Role, RoleAdmin)
	}

	var grantScope string
	if err := st.db.QueryRow(`SELECT scope_node FROM grants WHERE grant_id = ?`, minted.Grant.GrantID).
		Scan(&grantScope); err != nil {
		t.Fatalf("read grant scope_node: %v", err)
	}
	if grantScope != currentScreenNode {
		t.Errorf("grant scope_node = %q, want %q", grantScope, currentScreenNode)
	}
}

// TestRemapScopeNodesLeavesEverythingElseAlone: a record naming a node that did
// not move, the workspace-root sentinel, and an unscoped grant are all untouched.
// The sentinel matters most — it is not a ULID, so the authoring store never
// renames it, and rewriting it would move an owner off the whole workspace.
func TestRemapScopeNodesLeavesEverythingElseAlone(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	p, err := st.CreatePrincipal(ctx, KindUser, "operator")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := st.PutRoleBinding(ctx, p.PrincipalID, RootScopeNode, RoleOwner); err != nil {
		t.Fatalf("PutRoleBinding(root): %v", err)
	}
	other, err := st.CreatePrincipal(ctx, KindUser, "site-admin")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := st.PutRoleBinding(ctx, other.PrincipalID, otherNode, RoleAdmin); err != nil {
		t.Fatalf("PutRoleBinding(other): %v", err)
	}

	// A mapping that includes the sentinel and the empty node, which the
	// authoring store can never produce but which must be inert if one arrives.
	n, err := st.RemapScopeNodes(ctx, map[string]string{
		legacyScreenNode: currentScreenNode,
		RootScopeNode:    currentScreenNode,
		"":               currentScreenNode,
	})
	if err != nil {
		t.Fatalf("RemapScopeNodes: %v", err)
	}
	if n != 0 {
		t.Fatalf("RemapScopeNodes changed %d row(s), want 0", n)
	}

	rootBindings, err := st.RoleBindings(ctx, p.PrincipalID)
	if err != nil {
		t.Fatalf("RoleBindings: %v", err)
	}
	if len(rootBindings) != 1 || rootBindings[0].ScopeNode != RootScopeNode {
		t.Fatalf("root binding = %+v, want one binding still at %q", rootBindings, RootScopeNode)
	}
	otherBindings, err := st.RoleBindings(ctx, other.PrincipalID)
	if err != nil {
		t.Fatalf("RoleBindings: %v", err)
	}
	if len(otherBindings) != 1 || otherBindings[0].ScopeNode != otherNode {
		t.Fatalf("other binding = %+v, want one binding still at %q", otherBindings, otherNode)
	}
}

// TestRemapScopeNodesOnAnEmptyMappingIsANoOp: the overwhelmingly common case —
// a store that needed no canonicalization — must not open a write transaction
// at all.
func TestRemapScopeNodesOnAnEmptyMappingIsANoOp(t *testing.T) {
	st, _ := newTestStore(t)
	n, err := st.RemapScopeNodes(context.Background(), nil)
	if err != nil {
		t.Fatalf("RemapScopeNodes(nil): %v", err)
	}
	if n != 0 {
		t.Fatalf("RemapScopeNodes(nil) changed %d row(s), want 0", n)
	}
}
