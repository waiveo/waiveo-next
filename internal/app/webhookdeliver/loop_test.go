package webhookdeliver_test

// loop_test.go covers the Loop's lifecycle edges. The Loop's actual scheduling
// behaviour — that a registered endpoint receives events, and that an owed
// backlog drains rather than trickling — is exercised end to end against the
// real feeder wiring in cmd/waiveo-feeder, because that is where the bug this
// type exists to fix lived: every unit test of the deliverer passed while
// nothing in the binary called it.
//
// What is left to pin here is the edge a whole-stack test cannot reach without
// breaking the binary: a Loop that was never started must still shut down.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/webhookdeliver"
)

// TestLoopShutdownBeforeStartReturns pins that Shutdown on a Loop that never
// started returns rather than waiting for a run goroutine that does not exist.
//
// It is not a hypothetical ordering. main builds the loop, then keeps booting;
// anything between construction and Start that calls log.Fatalf, and any future
// wiring that constructs the loop and starts it conditionally, reaches the
// shutdown path with a Loop that never ran. A Shutdown that blocked there would
// turn a clean SIGTERM into a hang the operator has to escalate to SIGKILL —
// which is the one thing the graceful path exists to avoid.
//
// The deadline below is a FAILURE bound, not a wait: a correct Shutdown returns
// before the select is even reached in practice.
func TestLoopShutdownBeforeStartReturns(t *testing.T) {
	e := newEnv(t)
	e.seedTree()

	loop, err := webhookdeliver.NewLoop(webhookdeliver.Config{
		Store:   e.store,
		Log:     e.log,
		HTTP:    &http.Client{},
		NowMs:   e.clock.now,
		NewID:   e.nextID,
		Secrets: e.secrets,
	}, time.Hour)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	returned := make(chan error, 1)
	go func() { returned <- loop.Shutdown(context.Background()) }()

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Shutdown of an unstarted Loop = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked on a Loop that was never started")
	}

	// Shutdown is idempotent, and a Loop shut down before it ever ran must not
	// be startable afterwards — a second Shutdown must not block either.
	loop.Start()
	returned = make(chan error, 1)
	go func() { returned <- loop.Shutdown(context.Background()) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("second Shutdown = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start after Shutdown left a run goroutine the second Shutdown then waited on")
	}
}

// TestNewLoopValidatesLikeNew pins that the Loop constructor refuses the same
// incomplete configuration the Deliverer's own constructor refuses, rather than
// returning a Loop that would fail on its first pass. A deployment discovers a
// missing collaborator at boot, where it can still be fixed, not at the moment
// an operator's first webhook was owed.
func TestNewLoopValidatesLikeNew(t *testing.T) {
	e := newEnv(t)
	if _, err := webhookdeliver.NewLoop(webhookdeliver.Config{
		Store: e.store,
		Log:   e.log,
		HTTP:  &http.Client{},
		NowMs: e.clock.now,
		NewID: e.nextID,
		// Secrets deliberately absent: an endpoint is never delivered to
		// unsigned, so a Loop with no secret opener is not a Loop.
	}, time.Hour); err == nil {
		t.Fatal("NewLoop accepted a Config with no secret opener")
	}
}
