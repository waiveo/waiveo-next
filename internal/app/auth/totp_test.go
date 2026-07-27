package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/secretseal"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// testSealer is the real sealing construction over a fixed key — not a stub.
// Sealing is the property under test in several cases below (a secret must not
// be readable from the row), and a no-op stub would make every one of them pass
// against an implementation that stored the secret in the clear.
func testSealer(t *testing.T) SecretSealer {
	t.Helper()
	key := make([]byte, secretseal.KeySize)
	for i := range key {
		key[i] = byte(i * 7)
	}
	s, err := secretseal.New(key)
	if err != nil {
		t.Fatalf("secretseal.New: %v", err)
	}
	return s
}

// totpStore returns a store with a real sealer and a controllable clock.
func totpStore(t *testing.T) (*Store, *testClock) {
	t.Helper()
	clock := newTestClock()
	st, err := Open(":memory:", clock.now, ulid.New, WithSecretSealer(testSealer(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, clock
}

func totpPrincipal(t *testing.T, st *Store) PrincipalRow {
	t.Helper()
	p, err := st.CreatePrincipal(context.Background(), KindUser, "totp user")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	return p
}

// ---- RFC 6238 vector ------------------------------------------------------

// TestTOTPMatchesRFC6238Vectors pins the algorithm against RFC 6238's own
// published test vectors rather than against this implementation's output.
//
// That distinction is the whole value of the case. A test seeded from what the
// code currently produces proves only that the code is self-consistent; it would
// pass just as happily against a wrong truncation, a wrong counter endianness,
// or a wrong step size — and the first person to notice would be an operator
// whose authenticator app disagreed with the server. These digits come from the
// RFC's Appendix B table (the SHA1 rows), truncated to 6 as this implementation
// does.
func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	// RFC 6238's SHA1 seed: the ASCII string "12345678901234567890".
	secret := []byte("12345678901234567890")
	// {unix seconds, expected 8-digit code from the RFC}; this implementation
	// emits 6 digits, so each expectation is the RFC value's last 6.
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		step := TOTPStep(c.unix * 1000)
		if got := TOTPCode(secret, step); got != c.want {
			t.Errorf("TOTPCode at unix %d (step %d) = %s, want %s (RFC 6238 Appendix B)", c.unix, step, got, c.want)
		}
	}
}

func TestTOTPStepIsThirtySeconds(t *testing.T) {
	if TOTPStep(0) != 0 {
		t.Fatalf("TOTPStep(0) = %d, want 0", TOTPStep(0))
	}
	if TOTPStep(29_999) != 0 {
		t.Fatalf("TOTPStep(29.999s) = %d, want 0", TOTPStep(29_999))
	}
	if TOTPStep(30_000) != 1 {
		t.Fatalf("TOTPStep(30s) = %d, want 1", TOTPStep(30_000))
	}
}

// TestMatchTOTPCodeToleratesExactlyOneStep is the skew argument made executable:
// one step either side is accepted, two is not.
func TestMatchTOTPCodeToleratesExactlyOneStep(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	const nowMs int64 = 1_752_537_600_000
	now := TOTPStep(nowMs)

	for _, d := range []int64{-1, 0, 1} {
		step, ok := MatchTOTPCode(secret, TOTPCode(secret, now+d), nowMs)
		if !ok {
			t.Fatalf("a code %d step(s) away was refused; the tolerance is one step either side", d)
		}
		if step != now+d {
			t.Fatalf("matched step = %d, want %d — the caller consumes this exact step", step, now+d)
		}
	}
	for _, d := range []int64{-2, 2, 10} {
		if _, ok := MatchTOTPCode(secret, TOTPCode(secret, now+d), nowMs); ok {
			t.Fatalf("a code %d steps away was accepted; the tolerance must stay at one step (SEC-066-068)", d)
		}
	}
}

func TestMatchTOTPCodeRefusesMalformedInput(t *testing.T) {
	secret, _ := NewTOTPSecret()
	for _, in := range []string{"", "12345", "1234567", "abcdef", "  "} {
		if _, ok := MatchTOTPCode(secret, in, 1_752_537_600_000); ok {
			t.Fatalf("MatchTOTPCode accepted %q", in)
		}
	}
}

func TestTOTPSecretEncodingRoundTrips(t *testing.T) {
	secret, _ := NewTOTPSecret()
	encoded := EncodeTOTPSecret(secret)
	if strings.Contains(encoded, "=") {
		t.Fatalf("the encoded secret carries base32 padding (%q); authenticator apps reject it", encoded)
	}
	got, err := DecodeTOTPSecret(strings.ToLower(encoded[:4]) + " " + encoded[4:])
	if err != nil {
		t.Fatalf("DecodeTOTPSecret: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatal("the decoded secret differs from the encoded one")
	}
}

// TestDecodeTOTPSecretErrorCarriesNoSecret is the no-secrets-in-error-strings
// house rule, asserted rather than assumed.
func TestDecodeTOTPSecretErrorCarriesNoSecret(t *testing.T) {
	const bad = "NOTBASE32!!!SECRETMATERIAL"
	_, err := DecodeTOTPSecret(bad)
	if err == nil {
		t.Fatal("DecodeTOTPSecret accepted a non-base32 value")
	}
	if strings.Contains(err.Error(), "SECRETMATERIAL") {
		t.Fatalf("the error echoes the presented value: %v", err)
	}
}

func TestTOTPURIIsImportable(t *testing.T) {
	secret, _ := NewTOTPSecret()
	uri := TOTPURI("Waiveo", "owner@example.test", secret)
	for _, want := range []string{"otpauth://totp/", "secret=" + EncodeTOTPSecret(secret), "digits=6", "period=30", "algorithm=SHA1"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("otpauth URI %q is missing %q", uri, want)
		}
	}
}

// ---- store: enrollment is two-phase ---------------------------------------

// TestEnrollmentIsNotACredentialUntilProven drives the two-phase design: a
// started enrollment must not put an authenticating row in the credential
// relation, or an abandoned enrollment would lock the principal out.
func TestEnrollmentIsNotACredentialUntilProven(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()

	secret, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false)
	if err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}
	enrolled, err := st.HasTOTPCredential(ctx, p.PrincipalID)
	if err != nil {
		t.Fatalf("HasTOTPCredential: %v", err)
	}
	if enrolled {
		t.Fatal("beginning an enrollment created a credential row; nothing has yet proven the secret reached an authenticator")
	}

	step := TOTPStep(clock.now())
	cred, err := st.ArmTOTPCredential(ctx, p.PrincipalID, secret, step)
	if err != nil {
		t.Fatalf("ArmTOTPCredential: %v", err)
	}
	if cred.Kind != CredentialTOTP || cred.PrincipalID != p.PrincipalID {
		t.Fatalf("armed credential = %+v; want a totp credential for %s", cred, p.PrincipalID)
	}
	enrolled, _ = st.HasTOTPCredential(ctx, p.PrincipalID)
	if !enrolled {
		t.Fatal("arming did not produce a credential row (SEC-003)")
	}
	// The pending row is consumed, so the same secret cannot arm a second time.
	if _, err := st.PendingTOTPSecret(ctx, p.PrincipalID); !errors.Is(err, ErrNoPendingTOTP) {
		t.Fatalf("the pending enrollment survived arming: %v", err)
	}
}

