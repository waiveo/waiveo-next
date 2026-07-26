// remotetarget.go makes the player/1 driver's pairing-subset PlayerTarget
// pluggable toward an actual on-LAN player-v3 device, not just the in-process
// VirtualPlayerTarget — the "later wave... plugging in a different
// PlayerTarget" this package's own doc comment always anticipated.
//
// # Driving a real device
//
// player-v3/source/Main.brs's wvNormalizeArgs takes Roku's launch args (an
// roArray whose first element is the roAssociativeArray of launch params) and
// wvArgValue(params, "pairingCode") reads a "pairingCode" key out of it
// (Main.brs then sets scene.pairingCode from that value). ECP's own launch
// deep-link (`POST /launch/<channel>?k=v...`) is exactly how Roku turns query
// params into that launch-args AA, so RemoteECPPlayerTarget drives a pairing
// attempt by POSTing `/launch/<channel>?pairingCode=<code>` at the device's
// ECP port (httpECPLauncher) — the identical contract a real operator's
// pairing-code entry (or a signage-fleet automation) would use.
//
// # Observing the outcome, and why PLY-057 is the hard case
//
// A real device cannot be introspected in-process like VirtualPlayerTarget;
// RemoteECPPlayerTarget has two observation modes:
//
//   - debug-console mode (default): dials the device's BrightScript remote
//     debug console (telnet, default port 8085 — the same one
//     `docs`/memory's Roku fleet dev access already assumes is reachable)
//     BEFORE launching, then reads player-v3's own print statements
//     (PlayerTask.brs) for a terminal status line. This is the mode that can
//     drive ALL THREE pairing-subset cases (PLY-050/055/057), including the
//     commitment-mismatch rejection, because player-v3 itself prints the
//     exact outcome ("pairing FAILED: fingerprint commitment MISMATCH…") —
//     see telnetDebugConsole's own doc.
//   - relay-observed mode (RemoteTargetConfig.NoDebugConsole): infers the
//     outcome purely from the SAME Relay wire recorder the driver already
//     uses for the differential oracle (Relay.PairCallCount/PairResponses) —
//     usable when the device's debug console is unreachable (e.g.
//     firewalled off), but it CANNOT distinguish a commitment-mismatched
//     pairing code from a correct one: player-v3/source/Pairing.brs performs
//     its bootstrap POST to /player/v1/pair with TLS verification DISABLED
//     (peerVerify: false, hostVerify: false) and only compares the OOB
//     fingerprint_commitment against the response's own trust_anchors AFTER
//     that POST has already completed — so the relay redeems the SAME
//     one-time grant and returns the SAME channel_token whether the code was
//     correct or commitment-mismatched; the rejection is decided entirely
//     inside the device, with nothing distinguishing it on the wire the relay
//     can see. SupportsCommitmentMismatchDetection reports false in this
//     mode, and Run (driver.go) correspondingly records PLY-057 PENDING
//     rather than driving it to a guaranteed-wrong FAIL.
//
// Per-case remote-driveability, stated plainly:
//   - PLY-055 (cross-VLAN manual-entry commitment happy path): remote-driveable
//     in BOTH modes — the assertions it needs (grant_selector on the wire,
//     fingerprint_commitment never on the wire, a channel_token issued, a
//     screen_id present) are all relay-side wire observations already, and
//     "commitment_match=true" is exactly "the attempt Completed".
//   - PLY-050 (TOFU happy-path subset actually driven): remote-driveable in
//     BOTH modes, for the same reason — see drivePLY050's own doc for the
//     TOFU-specific fields this package has never asserted against ANY
//     target, virtual or real (player-v3 always performs a commitment check,
//     same as VirtualPlayerTarget).
//   - PLY-057 (OOB commitment-mismatch rejection): remote-driveable ONLY in
//     debug-console mode, for the reason above. In relay-observed mode it is
//     PENDING, not driven.
//
// # Env-gated wiring
//
// RemoteEnvFromEnv reads WAIVEO_CONF_PLAYER_TARGET and friends; CI never sets
// it, so every existing test's behavior (including the corpus PASS/PENDING
// set TestPlayer1DriverGreen/TestPlayer1CorpusFullyAccountedFor assert) is
// completely unchanged. See RemoteEnvFromEnv's own doc for the full env var
// list.
package player1

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Default tuning for a RemoteTargetConfig's zero-valued fields — see
// RemoteTargetConfig.withDefaults.
const (
	defaultECPPort        = 8060 // Roku ECP's well-known port (matches internal/relay/ecp's own defaultPort).
	defaultDebugPort      = 8085 // Roku's BrightScript remote debug console (telnet) port.
	defaultChannel        = "dev"
	defaultLaunchTimeout  = 5 * time.Second
	defaultObserveTimeout = 20 * time.Second
	relayPollInterval     = 200 * time.Millisecond
)

