package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// TestSetupRedemptionBudgetRefusesAGuesser pins SEC-033's attempt budget on the
// SETUP-code path.
//
// Both budgeted routes carry the same control, and only one of them was tested:
// deleting the budget check from the credential-reset handler fails the tree,
// and deleting it from the setup handler left every test green. Setup is the
// route that mints the FIRST owner of an unclaimed box, so the code it guards is
// the most valuable one the platform ever issues.
//
// The refusal must also come BEFORE the lookup — the budget "exists to stop the
// lookups, not to filter their results" — so this drives wrong codes only. A
// budget consulted after a failed lookup would still refuse here, but it would
// have done the work the control exists to prevent.
func TestSetupRedemptionBudgetRefusesAGuesser(t *testing.T) {
	clock := newTestClock()
	st, err := Open(":memory:", clock.now, ulid.New)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const limit = 5
	a := NewAuthenticator(st, nil, nil, nil)
	handlers := NewHandlers(a, NewGrantAttemptBudget(limit, 60_000), RootScopeNode)
	mux := apihttp.WithTraceID(http.HandlerFunc(handlers.Claim))

	claimFrom := func(addr, code string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(claimRequest{
			Code: code, Identifier: "first@example.test", Password: "a long enough passphrase",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(string(body)))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	const guesser = "192.168.50.9:41234"
	// Every attempt carries a wrong code, so nothing here can succeed by luck;
	// what is being measured is whether the ATTEMPTS run out.
	for i := 0; i < limit; i++ {
		if rec := claimFrom(guesser, "01J8Z0000000000000000GUESS"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d was rate-limited before the budget of %d was spent", i+1, limit)
		}
	}
	rec := claimFrom(guesser, "01J8Z0000000000000000GUESS")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d past a budget of %d returned %d, want 429 — an unclaimed box's setup code can be "+
			"guessed without limit (SEC-033)", limit+1, limit, rec.Code)
	}
	if code := problemCode(t, rec); code != "RATE_LIMITED" {
		t.Errorf("refusal code = %q, want RATE_LIMITED", code)
	}

	// The control, and the half that keeps the budget a bound on the GUESSER
	// rather than on everybody else: a different source still has its own.
	if rec := claimFrom("192.168.50.10:41234", "01J8Z0000000000000000GUESS"); rec.Code == http.StatusTooManyRequests {
		t.Error("an unrelated source was refused by the guesser's exhausted budget — the control would take the " +
			"whole box offline for every operator to slow one attacker")
	}
}
