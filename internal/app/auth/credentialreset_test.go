package auth

import (
	"context"
	"errors"
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
	redemption, err := st.RedeemCredentialResetGrant(ctx, handoff.Code, "new-passphrase", "")
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

// TestCredentialResetRefusesATargetWhoOutranksTheIssuer pins SEC-012a, and the
// escalation it closes is worth stating because it was reachable in three calls
// with nothing but an `admin` binding.
//
// SEC-012 permits an admin to issue a reset for "any `user` principal", and an
// owner IS a user principal holding a password. So: issue a reset for the owner
// (the one-time code comes back to the issuing admin, which is SEC-050's own
// design), redeem it (the redemption surface is necessarily reachable without an
// existing credential, since its caller is by definition someone who cannot sign
// in), then authenticate as the owner. Everything SEC-011 reserves to `owner` —
// acknowledging a capability-widening pack update, minting a break-glass grant,
// toggling developer mode, destroying the workspace — would be reachable by a
// role the contract deliberately withheld them from.
//
// The rule is STRICTLY above: an admin resetting a peer admin is lateral and
// grants no authority the issuer did not already hold, so it stays permitted.
func TestCredentialResetRefusesATargetWhoOutranksTheIssuer(t *testing.T) {
	ctx := context.Background()
	st := newSecurityTestStore(t)

	seed := func(name string, role Role) PrincipalRow {
		t.Helper()
		p, err := st.CreatePrincipal(ctx, KindUser, name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := st.PutPasswordCredential(ctx, p.PrincipalID, name, name+"-passphrase"); err != nil {
			t.Fatalf("seed %s password: %v", name, err)
		}
		if role != "" {
			if _, err := st.PutRoleBinding(ctx, p.PrincipalID, RootScopeNode, role); err != nil {
				t.Fatalf("bind %s: %v", name, err)
			}
		}
		return p
	}

	admin := seed("admin", RoleAdmin)
	owner := seed("owner", RoleOwner)
	peer := seed("peer-admin", RoleAdmin)
	operator := seed("operator", RoleOperator)
	unbound := seed("unbound", "")

	// The escalation: refused, and nothing is minted.
	if _, err := st.IssueCredentialResetGrant(ctx, admin.PrincipalID, owner.PrincipalID, CredentialResetOptions{}); !errors.Is(err, ErrResetTargetOutranksIssuer) {
		t.Fatalf("an admin issued a reset for the OWNER: err = %v, want ErrResetTargetOutranksIssuer — this is a three-call path to owner", err)
	}
	// The owner's password must still be the one they set.
	ownerCred, err := st.FindPasswordCredential(ctx, "owner")
	if err != nil {
		t.Fatalf("read the owner's credential: %v", err)
	}
	if err := VerifyPassword(ownerCred.Secret, "owner-passphrase"); err != nil {
		t.Fatalf("the refused issuance disturbed the owner's credential: %v", err)
	}

	// Still permitted: peers and anyone below, so the flow stays useful.
	for _, tc := range []struct {
		name   string
		target PrincipalRow
	}{
		{"a peer admin", peer},
		{"an operator", operator},
		{"an unbound principal", unbound},
	} {
		if _, err := st.IssueCredentialResetGrant(ctx, admin.PrincipalID, tc.target.PrincipalID, CredentialResetOptions{}); err != nil {
			t.Errorf("an admin was refused a reset for %s: %v", tc.name, err)
		}
	}

	// An owner may reset an admin (downward), which is the ordinary case.
	if _, err := st.IssueCredentialResetGrant(ctx, owner.PrincipalID, admin.PrincipalID, CredentialResetOptions{}); err != nil {
		t.Errorf("an owner was refused a reset for an admin: %v", err)
	}
}