// RemoteTargetConfig configures RemoteECPPlayerTarget. The zero value plus
// withDefaults() is a usable configuration for a device at Host reachable on
// its default ECP/debug ports, sideloaded to the "dev" channel slot.
type RemoteTargetConfig struct {
	// Host is the on-LAN player-v3 device's IP or hostname.
	Host string
	// ECPPort is the device's ECP control port. 0 => defaultECPPort (8060).
	ECPPort int
	// Channel is the sideloaded channel id RemoteECPPlayerTarget launches.
	// "" => defaultChannel ("dev", Roku's standard sideload slot).
	Channel string
	// DebugPort is the device's BrightScript remote debug console (telnet)
	// port. 0 => defaultDebugPort (8085). Ignored when NoDebugConsole.
	DebugPort int
	// NoDebugConsole switches RemoteECPPlayerTarget to relay-observed mode
	// (see package doc): usable when the device's debug console is
	// unreachable, at the cost of PLY-057 becoming PENDING instead of driven.
	NoDebugConsole bool
	// LaunchTimeout bounds a single ECP launch POST. 0 => defaultLaunchTimeout.
	LaunchTimeout time.Duration
	// ObserveTimeout bounds waiting for a terminal outcome after launch.
	// 0 => defaultObserveTimeout.
	ObserveTimeout time.Duration
}

// withDefaults returns cfg with every zero-valued tunable field replaced by
// its default — Host/Channel/NoDebugConsole are left exactly as given (there
// is no sensible non-empty default for Host, and NoDebugConsole's zero value,
// false, already IS its default).
func (cfg RemoteTargetConfig) withDefaults() RemoteTargetConfig {
	if cfg.ECPPort == 0 {
		cfg.ECPPort = defaultECPPort
	}
	if cfg.Channel == "" {
		cfg.Channel = defaultChannel
	}
	if cfg.DebugPort == 0 {
		cfg.DebugPort = defaultDebugPort
	}
	if cfg.LaunchTimeout == 0 {
		cfg.LaunchTimeout = defaultLaunchTimeout
	}
	if cfg.ObserveTimeout == 0 {
		cfg.ObserveTimeout = defaultObserveTimeout
	}
	return cfg
}

// RemoteEnv is the full environment-derived configuration for driving the
// player/1 pairing subset against an actual on-LAN player-v3 device instead
// of the in-process virtual player.
type RemoteEnv struct {
	Target RemoteTargetConfig
	// RelayBindHost is the interface the in-process feeder+relay bind to
	// (InProcessRelayOption WithBindHost) — a real device cannot reach a
	// loopback-only listener.
	RelayBindHost string
	// RelayDialHost is this box's LAN-reachable address (WithDialHost),
	// embedded into formed pairing codes and content-origin URLs a real
	// device dials.
	RelayDialHost string
}

// The env vars RemoteEnvFromEnv reads. WAIVEO_CONF_PLAYER_TARGET is the
// single gate: every other var is read (and, for the two Required ones,
// validated) ONLY when it equals "roku" — CI never sets it, so a driver test
// that calls RemoteEnvFromEnv and finds enabled==false runs the identical
// CI-default path it always has.
const (
	envTarget         = "WAIVEO_CONF_PLAYER_TARGET"          // must equal "roku" to enable remote-target mode at all
	envHost           = "WAIVEO_CONF_PLAYER_ROKU_HOST"       // required: the device's IP/hostname
	envECPPort        = "WAIVEO_CONF_PLAYER_ROKU_ECP_PORT"   // optional int, default 8060
	envChannel        = "WAIVEO_CONF_PLAYER_ROKU_CHANNEL"    // optional, default "dev"
	envDebugPort      = "WAIVEO_CONF_PLAYER_ROKU_DEBUG_PORT" // optional int, default 8085; "off" => NoDebugConsole
	envObserveTimeout = "WAIVEO_CONF_PLAYER_ROKU_TIMEOUT"    // optional Go duration (e.g. "20s"), default 20s
	envRelayBindHost  = "WAIVEO_CONF_RELAY_BIND_HOST"        // optional, default "0.0.0.0"
	envRelayDialHost  = "WAIVEO_CONF_RELAY_DIAL_HOST"        // required: this box's LAN-reachable address
)

