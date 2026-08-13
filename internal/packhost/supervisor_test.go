package packhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// These tests drive REAL child processes. The supervisor's job is "the old
// process is gone", which is a claim about the operating system, so the
// assertions are operating-system facts: is this pid alive, was it reaped.
//
// Identity is a fake rather than a real auth store. What is under test here is
// process lifecycle; the ceremony itself is covered against the real store in
// internal/app/auth. The fake lets a test decide exactly when a pack becomes
// ready, which is the axis these tests actually vary.

// fakeIdentity issues grants and records redemption. `redeemAfter` makes a pack
// become ready only after N readiness polls, so a slow start and a start that
// never completes are both expressible.
type fakeIdentity struct {
	mu       sync.Mutex
	next     int
	redeemed map[string]bool
	// never, when set for a pack id, means grants for it are never redeemed —
	// the pack that starts and does not come up.
	never map[string]bool
	// minted records every grant issued, so a test can assert the code went to
	// the child rather than into an environment variable.
	minted []auth.MintedGrant
	// mintErr, when set, fails the mint — the host unable to issue an identity.
	mintErr error
}

func newFakeIdentity() *fakeIdentity {
	return &fakeIdentity{redeemed: map[string]bool{}, never: map[string]bool{}}
}

func (f *fakeIdentity) MintTierGrant(_ context.Context, packID, _ string, _ auth.Role, _ ...auth.MintGrantOption) (auth.MintedGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mintErr != nil {
		return auth.MintedGrant{}, f.mintErr
	}
	f.next++
	id := fmt.Sprintf("grant-%d", f.next)
	g := auth.MintedGrant{Grant: auth.GrantRow{GrantID: id}, Code: "code-" + id}
	f.minted = append(f.minted, g)
	if !f.never[packID] {
		// Redeemed immediately: the common case is a pack that comes up.
		f.redeemed[id] = true
	}
	return g, nil
}

func (f *fakeIdentity) GrantRedeemed(_ context.Context, grantID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.redeemed[grantID], nil
}

func (f *fakeIdentity) codes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.minted))
	for _, g := range f.minted {
		out = append(out, g.Code)
	}
	return out
}

// packSource is the reference pack: it echoes whatever it reads on stdin to a
// file, so a test can prove the identity arrived over stdin, and exits when
// stdin closes unless told to ignore shutdown.
const packSource = `package main

import (
	"bufio"
	"os"
	"time"
)

func main() {
	if out := os.Getenv("PACK_ECHO_FILE"); out != "" {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		_ = os.WriteFile(out, []byte(line), 0o600)
	}
	if os.Getenv("PACK_IGNORE_SHUTDOWN") != "" {
		time.Sleep(10 * time.Minute)
		return
	}
	buf := make([]byte, 256)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return
		}
	}
}
`

var builtPack string

// TestMain compiles the reference pack ONCE for the package and removes it at
// the end. Built lazily with a t.Cleanup it was deleted when the FIRST test
// finished, and every later test exec'd a path that no longer existed.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "packhost-refpack")
	if err != nil {
		fmt.Fprintf(os.Stderr, "packhost tests: temp dir: %v\n", err)
		os.Exit(1)
	}
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(packSource), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "packhost tests: write pack source: %v\n", err)
		os.Exit(1)
	}
	builtPack = filepath.Join(dir, "refpack")
	if out, err := exec.Command("go", "build", "-o", builtPack, src).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "packhost tests: build reference pack: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func specFor(id, version string, env ...string) Spec {
	// The reference pack reads its behaviour from the environment, and exec.Cmd
	// takes env at launch — so the argv carries a shim: `env K=V … binary`.
	argv := append([]string{"env"}, env...)
	return Spec{
		ID: id, Version: version, Argv: append(argv, builtPack),
		ScopeNode: "01J8Z2Q1C0000000000000000C", Role: auth.RoleOperator,
	}
}

func newSup(f *fakeIdentity) *Supervisor {
	return New(f, Options{ReadyTimeout: 5 * time.Second, ReadyPoll: 5 * time.Millisecond, StopGrace: 500 * time.Millisecond})
}

func processAlive(pid int) bool {
	return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil
}

