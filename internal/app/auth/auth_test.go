package auth

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// testClock is an injectable clock: every timing-dependent assertion in this
// package moves it explicitly rather than sleeping, per the conformance notes'
// fake-clock policy ("Lockout backoff timing (SEC-090) and grant ttl expiry
// (SEC-032, SEC-035) are timing-dependent and exercised against an injectable
// clock in a driver harness, not wall-clock sleeps").
type testClock struct {
	mu sync.Mutex
	ms int64
}

func newTestClock() *testClock { return &testClock{ms: 1752537600000} }

func (c *testClock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}

func (c *testClock) advance(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += ms
}

func newTestStore(t *testing.T) (*Store, *testClock) {
	t.Helper()
	clock := newTestClock()
	st, err := Open(":memory:", clock.now, ulid.New)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, clock
}

// ---- SEC-001/003: the credential relation ---------------------------------

// TestCredentialsAreTheirOwnRelation drives SEC-001's structural rule: "A
// platform table MUST NOT store an authentication credential (a password hash or
// otherwise) as a column of any other resource — every credential is a row in
// the credential relation (SEC-003), keyed to a principal, never a column on a
// users-shaped table", together with SEC-003's "a principal MAY hold more than
// one credential, including more than one of the same kind."
//
// It asserts the shape from the OUTSIDE: the principals table has no column
// carrying a secret, and one principal can hold a password AND two API keys at
// once — which is only representable if credentials are rows.
func TestCredentialsAreTheirOwnRelation(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	rows, err := st.db.Query(`SELECT name FROM pragma_table_info('principals')`)
	if err != nil {
		t.Fatalf("read principals columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		lower := strings.ToLower(col)
		for _, banned := range []string{"password", "secret", "hash", "token", "credential"} {
			if strings.Contains(lower, banned) {
				t.Fatalf("principals.%s looks like a credential column; SEC-001 forbids storing a credential as a column of another resource", col)
			}
		}
	}

	p, err := st.CreatePrincipal(ctx, KindUser, "operator")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := st.PutPasswordCredential(ctx, p.PrincipalID, "op@example.test", "correct horse battery staple"); err != nil {
		t.Fatalf("PutPasswordCredential: %v", err)
	}
	if _, err := st.MintAPIKey(ctx, p.PrincipalID, "cli"); err != nil {
		t.Fatalf("MintAPIKey(cli): %v", err)
	}
	if _, err := st.MintAPIKey(ctx, p.PrincipalID, "ci"); err != nil {
		t.Fatalf("MintAPIKey(ci): %v", err)
	}

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM credentials WHERE principal_id = ?`, p.PrincipalID).Scan(&n); err != nil {
		t.Fatalf("count credentials: %v", err)
	}
	if n != 3 {
		t.Fatalf("one principal must be able to hold N credentials including two of one kind (SEC-003); got %d rows, want 3", n)
	}
}

// TestSystemConsoleCarriesNoCredential drives SEC-002: system-console "MUST be
// the sole principal kind with no corresponding credential row: it is assumable
// only by a request admitted over the console binding, never by presenting a
// stored secret."
func TestSystemConsoleCarriesNoCredential(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	p, err := st.CreatePrincipal(ctx, KindSystemConsole, "console")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := st.PutPasswordCredential(ctx, p.PrincipalID, "console", "hunter2"); err == nil {
		t.Fatal("attaching a password credential to a system-console principal must be refused (SEC-002)")
	}
	if _, err := st.MintAPIKey(ctx, p.PrincipalID, "console"); err == nil {
		t.Fatal("minting an API key for a system-console principal must be refused (SEC-002)")
	}
}

// ---- SEC-021: the AAL claim rides in the token's own format ---------------

// TestTokenCarriesAALInItsOwnFormat drives SEC-021: "Every session or API-key
// token MUST carry its aal claim in the token's own format (not merely as a
// database-side attribute invisible to the token itself), so a resource server
// can make an authorization decision from the token alone."
//
// It parses the AAL out of the raw token with NO store access, then proves the
// claim is not merely readable but tamper-evident: rewriting the segment yields
// a token that resolves to no session, because the stored hash covers it.
func TestTokenCarriesAALInItsOwnFormat(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	p, err := st.CreatePrincipal(ctx, KindUser, "u")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}

	for _, aal := range []AAL{AALStandard, AALRecovery} {
		minted, err := st.MintSession(ctx, p.PrincipalID, TokenKindSession, "", aal, nil)
		if err != nil {
			t.Fatalf("MintSession(%s): %v", aal, err)
		}
		kind, gotAAL, ok := ParseToken(minted.Token)
		if !ok {
			t.Fatalf("ParseToken(%q) must succeed", minted.Token)
		}
		if gotAAL != aal {
			t.Fatalf("token's own aal claim = %q, want %q (SEC-021)", gotAAL, aal)
		}
		if kind != TokenKindSession {
			t.Fatalf("token kind = %q, want %q", kind, TokenKindSession)
		}
		if minted.Session.TokenHash == minted.Token {
			t.Fatal("the session row must store the token HASHED, never the raw token")
		}
	}

	// Tamper: promote a recovery token's self-carried claim to standard.
	rec, err := st.MintSession(ctx, p.PrincipalID, TokenKindSession, "", AALRecovery, nil)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}
	forged := strings.Replace(rec.Token, "_rec_", "_std_", 1)
	if forged == rec.Token {
		t.Fatal("test bug: the recovery token did not carry a _rec_ segment")
	}
	if _, err := st.LookupSession(ctx, forged); err == nil {
		t.Fatal("a token whose aal segment was rewritten must resolve to no session — the stored hash covers the segment (SEC-021)")
	}
}

// ---- SEC-010: scope-node role resolution ---------------------------------

// TestResolveNearestBindingWins drives SEC-010: "A principal MAY hold different
// roles at different scope nodes; a role bound at a scope node applies to that
// node and, absent a more specific binding, to its descendants."
//
// The narrowing case is the one worth pinning: an admin at the org who is bound
// viewer at one site is a VIEWER at that site. Taking the max across the chain
// — the obvious alternative — would make a deliberately narrowed binding
// unexpressible.
func TestResolveNearestBindingWins(t *testing.T) {
	const (
		org    = "01J8Z0DEM00RGANCEST0RB0VND"
		site   = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
		screen = "01J8Z4DEM0SCREENF1RSTPH0TN"
	)
	bindings := []Binding{
		{ScopeNode: org, Role: RoleAdmin},
		{ScopeNode: site, Role: RoleViewer},
	}
	// AncestorChain's order: self first, then outward to the root.
	cases := []struct {
		name      string
		ancestors []string
		want      Role
		bound     bool
	}{
		{"at the org itself", []string{org}, RoleAdmin, true},
		{"at the site: the nearer viewer binding narrows the org admin", []string{site, org}, RoleViewer, true},
		{"at a screen under the site: inherits the site's viewer, not the org's admin", []string{screen, site, org}, RoleViewer, true},
		{"outside the bound tree entirely", []string{"01J8ZZZZZZZZZZZZZZZZZZZZZZ"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Resolve(bindings, c.ancestors)
			if ok != c.bound {
				t.Fatalf("Resolve bound = %v, want %v", ok, c.bound)
			}
			if got != c.want {
				t.Fatalf("Resolve = %q, want %q", got, c.want)
			}
		})
	}
}

// TestResolveRootSentinelInheritsEverywhere pins that a binding at the workspace
// root applies wherever no nearer binding does — the placement the first-boot
// owner gets before any org node necessarily exists (SEC-120).
func TestResolveRootSentinelInheritsEverywhere(t *testing.T) {
	bindings := []Binding{{ScopeNode: RootScopeNode, Role: RoleOwner}}
	got, ok := Resolve(bindings, []string{"01J8Z4DEM0SCREENF1RSTPH0TN", "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"})
	if !ok || got != RoleOwner {
		t.Fatalf("a root binding must inherit to every node; got %q ok=%v", got, ok)
	}
}

// TestUnboundPrincipalResolvesNothing is SEC-005's refuse-never-default-permit
// rule at the resolution layer: a principal with no binding yields no role, so
// the middleware has nothing to permit with.
func TestUnboundPrincipalResolvesNothing(t *testing.T) {
	if _, ok := Effective(nil); ok {
		t.Fatal("a principal with no role binding must resolve to no authority (SEC-005)")
	}
	if _, ok := Resolve(nil, []string{"01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"}); ok {
		t.Fatal("a principal with no role binding must resolve to no authority (SEC-005)")
	}
	// An unrecognized role name is authority for nothing, never an unknown that
	// might be permissive.
	if Role("superuser").AtLeast(RoleViewer) {
		t.Fatal("an unrecognized role must never satisfy an authority floor")
	}
}

// ---- SEC-011: the last owner binding is not deletable --------------------

// TestLastOwnerBindingIsNotDeletable drives SEC-011: "A deployment MUST always
// retain at least one owner-role principal in a claimed state: the last
// remaining owner role-binding MUST NOT be deletable through ordinary api/1
// mutation, only through factory reset."
func TestLastOwnerBindingIsNotDeletable(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	first, err := st.CreatePrincipal(ctx, KindUser, "owner-1")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	b1, err := st.PutRoleBinding(ctx, first.PrincipalID, RootScopeNode, RoleOwner)
	if err != nil {
		t.Fatalf("PutRoleBinding: %v", err)
	}
	if err := st.DeleteRoleBinding(ctx, b1.BindingID); err == nil {
		t.Fatal("deleting the only owner binding must be refused (SEC-011)")
	}

	second, err := st.CreatePrincipal(ctx, KindUser, "owner-2")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := st.PutRoleBinding(ctx, second.PrincipalID, RootScopeNode, RoleOwner); err != nil {
		t.Fatalf("PutRoleBinding: %v", err)
	}
	// With two owners the first is now deletable...
	if err := st.DeleteRoleBinding(ctx, b1.BindingID); err != nil {
		t.Fatalf("with two owners, deleting one must succeed; got %v", err)
	}
	// ...and the survivor is once more the last one.
	owners, err := st.CountOwnerBindings(ctx)
	if err != nil {
		t.Fatalf("CountOwnerBindings: %v", err)
	}
	if owners != 1 {
		t.Fatalf("owner bindings = %d, want 1", owners)
	}
}

// ---- SEC-020: independent revocation, one mechanism for both kinds -------

// TestSessionsAndAPIKeysRevokeThroughOneMechanism drives SEC-020: a session row
// "MUST be independently revocable" and "an API-key credential MUST be revocable
// through the same mechanism."
func TestSessionsAndAPIKeysRevokeThroughOneMechanism(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	p, err := st.CreatePrincipal(ctx, KindUser, "u")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	a, err := st.MintSession(ctx, p.PrincipalID, TokenKindSession, "", AALStandard, nil)
	if err != nil {
		t.Fatalf("MintSession(a): %v", err)
	}
	b, err := st.MintSession(ctx, p.PrincipalID, TokenKindSession, "", AALStandard, nil)
	if err != nil {
		t.Fatalf("MintSession(b): %v", err)
	}
	key, err := st.MintAPIKey(ctx, p.PrincipalID, "cli")
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}

	// Independent: revoking a leaves b alone.
	if err := st.RevokeSession(ctx, a.Session.SessionID); err != nil {
		t.Fatalf("RevokeSession(a): %v", err)
	}
	if _, err := st.LookupSession(ctx, a.Token); err == nil {
		t.Fatal("a revoked session token must resolve to nothing")
	}
	if _, err := st.LookupSession(ctx, b.Token); err != nil {
		t.Fatalf("revoking one session must not affect another; got %v", err)
	}

	// Same mechanism: the API key revokes through RevokeSession, and its own
	// credential row is revoked with it.
	if err := st.RevokeSession(ctx, key.Session.SessionID); err != nil {
		t.Fatalf("RevokeSession(api key): %v", err)
	}
	if _, err := st.LookupSession(ctx, key.Token); err == nil {
		t.Fatal("a revoked API key must resolve to nothing (SEC-020)")
	}
	var revokedAt *int64
	if err := st.db.QueryRow(`SELECT revoked_at FROM credentials WHERE credential_id = ?`,
		key.Session.CredentialID).Scan(&revokedAt); err != nil {
		t.Fatalf("read api-key credential: %v", err)
	}
	if revokedAt == nil {
		t.Fatal("revoking an API key's session must revoke its credential row in the same act (SEC-020)")
	}
}

// ---- SEC-036: atomic check-and-consume, proven under concurrency ---------

// TestGrantRedemptionIsAtomicUnderConcurrency drives SEC-036 as stated: "of two
// simultaneous requests presenting the same code, at most one MUST receive
// success and the other MUST receive GRANT_ALREADY_REDEEMED — a grant's
// redemption count MUST NOT ever exceed one regardless of concurrency."
//
// It runs many more than two concurrent redemptions, each of which also performs
// a write inside the consume callback, and asserts exactly one wins AND exactly
// one side effect landed. Both halves matter: a redemption that consumed the
// code without its effect committing would be a code burned for nothing.
func TestGrantRedemptionIsAtomicUnderConcurrency(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	minted, err := st.MintGrant(ctx, MintGrantOptions{
		Purpose:                PurposeSetup,
		ResultingPrincipalKind: KindUser,
		Role:                   RoleOwner,
		TTLMs:                  DefaultSetupGrantTTLMs,
	})
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}

	const racers = 16
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		redeemed  int
		other     []error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := st.RedeemGrant(ctx, minted.Code, PurposeSetup, func(tx *sql.Tx, g GrantRow) error {
				// The side effect a real claim performs: create the principal
				// the grant results in, inside the redeeming transaction.
				_, err := st.CreatePrincipalTx(ctx, tx, g.ResultingPrincipalKind, "claimed")
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case err == ErrGrantAlreadyRedeemed:
				redeemed++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("a concurrent redemption failed with an unexpected error: %v", other[0])
	}
	if successes != 1 {
		t.Fatalf("exactly one of %d concurrent redemptions must succeed (SEC-036); got %d", racers, successes)
	}
	if redeemed != racers-1 {
		t.Fatalf("every losing redemption must be refused GRANT_ALREADY_REDEEMED (SEC-035/036); got %d of %d", redeemed, racers-1)
	}

	var count int
	if err := st.db.QueryRow(`SELECT redemption_count FROM grants WHERE grant_id = ?`, minted.Grant.GrantID).Scan(&count); err != nil {
		t.Fatalf("read redemption count: %v", err)
	}
	if count != 1 {
		t.Fatalf("a grant's redemption count MUST NOT ever exceed one regardless of concurrency (SEC-036); got %d", count)
	}
	var principals int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM principals`).Scan(&principals); err != nil {
		t.Fatalf("count principals: %v", err)
	}
	if principals != 1 {
		t.Fatalf("exactly one claim's side effect must have landed; got %d principals", principals)
	}
}

