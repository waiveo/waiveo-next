package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// tierScopeNode is the fixture scope every pack principal below is bound at
// (fixture-ULID convention; no secrets).
const tierScopeNode = "01J8Z2Q1C0000000000000000C"

// The tier-grant ceremony (SEC-037). These tests are about the two properties
// that make handing a code to a child process safe at all: the code is spent by
// its first use, and the pack's identity comes off the GRANT rather than off
// anything the process says.

func mintTier(t *testing.T, st *Store, packID string, opts ...MintGrantOption) MintedGrant {
	t.Helper()
	g, err := st.MintTierGrant(context.Background(), packID, tierScopeNode, RoleOperator, opts...)
	if err != nil {
		t.Fatalf("MintTierGrant(%s): %v", packID, err)
	}
	return g
}

// A pack starts, redeems, and holds a session it can act with.
func TestATierGrantRedeemsToAPackServiceSession(t *testing.T) {
	st, _ := newTestStore(t)
	g := mintTier(t, st, "waiveo/slidecast")

	sess, err := st.RedeemTierGrant(context.Background(), g.Code, nil)
	if err != nil {
		t.Fatalf("RedeemTierGrant: %v", err)
	}
	if sess.PackID != "waiveo/slidecast" {
		t.Fatalf("pack id = %q, want waiveo/slidecast", sess.PackID)
	}
	if sess.Token == "" || sess.SessionID == "" {
		t.Fatalf("redemption yielded no usable session: %+v", sess)
	}
	if sess.Role != RoleOperator {
		t.Fatalf("role = %q, want the role the grant carried", sess.Role)
	}

	// The session must actually authenticate — a token that resolves to nothing
	// would make every assertion above cosmetic.
	row, err := st.LookupSession(context.Background(), sess.Token)
	if err != nil {
		t.Fatalf("the returned token does not resolve to a session: %v", err)
	}
	if row.PrincipalID != sess.PrincipalID {
		t.Fatalf("session resolves to %s, want the pack principal %s", row.PrincipalID, sess.PrincipalID)
	}
}

// The principal the pack runs as is a pack-service, not a user. An api-key for
// this kind is refused by SEC-003a, which is the whole reason the ceremony
// exists — if this minted a user, the refusal would be trivially bypassed.
func TestTheRedeemedPrincipalIsAPackServiceNotAUser(t *testing.T) {
	st, _ := newTestStore(t)
	sess, err := st.RedeemTierGrant(context.Background(), mintTier(t, st, "waiveo/backups").Code, nil)
	if err != nil {
		t.Fatalf("RedeemTierGrant: %v", err)
	}
	kind, _ := readPrincipal(t, st, sess.PrincipalID)
	if kind != KindPackService {
		t.Fatalf("principal kind = %q, want %q", kind, KindPackService)
	}
}

// SEC-037's one-time rule. The code is spent by its first use, which is what
// makes a leaked code worthless and what lets the host hand one over at all.
func TestATierGrantIsSpentByItsFirstRedemption(t *testing.T) {
	st, _ := newTestStore(t)
	g := mintTier(t, st, "waiveo/slidecast")

	if _, err := st.RedeemTierGrant(context.Background(), g.Code, nil); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	_, err := st.RedeemTierGrant(context.Background(), g.Code, nil)
	if !errors.Is(err, ErrGrantAlreadyRedeemed) {
		t.Fatalf("second redemption = %v, want ErrGrantAlreadyRedeemed", err)
	}
}

// A restart re-uses the pack's identity. A principal per launch would
// accumulate a row per restart and scatter the pack's audit trail across
// hundreds of identities, so "what did this extension do" would stop being
// answerable.
func TestARestartReusesThePackPrincipalAndOnlyTheSessionIsNew(t *testing.T) {
	st, _ := newTestStore(t)

	first, err := st.RedeemTierGrant(context.Background(), mintTier(t, st, "waiveo/slidecast").Code, nil)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := st.RedeemTierGrant(context.Background(), mintTier(t, st, "waiveo/slidecast").Code, nil)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}

	if second.PrincipalID != first.PrincipalID {
		t.Fatalf("restart minted a NEW principal (%s vs %s) — the audit trail would fragment per restart",
			second.PrincipalID, first.PrincipalID)
	}
	if second.SessionID == first.SessionID {
		t.Fatal("restart re-used the previous session; a pack's authority must not outlive its process")
	}
}

// Two different packs get two different identities. Collapsing them would make
// every pack able to act as every other, and make the audit trail meaningless.
func TestDifferentPacksGetDifferentPrincipals(t *testing.T) {
	st, _ := newTestStore(t)
	a, err := st.RedeemTierGrant(context.Background(), mintTier(t, st, "waiveo/slidecast").Code, nil)
	if err != nil {
		t.Fatalf("slidecast: %v", err)
	}
	b, err := st.RedeemTierGrant(context.Background(), mintTier(t, st, "waiveo/backups").Code, nil)
	if err != nil {
		t.Fatalf("backups: %v", err)
	}
	if a.PrincipalID == b.PrincipalID {
		t.Fatalf("two packs share principal %s", a.PrincipalID)
	}
}

