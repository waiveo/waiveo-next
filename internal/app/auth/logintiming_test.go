package auth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

// logintiming_test.go is the MEASURED half of the login surface's
// anti-enumeration property (SEC-090's neighbourhood, and the reason
// handlers.Login maps every failure onto one generic refusal).
//
// TestLoginDoesNotDiscloseWhetherAnIdentifierExists (http_test.go) proves the
// two refusals are indistinguishable in the response BODY. That is only half the
// claim: a login that short-circuits before Argon2id when the identifier
// resolves to nothing answers in microseconds where a wrong password costs the
// full KDF, and that difference is trivially observable over a network. The body
// assertion cannot see it, so it is asserted here instead — and asserted as a
// MEASUREMENT, so a future short-circuit that reintroduces the gap fails the
// build rather than being caught by the next person to think about it.
//
// # Why this is not a flaky test
//
// Three choices, each aimed at the same problem — a loaded CI runner adds time
// to whatever it happens to be running:
//
//   - The statistic is the MINIMUM of N rounds, never a mean. Scheduler
//     preemption, GC, and a busy runner can only ever make a sample SLOWER, so
//     the minimum is the least-contaminated estimate of what the path actually
//     costs. A mean would move with the noise; the minimum moves with the code.
//   - The two paths are INTERLEAVED, one round each, so a slow phase (a GC
//     cycle, another package's test hogging cores) lands on both rather than
//     biasing one.
//   - The bar is a RATIO with a wide band, not an absolute budget. Absolute
//     durations vary by an order of magnitude across machines; the ratio does
//     not, because both paths run exactly one Argon2id derivation at identical
//     parameters and that derivation dominates everything else in the request.
//
// The tolerance is 2x. The gap this test exists to prevent was ~749x (a ~480µs
// unknown-identifier refusal against a ~360ms wrong-password one), and any
// re-introduction of it is a path that skips the KDF entirely — which cannot
// land inside 2x of a path that runs it. So the band is ~375x looser than the
// defect and still leaves no room for the defect to hide, which is exactly the
// trade a timing assertion on shared hardware should make.
const loginTimingTolerance = 2.0

// loginTimingRounds is how many interleaved measurements each path gets. Seven
// is enough for the minimum to settle (both paths are one KDF plus a
// sub-millisecond SQLite read) while keeping the whole test under a second.
const loginTimingRounds = 7

// TestLoginTimingDoesNotDiscloseWhetherAnIdentifierExists asserts that a login
// naming a KNOWN identifier with the wrong password and a login naming an
// identifier that does not exist take the same time to refuse, within
// loginTimingTolerance.
func TestLoginTimingDoesNotDiscloseWhetherAnIdentifierExists(t *testing.T) {
	h := newHTTPHarness(t)

	cred, err := h.store.FindPasswordCredential(context.Background(), h.ident)
	if err != nil {
		t.Fatalf("the fixture's own credential must resolve: %v", err)
	}
	// The harness's lockout tolerates 3 consecutive failures per (credential,
	// IP class) and then refuses BEFORE any verification (SEC-090). A locked
	// refusal is a different path with a different cost, so the known
	// identifier's key is cleared after every round: each measured request is an
	// ordinary wrong-password refusal, which is the path under test.
	knownKey := LockoutKey(cred.CredentialID, IPClassLAN)

	unknownSeq := 0
	measure := func(knownIdentifier bool) time.Duration {
		t.Helper()
		identifier := h.ident
		if !knownIdentifier {
			// A fresh identifier per round for the same reason: an unresolved
			// identifier's lockout key is a hash of what was submitted, so
			// reusing one would lock it out partway through the run.
			unknownSeq++
			identifier = fmt.Sprintf("nobody-%d@example.test", unknownSeq)
		}
		start := time.Now()
		rec := h.login(t, identifier, "not this credential's password")
		elapsed := time.Since(start)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("every measured round must be an ordinary 401 refusal (a 429 would be measuring the "+
				"lockout path instead); got %d %s", rec.Code, rec.Body.String())
		}
		h.auth.lockout.Succeed(knownKey)
		return elapsed
	}

	// One untimed round of each: the first request pays for SQLite's page cache,
	// the first 64 MiB Argon2id allocation, and the handler's cold code paths.
	measure(true)
	measure(false)

	known, unknown := time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	for range loginTimingRounds {
		known = min(known, measure(true))
		unknown = min(unknown, measure(false))
	}

	slow, fast := known, unknown
	if fast > slow {
		slow, fast = fast, slow
	}
	ratio := float64(slow) / float64(fast)
	t.Logf("login refusal, min of %d interleaved rounds: known identifier %v, unknown identifier %v (ratio %.2fx)",
		loginTimingRounds, known, unknown, ratio)
	if ratio > loginTimingTolerance {
		t.Fatalf("login timing discriminates a known identifier from an unknown one: known %v vs unknown %v "+
			"(%.2fx apart, tolerance %.1fx). An unknown identifier must be verified against a dummy hash so "+
			"Argon2id runs either way — otherwise the elapsed time enumerates identifiers that the identical "+
			"response bodies exist to hide.", known, unknown, ratio, loginTimingTolerance)
	}
}

// TestDummyPasswordHashCostsWhatARealCredentialDoes is the structural half of
// the same property. The timing assertion above proves the two paths cost the
// same TODAY; this proves WHY, so a dummy quietly weakened to cheaper parameters
// is caught as itself rather than as a mysterious timing regression.
//
// The reference is a really-hashed credential, not the package's own constants:
// the question is whether the dummy matches what a credential written today
// actually costs.
func TestDummyPasswordHashCostsWhatARealCredentialDoes(t *testing.T) {
	real, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	_, realKey, realMem, realTime, realThreads, err := decodeHash(real)
	if err != nil {
		t.Fatalf("decode a real credential's hash: %v", err)
	}
	_, dummyKey, dummyMem, dummyTime, dummyThreads, err := decodeHash(DummyPasswordHash)
	if err != nil {
		t.Fatalf("decode the dummy hash: %v", err)
	}

	if dummyMem != realMem || dummyTime != realTime || dummyThreads != realThreads {
		t.Fatalf("the dummy hash must carry the SAME Argon2id parameters a real credential does, or verifying "+
			"against it is cheaper than verifying against a real hash and the timing oracle reopens: "+
			"dummy m=%d,t=%d,p=%d vs real m=%d,t=%d,p=%d",
			dummyMem, dummyTime, dummyThreads, realMem, realTime, realThreads)
	}
	// VerifyPassword derives exactly len(key) bytes, so an unequal key length is
	// an unequal amount of work even at equal parameters.
	if len(dummyKey) != len(realKey) {
		t.Fatalf("the dummy hash's key length (%d) must match a real credential's (%d): the KDF derives that "+
			"many bytes, so a shorter one is less work", len(dummyKey), len(realKey))
	}

	// And nothing verifies against it — the property that keeps a dummy
	// verification from ever being mistaken for a real one.
	for _, password := range []string{"", "correct horse battery staple", "not this credential's password"} {
		if err := VerifyPassword(DummyPasswordHash, password); err == nil {
			t.Fatalf("password %q verified against the dummy hash; nothing may", password)
		}
	}
}