// TestArmedSecretIsSealedAtRest is the secret-storage decision, asserted at the
// row: the stored value must not be the secret, in any encoding a database
// reader could unpick without the workspace key.
func TestArmedSecretIsSealedAtRest(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()

	secret, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false)
	if err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}
	cred, err := st.ArmTOTPCredential(ctx, p.PrincipalID, secret, TOTPStep(clock.now()))
	if err != nil {
		t.Fatalf("ArmTOTPCredential: %v", err)
	}
	for _, form := range []string{string(secret), EncodeTOTPSecret(secret)} {
		if strings.Contains(cred.Secret, form) {
			t.Fatal("the credential row carries the shared secret in a recoverable encoding")
		}
	}
	// And it round-trips for the one party that holds the key.
	got, err := st.TOTPSecret(cred)
	if err != nil {
		t.Fatalf("TOTPSecret: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatal("the sealed secret did not open back to the enrolled secret")
	}
}

// TestASealedSecretDoesNotMoveBetweenRows drives the AAD binding: a sealed blob
// copied onto another credential's row must not open, so an attacker with write
// access to the database cannot graft their own authenticator onto someone
// else's credential.
func TestASealedSecretDoesNotMoveBetweenRows(t *testing.T) {
	st, clock := totpStore(t)
	ctx := context.Background()
	victim := totpPrincipal(t, st)
	attacker := totpPrincipal(t, st)

	vs, _ := st.BeginTOTPEnrollment(ctx, victim.PrincipalID, false)
	vc, err := st.ArmTOTPCredential(ctx, victim.PrincipalID, vs, TOTPStep(clock.now()))
	if err != nil {
		t.Fatalf("arm victim: %v", err)
	}
	as, _ := st.BeginTOTPEnrollment(ctx, attacker.PrincipalID, false)
	ac, err := st.ArmTOTPCredential(ctx, attacker.PrincipalID, as, TOTPStep(clock.now()))
	if err != nil {
		t.Fatalf("arm attacker: %v", err)
	}

	// The victim's sealed blob, pasted onto the attacker's credential id.
	grafted := ac
	grafted.Secret = vc.Secret
	if _, err := st.TOTPSecret(grafted); err == nil {
		t.Fatal("a sealed secret opened under a different credential id — the ciphertext is not bound to its row")
	}
}

