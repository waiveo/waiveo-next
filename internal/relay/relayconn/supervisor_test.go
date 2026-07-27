package relayconn

import (
	"errors"
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
