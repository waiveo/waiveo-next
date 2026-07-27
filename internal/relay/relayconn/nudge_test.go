package relayconn

import (
	"sync"
	"testing"
	"time"
)

// newCellOnlyClient builds a Client with just the nudge cell wired — the
// white-box surface these tests exercise (the full transport rides the
// package's e2e proof in internal/feeder/relayconn).
func newCellOnlyClient() *Client {
	return &Client{
		done:    make(chan struct{}),
		nudgeCh: make(chan struct{}, 1),
	}
}

// TestNudgeCellCoalescesToLatest: three nudges folded in before the
// dispatcher wakes deliver exactly ONE callback, carrying the highest
// generation — REL-057's coalescible latest-wins delivery, with no silent
// loss of the latest announced generation.
func TestNudgeCellCoalescesToLatest(t *testing.T) {
	c := newCellOnlyClient()

	c.noteGeneration(41)
	c.noteGeneration(43)
	c.noteGeneration(42) // out-of-order arrival must not regress the slot

	var mu sync.Mutex
	var got []int64
	done := make(chan struct{})
	go func() {
		c.dispatchGenerations(func(gen int64) {
			mu.Lock()
			got = append(got, gen)
			mu.Unlock()
		})
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	close(c.done)
	<-done

	if len(got) != 1 || got[0] != 43 {
		t.Fatalf("dispatched generations = %v, want exactly [43] (coalesced latest-wins)", got)
	}
}

// TestNudgeArrivingDuringCallbackIsDelivered: a nudge folded in WHILE the
// callback runs is delivered next — coalescing never drops the newest
// generation on the floor.
func TestNudgeArrivingDuringCallbackIsDelivered(t *testing.T) {
	c := newCellOnlyClient()

	firstEntered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var got []int64

	go c.dispatchGenerations(func(gen int64) {
		mu.Lock()
		got = append(got, gen)
		n := len(got)
		mu.Unlock()
		if n == 1 {
			close(firstEntered)
			<-release // hold the callback so the next nudge lands mid-flight
		}
	})

	c.noteGeneration(10)
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first nudge never dispatched")
	}

	c.noteGeneration(11) // lands while the callback for 10 is still running
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(c.done)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != 10 || got[1] != 11 {
		t.Fatalf("dispatched generations = %v, want [10 11]", got)
	}
}
