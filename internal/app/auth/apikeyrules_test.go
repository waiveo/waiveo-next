package auth

import (
	"errors"
	"strings"
	"testing"
)

// SEC-003a/d/e, the three rules that decide what a minted api-key can do and
// for how long.
//
// Each is asserted through the PRESENTATION path (LookupSession) rather than by
// reading the row back, because that is the path a request takes and the only
// one where getting it wrong is exploitable.

func mintFor(t *testing.T, st *Store, kind, identifier string, expiresAtMs int64) (Minted, error) {
	t.Helper()
	p, err := st.CreatePrincipal(t.Context(), kind, identifier)
	if err != nil {
		t.Fatalf("CreatePrincipal(%s): %v", kind, err)
	}
	return st.MintAPIKey(t.Context(), p.PrincipalID, "cli", expiresAtMs)
}

// apiKeyCredential reads back the credential row a mint created.
func apiKeyCredential(t *testing.T, st *Store, minted Minted) CredentialRow {
	t.Helper()
	rows, err := st.Credentials(t.Context(), minted.Session.PrincipalID)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	for _, c := range rows {
		if c.CredentialID == minted.Session.CredentialID {
			return c
		}
	}
	t.Fatalf("the mint created no credential row matching session credential_id %q", minted.Session.CredentialID)
	return CredentialRow{}
}

// TestAnAPIKeyIsMintableOnlyForAUserPrincipal is SEC-003a. A screen, relay or
// pack-service has its own credential ceremony, and an api-key for one is a
// second, weaker path to an identity those ceremonies make expensive.
func TestAnAPIKeyIsMintableOnlyForAUserPrincipal(t *testing.T) {
	st, _ := newTestStore(t)

	if _, err := mintFor(t, st, KindUser, "operator", 0); err != nil {
		t.Fatalf("minting for a user principal was refused: %v", err)
	}
	for _, kind := range []string{KindScreen, KindRelay, KindPackService} {
		t.Run(kind, func(t *testing.T) {
			if _, err := mintFor(t, st, kind, "subject-"+kind, 0); err == nil {
				t.Errorf("an api-key was minted for a %q principal — that is a second, weaker path to an identity "+
					"its own enrollment ceremony deliberately makes expensive (SEC-003a)", kind)
			}
		})
	}
}

// TestAnExpiredAPIKeyIsRefusedOnPresentation is SEC-003d.
//
// The clock is driven rather than slept through, and the SAME key is presented
// before and after its deadline — so the only thing that changed is the time,
// which is what the requirement is about.
func TestAnExpiredAPIKeyIsRefusedOnPresentation(t *testing.T) {
	st, clock := newTestStore(t)
	expiresAt := clock.now() + 60_000

	minted, err := mintFor(t, st, KindUser, "expiring", expiresAt)
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}

	clock.advance(60_000)
	if _, err := st.LookupSession(t.Context(), minted.Token); err != nil {
		t.Fatalf("at exactly its expiry the key was refused (%v) — the deadline is the last instant it is good for, "+
			"and refusing AT it shortens every key's life by one tick", err)
	}

	clock.advance(1)
	if _, err := st.LookupSession(t.Context(), minted.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("one millisecond past its expiry the key resolved (err %v) — an expiry enforced nowhere is a "+
			"deadline that exists only in the mint response (SEC-003d)", err)
	}
}

// TestAKeyWithNoExpiryLastsUntilRevoked is the other half of SEC-003d: absent
// is "until revoked", not "already expired" and not "expired at the epoch".
func TestAKeyWithNoExpiryLastsUntilRevoked(t *testing.T) {
	st, clock := newTestStore(t)
	minted, err := mintFor(t, st, KindUser, "unattended", 0)
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}

	// Far past any plausible deadline. A key stored with 0 rather than NULL
	// would fail here under any comparison that treats 0 as a real instant.
	clock.advance(10 * 365 * 24 * 60 * 60 * 1000)
	if _, err := st.LookupSession(t.Context(), minted.Token); err != nil {
		t.Fatalf("a key minted with no expiry stopped working after time passed (%v) — 'no expiry' means until "+
			"revoked, and an unattended caller is part of what api-keys are for (SEC-003d)", err)
	}

	// And revocation is what ends it.
	if _, err := st.RevokePrincipalSessions(t.Context(), minted.Session.PrincipalID); err != nil {
		t.Fatalf("RevokePrincipalSessions: %v", err)
	}
	if _, err := st.LookupSession(t.Context(), minted.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("a revoked key still resolved (err %v)", err)
	}
}

// TestPresentingAKeyRecordsItsUse is SEC-003e's operational half.
//
// Without it the only way to answer "is anything still using this key?" is to
// revoke it and wait for something to break, which makes the safe act — retiring
// an unused credential — indistinguishable from an outage the operator caused.
func TestPresentingAKeyRecordsItsUse(t *testing.T) {
	st, clock := newTestStore(t)
	minted, err := mintFor(t, st, KindUser, "used", 0)
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}

	before := apiKeyCredential(t, st, minted)
	if before.LastUsedAt != nil {
		t.Fatalf("a freshly minted key already records a use at %d", *before.LastUsedAt)
	}

	clock.advance(5_000)
	usedAt := clock.now()
	if _, err := st.LookupSession(t.Context(), minted.Token); err != nil {
		t.Fatalf("LookupSession: %v", err)
	}

	after := apiKeyCredential(t, st, minted)
	if after.LastUsedAt == nil {
		t.Fatal("presenting a key recorded no use — 'is anything still using this?' is then answerable only by " +
			"revoking it and waiting for a breakage (SEC-003e)")
	}
	if *after.LastUsedAt != usedAt {
		t.Errorf("last_used_at = %d, want the presentation instant %d", *after.LastUsedAt, usedAt)
	}
}

// TestAnExpiredKeyIsIndistinguishableFromAnInventedOne: an expired key, a
// revoked one and a token that never existed all answer the same way.
//
// A caller that could tell them apart could probe which of its guesses once
// existed, which turns a refusal into a confirmation.
func TestAnExpiredKeyIsIndistinguishableFromAnInventedOne(t *testing.T) {
	st, clock := newTestStore(t)
	expiring, err := mintFor(t, st, KindUser, "expiring", clock.now()+1_000)
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
	revoked, err := mintFor(t, st, KindUser, "revoked", 0)
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
	if _, err := st.RevokePrincipalSessions(t.Context(), revoked.Session.PrincipalID); err != nil {
		t.Fatalf("RevokePrincipalSessions: %v", err)
	}
	clock.advance(60_000)

	invented := expiring.Token[:strings.LastIndex(expiring.Token, ".")+1] +
		strings.Repeat("ab", (len(expiring.Token)-strings.LastIndex(expiring.Token, ".")-1)/2)

	for name, tok := range map[string]string{
		"expired":  expiring.Token,
		"revoked":  revoked.Token,
		"invented": invented,
	} {
		_, err := st.LookupSession(t.Context(), tok)
		if !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("%s key: err = %v, want ErrSessionNotFound — the three must be indistinguishable, or a caller "+
				"can probe which of its guesses once existed", name, err)
		}
	}
}