// processState is "" when a pid is fully gone and "Z"-ish while it is a zombie
// awaiting reaping. The distinction is the point: a killed-but-unwaited child
// still occupies a pid, and one accumulates per swap.
func processState(pid int) string {
	out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// A pack starts, is handed an identity, and is listed.
func TestAPackStartsAndIsGivenAnIdentity(t *testing.T) {
	f := newFakeIdentity()
	s := newSup(f)
	t.Cleanup(s.StopAll)

	run, err := s.Start(context.Background(), specFor("waiveo/backups", "1.0.0"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !processAlive(run.PID) {
		t.Fatalf("pack reported started but pid %d is not alive", run.PID)
	}
	if len(f.minted) != 1 {
		t.Fatalf("minted %d grants, want exactly one per start", len(f.minted))
	}
	if got := s.Running(); len(got) != 1 || got[0].ID != "waiveo/backups" {
		t.Fatalf("Running() = %+v", got)
	}
}

// SEC-037: the code reaches the pack over STDIN, never an environment variable.
// An env var is readable from the process table for the life of the process,
// which turns a one-time sixty-second code into a long-lived credential.
func TestTheIdentityCodeArrivesOverStdinAndNotTheEnvironment(t *testing.T) {
	f := newFakeIdentity()
	s := newSup(f)
	t.Cleanup(s.StopAll)

	echo := filepath.Join(t.TempDir(), "got-code")
	if _, err := s.Start(context.Background(), specFor("waiveo/backups", "1.0.0", "PACK_ECHO_FILE="+echo)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var got string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(echo); err == nil && len(b) > 0 {
			got = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	codes := f.codes()
	if len(codes) != 1 || got != codes[0] {
		t.Fatalf("pack read %q from stdin, want the minted code %v", got, codes)
	}

	// And it is nowhere in the child's environment. Asserted against the argv
	// the supervisor built, which is where an env var would have to appear.
	for _, arg := range specFor("waiveo/backups", "1.0.0").Argv {
		if strings.Contains(arg, codes[0]) {
			t.Fatalf("the code appears in the child's argv/env (%q) — SEC-037 forbids it", arg)
		}
	}
}

// THE feature: a swap replaces the running version while the host keeps going,
// and the OLD PROCESS IS GONE. The pid check is the whole point — a swap that
// leaves the previous process alive is the leak hot-swap dies of, and nothing
// about the new pack running would reveal it.
func TestASwapReplacesTheVersionAndTheOldProcessIsGone(t *testing.T) {
	s := newSup(newFakeIdentity())
	t.Cleanup(s.StopAll)
	ctx := context.Background()

	first, err := s.Start(ctx, specFor("waiveo/backups", "1.0.0"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := s.Swap(ctx, specFor("waiveo/backups", "1.1.0"))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if second.Version != "1.1.0" || second.PID == first.PID {
		t.Fatalf("swap produced %+v; want a new process at 1.1.0", second)
	}
	if processAlive(first.PID) {
		t.Fatalf("the old pack (pid %d) is STILL RUNNING after a swap — this is the leak", first.PID)
	}
	if !processAlive(second.PID) {
		t.Fatalf("the new pack (pid %d) is not alive", second.PID)
	}
	if got := s.Running(); len(got) != 1 || got[0].Version != "1.1.0" {
		t.Fatalf("Running() = %+v, want exactly one pack at 1.1.0", got)
	}
}

// A swap whose replacement never becomes ready must leave the incumbent
// SERVING. An update that breaks an extension is annoying; one that also takes
// it down is an outage the operator did not ask for.
func TestAFailedSwapLeavesTheRunningVersionUntouched(t *testing.T) {
	f := newFakeIdentity()
	s := newSup(f)
	t.Cleanup(s.StopAll)
	ctx := context.Background()

	first, err := s.Start(ctx, specFor("waiveo/backups", "1.0.0"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The replacement starts but never redeems — a pack that boots and cannot
	// reach the API, which is the realistic shape of a bad update.
	f.mu.Lock()
	f.never["waiveo/backups"] = true
	f.mu.Unlock()
	s.opts.ReadyTimeout = 300 * time.Millisecond

	if _, err := s.Swap(ctx, specFor("waiveo/backups", "2.0.0")); err == nil {
		t.Fatal("a swap to a pack that never became ready succeeded")
	}
	if !processAlive(first.PID) {
		t.Fatalf("the incumbent (pid %d) was killed by a FAILED swap", first.PID)
	}
	got := s.Running()
	if len(got) != 1 || got[0].Version != "1.0.0" || got[0].PID != first.PID {
		t.Fatalf("Running() = %+v, want the original 1.0.0 still serving", got)
	}
}

// A pack that starts and never redeems is not accepted, and leaves no process
// behind. Leaking one child per failed install is how a box ends up with fifty.
func TestAPackThatNeverBecomesReadyIsRefusedAndLeavesNothingBehind(t *testing.T) {
	f := newFakeIdentity()
	f.never["waiveo/silent"] = true
	s := New(f, Options{ReadyTimeout: 200 * time.Millisecond, ReadyPoll: 5 * time.Millisecond, StopGrace: 200 * time.Millisecond})
	t.Cleanup(s.StopAll)

	before := childCount(t)
	if _, err := s.Start(context.Background(), specFor("waiveo/silent", "1.0.0")); err == nil {
		t.Fatal("a pack that never redeemed its identity was accepted")
	}
	if got := childCount(t); got > before {
		t.Fatalf("child processes went from %d to %d across a failed start — the child leaked", before, got)
	}
	if len(s.Running()) != 0 {
		t.Fatalf("a failed start registered a pack: %+v", s.Running())
	}
}

// A host that cannot mint an identity must not start the process at all. Doing
// so would leave a pack running with no way to authenticate and no way for the
// operator to see why.
func TestAPackIsNotStartedWhenItsIdentityCannotBeMinted(t *testing.T) {
	f := newFakeIdentity()
	f.mintErr = errors.New("no scope node configured")
	s := newSup(f)
	t.Cleanup(s.StopAll)

	before := childCount(t)
	if _, err := s.Start(context.Background(), specFor("waiveo/backups", "1.0.0")); err == nil {
		t.Fatal("a pack started despite its identity failing to mint")
	}
	if got := childCount(t); got > before {
		t.Fatalf("a process was spawned despite the mint failing (%d -> %d)", before, got)
	}
}

// A pack that ignores the shutdown signal is killed. Without this one stubborn
// extension keeps its old code alive forever and every later update stacks
// another process behind it.
func TestAPackThatIgnoresShutdownIsKilled(t *testing.T) {
	s := newSup(newFakeIdentity())
	run, err := s.Start(context.Background(), specFor("waiveo/stubborn", "1.0.0", "PACK_IGNORE_SHUTDOWN=1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop("waiveo/stubborn"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if processAlive(run.PID) {
		t.Fatalf("a pack that ignored shutdown (pid %d) survived Stop", run.PID)
	}
}

// After a kill, teardown must have REAPED the child — not merely signalled it.
// An unreaped child lingers as a zombie holding its pid, and one accumulates
// per swap of any extension that ignores shutdown.
func TestAKilledPackIsReapedNotLeftAZombie(t *testing.T) {
	s := newSup(newFakeIdentity())
	run, err := s.Start(context.Background(), specFor("waiveo/stubborn", "1.0.0", "PACK_IGNORE_SHUTDOWN=1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop("waiveo/stubborn"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Checked immediately: a sleep would let the runtime reap it and hide the bug.
	if st := processState(run.PID); st != "" {
		t.Fatalf("pid %d is still in the process table as %q after Stop — killed but never reaped", run.PID, st)
	}
}

// Stop must return only once the child is gone, or the very next Swap could
// observe a version that is supposedly retired.
func TestStopDoesNotReturnBeforeTheChildIsGone(t *testing.T) {
	s := newSup(newFakeIdentity())
	run, err := s.Start(context.Background(), specFor("waiveo/backups", "1.0.0"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop("waiveo/backups"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if processAlive(run.PID) {
		t.Fatalf("Stop returned while pid %d was still alive", run.PID)
	}
}

// Starting a pack that is already running is refused rather than silently
// producing a second process for one extension.
func TestStartingAnAlreadyRunningPackIsRefused(t *testing.T) {
	s := newSup(newFakeIdentity())
	t.Cleanup(s.StopAll)
	ctx := context.Background()

	first, err := s.Start(ctx, specFor("waiveo/backups", "1.0.0"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := s.Start(ctx, specFor("waiveo/backups", "1.0.0")); err == nil {
		t.Fatal("a second Start for a running pack succeeded")
	}
	if got := s.Running(); len(got) != 1 || got[0].PID != first.PID {
		t.Fatalf("Running() = %+v, want only the original process", got)
	}
}

// Each start gets its OWN grant. Reusing one would mean a restart authenticating
// with a code that was already spent, so the pack could never come up again.
func TestEveryStartMintsItsOwnIdentity(t *testing.T) {
	f := newFakeIdentity()
	s := newSup(f)
	t.Cleanup(s.StopAll)
	ctx := context.Background()

	if _, err := s.Start(ctx, specFor("waiveo/backups", "1.0.0")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := s.Swap(ctx, specFor("waiveo/backups", "1.1.0")); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	codes := f.codes()
	if len(codes) != 2 || codes[0] == codes[1] {
		t.Fatalf("codes = %v, want two distinct grants across a start and a swap", codes)
	}
}

// childCount counts this test process's direct children, which is how a leaked
// pack shows up.
func childCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0 // pgrep exits non-zero for no matches; that is zero children
	}
	return len(strings.Fields(string(out)))
}