// RemoteEnvFromEnv reads the process environment and reports whether
// remote-target mode is requested at all (envTarget == "roku"). When it is
// not, (RemoteEnv{}, false, nil) is returned and nothing else is read or
// validated — this is the branch every existing CI run takes, so CI
// behavior is completely untouched by this package's existence.
//
// When envTarget == "roku", envHost and envRelayDialHost are required (there
// is no safe default for either); a missing one is a configuration error, not
// a silent fall-back to CI-default behavior, since the caller explicitly
// opted in.
func RemoteEnvFromEnv() (env RemoteEnv, enabled bool, err error) {
	if os.Getenv(envTarget) != "roku" {
		return RemoteEnv{}, false, nil
	}

	host := os.Getenv(envHost)
	if host == "" {
		return RemoteEnv{}, true, fmt.Errorf("%s=roku requires %s (the on-LAN player-v3 device's IP/hostname)", envTarget, envHost)
	}
	dialHost := os.Getenv(envRelayDialHost)
	if dialHost == "" {
		return RemoteEnv{}, true, fmt.Errorf("%s=roku requires %s (this box's LAN-reachable address, embedded into pairing codes and content URLs the device dials)", envTarget, envRelayDialHost)
	}
	bindHost := os.Getenv(envRelayBindHost)
	if bindHost == "" {
		bindHost = "0.0.0.0"
	}

	cfg := RemoteTargetConfig{Host: host, Channel: os.Getenv(envChannel)}

	if v := os.Getenv(envECPPort); v != "" {
		p, perr := strconv.Atoi(v)
		if perr != nil {
			return RemoteEnv{}, true, fmt.Errorf("%s=%q: %w", envECPPort, v, perr)
		}
		cfg.ECPPort = p
	}
	if v := os.Getenv(envDebugPort); v != "" {
		if strings.EqualFold(v, "off") {
			cfg.NoDebugConsole = true
		} else {
			p, perr := strconv.Atoi(v)
			if perr != nil {
				return RemoteEnv{}, true, fmt.Errorf("%s=%q: %w (or \"off\" to disable debug-console mode)", envDebugPort, v, perr)
			}
			cfg.DebugPort = p
		}
	}
	if v := os.Getenv(envObserveTimeout); v != "" {
		d, derr := time.ParseDuration(v)
		if derr != nil {
			return RemoteEnv{}, true, fmt.Errorf("%s=%q: %w", envObserveTimeout, v, derr)
		}
		cfg.ObserveTimeout = d
	}

	return RemoteEnv{Target: cfg.withDefaults(), RelayBindHost: bindHost, RelayDialHost: dialHost}, true, nil
}

// ecpLauncher is the seam RemoteECPPlayerTarget drives a device's ECP launch
// deep-link through. httpECPLauncher is the real implementation;
// remotetarget_internal_test.go unit-tests Pair's own logic against a fake.
type ecpLauncher interface {
	// Launch POSTs an ECP launch deep-link for channel carrying args as
	// launch-arg query params (player-v3's Main.brs reads them via
	// wvArgValue against the launch-args AA Roku hands it).
	Launch(channel string, args map[string]string) error
}

// outcomeObserver is the seam RemoteECPPlayerTarget reads a driven attempt's
// terminal outcome through, after Launch. telnetDebugConsole is the real
// debug-console-mode implementation.
type outcomeObserver interface {
	AwaitOutcome(deadline time.Time) PairResult
	Close() error
}

// httpECPLauncher is the real ecpLauncher: a plain HTTP POST against a Roku
// device's ECP port, mirroring internal/relay/ecp.Controller's own launch
// dispatch (`POST /launch/<channel>`), extended with the extra launch-arg
// query params (e.g. pairingCode) a conformance drive needs that REG-066's
// production launch command does not carry today.
type httpECPLauncher struct {
	addr   string // host:port
	client *http.Client
}

