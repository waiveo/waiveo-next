package playerserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// attemptbudget_test.go covers security-model/1 SEC-033 on POST
// /player/v1/pair: "a redemption rate limit and an attempt budget against
// repeated guesses of a live grant's code — the enforced control".
//
// The entropy in a grant_id (SEC-032's 128-bit floor) makes guessing infeasible
// in practice, but SEC-033 requires the budget, not the entropy: the two bound
// different things, and a deployment that later shortens a selector, or a
// mechanism that leaks one a bit at a time, is bounded only by the budget.
//
// The keying is tested as carefully as the counting. A budget keyed on
// something that collides across unrelated clients is worse than none — the
// caller who never guesses correctly takes the operation away from everyone
// else — which is exactly why the app's own grant budget was rekeyed off an
// address CLASS onto a request source identity.

// pairFrom POSTs a PairingRequest carrying selector directly at the handler,
// with remoteAddr as the request's source — the seam that lets a test drive
// several distinct sources, which an httptest client (always loopback) cannot.
func pairFrom(t *testing.T, srv *Server, remoteAddr, selector string) int {
	t.Helper()
	body, err := json.Marshal(PairingRequest{
		HardwareID:    "hw-budget",
		GrantSelector: selector,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/player/v1/pair", bytes.NewReader(body))
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	apihttp.WithTraceID(http.HandlerFunc(srv.handlePair)).ServeHTTP(rec, req)
	return rec.Code
}

// budgetTestServer builds a Server holding one live grant, on a fixed clock so
// the budget's own window never advances underneath a case.
func budgetTestServer(t *testing.T) (*Server, wire.PairingGrant) {
	t.Helper()
	certPEM, _, _, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(testScreenIDA)
	grant.IssuedAt = fixedNowMs
	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, func() int64 { return fixedNowMs })
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, grant
}

// TestPairRedemptionIsBudgetedPerSource is SEC-033's counting half: a sweep of
// wrong selectors from one source is refused once the budget is spent, and the
// refusal is a distinct, retryable-with-backoff status rather than another
// "your code is wrong".
//
// Without a budget every one of these attempts is answered on its merits
// forever, which is precisely the unbounded guessing SEC-033 names as "the
// attack this bounds".
func TestPairRedemptionIsBudgetedPerSource(t *testing.T) {
	srv, _ := budgetTestServer(t)

	const source = "198.51.100.77:41234"
	for i := 0; i < pairAttemptLimit; i++ {
		if got := pairFrom(t, srv, source, "grant-wrong-guess"); got != http.StatusBadRequest {
			t.Fatalf("attempt %d status = %d, want 400 (a wrong selector, within budget)", i+1, got)
		}
	}
	if got := pairFrom(t, srv, source, "grant-wrong-guess"); got != http.StatusTooManyRequests {
		t.Fatalf("attempt %d status = %d, want 429 — %d wrong selectors from one source were never rate-limited (SEC-033)",
			pairAttemptLimit+1, got, pairAttemptLimit+1)
	}
}

// TestPairBudgetRefusesEvenACorrectSelectorOnceSpent proves the budget is
// enforced BEFORE the selector is resolved, not merely applied to refusals.
// SEC-033 bounds "repeated guesses of a LIVE grant's code": a budget consulted
// only after a lookup fails would let a guesser sweep unbounded and still be
// admitted the moment they hit, which is the only outcome that matters.
//
// The grant used here is live and correct, and MUST still be refused — and,
// crucially, MUST NOT be consumed: it still redeems afterwards from a source
// with budget left. A budget that burned the grant while refusing it would turn
// a rate limit into a way to destroy other people's pairing codes.
func TestPairBudgetRefusesEvenACorrectSelectorOnceSpent(t *testing.T) {
	srv, grant := budgetTestServer(t)

	const guesser = "198.51.100.78:5000"
	for i := 0; i < pairAttemptLimit; i++ {
		pairFrom(t, srv, guesser, "grant-wrong-guess")
	}

	if got := pairFrom(t, srv, guesser, grant.GrantID); got != http.StatusTooManyRequests {
		t.Errorf("a correct selector presented after the budget was spent = %d, want 429 — the budget is being applied after the lookup, so a sweep that guesses right is still admitted (SEC-033)", got)
	}

	// And the grant survived: it was never looked up, so it was never consumed.
	if got := pairFrom(t, srv, "198.51.100.79:5000", grant.GrantID); got != http.StatusOK {
		t.Errorf("the live grant redeemed from a fresh source = %d, want 200 — a refused-over-budget attempt consumed a one-time grant it never even resolved", got)
	}
}

// TestPairBudgetIsNotSharedAcrossSources is SEC-033's keying half, and the
// failure mode that matters more than the counting: one source exhausting its
// budget MUST NOT deny pairing to an unrelated source.
//
// This is the mistake the app's own grant budget was rekeyed to remove. Keyed on
// an address CLASS — a handful of values — every host on the LAN shares one
// bucket, so an unauthenticated caller who never guesses correctly can make
// pairing impossible for every real screen in the building until the window
// drains. On an appliance whose pairing surface is the only way to bring a
// screen up, that is the outage, not the guessing.
func TestPairBudgetIsNotSharedAcrossSources(t *testing.T) {
	srv, grant := budgetTestServer(t)

	// A guesser on the same LAN — same /24, same address class, different host
	// — burns its whole budget.
	const guesser = "192.168.50.200:5000"
	for i := 0; i < pairAttemptLimit+5; i++ {
		pairFrom(t, srv, guesser, "grant-wrong-guess")
	}

	// A real screen, next to it on the LAN, pairs normally.
	if got := pairFrom(t, srv, "192.168.50.12:60001", grant.GrantID); got != http.StatusOK {
		t.Errorf("a legitimate screen's pairing = %d, want 200 — an unrelated host's exhausted budget denied pairing to everyone sharing its address class (SEC-033 keying)", got)
	}
}

// TestPairBudgetSeparatesSourcesByAddressNotPort pins the other half of the
// keying: the port is stripped, because a new connection gets a fresh ephemeral
// port. A budget that counted (address, port) would hand every single attempt
// its own bucket and count nothing at all — present, passing a naive test, and
// enforcing nothing.
func TestPairBudgetSeparatesSourcesByAddressNotPort(t *testing.T) {
	srv, _ := budgetTestServer(t)

	// The same host, a fresh ephemeral port each time — exactly what a real
	// guesser's successive connections look like.
	for i := 0; i < pairAttemptLimit; i++ {
		port := 40000 + i
		if got := pairFrom(t, srv, "203.0.113.9:"+itoa(port), "grant-wrong-guess"); got != http.StatusBadRequest {
			t.Fatalf("attempt %d status = %d, want 400", i+1, got)
		}
	}
	if got := pairFrom(t, srv, "203.0.113.9:49999", "grant-wrong-guess"); got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 — varying only the source PORT bought a fresh budget, so the budget counts nothing", got)
	}
}

