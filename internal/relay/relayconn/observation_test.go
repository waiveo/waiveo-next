package relayconn

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The supervisor-observation tests. Until HV-22 this loop was mute: it
// handled an app-peer restart correctly and reported none of it, so a relay
// cut off from its app peer for 2h33m produced no line about the connection
// at all. These pin that every state change the loop makes reaches its
// owner.
//
// Every Client here is transport-backed and built on the TEST goroutine.
// Both matter: the supervisor calls Connect on its own goroutine (so a
// helper that t.Fatalf'd there would be illegal), and it calls Close on the
// live client at Stop, which a Client with a nil *websocket.Conn cannot
// survive. Real transports also keep these tests honest — Close and fail
// actually run.

// idleClient returns a client connected to a peer that is up and READING:
// alive until something fails it, and able to answer the RFC 6455 close
// handshake so a graceful Close (which the supervisor performs on Stop)
// returns promptly instead of waiting out the library's close timeout.
func idleClient(t *testing.T) *Client {
	t.Helper()
	return newLiveClient(t, wsEcho(t, func(_ context.Context, ws *websocket.Conn) {
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				return
			}
		}
	}))
}

// deadClientFor returns a client already failed with cause.
func deadClientFor(t *testing.T, cause error) *Client {
	t.Helper()
	c := idleClient(t)
	c.fail(sideRead, cause)
	return c
}

// handOut returns a Connect closure yielding the given clients in order,
// then failing every later attempt with a transport error.
func handOut(clients ...*Client) func() (*Client, error) {
	var mu sync.Mutex
	n := 0
	return func() (*Client, error) {
		mu.Lock()
		defer mu.Unlock()
		i := n
		n++
		if i < len(clients) {
			return clients[i], nil
		}
		return nil, errors.New("dial tcp: connection refused")
	}
}

// TestOnDisconnectedReportsTheDeathBeforeRedialling: the owner learns a
// connection died BEFORE the loop tries again, with the client's own cause
// and how long it had been up.
//
// Ordering is the assertion, not a detail. The owner's first job on this
// callback is to drop its own reference to the dead client — HV-22's
// connHolder held one for two and a half hours and kept writing to it — and
// a report arriving after the next connect attempt would leave a window in
// which the corpse is still the process's live connection.
func TestOnDisconnectedReportsTheDeathBeforeRedialling(t *testing.T) {
	cause := errors.New("relayconn: connection died on the read side: EOF")
	dead := deadClientFor(t, cause)

	var mu sync.Mutex
	var order []string
	var gotErr error
	var gotUptime time.Duration
	inner := handOut(dead)

	s := StartSupervisor(SupervisorConfig{
		Connect: func() (*Client, error) {
			mu.Lock()
			order = append(order, "connect")
			mu.Unlock()
			return inner()
		},
		OnDisconnected: func(err error, connectedFor time.Duration) {
			mu.Lock()
			order = append(order, "disconnected")
			gotErr = err
			gotUptime = connectedFor
			mu.Unlock()
		},
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	})
	defer stopAndWait(t, s)

	waitForCount(t, func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return int64(len(order))
	}, 3, "supervisor lifecycle events")

	mu.Lock()
	defer mu.Unlock()
	if order[0] != "connect" || order[1] != "disconnected" || order[2] != "connect" {
		t.Fatalf("order = %v, want connect -> disconnected -> connect (the death is reported BEFORE the redial)", order[:3])
	}
	if !errors.Is(gotErr, cause) {
		t.Fatalf("OnDisconnected err = %v, want the client's own Err (%v)", gotErr, cause)
	}
	if gotUptime < 0 {
		t.Fatalf("OnDisconnected connectedFor = %v, want a non-negative duration", gotUptime)
	}
}

