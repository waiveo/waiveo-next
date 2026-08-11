package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

const (
	driveScreenEntity = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	driveScreenDevice = "01J8Z3K4N5P6Q7R8S9T0V1DEVA"
	driveRelayID      = "01J8Z3K4N5P6Q7R8S9T0V1RELY"
)

// driveDelayRule is the operator-authored shape the whole defect was about:
// launch, wait one second, go home. One second is the smallest delay rules/1
// can express (RUL-190's duration_seconds is an integer), which is what keeps
// this test's real-time cost to about a second while still exercising the real
// ticker rather than a fake clock.
const driveDelayRule = `{
	"id": "01J8Z3K4N5P6Q7R8S9T0V1DRV1",
	"mode": "single",
	"triggers": [{"type":"state","entity_id":"` + driveScreenEntity + `","to":["on"]}],
	"conditions": [],
	"actions": [
		{"type":"device_command","entity_id":"` + driveScreenEntity + `","command":"launch"},
		{"type":"delay","duration_seconds":1},
		{"type":"device_command","entity_id":"` + driveScreenEntity + `","command":"home"}
	]
}`

type driveController struct {
	mu   sync.Mutex
	seen []string
}

func (c *driveController) Dispatch(entityID, command string, _ map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, entityID+"/"+command)
	return nil
}

func (c *driveController) commands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func driveResolver(entityID string) (deviceID, deviceClass string, ok bool) {
	if entityID == driveScreenEntity {
		return driveScreenDevice, "media-player", true
	}
	return "", "", false
}

// blockingSource is a DeviceStateSource that hands out its observations and then
// BLOCKS rather than reporting exhausted, modelling the real ECP poller: its
// stream does not end while the relay is up. It matters here because Host.Run
// returns as soon as a source reports exhausted, and a test whose observation
// loop had already exited would prove nothing about the two loops coexisting.
type blockingSource struct {
	mu    sync.Mutex
	obs   []state.Observation
	i     int
	block chan struct{}
}

func (s *blockingSource) Next() (state.Observation, bool) {
	s.mu.Lock()
	if s.i < len(s.obs) {
		o := s.obs[s.i]
		s.i++
		s.mu.Unlock()
		return o, true
	}
	s.mu.Unlock()
	<-s.block // never delivered; the caller's ctx cancel is what ends the loop
	return state.Observation{}, false
}