// TestGrantRefusals drives SEC-035's three named refusals, each against the
// injected clock rather than a sleep.
func TestGrantRefusals(t *testing.T) {
	st, clock := newTestStore(t)
	ctx := context.Background()

	t.Run("expired", func(t *testing.T) {
		g, err := st.MintGrant(ctx, MintGrantOptions{
			Purpose: PurposeCredentialReset, ResultingPrincipalKind: KindUser,
			TTLMs: DefaultResetGrantTTLMs,
		})
		if err != nil {
			t.Fatalf("MintGrant: %v", err)
		}
		clock.advance(DefaultResetGrantTTLMs)
		if _, err := st.RedeemGrant(ctx, g.Code, PurposeCredentialReset, nil); err != ErrGrantExpired {
			t.Fatalf("an expired grant must be refused GRANT_EXPIRED (SEC-035); got %v", err)
		}
	})

	t.Run("purpose mismatch", func(t *testing.T) {
		g, err := st.MintGrant(ctx, MintGrantOptions{
			Purpose: PurposePairing, ResultingPrincipalKind: KindScreen, TTLMs: 60_000,
		})
		if err != nil {
			t.Fatalf("MintGrant: %v", err)
		}
		// SEC-035's own example: "a pairing-purpose code MUST NOT redeem
		// against the credential-reset endpoint, even if otherwise well-formed."
		if _, err := st.RedeemGrant(ctx, g.Code, PurposeCredentialReset, nil); err != ErrGrantPurposeMismatch {
			t.Fatalf("a purpose mismatch must be refused GRANT_PURPOSE_MISMATCH (SEC-035); got %v", err)
		}
	})

	t.Run("already redeemed", func(t *testing.T) {
		g, err := st.MintGrant(ctx, MintGrantOptions{
			Purpose: PurposeInvite, ResultingPrincipalKind: KindUser, TTLMs: 60_000,
		})
		if err != nil {
			t.Fatalf("MintGrant: %v", err)
		}
		if _, err := st.RedeemGrant(ctx, g.Code, PurposeInvite, nil); err != nil {
			t.Fatalf("first redemption: %v", err)
		}
		if _, err := st.RedeemGrant(ctx, g.Code, PurposeInvite, nil); err != ErrGrantAlreadyRedeemed {
			t.Fatalf("a second redemption must be refused GRANT_ALREADY_REDEEMED (SEC-035); got %v", err)
		}
	})

	t.Run("unknown code", func(t *testing.T) {
		if _, err := st.RedeemGrant(ctx, "deadbeef", PurposeInvite, nil); err != ErrGrantNotFound {
			t.Fatalf("an unknown code must be refused; got %v", err)
		}
	})
}

