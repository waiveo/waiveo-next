package auth

import (
	"context"
	"testing"
)

// credentialreset_test.go covers the credential-reset flow's branches the
// FROZEN CORPUS does not reach: SEC-053's explicit opt-out (the corpus case sets
// `opt_out_session_revocation: false`) and SEC-012's issuer restriction.

// TestCredentialResetOptOutKeepsSessions is SEC-053's explicit opt-out — the
// "MAY" half the corpus case (which sets opt_out_session_revocation: false)
// never reaches.
func TestCredentialResetOptOutKeepsSessions(t *testing.T) {
	ctx := context.Background()
	st := newSecurityTestStore(t)

	admin, err := st.CreatePrincipal(ctx, KindUser, "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := st.PutRoleBinding(ctx, admin.PrincipalID, RootScopeNode, RoleAdmin); err != nil {
		t.Fatalf("bind admin: %v", err)
	}
	target, err := st.CreatePrincipal(ctx, KindUser, "target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := st.PutPasswordCredential(ctx, target.PrincipalID, "target", "old-passphrase"); err != nil {
		t.Fatalf("seed password: %v", err)
	}
	session, err := st.MintSession(ctx, target.PrincipalID, TokenKindSession, "", AALStandard, nil)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}

	handoff, err := st.IssueCredentialResetGrant(ctx, admin.PrincipalID, target.PrincipalID,
		CredentialResetOptions{KeepExistingSessions: true})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	redemption, err := st.RedeemCredentialResetGrant(ctx, handoff.Code, "new-passphrase")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if redemption.SessionsRevoked {
		t.Error("an opted-out issuance still revoked the target's sessions (SEC-053's MAY opt out)")
	}
	if _, err := st.LookupSession(ctx, session.Token); err != nil {
		t.Errorf("the target's session was revoked despite the opt-out: %v", err)
	}
	// The credential still changed — the opt-out is about eviction, not about
	// the reset itself.
	cred, err := st.FindPasswordCredential(ctx, "target")
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if err := VerifyPassword(cred.Secret, "new-passphrase"); err != nil {
		t.Errorf("the opted-out reset did not set the new credential: %v", err)
	}
}

// TestCredentialResetRefusesANonAdminIssuer is SEC-012's issuer restriction.
func TestCredentialResetRefusesANonAdminIssuer(t *testing.T) {
	ctx := context.Background()
	st := newSecurityTestStore(t)

	operator, err := st.CreatePrincipal(ctx, KindUser, "operator")
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	if _, err := st.PutRoleBinding(ctx, operator.PrincipalID, RootScopeNode, RoleOperator); err != nil {
		t.Fatalf("bind operator: %v", err)
	}
	target, err := st.CreatePrincipal(ctx, KindUser, "target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := st.PutPasswordCredential(ctx, target.PrincipalID, "target", "old-passphrase"); err != nil {
		t.Fatalf("seed password: %v", err)
	}
	if _, err := st.IssueCredentialResetGrant(ctx, operator.PrincipalID, target.PrincipalID, CredentialResetOptions{}); err != ErrResetNotAdmin {
		t.Errorf("an `operator` issued a credential-reset grant (err = %v); SEC-012 admits `admin` and above", err)
	}
}