// itoa is strconv.Itoa without the import churn in this file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestPairBudgetIsNotBypassedByRotatingWithinAnIPv6Prefix is the keying failure
// that mattered most on this endpoint, driven end to end through the handler.
//
// The budget bounds guesses per source, and a source that can be minted for free
// bounds nothing. A single IPv6 host holds a /64 and rotates addresses within it
// as ordinary behaviour (RFC 8981), so keyed on the full address one machine
// gets a fresh ten attempts per address and the budget on this unauthenticated
// endpoint is decorative. The attempts below all come from ONE /64 and must be
// refused exactly as though they came from one address.
func TestPairBudgetIsNotBypassedByRotatingWithinAnIPv6Prefix(t *testing.T) {
	srv, _ := budgetTestServer(t)

	// One host, a different address in its own /64 every time.
	for i := 0; i < pairAttemptLimit; i++ {
		addr := "[2001:db8:0:1::" + itoa(i+1) + "]:40000"
		if got := pairFrom(t, srv, addr, "grant-wrong-guess"); got != http.StatusBadRequest {
			t.Fatalf("attempt %d from %s status = %d, want 400 (a wrong selector, within budget)", i+1, addr, got)
		}
	}
	if got := pairFrom(t, srv, "[2001:db8:0:1::ff]:40000", "grant-wrong-guess"); got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 — rotating the address within one /64 bought a fresh budget every time, so this endpoint's SEC-033 budget is unenforced against any IPv6 host", got)
	}
}

// TestPairBudgetSeparatesDistinctIPv6Prefixes is the other side of that keying,
// and the reason it stops at the /64 rather than widening further: a real screen
// on its own allocation must still pair while an unrelated host is being
// refused. Widening the key is how a per-source budget becomes the shared bucket
// an unauthenticated caller can exhaust for the whole site.
func TestPairBudgetSeparatesDistinctIPv6Prefixes(t *testing.T) {
	srv, grant := budgetTestServer(t)

	for i := 0; i < pairAttemptLimit+5; i++ {
		pairFrom(t, srv, "[2001:db8:0:1::9]:5000", "grant-wrong-guess")
	}
	if got := pairFrom(t, srv, "[2001:db8:0:2::7]:60001", grant.GrantID); got != http.StatusOK {
		t.Errorf("a legitimate screen on a different /64 = %d, want 200 — an unrelated allocation's exhausted budget denied pairing (SEC-033 keying)", got)
	}
}
