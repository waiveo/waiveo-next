package player1

// remotetarget_internal_test.go unit-tests RemoteECPPlayerTarget's own logic
// (Pair's two observation modes, RemoteEnvFromEnv's env parsing, and
// classifyPlayerV3Line's line-matching) against FAKES — never a real Roku —
// exactly what remotetarget.go's own package doc promises ("...
// remotetarget_internal_test.go unit-tests Pair's own logic against a
// fake"). It is an internal (white-box) test file (package player1, not
// player1_test) because ecpLauncher, outcomeObserver, and
// RemoteECPPlayerTarget's own fields are all unexported — exactly the seams
// this file exists to drive fakes through. driver_test.go (package
// player1_test) covers the exported, target-agnostic surface
// (PLY057Driveable, Run) with a fake target of its own.

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// --- fakes -------------------------------------------------------------

// fakeLauncher is a test-only ecpLauncher: it never touches the network.
// onLaunch, when set, runs after recording the call — relay-observed-mode
// tests use it to simulate the real ECP-launch-causes-a-relay-hit sequence
// (the launched device dialing the relay), so awaitViaRelay's baseline
// correlation (Pair captures PairCallCount() BEFORE Launch, then waits for
// it to advance) has something to actually observe advancing.
type fakeLauncher struct {
	err      error
	calls    int
	channel  string
	args     map[string]string
	onLaunch func()
}

func (f *fakeLauncher) Launch(channel string, args map[string]string) error {
	f.calls++
	f.channel = channel
	f.args = args
	if f.onLaunch != nil {
		f.onLaunch()
	}
	return f.err
}

// fakeObserver is a test-only outcomeObserver returning a canned PairResult.
type fakeObserver struct {
	result PairResult
	closed bool
}

func (f *fakeObserver) AwaitOutcome(time.Time) PairResult { return f.result }
func (f *fakeObserver) Close() error                      { f.closed = true; return nil }

// fakeRelay is a minimal Relay fake for exercising awaitViaRelay
// (relay-observed mode) without booting the real in-process feeder+relay
// stack — only PairCallCount/PairResponses are ever read by that path.
type fakeRelay struct {
	pairCallCount int
	pairResponses [][]byte
}

func (f *fakeRelay) BaseURL() string                              { return "" }
func (f *fakeRelay) FormPairingCode() (string, error)             { return "", nil }
func (f *fakeRelay) FormPairingCodePair() (string, string, error) { return "", "", nil }
func (f *fakeRelay) PairCallCount() int                           { return f.pairCallCount }
func (f *fakeRelay) PairRequests() [][]byte                       { return nil }
func (f *fakeRelay) PairResponses() [][]byte                      { return f.pairResponses }
func (f *fakeRelay) Reset()                                       {}

// --- RemoteTargetConfig.withDefaults ------------------------------------

func TestRemoteTargetConfigWithDefaults(t *testing.T) {
	zero := RemoteTargetConfig{}.withDefaults()
	if zero.ECPPort != defaultECPPort {
		t.Errorf("ECPPort = %d, want default %d", zero.ECPPort, defaultECPPort)
	}
	if zero.Channel != defaultChannel {
		t.Errorf("Channel = %q, want default %q", zero.Channel, defaultChannel)
	}
	if zero.DebugPort != defaultDebugPort {
		t.Errorf("DebugPort = %d, want default %d", zero.DebugPort, defaultDebugPort)
	}
	if zero.LaunchTimeout != defaultLaunchTimeout {
		t.Errorf("LaunchTimeout = %v, want default %v", zero.LaunchTimeout, defaultLaunchTimeout)
	}
	if zero.ObserveTimeout != defaultObserveTimeout {
		t.Errorf("ObserveTimeout = %v, want default %v", zero.ObserveTimeout, defaultObserveTimeout)
	}
	if zero.NoDebugConsole {
		t.Errorf("NoDebugConsole = true, want false (zero value IS its own default)")
	}

	explicit := RemoteTargetConfig{
		Host: "192.168.50.99", ECPPort: 1, Channel: "c", DebugPort: 2,
		NoDebugConsole: true, LaunchTimeout: time.Second, ObserveTimeout: time.Minute,
	}.withDefaults()
	want := RemoteTargetConfig{
		Host: "192.168.50.99", ECPPort: 1, Channel: "c", DebugPort: 2,
		NoDebugConsole: true, LaunchTimeout: time.Second, ObserveTimeout: time.Minute,
	}
	if explicit != want {
		t.Errorf("withDefaults() overrode an explicit non-zero field: got %+v, want %+v", explicit, want)
	}
}

