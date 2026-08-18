package packhost

import (
	"context"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// restart_test.go covers the failure this policy exists to end, which was
// REPRODUCED on hardware before it was fixed: kill an extension and it never
// came back, the host said nothing, and an operator's queued action sat pending
// forever because nothing was left to lease it. Silent at every layer.

// killPID ends a process the way a crash does: abruptly, with nobody asking.
func killPID(t *testing.T, pid int) {
	t.Helper()
	if err := exec.Command("kill", "-9", strconv.Itoa(pid)).Run(); err != nil {
		t.Fatalf("kill %d: %v", pid, err)
	}
}

func restartSup(f *fakeIdentity, backoff time.Duration, onRestart func(string, string, int, time.Duration, error)) *Supervisor {
	return New(f, Options{
		ReadyTimeout: 5 * time.Second, ReadyPoll: 5 * time.Millisecond, StopGrace: 200 * time.Millisecond,
		RestartBackoff: backoff, RestartBackoffMax: 200 * time.Millisecond,
		RestartHealthy: 50 * time.Millisecond, OnRestart: onRestart,
	})
}

// TestACrashedPackComesBack is the defect, inverted. A pack that dies without
// being asked to must be running again shortly afterwards, with no operator
// action of any kind.
func TestACrashedPackComesBack(t *testing.T) {
	f := newFakeIdentity()
	var mu sync.Mutex
	restarts := 0
	s := restartSup(f, 10*time.Millisecond, func(_, _ string, _ int, _ time.Duration, err error) {
		mu.Lock()
		if err == nil {
			restarts++
		}
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run, err := s.Start(ctx, specFor("acme/crasher", "1.0.0"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := run.PID

	// A crash: the process dies and nobody asked it to.
	killPID(t, first)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := restarts
		mu.Unlock()
		if n > 0 {
			return // it came back
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a crashed pack was never restarted — this is the reproduced defect: it stays dead, silently, and an operator's queued work waits forever")
}

// TestARestartIsAnnounced. Half the original defect was the SILENCE: the host
// logged nothing at all, so an extension being gone looked exactly like an
// extension being idle.
func TestARestartIsAnnounced(t *testing.T) {
	f := newFakeIdentity()
	told := make(chan int, 4)
	s := restartSup(f, 10*time.Millisecond, func(id, _ string, attempt int, _ time.Duration, err error) {
		if err == nil && id == "acme/announce" {
			select {
			case told <- attempt:
			default:
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run, err := s.Start(ctx, specFor("acme/announce", "1.0.0"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	killPID(t, run.PID)

	select {
	case attempt := <-told:
		if attempt < 1 {
			t.Fatalf("attempt = %d, want the restart numbered so a crash-loop is legible in the log", attempt)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("a restart happened (or did not) without telling anyone — silence is what made the original failure invisible")
	}
}

// TestAStoppedPackStaysStopped: Stop is an instruction, not a crash. A
// supervisor that restarted a deliberately stopped pack would make uninstalling
// or disabling one impossible.
func TestAStoppedPackStaysStopped(t *testing.T) {
	f := newFakeIdentity()
	var mu sync.Mutex
	restarts := 0
	s := restartSup(f, 10*time.Millisecond, func(_, _ string, _ int, _ time.Duration, _ error) {
		mu.Lock()
		restarts++
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := s.Start(ctx, specFor("acme/stopper", "1.0.0")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop("acme/stopper"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := restarts
	mu.Unlock()
	if n != 0 {
		t.Fatalf("a deliberately stopped pack was restarted %d time(s) — Stop must mean stop, or nothing could ever be disabled", n)
	}
}

// A zero backoff keeps the old leave-it-dead behaviour, so a caller that wants
// to observe a single exit still can.
func TestRestartIsOptional(t *testing.T) {
	f := newFakeIdentity()
	var mu sync.Mutex
	restarts := 0
	s := restartSup(f, 0, func(_, _ string, _ int, _ time.Duration, _ error) {
		mu.Lock()
		restarts++
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run, err := s.Start(ctx, specFor("acme/optional", "1.0.0"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	killPID(t, run.PID)
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	n := restarts
	mu.Unlock()
	if n != 0 {
		t.Fatalf("restarts = %d with a zero backoff, want none", n)
	}
	if len(s.Exits()) == 0 {
		t.Fatal("the crash was not recorded as an exit")
	}
}
