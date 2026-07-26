package main

import (
	"context"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
)

// TestHelloRefusalIsRecoverable pins the taxonomy-driven classification
// helloRefusalIsRecoverable's own doc describes: CHANNEL_BINDING_INVALID and
// RELAY_IDENTITY_MISMATCH (the two refusals relay/1's Error taxonomy
// annotates "reconnect and retry the handshake") and any non-decisive
// transport failure are recoverable; PROTOCOL_VERSION_UNSUPPORTED (no such
// guidance in the taxonomy) is not; nil is not (nothing to recover from).
func TestHelloRefusalIsRecoverable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"channel binding invalid", &hello.RefusedError{Status: 403, Code: "CHANNEL_BINDING_INVALID"}, true},
		{"relay identity mismatch", &hello.RefusedError{Status: 403, Code: "RELAY_IDENTITY_MISMATCH"}, true},
		{"protocol version unsupported", &hello.RefusedError{Status: 409, Code: "PROTOCOL_VERSION_UNSUPPORTED"}, false},
		{"transport failure", errConnRefusedStub{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := helloRefusalIsRecoverable(tc.err); got != tc.want {
				t.Errorf("helloRefusalIsRecoverable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type errConnRefusedStub struct{}

func (errConnRefusedStub) Error() string { return "dial tcp: connection refused" }

// sendTick delivers one tick to ticks, failing the test (rather than hanging
// forever) if nothing receives it within the timeout — the observable
// signature of a helloRecoveryLoop that already stopped consuming ticks
// (e.g. the permanent-degradation bug this test guards against: attempt()
// returning true, and therefore the loop returning, after the very first
// refusal instead of retrying).
func sendTick(t *testing.T, ticks chan<- time.Time) {
	t.Helper()
	select {
	case ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("send on ticks timed out — helloRecoveryLoop is no longer receiving (it stopped retrying)")
	}
}

// TestHelloRecoveryLoopKeepsRetryingAfterChannelBindingInvalid is the
// recovery-half regression test: a relay whose hello was refused
// CHANNEL_BINDING_INVALID must NOT degrade permanently — it must keep
// retrying the handshake on every tick until the app peer finally accepts
// it, at which point onAccepted fires exactly once. Before this fix, a
// refused hello simply left the live desired-state loop never started for
// the rest of the process's life (main.go's own prior "until re-enrolled"
// behavior) — nothing here ever tried again.
func TestHelloRecoveryLoopKeepsRetryingAfterChannelBindingInvalid(t *testing.T) {
	attempts := 0
	var acceptedSite hello.SiteBinding
	accepted := false

	r := &helloRecoverer{
		hello: func() (hello.SiteBinding, error) {
			attempts++
			if attempts < 3 {
				return hello.SiteBinding{}, &hello.RefusedError{Status: 403, Code: "CHANNEL_BINDING_INVALID", Title: "Channel Binding Invalid"}
			}
			return hello.SiteBinding{TZ: "America/Chicago"}, nil
		},
		onAccepted: func(site hello.SiteBinding) {
			accepted = true
			acceptedSite = site
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		helloRecoveryLoop(ctx, ticks, r)
		close(done)
	}()

	// Two refused attempts: the loop must still be running afterward — a
	// permanently-degrading implementation would have stopped after the
	// first refusal, and sendTick's own timeout turns that into a fast
	// failure here rather than a hang.
	sendTick(t, ticks)
	sendTick(t, ticks)
	if accepted {
		t.Fatal("onAccepted fired before hello ever succeeded")
	}
	select {
	case <-done:
		t.Fatal("helloRecoveryLoop returned after a recoverable refusal — it must keep retrying, not degrade permanently")
	default:
	}

	// Third attempt succeeds.
	sendTick(t, ticks)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("helloRecoveryLoop did not stop after hello was accepted")
	}

	if !accepted {
		t.Fatal("onAccepted was never invoked despite a successful hello attempt")
	}
	if acceptedSite.TZ != "America/Chicago" {
		t.Errorf("onAccepted received site %+v, want the negotiated site_binding from the accepted hello", acceptedSite)
	}
	if attempts != 3 {
		t.Errorf("hello attempted %d time(s), want exactly 3 (two refusals + one acceptance)", attempts)
	}
}

// TestHelloRecoveryLoopStopsOnNonRecoverableRefusal: a PROTOCOL_VERSION_UNSUPPORTED
// refusal gets no "reconnect and retry" guidance from relay/1's own Error
// taxonomy, so the loop must stop attempting rather than retry forever
// against a refusal no amount of reconnecting can change.
func TestHelloRecoveryLoopStopsOnNonRecoverableRefusal(t *testing.T) {
	attempts := 0
	accepted := false
	r := &helloRecoverer{
		hello: func() (hello.SiteBinding, error) {
			attempts++
			return hello.SiteBinding{}, &hello.RefusedError{Status: 409, Code: "PROTOCOL_VERSION_UNSUPPORTED", Title: "Protocol Version Unsupported"}
		},
		onAccepted: func(hello.SiteBinding) { accepted = true },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		helloRecoveryLoop(ctx, ticks, r)
		close(done)
	}()

	sendTick(t, ticks)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("helloRecoveryLoop did not stop after a non-recoverable refusal")
	}
	if accepted {
		t.Error("onAccepted fired for a non-recoverable refusal")
	}
	if attempts != 1 {
		t.Errorf("hello attempted %d time(s), want exactly 1 (no retry for a non-recoverable refusal)", attempts)
	}
}

// TestHelloRecoveryLoopStopsOnContextCancel confirms the loop shape mirrors
// rePullLoop's own: cancelling ctx stops it even mid-retry, so process
// shutdown never leaves this goroutine running.
func TestHelloRecoveryLoopStopsOnContextCancel(t *testing.T) {
	r := &helloRecoverer{
		hello: func() (hello.SiteBinding, error) {
			return hello.SiteBinding{}, &hello.RefusedError{Status: 403, Code: "CHANNEL_BINDING_INVALID"}
		},
		onAccepted: func(hello.SiteBinding) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		helloRecoveryLoop(ctx, ticks, r)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("helloRecoveryLoop did not return after context cancellation")
	}
}
