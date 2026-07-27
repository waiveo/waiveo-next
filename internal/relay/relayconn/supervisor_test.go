package relayconn

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefusalIsRecoverable pins the taxonomy split the supervisor retries
// under — the same classification cmd/waiveo-relay's hellorecovery
// established, carried onto this transport's *Refusal type.
func TestRefusalIsRecoverable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"transport failure", errors.New("dial tcp: connection refused"), true},
		{"channel binding invalid", &Refusal{Code: "CHANNEL_BINDING_INVALID"}, true},
		{"relay identity mismatch", &Refusal{Code: "RELAY_IDENTITY_MISMATCH"}, true},
		{"protocol version unsupported", &Refusal{Code: "PROTOCOL_VERSION_UNSUPPORTED"}, false},
		{"cert revoked", &Refusal{Code: "CERT_REVOKED"}, false},
	}
	for _, tc := range cases {
		if got := RefusalIsRecoverable(tc.err); got != tc.want {
			t.Errorf("%s: RefusalIsRecoverable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSupervisorStopsOnPermanentRefusal: a PROTOCOL_VERSION_UNSUPPORTED
// refusal ends supervision after exactly ONE attempt — blind retry against
// an unchanged software pairing would repeat the identical refusal forever,
// so that posture ("operator must intervene") must not regress.
func TestSupervisorStopsOnPermanentRefusal(t *testing.T) {
	var attempts atomic.Int64
	var permanent atomic.Pointer[Refusal]

	s := StartSupervisor(SupervisorConfig{
		Connect: func() (*Client, error) {
			attempts.Add(1)
			return nil, &Refusal{Code: "PROTOCOL_VERSION_UNSUPPORTED", Message: "no shared major"}
		},
		OnPermanentRefusal: func(r *Refusal) { permanent.Store(r) },
		InitialBackoff:     time.Millisecond,
	})

	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("supervision never ended on a permanent refusal")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("connect attempts = %d, want exactly 1 (no retry on a permanent refusal)", got)
	}
	r := permanent.Load()
	if r == nil || r.Code != "PROTOCOL_VERSION_UNSUPPORTED" {
		t.Fatalf("OnPermanentRefusal got %+v, want the PROTOCOL_VERSION_UNSUPPORTED refusal", r)
	}
}

// TestSupervisorRetriesRecoverableFailures: transport failures and the two
// retryable refusal codes are re-dialed forever under growing backoff —
// never a give-up (the pre-hellorecovery permanent-offline field defect).
func TestSupervisorRetriesRecoverableFailures(t *testing.T) {
	errs := []error{
		errors.New("dial tcp: connection refused"),
		&Refusal{Code: "CHANNEL_BINDING_INVALID"},
		&Refusal{Code: "RELAY_IDENTITY_MISMATCH"},
	}
	var attempts atomic.Int64
	start := time.Now()

	s := StartSupervisor(SupervisorConfig{
		Connect: func() (*Client, error) {
			n := attempts.Add(1)
			return nil, errs[(n-1)%int64(len(errs))]
		},
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})

	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := attempts.Load(); got < 5 {
		t.Fatalf("connect attempts after 5s = %d, want >= 5 (retry forever on recoverable failures)", got)
	}
	// Backoff sanity: 4 retry delays of >= 5, 10, 20, 25ms (jitter floor is
	// half of 10, 20, 40, 50ms) must have elapsed — the loop is not a hot
	// spin.
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("5 attempts completed in %v — backoff is not being applied", elapsed)
	}

	s.Stop()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never ended supervision")
	}
}

// waitForCount polls until fn() >= want or the deadline passes — the same
// bounded-budget posture the retry tests above use (no wall-clock sleeps
// standing in for synchronization).
func waitForCount(t *testing.T, fn func() int64, want int64, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for int64(fn()) < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := fn(); int64(got) < want {
		t.Fatalf("%s: got %d, want >= %d", msg, got, want)
	}
}