// THE impersonation property. The redeeming process supplies nothing but the
// code — there is no parameter through which it could name a pack. This test
// exists to keep it that way: if an identity argument is ever added to
// RedeemTierGrant, this stops compiling, which is the point.
func TestThePackIdentityComesOffTheGrantNotTheCaller(t *testing.T) {
	st, _ := newTestStore(t)
	g := mintTier(t, st, "waiveo/backups")

	// The only inputs are the code and transport metadata. Device metadata is
	// self-asserted and must not influence identity.
	sess, err := st.RedeemTierGrant(context.Background(), g.Code,
		map[string]string{"pack_id": "waiveo/slidecast", "user_agent": "impostor"})
	if err != nil {
		t.Fatalf("RedeemTierGrant: %v", err)
	}
	if sess.PackID != "waiveo/backups" {
		t.Fatalf("pack id = %q — self-asserted metadata overrode the grant", sess.PackID)
	}
}

// A grant of another purpose must not redeem here, and a tier grant must not
// redeem through another purpose's flow. Either direction would let one
// ceremony mint the other's principal.
func TestPurposesDoNotCrossRedeem(t *testing.T) {
	st, _ := newTestStore(t)

	other, err := st.MintGrant(context.Background(), MintGrantOptions{
		Purpose: PurposeInvite, ResultingPrincipalKind: KindUser,
		ScopeNode: tierScopeNode, Role: RoleOperator, TTLMs: 60_000,
	})
	if err != nil {
		t.Fatalf("MintGrant(invite): %v", err)
	}
	if _, err := st.RedeemTierGrant(context.Background(), other.Code, nil); !errors.Is(err, ErrGrantPurposeMismatch) {
		t.Fatalf("an invite grant redeemed as a tier grant = %v, want ErrGrantPurposeMismatch", err)
	}

	tier := mintTier(t, st, "waiveo/slidecast")
	if _, err := st.RedeemGrant(context.Background(), tier.Code, PurposeInvite, nil); !errors.Is(err, ErrGrantPurposeMismatch) {
		t.Fatalf("a tier grant redeemed as an invite = %v, want ErrGrantPurposeMismatch", err)
	}
}

// A pack id is mandatory at mint. A tier grant that cannot say who it is for
// must never come into existence, let alone redeem.
func TestATierGrantWithoutAPackIdIsRefusedAtMint(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.MintTierGrant(context.Background(), "", tierScopeNode, RoleOperator); !errors.Is(err, ErrTierGrantNoPack) {
		t.Fatalf("mint without a pack id = %v, want ErrTierGrantNoPack", err)
	}
}

// The ceremony's shape is not negotiable through options: an option cannot turn
// a tier grant into some other purpose, or into a principal kind that is not
// pack-service, or re-point it at another pack.
func TestOptionsCannotSubvertTheCeremony(t *testing.T) {
	st, _ := newTestStore(t)
	g, err := st.MintTierGrant(context.Background(), "waiveo/backups", tierScopeNode, RoleOperator,
		func(o *MintGrantOptions) {
			o.Purpose = PurposeInvite
			o.ResultingPrincipalKind = KindUser
			o.Labels["pack_id"] = "waiveo/slidecast"
			o.RedemptionMode = RedemptionMulti
			o.MaxRedemptions = 99
		})
	if err != nil {
		t.Fatalf("MintTierGrant: %v", err)
	}
	if g.Grant.Purpose != PurposeTierGrant {
		t.Fatalf("purpose = %q; an option overrode the ceremony", g.Grant.Purpose)
	}
	if g.Grant.ResultingPrincipalKind != KindPackService {
		t.Fatalf("kind = %q; an option overrode the ceremony", g.Grant.ResultingPrincipalKind)
	}
	sess, err := st.RedeemTierGrant(context.Background(), g.Code, nil)
	if err != nil {
		t.Fatalf("RedeemTierGrant: %v", err)
	}
	if sess.PackID != "waiveo/backups" {
		t.Fatalf("pack id = %q; an option re-pointed the grant at another pack", sess.PackID)
	}
}

// The default ttl is short. This code is redeemed by a process the host started
// microseconds ago, not by a human being given time to type it.
func TestTheDefaultTTLIsProcessScopedNotHumanScoped(t *testing.T) {
	if DefaultTierGrantTTLMs > 5*60_000 {
		t.Fatalf("default tier-grant ttl is %dms — that is a human-typing budget, not a process start (SEC-037)",
			DefaultTierGrantTTLMs)
	}
}