// TestGrantCodeEntropyAndStorage drives SEC-032's entropy floor and the
// never-store-the-code discipline.
func TestGrantCodeEntropyAndStorage(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	g, err := st.MintGrant(ctx, MintGrantOptions{
		Purpose: PurposeSetup, ResultingPrincipalKind: KindUser, TTLMs: 60_000,
	})
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}
	// 64 hex characters = 256 bits, above SEC-032's 128-bit floor.
	if len(g.Code) != 64 {
		t.Fatalf("grant code length = %d hex chars, want 64 (256 bits, SEC-032 floor is 128)", len(g.Code))
	}
	var stored string
	if err := st.db.QueryRow(`SELECT code_hash FROM grants WHERE grant_id = ?`, g.Grant.GrantID).Scan(&stored); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if stored == g.Code {
		t.Fatal("a grant's code must be stored hashed, never verbatim")
	}
	if stored != HashToken(g.Code) {
		t.Fatal("the stored grant hash must be HashToken(code)")
	}
}

// ---- SEC-090: lockout ----------------------------------------------------

// TestLockoutIsPerCredentialAndSourceNotPrincipalGlobal drives SEC-090's central
// rule: "an attacker spraying the login endpoint from one source MUST NOT be
// able to lock the legitimate owner out of their own account by exhausting a
// shared, principal-wide attempt budget."
func TestLockoutIsPerCredentialAndSourceNotPrincipalGlobal(t *testing.T) {
	clock := newTestClock()
	l := NewLockout(3, 1000, 60_000)
	const credential = "01J8Z9CREDENT1AL00000000AA"

	attacker := LockoutKey(credential, IPClassWAN)
	owner := LockoutKey(credential, IPClassLAN)

	for i := 0; i < 10; i++ {
		l.Fail(attacker, clock.now())
	}
	if locked, _ := l.Locked(attacker, clock.now()); !locked {
		t.Fatal("the spraying source must lock out")
	}
	if locked, _ := l.Locked(owner, clock.now()); locked {
		t.Fatal("a spray from one source class MUST NOT lock the same credential out from another (SEC-090)")
	}
}

