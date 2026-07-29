package auth

import (
	"context"
	"testing"
)

// TestEnsureDevScriptKeyGrantsExactlyAdminAtRoot pins the authority the dev key
// carries. It is the assertion that would fail if someone "fixed" a probe by
// widening the binding to `owner`: the whole argument for this credential (see
// scripts/devcred's package doc) is that admin is the least authority that
// serves every probe, and that not taking owner keeps the deployment's owner
// count at zero so the first-boot claim window stays open.
func TestEnsureDevScriptKeyGrantsExactlyAdminAtRoot(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	key, err := st.EnsureDevScriptKey(ctx, "")
	if err != nil {
		t.Fatalf("EnsureDevScriptKey: %v", err)
	}
	if key.Role != RoleAdmin {
		t.Fatalf("role = %q, want %q", key.Role, RoleAdmin)
	}
	if key.ScopeNode != RootScopeNode {
		t.Fatalf("scope node = %q, want %q", key.ScopeNode, RootScopeNode)
	}

	bindings, err := st.RoleBindings(ctx, key.PrincipalID)
	if err != nil {
		t.Fatalf("RoleBindings: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want exactly 1: %+v", len(bindings), bindings)
	}

	owners, err := st.CountOwnerBindings(ctx)
	if err != nil {
		t.Fatalf("CountOwnerBindings: %v", err)
	}
	if owners != 0 {
		t.Fatalf("provisioning a dev key created %d owner binding(s); it must not claim the box", owners)
	}
}

// TestEnsureDevScriptKeyAuthenticates proves the minted token is an ordinary
// live api-key session — the thing the middleware resolves — rather than a row
// that merely exists.
func TestEnsureDevScriptKeyAuthenticates(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	key, err := st.EnsureDevScriptKey(ctx, "")
	if err != nil {
		t.Fatalf("EnsureDevScriptKey: %v", err)
	}
	session, err := st.LookupSession(ctx, key.Token)
	if err != nil {
		t.Fatalf("LookupSession on the minted key: %v", err)
	}
	if session.TokenKind != TokenKindAPIKey {
		t.Fatalf("token kind = %q, want %q", session.TokenKind, TokenKindAPIKey)
	}
	if session.PrincipalID != key.PrincipalID {
		t.Fatalf("session principal %q != reported principal %q", session.PrincipalID, key.PrincipalID)
	}
}

// TestEnsureDevScriptKeyReusesALiveKey covers the property that makes this safe
// to run on every `make dev-up`: a key that still authenticates is left exactly
// as it is, and no second principal appears.
func TestEnsureDevScriptKeyReusesALiveKey(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	first, err := st.EnsureDevScriptKey(ctx, "")
	if err != nil {
		t.Fatalf("EnsureDevScriptKey (first): %v", err)
	}
	before, err := st.CountPrincipals(ctx)
	if err != nil {
		t.Fatalf("CountPrincipals: %v", err)
	}

	second, err := st.EnsureDevScriptKey(ctx, first.Token)
	if err != nil {
		t.Fatalf("EnsureDevScriptKey (second): %v", err)
	}
	if !second.Reused {
		t.Fatalf("a live key was re-minted instead of reused")
	}
	if second.Token != first.Token {
		t.Fatalf("token changed while reporting Reused")
	}
	if second.PrincipalID != first.PrincipalID {
		t.Fatalf("principal changed across runs: %q -> %q", first.PrincipalID, second.PrincipalID)
	}

	after, err := st.CountPrincipals(ctx)
	if err != nil {
		t.Fatalf("CountPrincipals: %v", err)
	}
	if after != before {
		t.Fatalf("principal count moved %d -> %d across an idempotent run", before, after)
	}
}

// TestEnsureDevScriptKeyRevokesThePriorKey pins the rotation half: when the
// presented key no longer authenticates (or none was presented), the previous
// key is revoked BEFORE a new one is minted, so a stale copy in a shell history
// or a file backup stops working the moment a fresh key exists.
func TestEnsureDevScriptKeyRevokesThePriorKey(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	first, err := st.EnsureDevScriptKey(ctx, "")
	if err != nil {
		t.Fatalf("EnsureDevScriptKey (first): %v", err)
	}
	// Present nothing, as a lost or wrongly-permissioned key file would.
	second, err := st.EnsureDevScriptKey(ctx, "")
	if err != nil {
		t.Fatalf("EnsureDevScriptKey (second): %v", err)
	}
	if second.Reused {
		t.Fatalf("re-minted key reported Reused")
	}
	if second.Token == first.Token {
		t.Fatalf("a fresh mint returned the previous token")
	}
	if _, err := st.LookupSession(ctx, first.Token); err == nil {
		t.Fatalf("the previous dev key still authenticates after a re-mint")
	}
	if _, err := st.LookupSession(ctx, second.Token); err != nil {
		t.Fatalf("the fresh dev key does not authenticate: %v", err)
	}

	// Re-minting must keep working. A fixed credential label would collide with
	// the revoked row's identifier on the UNIQUE (kind, identifier) index and
	// fail from the second re-provision onward — the exact path a developer who
	// deleted the key file walks.
	for i := 0; i < 3; i++ {
		next, err := st.EnsureDevScriptKey(ctx, "")
		if err != nil {
			t.Fatalf("EnsureDevScriptKey (re-mint %d): %v", i+3, err)
		}
		if next.Label == second.Label {
			t.Fatalf("re-mint %d reused credential label %q", i+3, next.Label)
		}
	}
}

// TestEnsureDevScriptKeyIgnoresAnotherPrincipalsToken guards the identity check:
// a live session that belongs to somebody else must not be adopted as the dev
// key, or the tool would silently hand a probe an unrelated principal's
// authority.
func TestEnsureDevScriptKeyIgnoresAnotherPrincipalsToken(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	other, err := st.CreatePrincipal(ctx, KindUser, "somebody-else")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	foreign, err := st.MintAPIKey(ctx, other.PrincipalID, "cli")
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}

	key, err := st.EnsureDevScriptKey(ctx, foreign.Token)
	if err != nil {
		t.Fatalf("EnsureDevScriptKey: %v", err)
	}
	if key.Reused {
		t.Fatalf("adopted another principal's live session as the dev key")
	}
	if key.PrincipalID == other.PrincipalID {
		t.Fatalf("dev key was minted for the foreign principal")
	}
	if _, err := st.LookupSession(ctx, foreign.Token); err != nil {
		t.Fatalf("the foreign principal's key was revoked as collateral: %v", err)
	}
}