// TestOnDisconnectedCarriesTheSideLabel is the seam between the two halves
// of this fix: Client.fail labels the cause with the half that noticed, and
// the supervisor must pass that through unmodified so the binary can print
// it. A supervisor that summarised the error would discard the one detail
// distinguishing "the peer stopped answering" from "we could not get a
// frame out".
func TestOnDisconnectedCarriesTheSideLabel(t *testing.T) {
	c := idleClient(t)
	c.fail(sideWrite, errors.New("broken pipe"))

	var mu sync.Mutex
	var got error
	seen := make(chan struct{})
	var once sync.Once
	inner := handOut(c)

	s := StartSupervisor(SupervisorConfig{
		Connect: inner,
		OnDisconnected: func(err error, _ time.Duration) {
			mu.Lock()
			got = err
			mu.Unlock()
			once.Do(func() { close(seen) })
		},
		InitialBackoff: time.Millisecond,
		MaxBackoff:     3 * time.Millisecond,
	})
	defer stopAndWait(t, s)

	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("OnDisconnected never fired")
	}
	mu.Lock()
	defer mu.Unlock()
	if got == nil || !strings.Contains(got.Error(), "on the write side") {
		t.Fatalf("OnDisconnected err = %v, want the side label Client.fail attached", got)
	}
}

// TestOnDisconnectedDoesNotFireOnStop: an orderly shutdown is not a
// connection loss. Reporting one would put a "connection LOST" line in
// every clean relay shutdown, and a warning that cries wolf at teardown is
// one an operator learns to skip — the exact failure this reporting change
// exists to avoid.
func TestOnDisconnectedDoesNotFireOnStop(t *testing.T) {
	live := idleClient(t)

	var mu sync.Mutex
	disconnects := 0
	connected := make(chan struct{})
	var once sync.Once
	inner := handOut(live)

	s := StartSupervisor(SupervisorConfig{
		Connect: func() (*Client, error) {
			c, err := inner()
			once.Do(func() { close(connected) })
			return c, err
		},
		OnDisconnected: func(error, time.Duration) {
			mu.Lock()
			disconnects++
			mu.Unlock()
		},
		InitialBackoff: time.Millisecond,
	})

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor never connected")
	}
	stopAndWait(t, s)

	mu.Lock()
	defer mu.Unlock()
	if disconnects != 0 {
		t.Fatalf("OnDisconnected fired %d time(s) on an orderly Stop, want 0", disconnects)
	}
}

// TestOnConnectFailedCountsConsecutiveFailures: the count an owner thins its
// reporting by must count CONSECUTIVE failures and RESET on a success.
// Without the reset, a relay that flaps for a week reports its hundredth
// brief outage as if it were one long one, and the thinning then silences
// exactly the outages worth reading.
func TestOnConnectFailedCountsConsecutiveFailures(t *testing.T) {
	// Fail three times, then hand out a client that dies at once, then fail
	// again: the counter must restart at 1 after the (brief) success.
	dead := deadClientFor(t, errors.New("died"))

	var mu sync.Mutex
	var counts []int
	var delays []time.Duration
	attempt := 0

	s := StartSupervisor(SupervisorConfig{
		Connect: func() (*Client, error) {
			mu.Lock()
			attempt++
			n := attempt
			mu.Unlock()
			if n == 4 {
				return dead, nil
			}
			return nil, errors.New("dial tcp: connection refused")
		},
		OnConnectFailed: func(err error, consecutive int, retryIn time.Duration) {
			mu.Lock()
			counts = append(counts, consecutive)
			delays = append(delays, retryIn)
			mu.Unlock()
		},
		InitialBackoff: time.Millisecond,
		MaxBackoff:     3 * time.Millisecond,
	})
	defer stopAndWait(t, s)

	waitForCount(t, func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return int64(len(counts))
	}, 5, "reported connect failures")

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 3, 1, 2}
	for i, w := range want {
		if counts[i] != w {
			t.Fatalf("consecutive counts = %v, want %v (a success must reset the counter)", counts[:len(want)], want)
		}
	}
	for i, d := range delays[:len(want)] {
		if d <= 0 {
			t.Fatalf("retryIn[%d] = %v, want the positive delay actually slept", i, d)
		}
	}
}

