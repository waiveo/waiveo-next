package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// The pack-invocation queue: how the host calls INTO an extension it cannot
// dial. The two properties worth pinning are that a row is leased by exactly
// one worker, and that an expired lease resolves according to the ACTION's
// idempotency class rather than to a blanket retry policy.

// clockStore opens a store whose clock the test drives, so lease expiry is a
// decision rather than a sleep.
type testClock struct {
	mu sync.Mutex
	ms int64
}

func (c *testClock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}

func (c *testClock) advance(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += ms
}

func invocationStore(t *testing.T) (*store.Store, *testClock) {
	t.Helper()
	clock := &testClock{ms: 1_752_537_600_000}
	s, err := store.Open(":memory:", clock.now)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, _, err := s.InstallPack(context.Background(), packSpec("acme/menu-board", "1.0.0", 1)); err != nil {
		t.Fatalf("install fixture pack: %v", err)
	}
	return s, clock
}

func enqueue(t *testing.T, s *store.Store, action, idem string) store.PackInvocation {
	t.Helper()
	inv, err := s.EnqueuePackInvocation(context.Background(), store.PackInvocation{
		PackID: "acme/menu-board", Action: action, Idempotency: idem,
		Params: json.RawMessage(`{"n":1}`),
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", action, err)
	}
	return inv
}

// The round trip: queued, leased, answered.
func TestAnInvocationIsQueuedLeasedAndAnswered(t *testing.T) {
	s, _ := invocationStore(t)
	ctx := context.Background()
	queued := enqueue(t, s, "run-backup", store.IdempotencySafeToRetry)
	if queued.State != store.InvocationPending {
		t.Fatalf("state = %q, want pending", queued.State)
	}

	leased, ok, err := s.LeasePackInvocation(ctx, "acme/menu-board", 30_000)
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if leased.InvocationID != queued.InvocationID || leased.State != store.InvocationLeased {
		t.Fatalf("leased = %+v", leased)
	}
	if string(leased.Params) != `{"n":1}` {
		t.Fatalf("params = %s, want the enqueued params", leased.Params)
	}

	done, err := s.CompletePackInvocation(ctx, leased.InvocationID, json.RawMessage(`{"archive":"a.zip"}`), "", "")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.State != store.InvocationSucceeded || !done.Terminal() {
		t.Fatalf("completed = %+v, want succeeded and terminal", done)
	}
}

// An empty queue is not an error. A pack polling an idle host is the common
// case, and an error there would make "nothing to do" indistinguishable from a
// broken queue.
func TestLeasingAnEmptyQueueIsNotAnError(t *testing.T) {
	s, _ := invocationStore(t)
	_, ok, err := s.LeasePackInvocation(context.Background(), "acme/menu-board", 30_000)
	if err != nil || ok {
		t.Fatalf("empty lease: ok=%v err=%v, want false/nil", ok, err)
	}
}

// Exactly one worker gets a given row, under real concurrency. Two packs
// polling at once — a swap in progress, say — must not both run the action.
func TestOneInvocationIsLeasedByExactlyOneWorker(t *testing.T) {
	s, _ := invocationStore(t)
	enqueue(t, s, "run-backup", store.IdempotencySafeToRetry)

	const racers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	claims := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.LeasePackInvocation(context.Background(), "acme/menu-board", 30_000)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && ok {
				claims++
			}
		}()
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("%d of %d workers leased the same invocation; want exactly 1", claims, racers)
	}
}

// MAN-103, the half that protects the operator: an expired lease on a
// SAFE-TO-RETRY action returns to pending so another worker can take it.
func TestAnExpiredLeaseOnARetryableActionIsOfferedAgain(t *testing.T) {
	s, clock := invocationStore(t)
	ctx := context.Background()
	enqueue(t, s, "run-backup", store.IdempotencySafeToRetry)

	first, ok, err := s.LeasePackInvocation(ctx, "acme/menu-board", 1_000)
	if err != nil || !ok {
		t.Fatalf("first lease: ok=%v err=%v", ok, err)
	}
	clock.advance(5_000) // the holder died mid-handler

	again, ok, err := s.LeasePackInvocation(ctx, "acme/menu-board", 1_000)
	if err != nil || !ok {
		t.Fatalf("re-lease after expiry: ok=%v err=%v", ok, err)
	}
	if again.InvocationID != first.InvocationID {
		t.Fatalf("re-leased %s, want the expired %s", again.InvocationID, first.InvocationID)
	}
}