// Launch implements ecpLauncher.
func (l *httpECPLauncher) Launch(channel string, args map[string]string) error {
	q := make(url.Values, len(args))
	for k, v := range args {
		q.Set(k, v)
	}
	reqURL := fmt.Sprintf("http://%s/launch/%s", l.addr, url.PathEscape(channel))
	if enc := q.Encode(); enc != "" {
		reqURL += "?" + enc
	}

	req, err := http.NewRequest(http.MethodPost, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build ECP launch request: %w", err)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("ECP launch %s: %w", reqURL, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ECP launch %s: status %d", reqURL, resp.StatusCode)
	}
	return nil
}

// telnetDebugConsole reads a real Roku's BrightScript remote debug console —
// plain text, one line per `print`, no telnet option negotiation required for
// Roku's implementation — over a raw TCP connection, classifying player-v3's
// own terminal status lines (PlayerTask.brs's print statements, quoted
// exactly in classifyPlayerV3Line) into a PairResult.
//
// This is the ONLY network-observable signal of player-v3's client-local
// decisions. See Pairing.brs: the bootstrap POST to /player/v1/pair
// completes with certificate verification DISABLED, and the OOB
// fingerprint-commitment check happens AFTER that response, entirely inside
// the device — so the relay's own wire view is identical whether the code
// was correct or commitment-mismatched. Reading the device's own debug print
// of "pairing FAILED: fingerprint commitment MISMATCH…" is the only way this
// driver can tell the two apart against real hardware, exactly as a developer
// would by running `telnet <roku-ip> 8085`.
type telnetDebugConsole struct {
	conn net.Conn
	r    *bufio.Reader
}

// dialTelnetDebugConsole dials addr (host:port) and returns a ready
// telnetDebugConsole. The caller MUST Close it.
func dialTelnetDebugConsole(addr string, dialTimeout time.Duration) (*telnetDebugConsole, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial debug console %s: %w", addr, err)
	}
	return &telnetDebugConsole{conn: conn, r: bufio.NewReader(conn)}, nil
}

// Close implements outcomeObserver.
func (c *telnetDebugConsole) Close() error { return c.conn.Close() }

// AwaitOutcome implements outcomeObserver: it reads lines until one matches a
// player-v3 terminal status line (classifyPlayerV3Line) or deadline elapses,
// never inspecting anything but text the device itself chose to print —
// exactly the black-box observation PlayerTarget.Pair's own doc requires.
func (c *telnetDebugConsole) AwaitOutcome(deadline time.Time) PairResult {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return PairResult{Rejected: true, Err: "debug console: no terminal player-v3 status line observed before the deadline"}
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(remaining))
		line, err := c.r.ReadString('\n')
		if line != "" {
			if res, ok := classifyPlayerV3Line(line); ok {
				return res
			}
		}
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue // deadline check above will end the loop
			}
			return PairResult{Rejected: true, Err: fmt.Sprintf("debug console: %v", err)}
		}
	}
}

// classifyPlayerV3Line matches one line of player-v3's own debug output to a
// terminal PairResult, quoting PlayerTask.brs's/Pairing.brs's exact,
// committed print strings. Non-terminal lines (e.g. "pairing…", "pulling
// program…") return ok=false so AwaitOutcome keeps reading.
func classifyPlayerV3Line(line string) (PairResult, bool) {
	switch {
	case strings.Contains(line, "PHOTON — image verified"):
		// Program.brs's full thread completed: paired, leased, fetched, and
		// content-address verified (PlayerTask.brs's final print).
		return PairResult{Completed: true}, true
	case strings.Contains(line, "pairing FAILED"):
		// PlayerTask.brs prints "pairing FAILED: " + pair.error; Pairing.brs's
		// own commitment-mismatch error text is quoted verbatim here.
		mismatch := strings.Contains(line, "fingerprint commitment MISMATCH")
		return PairResult{Rejected: true, CommitmentMismatch: mismatch, Err: strings.TrimSpace(line)}, true
	case strings.Contains(line, "paired (screen"):
		// PlayerTask.brs's "paired (screen "+screenId+"), token persisted":
		// redemption + channel-token issuance succeeded. Program.brs has not
		// necessarily run yet, but PLY-050/055 assert redemption/token
		// issuance, not the full render — sufficient for both.
		return PairResult{Completed: true}, true
	}
	return PairResult{}, false
}

