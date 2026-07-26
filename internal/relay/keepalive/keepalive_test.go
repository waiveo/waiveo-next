package keepalive

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// dispatchCall records one Controller.Dispatch invocation's shape, minus the
// error return — mirroring cmd/waiveo-relay/main_test.go's own
// recordingController pattern (this codebase's established fake-controller
// convention).
type dispatchCall struct {
	entityID string
	command  string
	channel  string
}

// fakeController is a deviceplane.DeviceController test double that records
// every Dispatch call in order and, when err is set, fails every call with it
// (still recording the attempt).
type fakeController struct {
	mu    sync.Mutex
	calls []dispatchCall
	err   error
}

func (f *fakeController) Dispatch(entityID, command string, params map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, _ := params[channelParam].(string)
	f.calls = append(f.calls, dispatchCall{entityID: entityID, command: command, channel: ch})
	return f.err
}

func (f *fakeController) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeController) callsFor(entityID string) []dispatchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dispatchCall
	for _, c := range f.calls {
		if c.entityID == entityID {
			out = append(out, c)
		}
	}
	return out
}

// obsFor builds a canned state.Observation carrying exactly the two
// REG-064 attributes evaluate consults — the injected-observation seam
// recordObservation drives, standing in for a real or synthetic Poller's
// Next() result (the package doc's "inject observations rather than real
// polling" test posture). powerMode/appType == "" models an absent attribute
// identically to attrString's own nil/absent handling.
func obsFor(entityID, powerMode, appType string) state.Observation {
	return state.Observation{Entity: state.Entity{
		ID:         entityID,
		Attributes: map[string]any{"power_mode": powerMode, "app_type": appType},
	}}
}

func TestNewAppliesDefaults(t *testing.T) {
	k := New(Config{})
	if k.pollInterval != defaultPollInterval {
		t.Errorf("pollInterval = %s, want default %s", k.pollInterval, defaultPollInterval)
	}
	if k.launchDelay != defaultLaunchDelay {
		t.Errorf("launchDelay = %s, want default %s", k.launchDelay, defaultLaunchDelay)
	}
	if k.channel != defaultChannel {
		t.Errorf("channel = %q, want default %q", k.channel, defaultChannel)
	}
}

func TestNewHonorsOverrides(t *testing.T) {
	k := New(Config{PollInterval: 7 * time.Second, LaunchDelay: 3 * time.Second, Channel: "custom-channel"})
	if k.pollInterval != 7*time.Second {
		t.Errorf("pollInterval = %s, want 7s", k.pollInterval)
	}
	if k.launchDelay != 3*time.Second {
		t.Errorf("launchDelay = %s, want 3s", k.launchDelay)
	}
	if k.channel != "custom-channel" {
		t.Errorf("channel = %q, want custom-channel", k.channel)
	}
}

// TestPowerOnDelayHonored pins rule 1 (PLY-154): even though the screen reads
// Home from the very first poll, no launch fires until launchDelay has
// elapsed since the power-on transition — not merely until the home streak
// counts up.
func TestPowerOnDelayHonored(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: 1500 * time.Millisecond, Controller: fc})
	t0 := time.Unix(1700000000, 0)

	for _, elapsed := range []time.Duration{0, 500 * time.Millisecond, 1000 * time.Millisecond} {
		k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(elapsed))
	}
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls before the delay elapsed = %d, want 0 (%+v)", got, fc.calls)
	}

	// Past the delay, but this is only the FIRST qualifying poll since the
	// delay cleared — rule 2 still requires a second one.
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(1600*time.Millisecond))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls on the first post-delay poll = %d, want 0 (rule 2 needs a second)", got)
	}

	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2000*time.Millisecond))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls on the second post-delay Home poll = %d, want exactly 1", got)
	}
}

