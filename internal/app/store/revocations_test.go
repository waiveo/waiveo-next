package store_test

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// TestReRevokingKeepsTheFirstInstant pins what makes the audit trail answerable.
//
// Revocation is terminal (`api/1` API-142), so a second revocation of the same
// subject is not a new decision. Overwriting `revoked_at` would move the trail's
// own answer to "when did this stop being trusted" — which is precisely the
// question a later investigation asks, and the one an operator re-clicking a
// stale page would silently change the answer to.
//
// It is asserted through the ordered read rather than by selecting the column,
// because that read is what the snapshot carries, and a property no consumer can
// observe is one that will not survive a refactor.
func TestReRevokingKeepsTheFirstInstant(t *testing.T) {
	now := int64(1_700_000_000_000)
	s, err := store.Open(":memory:", func() int64 { return now })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const screenID = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	if err := s.RevokeSubject(t.Context(), store.RevocationSubjectScreen, screenID, "first-actor"); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	first := revokedAt(t, s, screenID)

	now += 86_400_000 // a day later, a second click
	if err := s.RevokeSubject(t.Context(), store.RevocationSubjectScreen, screenID, "second-actor"); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if got := revokedAt(t, s, screenID); got != first {
		t.Errorf("revoked_at moved from %d to %d on a second revocation — the trail's answer to 'when did this stop "+
			"being trusted' must be the FIRST decision, not the most recent click", first, got)
	}

	// And the subject is still revoked exactly once in the list the snapshot
	// carries: a second record would be a second id for one screen.
	ids, err := s.RevokedScreens(t.Context())
	if err != nil {
		t.Fatalf("RevokedScreens: %v", err)
	}
	if len(ids) != 1 || ids[0] != screenID {
		t.Errorf("revoked screens = %v, want exactly [%s]", ids, screenID)
	}
}

// revokedAt reads the recorded instant through the store's own connection.
func revokedAt(t *testing.T, s *store.Store, screenID string) int64 {
	t.Helper()
	ok, err := s.IsRevoked(t.Context(), store.RevocationSubjectScreen, screenID)
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !ok {
		t.Fatalf("screen %s is not revoked", screenID)
	}
	at, err := s.RevokedAt(t.Context(), store.RevocationSubjectScreen, screenID)
	if err != nil {
		t.Fatalf("RevokedAt: %v", err)
	}
	return at
}

// TestAnUnknownSubjectKindIsRefusedAtTheStore keeps the closed set closed below
// the api layer too — a caller reaching the store directly (a CLI, a migration)
// must not be able to write a row nothing will ever read.
func TestAnUnknownSubjectKindIsRefusedAtTheStore(t *testing.T) {
	s, err := store.Open(":memory:", func() int64 { return 1_700_000_000_000 })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, kind := range []string{"", "device", "Screen"} {
		if err := s.RevokeSubject(t.Context(), kind, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "actor"); err == nil {
			t.Errorf("RevokeSubject accepted subject kind %q", kind)
		}
	}
	if err := s.RevokeSubject(t.Context(), store.RevocationSubjectRelay, "", "actor"); err == nil {
		t.Error("RevokeSubject accepted an empty subject id")
	}
}
