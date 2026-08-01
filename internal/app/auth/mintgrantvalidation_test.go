package auth

import (
	"context"
	"strings"
	"testing"
)

// validMintOptions is the option set every case below starts from and mutates
// ONE member of, so a row fails for its own reason rather than for whichever
// check happens to run first.
func validMintOptions() MintGrantOptions {
	return MintGrantOptions{
		Purpose:                PurposeSetup,
		ResultingPrincipalKind: KindUser,
		Role:                   RoleOwner,
		TTLMs:                  DefaultSetupGrantTTLMs,
	}
}

// TestMintGrantRefusesOptionsThatWouldProduceAnImpossibleGrant pins MintGrant's
// option validation, which was enforced and defended by nothing: each of these
// refusals could be deleted individually and the WHOLE tree stayed green.
//
// MintGrant is the credential-minting entry point — it returns a one-time code
// that redeems into a principal — so every rule here refuses an option set that
// would produce a grant which should not exist: an unbounded lifetime, a grant
// redeemable without limit, a role outside the closed set, a purpose the
// taxonomy does not define.
//
// Each case asserts the SPECIFIC message rather than "some error". These share a
// return type and no error code, so a test satisfied by any error passes when
// the wrong rule fires — which is how six rules end up looking covered by one.
func TestMintGrantRefusesOptionsThatWouldProduceAnImpossibleGrant(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		mutate  func(*MintGrantOptions)
		wantMsg string
	}{
		{"a purpose the taxonomy does not define", func(o *MintGrantOptions) {
			o.Purpose = "not-a-purpose"
		}, "is not a grant purpose (SEC-030)"},

		{"a principal kind that does not exist", func(o *MintGrantOptions) {
			o.ResultingPrincipalKind = "not-a-kind"
		}, "is not a principal kind (SEC-001)"},

		// A grant with no positive lifetime never expires, which is the one
		// property SEC-032 fixes about a minted code.
		{"a zero ttl", func(o *MintGrantOptions) { o.TTLMs = 0 }, "positive ttl (SEC-032)"},
		{"a negative ttl", func(o *MintGrantOptions) { o.TTLMs = -1 }, "positive ttl (SEC-032)"},

		// Only the MULTI mode takes a bound from the caller: a one-time grant's
		// bound is fixed to 1 by MintGrant itself, so this cannot be reached
		// without also selecting the mode.
		{"a multi grant with no redemption bound", func(o *MintGrantOptions) {
			o.RedemptionMode, o.MaxRedemptions = RedemptionMulti, 0
		}, "must state its redemption bound (SEC-031)"},

		{"a redemption mode that does not exist", func(o *MintGrantOptions) {
			o.RedemptionMode = "unlimited"
		}, "is not a redemption mode (SEC-031)"},

		{"an issuance channel outside the two", func(o *MintGrantOptions) {
			o.IssuedVia = "smuggled"
		}, "is not an issuance channel (SEC-030)"},

		{"a role outside the closed set", func(o *MintGrantOptions) {
			o.Role = Role("superuser")
		}, "is not a role (SEC-010)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := validMintOptions()
			tc.mutate(&opt)
			_, err := st.MintGrant(ctx, opt)
			if err == nil {
				t.Fatalf("MintGrant accepted options that would produce a grant which should not exist")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %v, want one naming %q — a refusal for the wrong reason is not this rule being enforced", err, tc.wantMsg)
			}
		})
	}
}

// TestMintGrantAcceptsAConformantOptionSet is the control, and it is what stops
// the table above from passing against a MintGrant that refuses everything.
//
// It also pins the two DEFAULTS the validation leans on: a one-time grant's
// bound is fixed to 1 by MintGrant rather than taken from the caller (SEC-031),
// and an unset issuance channel becomes `api` rather than being refused.
func TestMintGrantAcceptsAConformantOptionSet(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	minted, err := st.MintGrant(ctx, validMintOptions())
	if err != nil {
		t.Fatalf("a conformant option set was refused: %v", err)
	}
	if minted.Code == "" {
		t.Error("an accepted grant carried no code")
	}
	if minted.Grant.MaxRedemptions != 1 {
		t.Errorf("a one-time grant's bound = %d, want 1 fixed by MintGrant itself (SEC-031/036), never taken from the caller",
			minted.Grant.MaxRedemptions)
	}
	if minted.Grant.IssuedVia != IssuedViaAPI {
		t.Errorf("an unset issuance channel defaulted to %q, want %q (SEC-030)", minted.Grant.IssuedVia, IssuedViaAPI)
	}
}

// TestRequirePrincipalFailsLoudlyWhenMountedWrong pins the guard whose own doc
// says it "exists so a handler that depends on an authenticated identity fails
// loudly when mounted wrong, instead of quietly attributing the request to the
// empty string" — and which, when disabled, does exactly the thing the doc says
// it prevents. It survived the whole tree.
func TestRequirePrincipalFailsLoudlyWhenMountedWrong(t *testing.T) {
	// A context that never passed through the authenticating middleware.
	p, err := RequirePrincipal(context.Background())
	if err == nil {
		t.Fatalf("RequirePrincipal returned principal %+v and no error for an unauthenticated context — "+
			"a handler would attribute the request to the empty principal", p)
	}
	if err != ErrNoPrincipal {
		t.Errorf("error = %v, want ErrNoPrincipal so a caller can branch on it", err)
	}

	// The control: a context that DID carry one still resolves.
	want := Principal{ID: "01J8Z0000000000000000USER", Kind: KindUser}
	if got, err := RequirePrincipal(WithPrincipal(context.Background(), want)); err != nil || got.ID != want.ID {
		t.Errorf("RequirePrincipal(authenticated) = (%+v, %v), want %+v with no error", got, err, want)
	}
}