// TestHomeTwoConsecutivePollsFiresExactlyOneLaunch pins rule 2's own
// threshold in isolation (past the power-on delay throughout): the first
// Home poll never fires, the second does, exactly once, and the dispatched
// command carries the configured channel.
func TestHomeTwoConsecutivePollsFiresExactlyOneLaunch(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: time.Millisecond, Channel: "myapp", Controller: fc})
	t0 := time.Unix(1700000000, 0)

	// Baseline PowerOn+foreign poll, far enough back that the delay is long
	// cleared by the time the Home polls below arrive.
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)

	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls after ONE Home poll = %d, want 0", got)
	}

	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))
	calls := fc.callsFor("scr1")
	if len(calls) != 1 {
		t.Fatalf("launch calls after TWO consecutive Home polls = %d, want exactly 1 (%+v)", len(calls), calls)
	}
	if calls[0].command != launchCommand || calls[0].channel != "myapp" {
		t.Errorf("dispatched call = %+v, want command=%q channel=myapp", calls[0], launchCommand)
	}
}

// TestForeignAppNeverFires pins rule 2's negative case: a screen that is
// PowerOn and past the settle delay, but sitting on a real foreground app
// (never "home"/"menu"), never dispatches a recovery launch, no matter how
// many polls accumulate — a person may be using the TV.
func TestForeignAppNeverFires(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: time.Millisecond, Controller: fc})
	t0 := time.Unix(1700000000, 0)

	for i := 0; i < 10; i++ {
		k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(time.Duration(i)*time.Second))
	}
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls with a foreign app foreground = %d, want 0 (%+v)", got, fc.calls)
	}
}

// TestStandbyNeverFires pins rule 3: ANY raw power_mode other than the exact
// literal "PowerOn" — a recognized low-power reading, an unrecognized one, or
// absent (unreachable/unavailable) — never dispatches, even paired with an
// app_type of "home", and even across many polls over a long span.
func TestStandbyNeverFires(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: time.Millisecond, Controller: fc})
	t0 := time.Unix(1700000000, 0)

	nonPowerOnValues := []string{"Suspend", "Ready", "DisplayOff", "", "SomeFutureFirmwareValue"}
	for i, pm := range nonPowerOnValues {
		k.recordObservation(obsFor("scr1", pm, "home"), t0.Add(time.Duration(i)*time.Hour))
	}
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls while never PowerOn = %d, want 0 (%+v)", got, fc.calls)
	}
}

// TestCooldownDoesNotMachineGun pins the cooldown behavior: once a launch has
// fired for an unbroken Home streak, further Home polls on that SAME streak
// never fire again (no machine-gunning a device with repeated launches);
// only after app_type leaves home/menu (a genuine state change) and returns
// for a fresh 2-poll streak does keepalive fire again.
func TestCooldownDoesNotMachineGun(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: time.Millisecond, Controller: fc})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0) // baseline, clears the delay window
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(1*time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls after the confirming Home poll = %d, want exactly 1", got)
	}

	// Still sitting on Home for several more polls: must NOT re-fire.
	for i := 3; i <= 6; i++ {
		k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Duration(i)*time.Second))
	}
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls after %d further unbroken Home polls = %d, want still exactly 1 (machine-gunning)", 4, got)
	}

	// A genuine state change (foreign app) clears the cooldown; a fresh
	// 2-poll Home streak may then fire again.
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0.Add(7*time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(8*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls after only ONE Home poll on the new streak = %d, want still 1", got)
	}
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(9*time.Second))
	if got := fc.callCount(); got != 2 {
		t.Fatalf("launch calls after a second confirmed Home streak = %d, want exactly 2", got)
	}
}

// TestPowerOffThenBackOnRestartsDelayAndStreak pins the reset side of rules
// 1/3 together: leaving PowerOn (even briefly) and coming back MUST re-arm
// the full power-on delay and a fresh streak — a screen that blips through
// standby is not allowed to skip straight back to a confirmed streak using
// state left over from before the blip.
func TestPowerOffThenBackOnRestartsDelayAndStreak(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: 1500 * time.Millisecond, Controller: fc})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second)) // would confirm, if uninterrupted
	k.recordObservation(obsFor("scr1", "Suspend", "home"), t0.Add(3*time.Second)) // drops out of PowerOn
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls after dropping to standby before confirming = %d, want 0", got)
	}

	// Back on: must re-run the full delay, not resume the old streak.
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(4*time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls immediately after a fresh power-on = %d, want 0 (delay must restart)", got)
	}
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(6*time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls on the first post-delay poll of the fresh streak = %d, want 0", got)
	}
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(7*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls after the fresh streak's second confirming poll = %d, want exactly 1", got)
	}
}