// TestStoreWithoutASealerRefusesToEnroll pins the fail-closed default: no
// sealer means no enrollment, never an enrollment stored in the clear.
func TestStoreWithoutASealerRefusesToEnroll(t *testing.T) {
	clock := newTestClock()
	st, err := Open(":memory:", clock.now, ulid.New)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p := totpPrincipal(t, st)
	if _, err := st.BeginTOTPEnrollment(context.Background(), p.PrincipalID, false); !errors.Is(err, ErrNoSecretSealer) {
		t.Fatalf("BeginTOTPEnrollment without a sealer = %v; want ErrNoSecretSealer", err)
	}
}

func TestOrdinaryReEnrollmentIsRefused(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()
	secret, _ := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false)
	if _, err := st.ArmTOTPCredential(ctx, p.PrincipalID, secret, TOTPStep(clock.now())); err != nil {
		t.Fatalf("ArmTOTPCredential: %v", err)
	}
	if _, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false); !errors.Is(err, ErrTOTPAlreadyEnrolled) {
		t.Fatalf("re-enrolling an armed principal = %v; want ErrTOTPAlreadyEnrolled (SEC-052)", err)
	}
	// The recovery path stays open (SEC-022) — otherwise a recovery session
	// could never leave its restricted tier.
	if _, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, true); err != nil {
		t.Fatalf("a replacement enrollment must remain possible for SEC-022's re-enrollment: %v", err)
	}
}

func TestSystemConsoleCannotEnrollTOTP(t *testing.T) {
	st, _ := totpStore(t)
	p, err := st.CreatePrincipal(context.Background(), KindSystemConsole, "console")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := st.BeginTOTPEnrollment(context.Background(), p.PrincipalID, false); err == nil {
		t.Fatal("system-console enrolled a credential; SEC-002 makes it the sole kind with none")
	}
}

// ---- store: step consumption is the replay defense ------------------------

// TestConsumeTOTPStepRefusesAReplay is the replay requirement at the store: the
// same step cannot be claimed twice, and neither can an earlier one.
func TestConsumeTOTPStepRefusesAReplay(t *testing.T) {
	st, _ := totpStore(t)
	ctx := context.Background()
	const cred = "01J8Z9AVTHTEST0F1XTVRE0001"

	ok, err := st.ConsumeTOTPStep(ctx, cred, 100)
	if err != nil || !ok {
		t.Fatalf("first claim of step 100 = %v, %v; want true", ok, err)
	}
	ok, err = st.ConsumeTOTPStep(ctx, cred, 100)
	if err != nil {
		t.Fatalf("ConsumeTOTPStep: %v", err)
	}
	if ok {
		t.Fatal("the same step was claimed twice — a code accepted twice is a replay")
	}
	// An EARLIER step is refused too, which is the clock-floor half: a host
	// clock moved backward must not re-open a spent window (SEC-066).
	ok, _ = st.ConsumeTOTPStep(ctx, cred, 99)
	if ok {
		t.Fatal("a step below the high-water mark was claimed; the floor moved backward")
	}
	ok, _ = st.ConsumeTOTPStep(ctx, cred, 101)
	if !ok {
		t.Fatal("the next step was refused; the floor must admit forward progress")
	}
	step, seen, err := st.TOTPLastStep(ctx, cred)
	if err != nil || !seen || step != 101 {
		t.Fatalf("TOTPLastStep = %d, %v, %v; want 101, true, nil", step, seen, err)
	}
}

// TestConsumeTOTPStepIsAtomicUnderConcurrency drives the race the conditional
// upsert exists to close: N requests presenting the SAME code at once must
// produce exactly one success. A read-then-write implementation lets all N read
// the old mark and all N conclude they were first.
func TestConsumeTOTPStepIsAtomicUnderConcurrency(t *testing.T) {
	st, _ := totpStore(t)
	ctx := context.Background()
	const cred = "01J8Z9AVTHTEST0F1XTVRE0002"

	const racers = 12
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := st.ConsumeTOTPStep(ctx, cred, 500)
			if err != nil {
				t.Errorf("ConsumeTOTPStep: %v", err)
				return
			}
			if ok {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent claims of one step succeeded; exactly one must", succeeded, racers)
	}
}