// TestLockoutBacksOffExponentiallyAndLifts drives SEC-090's "exponential
// backoff", entirely against the injected clock.
func TestLockoutBacksOffExponentiallyAndLifts(t *testing.T) {
	clock := newTestClock()
	l := NewLockout(2, 1000, 60_000)
	key := LockoutKey("cred", IPClassLAN)

	if d := l.Fail(key, clock.now()); d != 0 {
		t.Fatalf("failure 1 is under the threshold and must not lock; got %dms", d)
	}
	if d := l.Fail(key, clock.now()); d != 0 {
		t.Fatalf("failure 2 is at the threshold and must not lock; got %dms", d)
	}
	if d := l.Fail(key, clock.now()); d != 1000 {
		t.Fatalf("failure 3 must lock for the base duration; got %dms want 1000", d)
	}
	if d := l.Fail(key, clock.now()); d != 2000 {
		t.Fatalf("failure 4 must double the lock; got %dms want 2000", d)
	}
	if d := l.Fail(key, clock.now()); d != 4000 {
		t.Fatalf("failure 5 must double again; got %dms want 4000", d)
	}

	// The lock lifts when the clock passes it — no sleeping.
	if locked, retry := l.Locked(key, clock.now()); !locked || retry != 4000 {
		t.Fatalf("locked=%v retry=%dms; want locked with 4000ms remaining", locked, retry)
	}
	clock.advance(4000)
	if locked, _ := l.Locked(key, clock.now()); locked {
		t.Fatal("the lock must lift once its duration elapses")
	}

	// A success clears the history, so the next failure starts from zero again.
	l.Succeed(key)
	if d := l.Fail(key, clock.now()); d != 0 {
		t.Fatalf("after a success the backoff must reset; got %dms", d)
	}
}