// TestDispatchErrorIsNonFatal proves a Controller.Dispatch failure is logged
// and swallowed, never propagated — keepalive gets another chance on its next
// qualifying poll rather than wedging.
func TestDispatchErrorIsNonFatal(t *testing.T) {
	fc := &fakeController{err: errors.New("device unreachable")}
	k := New(Config{LaunchDelay: time.Millisecond, Controller: fc})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("dispatch attempts = %d, want exactly 1 even though it failed", got)
	}
}

// TestNilControllerDoesNotPanic proves dispatchLaunch's nil-Controller guard:
// a Config left without one (only ever useful for exercising the state
// machine alone) never panics when a launch decision fires.
func TestNilControllerDoesNotPanic(t *testing.T) {
	k := New(Config{LaunchDelay: time.Millisecond})
	t0 := time.Unix(1700000000, 0)
	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second)) // would panic here if unguarded
}

// TestScreensAreIndependent proves per-screen state never leaks across
// entity IDs: a second screen sitting on a foreign app the whole time must
// never be affected by the first screen's confirmed Home recovery.
func TestScreensAreIndependent(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: time.Millisecond, Controller: fc})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)
	k.recordObservation(obsFor("scr2", "PowerOn", "app"), t0)

	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr2", "PowerOn", "app"), t0.Add(time.Second))

	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))
	k.recordObservation(obsFor("scr2", "PowerOn", "app"), t0.Add(2*time.Second))

	if got := fc.callsFor("scr1"); len(got) != 1 {
		t.Fatalf("scr1 launch calls = %d, want 1", len(got))
	}
	if got := fc.callsFor("scr2"); len(got) != 0 {
		t.Fatalf("scr2 launch calls = %d, want 0 (never left its foreign app)", len(got))
	}
}

// --- Run wiring: a real (fake ECP) Poller + the ticker-driven re-evaluation ---

// newECPFixture starts an httptest server answering ECP's active-app/
// device-info queries with a screen permanently parked at Home/PowerOn — the
// exact "never changes" case the package doc's "Poll cadence" section exists
// for, since ecppoll's own Next() stream would otherwise emit only the
// arrival seed and nothing more.
func newECPFixture(t *testing.T) Target {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/query/active-app", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<active-app><app id="1" type="home">Home</app></active-app>`))
	})
	mux.HandleFunc("/query/device-info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<device-info><power-mode>PowerOn</power-mode></device-info>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fixture URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split fixture host/port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse fixture port: %v", err)
	}
	return Target{Host: host, Port: port}
}