// RemoteECPPlayerTarget is the pluggable PlayerTarget that drives an actual
// on-LAN player-v3 device — see this file's own package doc for the full
// design (launch mechanism, the two observation modes, and per-case
// remote-driveability).
type RemoteECPPlayerTarget struct {
	cfg      RemoteTargetConfig
	launcher ecpLauncher
	relay    Relay // used only in relay-observed mode (cfg.NoDebugConsole)

	// dialObserver is nil in relay-observed mode; otherwise it dials a fresh
	// outcomeObserver for one Pair attempt (production: telnetDebugConsole;
	// tests: a fake). A fresh dial per attempt avoids stale buffered lines
	// from a previous case's run bleeding into this one's classification.
	dialObserver func() (outcomeObserver, error)
}

// NewRemoteECPPlayerTarget builds the production RemoteECPPlayerTarget for
// cfg. relay is REQUIRED in relay-observed mode (cfg.NoDebugConsole) — it is
// how that mode infers an outcome — and is otherwise unused.
func NewRemoteECPPlayerTarget(cfg RemoteTargetConfig, relay Relay) *RemoteECPPlayerTarget {
	cfg = cfg.withDefaults()
	t := &RemoteECPPlayerTarget{
		cfg: cfg,
		launcher: &httpECPLauncher{
			addr:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.ECPPort)),
			client: &http.Client{Timeout: cfg.LaunchTimeout},
		},
		relay: relay,
	}
	if !cfg.NoDebugConsole {
		debugAddr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.DebugPort))
		t.dialObserver = func() (outcomeObserver, error) {
			return dialTelnetDebugConsole(debugAddr, cfg.LaunchTimeout)
		}
	}
	return t
}

// Name implements PlayerTarget.
func (t *RemoteECPPlayerTarget) Name() string {
	mode := "debug-console"
	if t.dialObserver == nil {
		mode = "relay-observed"
	}
	return fmt.Sprintf("roku-ecp:%s(%s)", t.cfg.Host, mode)
}

// SupportsCommitmentMismatchDetection implements CommitmentMismatchCapable —
// see this file's own package doc for why only debug-console mode can.
func (t *RemoteECPPlayerTarget) SupportsCommitmentMismatchDetection() bool {
	return t.dialObserver != nil
}

// Pair implements PlayerTarget: it launches the device's channel over ECP
// with pairingCode as a launch-arg deep-link, then observes the outcome
// (debug console, or the relay's own wire view — see this file's own package
// doc).
func (t *RemoteECPPlayerTarget) Pair(pairingCode string) PairResult {
	var obs outcomeObserver
	if t.dialObserver != nil {
		o, err := t.dialObserver()
		if err != nil {
			return PairResult{Rejected: true, Err: fmt.Sprintf("dial debug console: %v", err)}
		}
		obs = o
		defer func() { _ = obs.Close() }()
	}

	if err := t.launcher.Launch(t.cfg.Channel, map[string]string{"pairingCode": pairingCode}); err != nil {
		return PairResult{Rejected: true, Err: fmt.Sprintf("ECP launch: %v", err)}
	}

	deadline := time.Now().Add(t.cfg.ObserveTimeout)
	if obs != nil {
		return obs.AwaitOutcome(deadline)
	}
	return t.awaitViaRelay(deadline)
}

// awaitViaRelay implements relay-observed mode: it polls the Relay's own wire
// recorder until it sees a /player/v1/pair call this attempt caused, or
// deadline elapses. It can report Completed/Rejected but never
// CommitmentMismatch — see SupportsCommitmentMismatchDetection.
func (t *RemoteECPPlayerTarget) awaitViaRelay(deadline time.Time) PairResult {
	for {
		if t.relay.PairCallCount() > 0 {
			resps := t.relay.PairResponses()
			last := resps[len(resps)-1]
			if jsonNonEmptyString(last, "channel_token") {
				return PairResult{Completed: true}
			}
			return PairResult{Rejected: true, Err: "relay observed a /player/v1/pair response without a channel_token"}
		}
		if time.Now().After(deadline) {
			return PairResult{Rejected: true, Err: "timed out: relay observed no /player/v1/pair call for this attempt"}
		}
		time.Sleep(relayPollInterval)
	}
}