// TestLockoutCapsAtMax pins that the doubling stops at the configured ceiling
// rather than overflowing into a negative (i.e. permanent) lock.
func TestLockoutCapsAtMax(t *testing.T) {
	clock := newTestClock()
	l := NewLockout(0, 1000, 5000)
	key := "k"
	var last int64
	for i := 0; i < 40; i++ {
		last = l.Fail(key, clock.now())
	}
	if last != 5000 {
		t.Fatalf("the backoff must cap at maxMs; got %dms want 5000", last)
	}
}

// TestIPClass pins the coarse source classification.
func TestIPClass(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:5555":   IPClassLoopback,
		"[::1]:5555":       IPClassLoopback,
		"192.168.50.12:80": IPClassLAN,
		"10.0.0.9:80":      IPClassLAN,
		"203.0.113.7:443":  IPClassWAN,
		"not-an-address":   IPClassWAN,
	}
	for addr, want := range cases {
		if got := IPClass(addr); got != want {
			t.Fatalf("IPClass(%q) = %q, want %q", addr, got, want)
		}
	}
}

// ---- password hashing -----------------------------------------------------

// TestPasswordHashing pins the Argon2id round trip, the per-hash salt, and that
// the parameters travel WITH the hash so they can be raised later.
func TestPasswordHashing(t *testing.T) {
	const pw = "correct horse battery staple"
	a, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password must differ — each carries its own random salt")
	}
	if !strings.HasPrefix(a, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("hash must be a PHC string carrying its own parameters; got %q", a)
	}
	if err := VerifyPassword(a, pw); err != nil {
		t.Fatalf("VerifyPassword must accept the right password; got %v", err)
	}
	if err := VerifyPassword(a, pw+"!"); err != ErrPasswordMismatch {
		t.Fatalf("VerifyPassword must reject a wrong password with ErrPasswordMismatch; got %v", err)
	}
	for _, bad := range []string{"", "not-a-hash", "$argon2i$v=19$m=8,t=1,p=1$AAAA$AAAA", "$argon2id$v=99$m=8,t=1,p=1$AAAA$AAAA"} {
		if err := VerifyPassword(bad, pw); err == nil || err == ErrPasswordMismatch {
			t.Fatalf("a malformed hash %q must be a distinct error, not a password mismatch; got %v", bad, err)
		}
	}
}

// TestPasswordVerifiesAcrossParameterChange proves the stored-parameters
// property concretely: a hash produced with weaker parameters than this build's
// constants still verifies, which is what makes raising the constants a
// non-breaking change.
func TestPasswordVerifiesAcrossParameterChange(t *testing.T) {
	const pw = "another passphrase entirely"
	// Derive with deliberately weaker-than-default parameters, encoded the same
	// way HashPassword would.
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(pw), salt, 1, 8192, 1, 32)
	encoded := encodeHash(salt, key, 8192, 1, 1)
	if err := VerifyPassword(encoded, pw); err != nil {
		t.Fatalf("a hash carrying its own weaker parameters must still verify; got %v", err)
	}
	if err := VerifyPassword(encoded, pw+"x"); err != ErrPasswordMismatch {
		t.Fatalf("...and must still reject a wrong password; got %v", err)
	}
}
