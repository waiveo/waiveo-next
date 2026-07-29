package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// TestGrantAttemptBudgetIsSpentPerSource pins the property that makes the
// SEC-033 budget a bound on the guesser rather than on everybody else.
//
// The budget was once keyed on the source-IP CLASS, of which there are three.
// That put every host on the LAN in one bucket, so ten attempts per window from
// any single machine — drivable cross-origin from a victim's own browser, since
// the redeem route accepts a simple POST — denied credential reset to every
// other host in that class until the window drained. On an appliance whose only
// other recovery path is a root-only console socket, that is account recovery
// removed by a caller who never has to guess correctly.
//
// Two halves, and both matter: a host's own exhaustion must bind that host, and
// must NOT bind a different one. The third assertion is the mirror-image
// mistake — keying on address:port would give every new connection a fresh
// bucket, which is a budget that counts nothing.
func TestGrantAttemptBudgetIsSpentPerSource(t *testing.T) {
	b := auth.NewGrantAttemptBudget(10, 15*60_000)
	now := int64(1_700_000_000_000)

	source := func(addr string) string {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.RemoteAddr = addr
		return auth.RequestSource(r)
	}

	attacker := source("192.168.1.66:41000")
	for i := 0; i < 10; i++ {
		if !b.Allow(auth.PurposeCredentialReset, attacker, now) {
			t.Fatalf("the attacker was refused at attempt %d of its own 10", i)
		}
	}
	if b.Allow(auth.PurposeCredentialReset, attacker, now) {
		t.Fatal("the attacker got an 11th attempt — its own budget does not bind it")
	}
	if !b.Allow(auth.PurposeCredentialReset, source("192.168.1.99:52000"), now) {
		t.Fatal("a DIFFERENT host was refused because the attacker exhausted the budget — the bucket is still shared")
	}
	if b.Allow(auth.PurposeCredentialReset, source("192.168.1.66:41001"), now) {
		t.Fatal("a new source PORT bought a fresh budget — the key is the connection, not the host")
	}
}