// TestStartAutomationDriveLoopsRunsBothHalves drives the REAL production
// function — real Host, real sysClock, real time.Ticker at the real production
// cadence — and asserts that a rule with a `delay` runs its head from the
// observation loop and its tail from the time loop. Nothing here is stubbed
// except the physical device.
//
// It is the behavioural half of the reachability claim. TestMainStartsThe
// AutomationDriveLoops below is the other half: this proves the function does
// the right thing, that one proves main calls it.
func TestStartAutomationDriveLoopsRunsBothHalves(t *testing.T) {
	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	defer store.Close()

	controller := &driveController{}
	host, err := automationhost.New(store, deviceclass.Builtin(), controller, driveResolver, driveRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(driveDelayRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	reg := registry.FixtureRegistry{}
	ent := func(st string) state.Entity {
		return state.Entity{ID: driveScreenEntity, DeviceClass: "media-player", State: st}
	}
	src := &blockingSource{
		block: make(chan struct{}),
		obs: []state.Observation{
			state.NewObservation(reg, ent("off"), ent("off")),
			state.NewObservation(reg, ent("off"), ent("on")),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { close(src.block) })

	start := time.Now()
	startAutomationDriveLoops(ctx, host, src, automationTickInterval)

	// The observation loop must dispatch the head promptly — it is not waiting on
	// a tick.
	waitFor(t, 2*time.Second, func() bool { return len(controller.commands()) >= 1 })
	if got := controller.commands(); got[0] != driveScreenEntity+"/launch" {
		t.Fatalf("first command was %q, want the head (launch)", got[0])
	}

	// The time loop must dispatch the tail after the delay elapses, with nothing
	// further observed. Without the tick half of this function that never
	// happens, at any timeout.
	waitFor(t, 5*time.Second, func() bool { return len(controller.commands()) >= 2 })
	elapsed := time.Since(start)

	got := controller.commands()
	if len(got) != 2 || got[1] != driveScreenEntity+"/home" {
		t.Fatalf("device saw %v, want [launch home]", got)
	}
	// The tail must not arrive EARLY either: a one-second delay is one second.
	if elapsed < time.Second {
		t.Fatalf("the delayed tail dispatched after %s, want at least the declared 1s (RUL-190)", elapsed)
	}
	// And it must not be more than a tick or two late — the cadence's whole
	// justification is that it bounds this.
	if elapsed > 2*time.Second {
		t.Errorf("the delayed tail dispatched after %s, want ~1s + at most a tick (%s); the drive cadence is not bounding lateness",
			elapsed, automationTickInterval)
	}
}

// waitFor polls cond until it holds or the budget expires. It exists so this
// file's timing assertions are "within N" rather than "sleep N", which is what
// keeps them from being both slow and flaky.
func waitFor(t *testing.T, budget time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition did not hold within %s", budget)
}

// TestStartAutomationDriveLoopsStopWithTheirContext pins the lifecycle claim in
// the only way that means anything operationally: after the context is
// cancelled the engine must stop being advanced. A ticker goroutine that
// outlived rootCtx would keep firing rules — dispatching real commands at real
// devices — out of a binary that is shutting down.
//
// The probe is a delay caught mid-flight: the head has dispatched and the tail
// is parked when cancel lands, so if the tick loop is still running the tail
// arrives within the second, and if it has stopped it never arrives at all.
func TestStartAutomationDriveLoopsStopWithTheirContext(t *testing.T) {
	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	defer store.Close()

	controller := &driveController{}
	host, err := automationhost.New(store, deviceclass.Builtin(), controller, driveResolver, driveRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(driveDelayRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	reg := registry.FixtureRegistry{}
	ent := func(st string) state.Entity {
		return state.Entity{ID: driveScreenEntity, DeviceClass: "media-player", State: st}
	}
	src := &blockingSource{
		block: make(chan struct{}),
		obs: []state.Observation{
			state.NewObservation(reg, ent("off"), ent("off")),
			state.NewObservation(reg, ent("off"), ent("on")),
		},
	}
	t.Cleanup(func() { close(src.block) })

	ctx, cancel := context.WithCancel(context.Background())
	startAutomationDriveLoops(ctx, host, src, automationTickInterval)

	// Catch the run mid-delay: the head has dispatched, the tail has not.
	waitFor(t, 2*time.Second, func() bool { return len(controller.commands()) >= 1 })
	cancel()

	// Well past the declared one-second delay and many tick periods: a stopped
	// loop never resumes the tail.
	time.Sleep(1500 * time.Millisecond)
	if got := controller.commands(); len(got) != 1 {
		t.Fatalf("device saw %v after the drive context was cancelled, want only the head — the time loop must stop with the binary, not keep firing rules on the way down", got)
	}
}

// fakeFloors is a clockFloorReader with each of the three answers the real store
// can give.
type fakeFloors struct {
	ms  int64
	ok  bool
	err error
}

func (f fakeFloors) ClockFloor() (int64, bool, error) { return f.ms, f.ok, f.err }

func TestSeedScheduleFloor(t *testing.T) {
	const boot = 1786521570_000

	cases := []struct {
		name  string
		store fakeFloors
		want  int64
		why   string
	}{
		{
			name:  "persisted floor is used",
			store: fakeFloors{ms: boot - 3600_000, ok: true},
			want:  boot - 3600_000,
			why:   "the relay's real downtime is (floor, now]; using anything later silently drops the catch-up RUL-350 exists for",
		},
		{
			name:  "no persisted floor falls back to boot",
			store: fakeFloors{ok: false},
			want:  boot,
			why:   "an engine that has never run on this box was not 'unable to evaluate' the last fifty years; a zero floor enumerates from the epoch",
		},
		{
			name:  "store error falls back to boot",
			store: fakeFloors{err: errFloorProbe},
			want:  boot,
			why:   "a degraded store must not leave the floor at zero — that is the boot-stall case, and it must not be fatal either",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := identity.Open(":memory:")
			if err != nil {
				t.Fatalf("identity.Open: %v", err)
			}
			defer store.Close()
			host, err := automationhost.New(store, deviceclass.Builtin(), &driveController{}, driveResolver, driveRelayID)
			if err != nil {
				t.Fatalf("automationhost.New: %v", err)
			}
			if got := seedScheduleFloor(host, tc.store, boot); got != tc.want {
				t.Errorf("seedScheduleFloor = %d, want %d — %s", got, tc.want, tc.why)
			}
		})
	}
}

var errFloorProbe = errors.New("clock_floor table is unreadable")

// TestBootAutomationStackSeedsTheScheduleFloor is the wiring half of the seed,
// pinned the same way the drive loops are. The seed is only worth anything if it
// happens before the first Tick, and the only thing standing between "seeded" and
// "not seeded" is one line in a boot function — precisely the shape that has gone
// missing here before.
func TestBootAutomationStackSeedsTheScheduleFloor(t *testing.T) {
	fn := parseRelayFunc(t, "bootAutomationStack")

	found := 0
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); !ok || ident.Name != "seedScheduleFloor" {
			return true
		}
		found++
		if len(call.Args) != 3 {
			t.Errorf("seedScheduleFloor is called with %d argument(s), want 3", len(call.Args))
			return true
		}
		if got := renderExpr(call.Args[0]); got != "host" {
			t.Errorf("seedScheduleFloor argument 0 = %s, want host", got)
		}
		if got := renderExpr(call.Args[1]); got != "store" {
			t.Errorf("seedScheduleFloor argument 1 = %s, want store — the persisted verified clock floor lives there; anything else discards the relay's real downtime", got)
		}
		return true
	})

	if found != 1 {
		t.Fatalf("bootAutomationStack calls seedScheduleFloor %d times, want exactly 1. Unseeded, the engine's schedule resume cursor is zero, and the first Tick enumerates every schedule occurrence since the Unix epoch — measured at 3.6s and ~240MB for a once-a-minute `time_pattern`, under the host lock, before the relay serves anything, and under a `fire_each` misfire policy it dispatches every one of them at a real device.", found)
	}
}

// parseRelayFunc returns the named top-level func declared in main.go.
func parseRelayFunc(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("main.go declares no func %s", name)
	return nil
}

// TestMainStartsTheAutomationDriveLoops is a check on main's SOURCE rather than
// on its behaviour, the same trade TestMainStartsTheDevicePlaneSync and
// cmd/waiveo-feeder's TestMainStartsTheConsoleBinding record, and made for the
// same reason.
//
// The two cases above prove startAutomationDriveLoops does the right thing. They
// cannot prove main calls it, and "the mechanism is proven, its use is not" is
// exactly how this defect existed in the first place: engine.Tick had passing
// unit tests in internal/rules/engine and NO production caller, so a rule with a
// `delay` ran its head and never its tail, a `for`-hold never elapsed, and a
// `time`/`sun` trigger never fired — with `go build`, the whole test suite,
// `go vet` and validate-deadcode all green. A test that drives Tick manually
// proves the engine works. This is the one that fails if the wiring goes away.
//
// The ARGUMENTS are asserted, not just the call, because two of them can be
// wrong in ways that leave a running, ticking, silently-broken loop: a cadence
// argument swapped for the ECP poll interval makes every delay seconds late,
// and a context that is not rootCtx makes the loop outlive or predecease the
// binary.
func TestMainStartsTheAutomationDriveLoops(t *testing.T) {
	mainFn := parseRelayMainFunc(t)

	wantArgs := []struct {
		arg string
		why string
	}{
		{"rootCtx", "both loops must live as long as the binary and stop with it"},
		{"host", "the automation host owns the engine; there is nothing else to drive"},
		{"poller", "the ECP state poller is the observation half's source — without it no rule fires on a real device at all"},
		{"automationTickInterval", "the time half's cadence, chosen against rules/1's one-second duration granularity; the ECP poll interval here would make every delay and hold arrive seconds late"},
	}

	found := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); !ok || ident.Name != "startAutomationDriveLoops" {
			return true
		}
		found++
		if len(call.Args) != len(wantArgs) {
			t.Errorf("startAutomationDriveLoops is called with %d argument(s), want %d", len(call.Args), len(wantArgs))
			return true
		}
		for i, want := range wantArgs {
			if got := renderExpr(call.Args[i]); got != want.arg {
				t.Errorf("startAutomationDriveLoops argument %d = %s, want %s — %s", i, got, want.arg, want.why)
			}
		}
		return true
	})

	if found == 0 {
		t.Fatal("func main never calls startAutomationDriveLoops. The edge engine is then loaded but not driven: a rule with a `delay` runs its head and never its tail (the sign turns on and never turns off), a `for`-hold never elapses, a stabilization-held fire is never released, and no `time`/`time_pattern`/`sun` trigger ever fires — for the life of the process, with every other gate green. That is the exact defect this file was added to close.")
	}
	if found > 1 {
		t.Errorf("func main calls startAutomationDriveLoops %d times; two engines' worth of drive loops over one engine would double-dispatch every firing", found)
	}
}