// --- classifyPlayerV3Line ------------------------------------------------

// TestClassifyPlayerV3Line quotes player-v3's OWN print statements verbatim
// (PlayerTask.brs/Pairing.brs) — see this package's own doc for the file:line
// provenance of each string.
func TestClassifyPlayerV3Line(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		wantOK bool
		want   PairResult
	}{
		{
			name:   "PHOTON image verified (Program.brs's full-thread completion)",
			line:   "[player-v3] PHOTON — image verified + ready to render (foo)\n",
			wantOK: true,
			want:   PairResult{Completed: true},
		},
		{
			name:   "paired (screen ...), token persisted (redemption succeeded)",
			line:   "[player-v3] paired (screen 01J8Z3), token persisted\n",
			wantOK: true,
			want:   PairResult{Completed: true},
		},
		{
			name:   "pairing FAILED: commitment mismatch (PLY-057)",
			line:   "[player-v3] pairing FAILED: fingerprint commitment MISMATCH — refusing to pair (possible MITM)\n",
			wantOK: true,
			want: PairResult{
				Rejected:           true,
				CommitmentMismatch: true,
				Err:                "[player-v3] pairing FAILED: fingerprint commitment MISMATCH — refusing to pair (possible MITM)",
			},
		},
		{
			name:   "pairing FAILED: unrelated reason (not a commitment mismatch)",
			line:   "[player-v3] pairing FAILED: could not reach relay: timeout\n",
			wantOK: true,
			want: PairResult{
				Rejected: true,
				Err:      "[player-v3] pairing FAILED: could not reach relay: timeout",
			},
		},
		{
			name:   "non-terminal line: keep reading",
			line:   "[player-v3] pulling program…\n",
			wantOK: false,
		},
		{
			name:   "non-terminal line: startup banner",
			line:   "###################################################################\n",
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := classifyPlayerV3Line(c.line)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, c.wantOK, got)
			}
			if !ok {
				return
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// --- telnetDebugConsole.AwaitOutcome -------------------------------------

// TestTelnetDebugConsoleAwaitOutcome drives AwaitOutcome over a real net.Conn
// (net.Pipe, not a socket) so the read/buffer/deadline logic runs for real —
// only the print lines fed to it are fake.
func TestTelnetDebugConsoleAwaitOutcome(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	c := &telnetDebugConsole{conn: client, r: bufio.NewReader(client)}

	go func() {
		_, _ = server.Write([]byte("[player-v3] pairing…\n"))
		_, _ = server.Write([]byte("[player-v3] pulling program…\n"))
		_, _ = server.Write([]byte("[player-v3] PHOTON — image verified + ready to render (x)\n"))
		_ = server.Close()
	}()

	got := c.AwaitOutcome(time.Now().Add(5 * time.Second))
	if !got.Completed {
		t.Errorf("AwaitOutcome = %+v, want Completed (non-terminal lines must be skipped, not misclassified)", got)
	}
}

// TestTelnetDebugConsoleAwaitOutcomeTimeout proves AwaitOutcome gives up at
// its deadline (a real device that hung, or a debug console with nothing
// left to say) rather than blocking forever.
func TestTelnetDebugConsoleAwaitOutcomeTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()
	c := &telnetDebugConsole{conn: client, r: bufio.NewReader(client)}

	got := c.AwaitOutcome(time.Now().Add(50 * time.Millisecond))
	if !got.Rejected {
		t.Errorf("AwaitOutcome = %+v, want Rejected on deadline elapsed", got)
	}
}

// --- httpECPLauncher.Launch ----------------------------------------------