// An expired code is refused. Belt and braces on the ttl above: a short default
// nobody enforces is decoration.
func TestAnExpiredTierGrantIsRefused(t *testing.T) {
	st, clock := newTestStore(t)
	g := mintTier(t, st, "waiveo/slidecast", func(o *MintGrantOptions) { o.TTLMs = 1 })
	clock.advance(5_000)
	if _, err := st.RedeemTierGrant(context.Background(), g.Code, nil); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("expired redemption = %v, want ErrGrantExpired", err)
	}
}

// The principal's name is namespaced so a pack can never be resolved as a human
// of the same name.
func TestThePackPrincipalNameIsNamespaced(t *testing.T) {
	st, _ := newTestStore(t)
	sess, err := st.RedeemTierGrant(context.Background(), mintTier(t, st, "waiveo/slidecast").Code, nil)
	if err != nil {
		t.Fatalf("RedeemTierGrant: %v", err)
	}
	_, name := readPrincipal(t, st, sess.PrincipalID)
	if !strings.HasPrefix(name, "pack:") {
		t.Fatalf("principal display name %q is not namespaced; a human could collide with it", name)
	}
}

// readPrincipal reads a principal's kind and display name straight from the
// table — there is no exported by-id read on this store, and the test wants the
// stored row rather than anything the ceremony reported about it.
func readPrincipal(t *testing.T, st *Store, principalID string) (kind, name string) {
	t.Helper()
	if err := st.db.QueryRow(
		`SELECT kind, display_name FROM principals WHERE principal_id = ?`, principalID,
	).Scan(&kind, &name); err != nil {
		t.Fatalf("read principal %s: %v", principalID, err)
	}
	return kind, name
}

// A tier grant with no scope node is refused at MINT, not discovered at
// redemption — a pack-service principal's authority is its role binding, and a
// grant that cannot produce one is a code that fails in a child process with
// the pack already started.
func TestATierGrantWithoutAScopeIsRefusedAtMint(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.MintTierGrant(context.Background(), "waiveo/slidecast", "", RoleOperator); !errors.Is(err, ErrTierGrantNoScope) {
		t.Fatalf("mint without a scope = %v, want ErrTierGrantNoScope", err)
	}
}

// The cross-check is reachable, not decoration: MintTierGrant forces the kind,
// but the GENERIC MintGrant will happily mint purpose=tier-grant against any
// principal kind. Redeeming that must refuse rather than mint a pack-service
// session for something that was never a pack.
func TestATierPurposeGrantResolvingAnotherKindIsRefused(t *testing.T) {
	st, _ := newTestStore(t)
	g, err := st.MintGrant(context.Background(), MintGrantOptions{
		Purpose:                PurposeTierGrant,
		ResultingPrincipalKind: KindUser, // not pack-service
		ScopeNode:              tierScopeNode,
		Labels:                 map[string]string{"pack_id": "waiveo/slidecast"},
		Role:                   RoleOperator,
		TTLMs:                  60_000,
	})
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}
	if _, err := st.RedeemTierGrant(context.Background(), g.Code, nil); err == nil {
		t.Fatal("a tier-purpose grant resolving kind=user minted a pack-service session")
	}
}

// A tier grant carrying no pack_id label is refused at REDEMPTION too. The mint
// path guards it, but the generic MintGrant can produce one, and a grant that
// cannot say who it is for must never resolve to an identity.
func TestATierGrantWithNoPackLabelIsRefusedAtRedemption(t *testing.T) {
	st, _ := newTestStore(t)
	g, err := st.MintGrant(context.Background(), MintGrantOptions{
		Purpose: PurposeTierGrant, ResultingPrincipalKind: KindPackService,
		ScopeNode: tierScopeNode, Role: RoleOperator, TTLMs: 60_000,
	})
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}
	if _, err := st.RedeemTierGrant(context.Background(), g.Code, nil); !errors.Is(err, ErrTierGrantNoPack) {
		t.Fatalf("redemption of a label-less tier grant = %v, want ErrTierGrantNoPack", err)
	}
}

// The principal lookup keys on kind AND name. A principal of another kind that
// happens to carry the pack's namespaced name must NOT be adopted — that would
// let anything able to create a principal pre-register a name and have the next
// pack start inherit it.
func TestAPackDoesNotAdoptASameNamedPrincipalOfAnotherKind(t *testing.T) {
	st, _ := newTestStore(t)

	impostor, err := st.CreatePrincipal(context.Background(), KindUser, packPrincipalName("waiveo/slidecast"))
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}

	sess, err := st.RedeemTierGrant(context.Background(), mintTier(t, st, "waiveo/slidecast").Code, nil)
	if err != nil {
		t.Fatalf("RedeemTierGrant: %v", err)
	}
	if sess.PrincipalID == impostor.PrincipalID {
		t.Fatalf("the pack adopted a %s principal that merely shared its name", KindUser)
	}
	if kind, _ := readPrincipal(t, st, sess.PrincipalID); kind != KindPackService {
		t.Fatalf("adopted principal kind = %q, want %q", kind, KindPackService)
	}
}