// TestOnConnectFailedReportsTheDelayActuallySlept: retryIn must be the
// JITTERED value the loop sleeps, not the un-jittered ladder rung. An owner
// printing "retrying in 30s" while the loop waits 17s is telling an
// operator watching a log a thing that will not happen.
func TestOnConnectFailedReportsTheDelayActuallySlept(t *testing.T) {
	firstAt := make(chan time.Time, 1)
	secondAt := make(chan time.Time, 1)
	var mu sync.Mutex
	var reported time.Duration
	attempt := 0

	s := StartSupervisor(SupervisorConfig{
		Connect: func() (*Client, error) {
			mu.Lock()
			attempt++
			n := attempt
			mu.Unlock()
			switch n {
			case 1:
				firstAt <- time.Now()
			case 2:
				secondAt <- time.Now()
			}
			return nil, errors.New("dial tcp: connection refused")
		},
		OnConnectFailed: func(_ error, consecutive int, retryIn time.Duration) {
			if consecutive == 1 {
				mu.Lock()
				reported = retryIn
				mu.Unlock()
			}
		},
		// Large enough that jitter is measurable against scheduler noise.
		InitialBackoff: 200 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
	})
	defer stopAndWait(t, s)

	var t1, t2 time.Time
	select {
	case t1 = <-firstAt:
	case <-time.After(5 * time.Second):
		t.Fatal("no first attempt")
	}
	select {
	case t2 = <-secondAt:
	case <-time.After(5 * time.Second):
		t.Fatal("no second attempt")
	}

	mu.Lock()
	got := reported
	mu.Unlock()
	if got <= 0 {
		t.Fatalf("reported retryIn = %v, want positive", got)
	}
	// The un-jittered rung is exactly InitialBackoff; jitter spreads it over
	// [100ms, 300ms). Asserting the report tracks the OBSERVED gap is what
	// catches a report of the rung instead of the jittered value.
	actual := t2.Sub(t1)
	if actual < got-50*time.Millisecond || actual > got+500*time.Millisecond {
		t.Fatalf("reported retryIn %v but the loop actually waited %v — the report is not the delay slept", got, actual)
	}
}

// TestPermanentRefusalDoesNotReportAsARetryableFailure: a refusal that ends
// supervision goes to OnPermanentRefusal and must NOT also arrive as "we
// will retry", which would tell an operator to wait for a recovery that is
// never coming.
func TestPermanentRefusalDoesNotReportAsARetryableFailure(t *testing.T) {
	var mu sync.Mutex
	retryable, permanent := 0, 0

	s := StartSupervisor(SupervisorConfig{
		Connect: func() (*Client, error) {
			return nil, &Refusal{Code: "PROTOCOL_VERSION_UNSUPPORTED"}
		},
		OnConnectFailed: func(error, int, time.Duration) {
			mu.Lock()
			retryable++
			mu.Unlock()
		},
		OnPermanentRefusal: func(*Refusal) {
			mu.Lock()
			permanent++
			mu.Unlock()
		},
		InitialBackoff: time.Millisecond,
	})

	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("supervision never ended")
	}
	mu.Lock()
	defer mu.Unlock()
	if retryable != 0 {
		t.Fatalf("OnConnectFailed fired %d time(s) for a PERMANENT refusal, want 0", retryable)
	}
	if permanent != 1 {
		t.Fatalf("OnPermanentRefusal fired %d time(s), want exactly 1", permanent)
	}
}

// TestSupervisorRunsWithNoObservers: both callbacks are optional and a nil
// one must not panic the loop. Every caller outside cmd/waiveo-relay — the
// conformance drivers, the e2e proof — leaves them nil.
func TestSupervisorRunsWithNoObservers(t *testing.T) {
	dead := deadClientFor(t, errors.New("died"))
	var mu sync.Mutex
	attempts := 0
	inner := handOut(dead)

	s := StartSupervisor(SupervisorConfig{
		Connect: func() (*Client, error) {
			mu.Lock()
			attempts++
			mu.Unlock()
			return inner()
		},
		InitialBackoff: time.Millisecond,
		MaxBackoff:     3 * time.Millisecond,
	})
	defer stopAndWait(t, s)

	waitForCount(t, func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return int64(attempts)
	}, 3, "connect attempts with no observers wired")
}