// TestHTTPECPLauncherLaunch proves the ECP deep-link contract this package's
// own doc claims: POST /launch/<channel>?<launch-arg query params>, the
// contract player-v3/source/Main.brs's wvNormalizeArgs/wvArgValue reads back
// out via Roku's launch-args AA.
func TestHTTPECPLauncherLaunch(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	l := &httpECPLauncher{addr: strings.TrimPrefix(srv.URL, "http://"), client: srv.Client()}
	if err := l.Launch("dev", map[string]string{"pairingCode": "abc123"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/launch/dev" {
		t.Errorf("path = %s, want /launch/dev", gotPath)
	}
	if gotQuery != "pairingCode=abc123" {
		t.Errorf("query = %s, want pairingCode=abc123", gotQuery)
	}
}

// TestHTTPECPLauncherLaunchErrorStatus proves a non-2xx ECP response surfaces
// as an error rather than a silent success.
func TestHTTPECPLauncherLaunchErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	l := &httpECPLauncher{addr: strings.TrimPrefix(srv.URL, "http://"), client: srv.Client()}
	if err := l.Launch("dev", nil); err == nil {
		t.Errorf("Launch: expected error on HTTP 500, got nil")
	}
}

// --- RemoteECPPlayerTarget.Pair: debug-console mode ----------------------

func TestRemoteECPPlayerTargetPairDebugConsoleMode(t *testing.T) {
	launcher := &fakeLauncher{}
	obs := &fakeObserver{result: PairResult{Completed: true}}
	target := &RemoteECPPlayerTarget{
		cfg:      RemoteTargetConfig{Channel: "dev"}.withDefaults(),
		launcher: launcher,
		dialObserver: func() (outcomeObserver, error) {
			return obs, nil
		},
	}

	// Debug-console mode can correctly OBSERVE a commitment mismatch (see
	// TestClassifyPlayerV3Line), but that alone does not make drivePLY057's
	// relay-side assertions hold against a real device — DriveablePLY057
	// reports false in BOTH modes (see remotetarget.go's own package doc).
	if target.DriveablePLY057() {
		t.Errorf("DriveablePLY057() = true, want false in every mode (see package doc)")
	}
	if got, want := target.Name(), "roku-ecp:(debug-console)"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	got := target.Pair("some-code")
	if got != (PairResult{Completed: true}) {
		t.Errorf("Pair = %+v, want Completed", got)
	}
	if launcher.calls != 1 {
		t.Errorf("launcher.calls = %d, want 1", launcher.calls)
	}
	if launcher.channel != "dev" {
		t.Errorf("launcher channel = %q, want dev", launcher.channel)
	}
	if launcher.args["pairingCode"] != "some-code" {
		t.Errorf("launcher args[pairingCode] = %q, want some-code", launcher.args["pairingCode"])
	}
	if !obs.closed {
		t.Errorf("observer was never Closed")
	}
}

// TestRemoteECPPlayerTargetPairDialObserverError proves a debug console that
// cannot be dialed (firewalled, wrong port) is reported as Rejected WITHOUT
// ever launching the device — a half-observed attempt would be worse than an
// honest failure.
func TestRemoteECPPlayerTargetPairDialObserverError(t *testing.T) {
	launcher := &fakeLauncher{}
	dialErr := errors.New("dial tcp: connection refused")
	target := &RemoteECPPlayerTarget{
		cfg:      RemoteTargetConfig{}.withDefaults(),
		launcher: launcher,
		dialObserver: func() (outcomeObserver, error) {
			return nil, dialErr
		},
	}

	got := target.Pair("some-code")
	if !got.Rejected {
		t.Errorf("Pair = %+v, want Rejected on dial failure", got)
	}
	if launcher.calls != 0 {
		t.Errorf("launcher.calls = %d, want 0 (must not launch when the observer can't be dialed)", launcher.calls)
	}
}

// TestRemoteECPPlayerTargetPairLaunchError proves an ECP launch failure is
// reported as Rejected, and that the (already-dialed) observer is still
// Closed.
func TestRemoteECPPlayerTargetPairLaunchError(t *testing.T) {
	obs := &fakeObserver{}
	launcher := &fakeLauncher{err: errors.New("ECP launch: connection refused")}
	target := &RemoteECPPlayerTarget{
		cfg:      RemoteTargetConfig{}.withDefaults(),
		launcher: launcher,
		dialObserver: func() (outcomeObserver, error) {
			return obs, nil
		},
	}

	got := target.Pair("some-code")
	if !got.Rejected {
		t.Errorf("Pair = %+v, want Rejected on launch failure", got)
	}
	if !obs.closed {
		t.Errorf("observer was never Closed after a launch failure")
	}
}