// TestRunFiresRecoveryForAScreenStuckAtHome is the end-to-end proof that Run's
// own ticker — not ecppoll's change-only Next() stream — is what lets rule 2
// progress for a screen whose ECP responses never change at all: without it,
// exactly one Observation (the arrival seed) would ever be delivered and no
// launch could ever fire.
func TestRunFiresRecoveryForAScreenStuckAtHome(t *testing.T) {
	target := newECPFixture(t)
	fc := &fakeController{}
	k := New(Config{
		Targets:      map[string]Target{"scr1": target},
		PollInterval: 20 * time.Millisecond,
		LaunchDelay:  time.Millisecond,
		Controller:   fc,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = k.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fc.callCount() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := fc.callsFor("scr1")
	if len(calls) < 1 {
		t.Fatalf("Run dispatched %d launch(es) within the deadline for a screen stuck at Home, want at least 1", len(calls))
	}
	if calls[0].command != launchCommand || calls[0].channel != defaultChannel {
		t.Errorf("first dispatched call = %+v, want command=%q channel=%q", calls[0], launchCommand, defaultChannel)
	}
}

// TestRunStopsOnContextCancellation proves Run returns ctx.Err() once ctx is
// canceled, rather than leaking its ticker loop.
func TestRunStopsOnContextCancellation(t *testing.T) {
	k := New(Config{PollInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- k.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// --- PLY-155/156: blank-Lease suppression, delegated to playerserver.EvaluateRecovery ---

// TestBlankActiveLeaseSuppressesRecovery pins PLY-155: a screen that would
// otherwise confirm a Home recovery (PowerOn, past the settle delay, two
// consecutive Home polls) must NEVER dispatch while Config.ActiveDisplay
// reports the screen's own currently active Lease is blank — an
// intentionally off-air screen is not a recovery target, regardless of how
// far the Home streak has progressed. Once the Lease stops being blank, the
// streak must be reconfirmed fresh (not fire immediately on stale
// pre-suppression progress).
func TestBlankActiveLeaseSuppressesRecovery(t *testing.T) {
	fc := &fakeController{}
	var blank int32 = 1 // atomic bool: 1 = the active Lease is blank
	k := New(Config{
		LaunchDelay: time.Millisecond,
		Controller:  fc,
		ActiveDisplay: func(string) string {
			if atomic.LoadInt32(&blank) == 1 {
				return playerserver.DisplayBlank
			}
			return playerserver.DisplayContent
		},
	})
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls while the active Lease is blank = %d, want 0 (PLY-155 must suppress regardless of the confirmed Home streak)", got)
	}

	// Un-blank: PLY-155 no longer suppresses, but the streak must be
	// reconfirmed fresh — never fire immediately on stale pre-suppression
	// progress.
	atomic.StoreInt32(&blank, 0)
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(3*time.Second))
	if got := fc.callCount(); got != 0 {
		t.Fatalf("launch calls on the FIRST post-suppression Home poll = %d, want 0 (the streak resets under suppression, PLY-155)", got)
	}
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(4*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls after the second post-suppression confirming Home poll = %d, want exactly 1", got)
	}
}

// TestNilActiveDisplayNeverSuppresses proves the documented Config.ActiveDisplay
// zero-value degrade: leaving it unset behaves exactly as every pre-existing
// test above already assumes — PLY-155 has no effect, never a false
// suppression a production relay would notice as "keepalive silently stopped
// working." (A production relay MUST wire it — see Config's own doc — this
// only pins the safe degrade for a test/Config that doesn't.)
func TestNilActiveDisplayNeverSuppresses(t *testing.T) {
	fc := &fakeController{}
	k := New(Config{LaunchDelay: time.Millisecond, Controller: fc}) // ActiveDisplay left nil
	t0 := time.Unix(1700000000, 0)

	k.recordObservation(obsFor("scr1", "PowerOn", "app"), t0)
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(time.Second))
	k.recordObservation(obsFor("scr1", "PowerOn", "home"), t0.Add(2*time.Second))
	if got := fc.callCount(); got != 1 {
		t.Fatalf("launch calls with ActiveDisplay left nil = %d, want exactly 1 (nil must not suppress)", got)
	}
}

// --- REL-115: keepalive's dispatch must be serialized like every other command ---

// blockingKAController is a deviceplane.DeviceController test double whose
// Dispatch records the call and then invokes onDispatch synchronously (which
// may itself block until a test releases it) — this package's own analogue
// of internal/relay/deviceplane/serialize_test.go's blockingController, used
// here to prove keepalive's OWN dispatch call sites (dispatchLaunch,
// evaluateAll) behave correctly once wired through a real
// deviceplane.CommandSurface, exactly as cmd/waiveo-relay/main.go wires them.
type blockingKAController struct {
	mu         sync.Mutex
	calls      []string
	onDispatch func(entityID string)
}

func (b *blockingKAController) Dispatch(entityID, command string, params map[string]any) error {
	b.mu.Lock()
	b.calls = append(b.calls, entityID)
	b.mu.Unlock()
	if b.onDispatch != nil {
		b.onDispatch(entityID)
	}
	return nil
}

// seedConfirmable directly seeds k's internal cache/state so evaluateAll's
// very next call fires a launch for id on its own — one confirming Home poll
// away from threshold, already past the settle delay — without driving the
// whole recordObservation sequence (which would itself synchronously
// dispatch before the test can observe concurrency). Same-package test file,
// so it may reach into k's unexported fields directly.
func seedConfirmable(k *Keepalive, id string, asOf time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.known[id] = pollSnapshot{powerMode: "PowerOn", appType: "home"}
	k.screens[id] = &screenState{wasPoweredOn: true, poweredOnAt: asOf.Add(-time.Hour), homeStreak: 1}
}

// TestEvaluateAllSerializesPerDeviceButNotAcrossScreens is the integrated
// proof of both the REL-115 dispatch fix and the per-screen concurrency fix
// together: entities scrA and scrB are two entities of the SAME physical
// device (data-model/1 fan-out), while scrC is an entirely independent
// device. All three become confirmable in the SAME evaluateAll pass.
//
//   - scrA/scrB's dispatches must never overlap inside the controller (REL-115
//     — proves keepalive's Controller now goes through a
//     deviceplane.CommandSurface, not a bare DeviceController, per finding 2).
//   - scrC's dispatch must complete without waiting for scrA/scrB to be
//     released (proves evaluateAll's own per-screen dispatch is concurrent,
//     per finding 5) — the exact scenario finding 2 named: "an edge rule or a
//     scheduled preset batch dispatches a command to the same device_id at
//     the same moment keepalive's ticker fires a recovery launch."
func TestEvaluateAllSerializesPerDeviceButNotAcrossScreens(t *testing.T) {
	entered := make(chan string, 8)
	release := make(chan struct{})
	var inFlightDeviceX int32
	var overlapDeviceX int32

	controller := &blockingKAController{onDispatch: func(entityID string) {
		if entityID == "scrA" || entityID == "scrB" {
			if atomic.AddInt32(&inFlightDeviceX, 1) > 1 {
				atomic.StoreInt32(&overlapDeviceX, 1)
			}
			entered <- entityID
			<-release
			atomic.AddInt32(&inFlightDeviceX, -1)
			return
		}
		entered <- entityID // scrC: an independent device, never blocks
	}}

	surface := deviceplane.NewCommandSurface(controller, registry.FixtureRegistry{},
		func(entityID string) (deviceID, deviceClass string, ok bool) {
			switch entityID {
			case "scrA", "scrB":
				return "device-X", "media-player", true
			case "scrC":
				return "device-Y", "media-player", true
			}
			return "", "", false
		})
	sink := automation.NewCommandSink(surface, "01J8Z3K4N5P6Q7R8S9T0V1RELY")

	k := New(Config{LaunchDelay: time.Millisecond, Controller: sink})
	now := time.Now()
	seedConfirmable(k, "scrA", now)
	seedConfirmable(k, "scrB", now)
	seedConfirmable(k, "scrC", now)

	k.evaluateAll(now)

	seenC := false
	deadline := time.After(2 * time.Second)
	for !seenC {
		select {
		case id := <-entered:
			if id == "scrC" {
				seenC = true
			}
		case <-deadline:
			t.Fatal("scrC's (independent device) dispatch never entered the controller while scrA/scrB were blocked — evaluateAll must not serialize dispatch across independent screens (finding 5)")
		}
	}

	close(release)
	k.dispatchWG.Wait()

	if atomic.LoadInt32(&overlapDeviceX) != 0 {
		t.Fatal("scrA and scrB (two entities of the SAME physical device) were dispatched into the controller concurrently — REL-115 requires per-device serialization; keepalive must dispatch through a deviceplane.CommandSurface to get it (finding 2)")
	}
}

// TestRunCancelsPromptlyWhileADispatchIsInFlight proves Run's ctx-cancellation
// promptness holds even UNDER LOAD — while one screen's recovery dispatch is
// still outstanding (blocked, modeling a slow/unreachable device) — rather
// than only when idle. Before evaluateAll's per-screen dispatch was made
// concurrent (finding 5), a single stuck dispatch ran inline inside Run's own
// ticker branch, so cancellation would have had to wait behind it too.
func TestRunCancelsPromptlyWhileADispatchIsInFlight(t *testing.T) {
	hold := make(chan struct{}) // never closed: models a permanently hung device
	entered := make(chan struct{}, 1)
	controller := &blockingKAController{onDispatch: func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
	}}
	k := New(Config{PollInterval: 20 * time.Millisecond, LaunchDelay: time.Millisecond, Controller: controller})
	seedConfirmable(k, "scr1", time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- k.Run(ctx) }()

	select {
	case <-entered:
		// The seeded screen's dispatch is now in flight and permanently
		// blocked — this is the "under load" condition the minor asked for.
	case <-time.After(2 * time.Second):
		t.Fatal("the seeded screen's dispatch never entered the controller — test setup produced no load to cancel under")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return promptly after cancellation while a dispatch was still in flight — evaluateAll's per-screen dispatch must never block ctx cancellation")
	}
}