// TestSupervisorRenewsWhenDueBeforeDialing: the proactive-renewal predicate
// is evaluated at the top of every loop iteration, BEFORE the connection
// attempt — so a relay whose leaf entered the renewal window (or already
// expired, REL-015/020) renews first and its very next dial presents the
// fresh leaf, deterministically, without ever depending on the peer's
// handshake error.
func TestSupervisorRenewsWhenDueBeforeDialing(t *testing.T) {
	var mu sync.Mutex
	var order []string
	due := true

	s := StartSupervisor(SupervisorConfig{
		NeedsRenewal: func() bool {
			mu.Lock()
			defer mu.Unlock()
			return due
		},
		Renew: func() error {
			mu.Lock()
			order = append(order, "renew")
			due = false // renewed: the predicate stops firing
			mu.Unlock()
			return nil
		},
		Connect: func() (*Client, error) {
			mu.Lock()
			order = append(order, "connect")
			mu.Unlock()
			return nil, errors.New("dial tcp: connection refused")
		},
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	})
	defer stopAndWait(t, s)

	waitForCount(t, func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return int64(len(order))
	}, 3, "supervisor loop iterations")

	mu.Lock()
	defer mu.Unlock()
	if order[0] != "renew" || order[1] != "connect" {
		t.Fatalf("order = %v, want renewal BEFORE the first connection attempt", order[:2])
	}
	for _, step := range order[1:] {
		if step == "renew" {
			t.Fatalf("order = %v, want no further renewals once the predicate reports not-due", order)
		}
	}
}

// TestSupervisorRenewalFailureDoesNotBlockConnect: a failing Renew is
// retried on the next evaluation and NEVER blocks dialing — the current
// leaf may still be perfectly valid, and the renewal window is deliberately
// wide enough that one failed attempt costs nothing (REL-015).
func TestSupervisorRenewalFailureDoesNotBlockConnect(t *testing.T) {
	var renews, connects atomic.Int64

	s := StartSupervisor(SupervisorConfig{
		NeedsRenewal: func() bool { return true },
		Renew: func() error {
			renews.Add(1)
			return errors.New("feeder unreachable")
		},
		Connect: func() (*Client, error) {
			connects.Add(1)
			return nil, errors.New("dial tcp: connection refused")
		},
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	})
	defer stopAndWait(t, s)

	waitForCount(t, connects.Load, 3, "connect attempts despite failing renewals")
	waitForCount(t, renews.Load, 3, "renewal retries on the loop cadence")
}

// TestSupervisorExpiredLeafHandshakeTriggersRenewDespitePredicate: the
// belt-and-suspenders classification for clock skew. The pre-dial predicate
// says "not due" (a skewed relay clock believes the leaf is valid), but the
// peer's TLS stack refuses the handshake with the empirically-pinned
// "remote error: tls: expired certificate" — a transport-level error, not a
// typed *Refusal. The supervisor must trigger the SAME Renew rather than
// retrying the identical doomed handshake forever (the known
// retry-forever-at-MaxBackoff gap).
func TestSupervisorExpiredLeafHandshakeTriggersRenewDespitePredicate(t *testing.T) {
	var renews atomic.Int64

	s := StartSupervisor(SupervisorConfig{
		NeedsRenewal: func() bool { return false }, // skewed clock: never "due"
		Renew: func() error {
			renews.Add(1)
			return nil
		},
		Connect: func() (*Client, error) {
			return nil, errors.New("relayconn: Dial: remote error: tls: expired certificate")
		},
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	})
	defer stopAndWait(t, s)

	waitForCount(t, renews.Load, 1, "renewal triggered by the expired-certificate handshake refusal")
}

// TestSupervisorTypedRefusalNeverTriggersRenew: a typed CERT_REVOKED refusal
// is an operator matter (Error taxonomy: "no — re-enroll"), never a renewal
// trigger — renewal must not be a way to churn past revocation.
func TestSupervisorTypedRefusalNeverTriggersRenew(t *testing.T) {
	var renews atomic.Int64

	s := StartSupervisor(SupervisorConfig{
		NeedsRenewal: func() bool { return false },
		Renew: func() error {
			renews.Add(1)
			return nil
		},
		Connect: func() (*Client, error) {
			return nil, &Refusal{Code: "CERT_REVOKED", Message: "revoked (expired certificate wording absent)"}
		},
		InitialBackoff: time.Millisecond,
	})

	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("supervision never ended on the permanent CERT_REVOKED refusal")
	}
	if got := renews.Load(); got != 0 {
		t.Fatalf("renewals = %d, want 0 — a typed refusal must never trigger renewal", got)
	}
}

// stopAndWait stops s and waits for supervision to fully end, bounding the
// wait so a hung Stop fails the test instead of the suite.
func stopAndWait(t *testing.T, s *Supervisor) {
	t.Helper()
	s.Stop()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never ended supervision")
	}
}