// --- RemoteECPPlayerTarget.Pair: relay-observed mode ---------------------

func TestRemoteECPPlayerTargetPairRelayObservedModeCompleted(t *testing.T) {
	// pairCallCount starts at 0 (a PRIOR attempt, not this one) so Pair's
	// baseline capture is exercised for real; onLaunch simulates the launched
	// device's own dial reaching the relay exactly once, recording the
	// response THIS attempt caused.
	relay := &fakeRelay{}
	launcher := &fakeLauncher{onLaunch: func() {
		relay.pairCallCount++
		relay.pairResponses = append(relay.pairResponses, []byte(`{"channel_token":"tok-123"}`))
	}}
	target := &RemoteECPPlayerTarget{
		cfg:      RemoteTargetConfig{}.withDefaults(),
		launcher: launcher,
		relay:    relay,
		// dialObserver left nil => relay-observed mode.
	}

	if target.DriveablePLY057() {
		t.Errorf("DriveablePLY057() = true, want false in every mode (see package doc)")
	}
	if got, want := target.Name(), "roku-ecp:(relay-observed)"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	got := target.Pair("some-code")
	if !got.Completed {
		t.Errorf("Pair = %+v, want Completed", got)
	}
	if got.CommitmentMismatch {
		t.Errorf("relay-observed mode must never report CommitmentMismatch, got %+v", got)
	}
}

func TestRemoteECPPlayerTargetPairRelayObservedModeRejectedNoToken(t *testing.T) {
	relay := &fakeRelay{}
	launcher := &fakeLauncher{onLaunch: func() {
		relay.pairCallCount++
		relay.pairResponses = append(relay.pairResponses, []byte(`{}`))
	}}
	target := &RemoteECPPlayerTarget{
		cfg:      RemoteTargetConfig{}.withDefaults(),
		launcher: launcher,
		relay:    relay,
	}

	got := target.Pair("some-code")
	if !got.Rejected {
		t.Errorf("Pair = %+v, want Rejected (response carried no channel_token)", got)
	}
}

// TestRemoteECPPlayerTargetPairRelayObservedModeIgnoresStaleCall proves the
// per-attempt correlation fix directly: a call already recorded BEFORE this
// attempt's Launch (e.g. a previous, unrelated attempt) must not be mistaken
// for THIS attempt's own outcome — Pair must wait for PairCallCount to
// advance PAST its pre-Launch baseline, not merely become nonzero.
func TestRemoteECPPlayerTargetPairRelayObservedModeIgnoresStaleCall(t *testing.T) {
	// One call already on the books before this attempt even launches, with a
	// response that would misclassify as Rejected if (incorrectly) read as
	// "the" latest response.
	relay := &fakeRelay{pairCallCount: 1, pairResponses: [][]byte{[]byte(`{}`)}}
	launcher := &fakeLauncher{onLaunch: func() {
		relay.pairCallCount++
		relay.pairResponses = append(relay.pairResponses, []byte(`{"channel_token":"tok-456"}`))
	}}
	target := &RemoteECPPlayerTarget{
		cfg:      RemoteTargetConfig{}.withDefaults(),
		launcher: launcher,
		relay:    relay,
	}

	got := target.Pair("some-code")
	if !got.Completed {
		t.Errorf("Pair = %+v, want Completed (must read back THIS attempt's own response, not the stale prior one)", got)
	}
}

// TestAwaitViaRelayTimeout drives awaitViaRelay directly with an
// already-elapsed deadline so the timeout path is deterministic (no real
// relayPollInterval sleep needed to observe it).
func TestAwaitViaRelayTimeout(t *testing.T) {
	target := &RemoteECPPlayerTarget{relay: &fakeRelay{}}
	got := target.awaitViaRelay(time.Now().Add(-time.Millisecond), 0)
	if !got.Rejected {
		t.Errorf("awaitViaRelay = %+v, want Rejected on an already-elapsed deadline", got)
	}
}

// --- RemoteEnvFromEnv -----------------------------------------------------