// MAN-103, the half that protects the WORLD: an expired lease on a
// NOT-IDEMPOTENT action FAILS rather than being re-offered. Re-offering it would
// be exactly the automatic replay MAN-103 forbids, and it is how "send the
// invoice" becomes "send the invoice twice".
func TestAnExpiredLeaseOnANotIdempotentActionFailsAndIsNotReoffered(t *testing.T) {
	s, clock := invocationStore(t)
	ctx := context.Background()
	queued := enqueue(t, s, "charge-card", store.IdempotencyNotIdempotent)

	if _, ok, err := s.LeasePackInvocation(ctx, "acme/menu-board", 1_000); err != nil || !ok {
		t.Fatalf("first lease: ok=%v err=%v", ok, err)
	}
	clock.advance(5_000)

	if _, ok, err := s.LeasePackInvocation(ctx, "acme/menu-board", 1_000); err != nil || ok {
		t.Fatalf("a not-idempotent invocation was RE-OFFERED after its lease expired (ok=%v err=%v)", ok, err)
	}
	got, err := s.GetPackInvocation(ctx, queued.InvocationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != store.InvocationFailed || got.ErrorCode != "LEASE_EXPIRED" {
		t.Fatalf("expired not-idempotent invocation = state %q code %q, want failed/LEASE_EXPIRED", got.State, got.ErrorCode)
	}
	if got.ErrorMessage == "" {
		t.Fatal("the failure carries no explanation; an operator needs to know the action's fate is UNKNOWN, not that it did not run")
	}
}

// A result arriving after the lease expired is refused. Accepting it would let a
// not-idempotent invocation that was failed at expiry flip to succeeded, erasing
// the very uncertainty the failure recorded.
func TestAResultPostedAfterTheLeaseExpiredIsRefused(t *testing.T) {
	s, clock := invocationStore(t)
	ctx := context.Background()
	queued := enqueue(t, s, "charge-card", store.IdempotencyNotIdempotent)

	leased, ok, err := s.LeasePackInvocation(ctx, "acme/menu-board", 1_000)
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	clock.advance(5_000)
	// Force the sweep the way a poll would.
	if _, _, err := s.LeasePackInvocation(ctx, "acme/menu-board", 1_000); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := s.CompletePackInvocation(ctx, leased.InvocationID, json.RawMessage(`{"ok":true}`), "", ""); !errors.Is(err, store.ErrInvocationNotLeased) {
		t.Fatalf("late result = %v, want ErrInvocationNotLeased", err)
	}
	got, _ := s.GetPackInvocation(ctx, queued.InvocationID)
	if got.State != store.InvocationFailed {
		t.Fatalf("a late result flipped the state to %q; the expiry verdict must stand", got.State)
	}
}

// A pack reporting failure records the failure, not a success with an error
// attached — a caller branching on state must not have to also inspect a code.
func TestAPackReportedFailureIsRecordedAsFailed(t *testing.T) {
	s, _ := invocationStore(t)
	ctx := context.Background()
	enqueue(t, s, "run-backup", store.IdempotencySafeToRetry)
	leased, _, _ := s.LeasePackInvocation(ctx, "acme/menu-board", 30_000)

	done, err := s.CompletePackInvocation(ctx, leased.InvocationID, nil, "DISK_FULL", "no space for the archive")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.State != store.InvocationFailed || done.ErrorCode != "DISK_FULL" {
		t.Fatalf("failed completion = %+v", done)
	}
}

// Work queued for a pack that is not installed is refused. The row would be one
// nothing can ever lease, which reads to an operator as an extension ignoring
// its work rather than as a caller naming a pack that is not there.
func TestQueuingForAnUninstalledPackIsRefused(t *testing.T) {
	s, _ := invocationStore(t)
	_, err := s.EnqueuePackInvocation(context.Background(), store.PackInvocation{
		PackID: "nobody/here", Action: "run", Idempotency: store.IdempotencySafeToRetry,
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("enqueue for an uninstalled pack = %v, want ErrNotFound", err)
	}
}

// An invocation must state its idempotency class (MAN-103). Defaulting it would
// silently pick one of the two expiry behaviours on the author's behalf, and the
// wrong default is the one that replays a payment.
func TestAnInvocationWithoutAnIdempotencyClassIsRefused(t *testing.T) {
	s, _ := invocationStore(t)
	_, err := s.EnqueuePackInvocation(context.Background(), store.PackInvocation{
		PackID: "acme/menu-board", Action: "run",
	})
	if err == nil {
		t.Fatal("an invocation with no idempotency class was accepted")
	}
}

// Invocations are leased oldest-first, so a queue cannot starve its own head.
func TestInvocationsAreLeasedOldestFirst(t *testing.T) {
	s, clock := invocationStore(t)
	ctx := context.Background()
	first := enqueue(t, s, "first", store.IdempotencySafeToRetry)
	clock.advance(10)
	enqueue(t, s, "second", store.IdempotencySafeToRetry)

	got, ok, err := s.LeasePackInvocation(ctx, "acme/menu-board", 30_000)
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if got.InvocationID != first.InvocationID {
		t.Fatalf("leased %s (%s), want the oldest %s", got.InvocationID, got.Action, first.InvocationID)
	}
}