// TestArmingSpendsTheConfirmingStep closes the enrollment-side replay: the code
// that proved the enrollment must not also work at the login form.
func TestArmingSpendsTheConfirmingStep(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()

	secret, _ := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false)
	step := TOTPStep(clock.now())
	cred, err := st.ArmTOTPCredential(ctx, p.PrincipalID, secret, step)
	if err != nil {
		t.Fatalf("ArmTOTPCredential: %v", err)
	}
	ok, err := st.ConsumeTOTPStep(ctx, cred.CredentialID, step)
	if err != nil {
		t.Fatalf("ConsumeTOTPStep: %v", err)
	}
	if ok {
		t.Fatal("the confirming code's step was still spendable at login — the enrollment code is replayable")
	}
}

func TestArmingWithoutAPendingEnrollmentIsRefused(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	secret, _ := NewTOTPSecret()
	if _, err := st.ArmTOTPCredential(context.Background(), p.PrincipalID, secret, TOTPStep(clock.now())); !errors.Is(err, ErrNoPendingTOTP) {
		t.Fatalf("arming with no enrollment in flight = %v; want ErrNoPendingTOTP", err)
	}
}

// ---- store: a pending enrollment expires ----------------------------------

// pendingTOTPRows counts principalID's rows in the pending relation, read
// straight off the table.
//
// The count is the point. Every other assertion below could be satisfied by an
// implementation that merely REFUSED an aged-out enrollment while leaving the
// sealed secret in the database forever, and "the row is gone" is a different
// claim from "the row is ignored" — this is what tells them apart.
func pendingTOTPRows(t *testing.T, st *Store, principalID string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM totp_pending WHERE principal_id = ?`, principalID).Scan(&n); err != nil {
		t.Fatalf("count pending enrollments: %v", err)
	}
	return n
}

// TestPendingEnrollmentExpiresAtItsTTL drives the window's far edge on the read
// path: at exactly created_at + ttl the enrollment is gone, and the row with it.
//
// The clock is INJECTED — the enrollment ages by half an hour without the test
// taking half an hour, and without a sleep whose duration would have to be
// guessed against a machine's load.
func TestPendingEnrollmentExpiresAtItsTTL(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()

	if _, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false); err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}
	clock.advance(PendingTOTPEnrollmentTTLMs)

	if _, err := st.PendingTOTPSecret(ctx, p.PrincipalID); !errors.Is(err, ErrNoPendingTOTP) {
		t.Fatalf("an enrollment at its ttl was still readable: %v", err)
	}
	if n := pendingTOTPRows(t, st, p.PrincipalID); n != 0 {
		t.Fatalf("%d expired pending row(s) survived the read; an expired secret must be removed, not merely ignored", n)
	}
}

// TestPendingEnrollmentInsideItsWindowStillArms is the other edge: one
// millisecond short of the ttl the enrollment is untouched and arms normally.
// Without it, "expired" would be satisfied by an implementation that expired
// everything.
func TestPendingEnrollmentInsideItsWindowStillArms(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()

	secret, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false)
	if err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}
	clock.advance(PendingTOTPEnrollmentTTLMs - 1)

	got, err := st.PendingTOTPSecret(ctx, p.PrincipalID)
	if err != nil {
		t.Fatalf("an enrollment one millisecond inside its window was refused: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatal("the pending secret changed inside its window")
	}
	if _, err := st.ArmTOTPCredential(ctx, p.PrincipalID, secret, TOTPStep(clock.now())); err != nil {
		t.Fatalf("ArmTOTPCredential inside the window: %v", err)
	}
	armed, _ := st.HasTOTPCredential(ctx, p.PrincipalID)
	if !armed {
		t.Fatal("confirming inside the window did not arm the credential")
	}
}

// TestArmingRefusesAnExpiredEnrollment goes straight at the arming transaction,
// never touching the read path first.
//
// That is the check that has to hold: a caller that read the secret while the
// window was open and arrived here after it closed reaches this code with a
// valid secret in hand, and only the check inside the consuming transaction
// stands between it and a live second factor.
func TestArmingRefusesAnExpiredEnrollment(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()

	secret, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false)
	if err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}
	clock.advance(PendingTOTPEnrollmentTTLMs)

	if _, err := st.ArmTOTPCredential(ctx, p.PrincipalID, secret, TOTPStep(clock.now())); !errors.Is(err, ErrNoPendingTOTP) {
		t.Fatalf("arming an expired enrollment = %v; want ErrNoPendingTOTP", err)
	}
	armed, err := st.HasTOTPCredential(ctx, p.PrincipalID)
	if err != nil {
		t.Fatalf("HasTOTPCredential: %v", err)
	}
	if armed {
		t.Fatal("an expired enrollment armed a second factor")
	}
	if n := pendingTOTPRows(t, st, p.PrincipalID); n != 0 {
		t.Fatalf("%d expired pending row(s) survived the refused arming", n)
	}
}

// TestRestartingAnEnrollmentResetsItsWindow: the upsert rewrites created_at, so
// an operator who lets one attempt lapse and starts again gets a full window on
// the NEW secret rather than the remains of the old one's.
func TestRestartingAnEnrollmentResetsItsWindow(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()

	if _, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false); err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}
	clock.advance(PendingTOTPEnrollmentTTLMs - 1)
	restarted, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false)
	if err != nil {
		t.Fatalf("restart BeginTOTPEnrollment: %v", err)
	}
	// Past the FIRST enrollment's ttl, comfortably, but inside the second's.
	clock.advance(PendingTOTPEnrollmentTTLMs - 1)

	if _, err := st.ArmTOTPCredential(ctx, p.PrincipalID, restarted, TOTPStep(clock.now())); err != nil {
		t.Fatalf("a restarted enrollment inside its own window was refused: %v", err)
	}
}

// TestPruneExpiredTOTPEnrollmentsRetiresOnlyExpiredRows drives the sweep the
// feeder runs at boot and hourly: it must clear what has aged out and leave an
// enrollment somebody is in the middle of completely alone.
func TestPruneExpiredTOTPEnrollmentsRetiresOnlyExpiredRows(t *testing.T) {
	st, clock := totpStore(t)
	stale := totpPrincipal(t, st)
	live := totpPrincipal(t, st)
	ctx := context.Background()

	if _, err := st.BeginTOTPEnrollment(ctx, stale.PrincipalID, false); err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}
	clock.advance(PendingTOTPEnrollmentTTLMs)
	liveSecret, err := st.BeginTOTPEnrollment(ctx, live.PrincipalID, false)
	if err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}

	n, err := st.PruneExpiredTOTPEnrollments(ctx)
	if err != nil {
		t.Fatalf("PruneExpiredTOTPEnrollments: %v", err)
	}
	if n != 1 {
		t.Fatalf("the sweep retired %d row(s); exactly one had aged out", n)
	}
	if rows := pendingTOTPRows(t, st, stale.PrincipalID); rows != 0 {
		t.Fatalf("the expired enrollment survived the sweep (%d row(s))", rows)
	}
	got, err := st.PendingTOTPSecret(ctx, live.PrincipalID)
	if err != nil {
		t.Fatalf("the sweep took an enrollment that was still in its window: %v", err)
	}
	if string(got) != string(liveSecret) {
		t.Fatal("the surviving enrollment's secret changed")
	}

	// Idempotent: a second sweep with nothing left to do retires nothing.
	if n, err := st.PruneExpiredTOTPEnrollments(ctx); err != nil || n != 0 {
		t.Fatalf("a repeat sweep retired %d row(s) (err=%v); want 0", n, err)
	}
}

// TestFactoryResetClearsTOTPState is SEC-121's "force fresh enrollment on every
// principal", applied to the two relations this feature adds.
func TestFactoryResetClearsTOTPState(t *testing.T) {
	st, clock := totpStore(t)
	p := totpPrincipal(t, st)
	ctx := context.Background()
	secret, _ := st.BeginTOTPEnrollment(ctx, p.PrincipalID, false)
	cred, err := st.ArmTOTPCredential(ctx, p.PrincipalID, secret, TOTPStep(clock.now()))
	if err != nil {
		t.Fatalf("ArmTOTPCredential: %v", err)
	}
	if _, err := st.BeginTOTPEnrollment(ctx, p.PrincipalID, true); err != nil {
		t.Fatalf("BeginTOTPEnrollment: %v", err)
	}

	if err := st.DestroyAllPrincipals(ctx); err != nil {
		t.Fatalf("DestroyAllPrincipals: %v", err)
	}
	if _, seen, err := st.TOTPLastStep(ctx, cred.CredentialID); err != nil || seen {
		t.Fatalf("a step floor survived factory reset (seen=%v, err=%v)", seen, err)
	}
	if _, err := st.PendingTOTPSecret(ctx, p.PrincipalID); !errors.Is(err, ErrNoPendingTOTP) {
		t.Fatalf("a pending enrollment survived factory reset: %v", err)
	}
}