// clearRemoteEnv unsets every env var RemoteEnvFromEnv reads, via t.Setenv
// (auto-restored at test end) — the disabled-by-default baseline every case
// below starts from, then overrides only what it needs.
func clearRemoteEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envTarget, envHost, envECPPort, envChannel, envDebugPort,
		envObserveTimeout, envRelayBindHost, envRelayDialHost,
	} {
		t.Setenv(k, "")
	}
}

func TestRemoteEnvFromEnvDisabledByDefault(t *testing.T) {
	clearRemoteEnv(t)

	env, enabled, err := RemoteEnvFromEnv()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if enabled {
		t.Errorf("enabled = true, want false when %s is unset", envTarget)
	}
	if env != (RemoteEnv{}) {
		t.Errorf("env = %+v, want the zero value", env)
	}
}

func TestRemoteEnvFromEnvRequiresHost(t *testing.T) {
	clearRemoteEnv(t)
	t.Setenv(envTarget, "roku")
	t.Setenv(envRelayDialHost, "192.168.50.12")

	_, enabled, err := RemoteEnvFromEnv()
	if !enabled {
		t.Errorf("enabled = false, want true (%s=roku was requested)", envTarget)
	}
	if err == nil {
		t.Errorf("err = nil, want a configuration error for missing %s", envHost)
	}
}

func TestRemoteEnvFromEnvRequiresRelayDialHost(t *testing.T) {
	clearRemoteEnv(t)
	t.Setenv(envTarget, "roku")
	t.Setenv(envHost, "192.168.50.99")

	_, enabled, err := RemoteEnvFromEnv()
	if !enabled {
		t.Errorf("enabled = false, want true (%s=roku was requested)", envTarget)
	}
	if err == nil {
		t.Errorf("err = nil, want a configuration error for missing %s", envRelayDialHost)
	}
}

func TestRemoteEnvFromEnvFullConfigDefaults(t *testing.T) {
	clearRemoteEnv(t)
	t.Setenv(envTarget, "roku")
	t.Setenv(envHost, "192.168.50.99")
	t.Setenv(envRelayDialHost, "192.168.50.12")

	env, enabled, err := RemoteEnvFromEnv()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !enabled {
		t.Fatalf("enabled = false, want true")
	}
	want := RemoteEnv{
		Target: RemoteTargetConfig{
			Host:           "192.168.50.99",
			ECPPort:        defaultECPPort,
			Channel:        defaultChannel,
			DebugPort:      defaultDebugPort,
			LaunchTimeout:  defaultLaunchTimeout,
			ObserveTimeout: defaultObserveTimeout,
		},
		RelayBindHost: "0.0.0.0", // default when WAIVEO_CONF_RELAY_BIND_HOST is unset
		RelayDialHost: "192.168.50.12",
	}
	if env != want {
		t.Errorf("env = %+v, want %+v", env, want)
	}
}

func TestRemoteEnvFromEnvDebugPortOff(t *testing.T) {
	clearRemoteEnv(t)
	t.Setenv(envTarget, "roku")
	t.Setenv(envHost, "192.168.50.99")
	t.Setenv(envRelayDialHost, "192.168.50.12")
	t.Setenv(envDebugPort, "off")

	env, enabled, err := RemoteEnvFromEnv()
	if err != nil || !enabled {
		t.Fatalf("RemoteEnvFromEnv: enabled=%v err=%v", enabled, err)
	}
	if !env.Target.NoDebugConsole {
		t.Errorf("NoDebugConsole = false, want true (%s=off)", envDebugPort)
	}
}

func TestRemoteEnvFromEnvBadECPPort(t *testing.T) {
	clearRemoteEnv(t)
	t.Setenv(envTarget, "roku")
	t.Setenv(envHost, "192.168.50.99")
	t.Setenv(envRelayDialHost, "192.168.50.12")
	t.Setenv(envECPPort, "not-a-number")

	_, enabled, err := RemoteEnvFromEnv()
	if !enabled {
		t.Errorf("enabled = false, want true")
	}
	if err == nil {
		t.Errorf("err = nil, want a parse error for %s=not-a-number", envECPPort)
	}
}

func TestRemoteEnvFromEnvBadObserveTimeout(t *testing.T) {
	clearRemoteEnv(t)
	t.Setenv(envTarget, "roku")
	t.Setenv(envHost, "192.168.50.99")
	t.Setenv(envRelayDialHost, "192.168.50.12")
	t.Setenv(envObserveTimeout, "not-a-duration")

	_, enabled, err := RemoteEnvFromEnv()
	if !enabled {
		t.Errorf("enabled = false, want true")
	}
	if err == nil {
		t.Errorf("err = nil, want a parse error for %s=not-a-duration", envObserveTimeout)
	}
}

func TestRemoteEnvFromEnvExplicitOverrides(t *testing.T) {
	clearRemoteEnv(t)
	t.Setenv(envTarget, "roku")
	t.Setenv(envHost, "192.168.50.99")
	t.Setenv(envRelayDialHost, "192.168.50.12")
	t.Setenv(envRelayBindHost, "192.168.50.12") // e.g. a box with exactly one usable interface
	t.Setenv(envECPPort, "9060")
	t.Setenv(envChannel, "prod")
	t.Setenv(envDebugPort, "9085")
	t.Setenv(envObserveTimeout, "30s")

	env, enabled, err := RemoteEnvFromEnv()
	if err != nil || !enabled {
		t.Fatalf("RemoteEnvFromEnv: enabled=%v err=%v", enabled, err)
	}
	want := RemoteEnv{
		Target: RemoteTargetConfig{
			Host:           "192.168.50.99",
			ECPPort:        9060,
			Channel:        "prod",
			DebugPort:      9085,
			LaunchTimeout:  defaultLaunchTimeout,
			ObserveTimeout: 30 * time.Second,
		},
		RelayBindHost: "192.168.50.12",
		RelayDialHost: "192.168.50.12",
	}
	if env != want {
		t.Errorf("env = %+v, want %+v", env, want)
	}
}

// --- ply057Driveable / pendPLY057NotDriveable (driver.go) ----------------

// capabilityFakeTarget is a minimal PlayerTarget (+ optionally
// PLY057Driveable) fake for exercising driver.go's target-capability gate
// directly, without booting the in-process relay.
type capabilityFakeTarget struct {
	name      string
	driveable bool
}

func (f capabilityFakeTarget) Name() string           { return f.name }
func (f capabilityFakeTarget) Pair(string) PairResult { return PairResult{Completed: true} }
func (f capabilityFakeTarget) DriveablePLY057() bool  { return f.driveable }

// noInterfaceTarget implements ONLY PlayerTarget — no PLY057Driveable at
// all — mirroring VirtualPlayerTarget's actual shape, so ply057Driveable's
// "not-implementing = assumed driveable" fallback is exercised against a
// target that genuinely lacks the interface, not just one that reports true.
type noInterfaceTarget struct{}

func (noInterfaceTarget) Name() string           { return "no-interface-fake" }
func (noInterfaceTarget) Pair(string) PairResult { return PairResult{Completed: true} }

func TestPLY057Driveable(t *testing.T) {
	if !ply057Driveable(noInterfaceTarget{}) {
		t.Errorf("a target not implementing PLY057Driveable must be assumed driveable")
	}
	if !ply057Driveable(capabilityFakeTarget{driveable: true}) {
		t.Errorf("a target reporting driveable=true must be reported driveable")
	}
	if ply057Driveable(capabilityFakeTarget{driveable: false}) {
		t.Errorf("a target reporting driveable=false must be reported NOT driveable")
	}
}

func TestPendPLY057NotDriveableRecordsExactlyPLY057Pending(t *testing.T) {
	cases, err := LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	var rep report.Report
	pendPLY057NotDriveable(&rep, capabilityFakeTarget{name: "fake-relay-observed", driveable: false}, cases)

	ids := rep.PendingIDs()
	if len(ids) != 1 || ids[0] != "PLY-057-invalid-oob-authentication-mismatch-rejected" {
		t.Errorf("PendingIDs() = %v, want exactly [PLY-057-invalid-oob-authentication-mismatch-rejected]", ids)
	}
	if len(rep.Cases) != 1 || rep.Cases[0].Reason == "" {
		t.Errorf("expected exactly one PENDING case with a non-empty reason, got %+v", rep.Cases)
	}
}
