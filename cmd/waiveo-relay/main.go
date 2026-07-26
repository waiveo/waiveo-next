// Command waiveo-relay is the Wave-1 skeleton for the relay component: a Go
// process that will speak the relay/1 protocol (contracts/relay-1.md) to an
// app peer and the player over its own future channels. On start, it opens
// its persistent operational identity store (internal/relay/identity),
// enrolls against the co-located feeder (internal/relay/enroll, relay/1
// REL-010–014) if it hasn't already — persisting the feeder's own
// desired-state signing key as its enrollment-anchored trust anchor
// (REL-071, `#28`) — and pulls + verifies the feeder's signed desired-state
// snapshot (internal/relay/desiredstate, REL-051/052/055/071/072),
// persisting last-applied and holding the resulting applied screen-program
// in memory. If the app peer is unreachable at boot but a prior pull already
// persisted a last-applied generation, the handshake/pull failure is NOT fatal:
// the relay proceeds and serves that durable copy offline (REL-055/061 offline
// continuity), so a restart during a disconnection still serves screens. It
// then serves player/1's pairing surface
// (internal/relay/playerserver, PLY-030–037) over its own HTTPS listener,
// using the exact same certificate — the enrollment identity persisted in
// its identity store — that FormPairingCode's commitment (REL-126) is
// computed over, so a player's local PLY-052 comparison is always checking
// the cert this listener actually presents.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/discovery"
	"github.com/maaxton/waiveo-next/internal/relay/ecp"
	"github.com/maaxton/waiveo-next/internal/relay/ecppoll"
	"github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/keepalive"
	"github.com/maaxton/waiveo-next/internal/relay/mdns"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
	"github.com/maaxton/waiveo-next/internal/relay/ssdpresponder"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/relay/telemetryhttp"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// buildVersion/buildChannel are this binary's channel-index/1 identity
// (contracts/channel-index.md): the exact `{version}` entry, on the exact
// `channel` (CHI-040), that scripts/install-from-channel-index.sh resolved
// and installed this binary from. Both default to "dev" for an ordinary
// `go build`/`go run` (Wave-1 first-photon's own dev/CI binary keeps today's
// identity-free behavior byte-for-byte); a released build overrides both via
// -ldflags at build time -- see scripts/install-from-channel-index.sh's own
// header comment for the exact invocation this mirrors.
var (
	buildVersion = "dev"
	buildChannel = "dev"
)

// printVersion writes this binary's channel-index/1 identity
// (buildVersion/buildChannel) to w in the one line both the --version flag
// and the boot-time startup log (main, below) print, so a deployed unit's
// answer to "what did I install" reads identically whether taken from a
// live process's stdout or its own log.
func printVersion(w io.Writer, version, channel string) {
	fmt.Fprintf(w, "waiveo-relay %s (channel %s)\n", version, channel)
}

// config is the relay's deployment-time addressing. Defaults keep the Wave-1
// loopback dev/CI behavior byte-identical; the on-box deployment overrides
// listen (bind LAN-reachable) and pairHost/pairPort (what a formed pairing code
// tells a screen to dial, REL-126/PLY-024) so a Roku on the LAN can actually
// reach this relay and use the code. feederURL stays loopback on-box (the relay
// and feeder are co-located).
type config struct {
	listen    string // TCP bind address for the player/1 HTTPS listener
	feederURL string // co-located feeder base URL for enroll + desired-state pull
	pairHost  string // dial host a formed pairing code encodes
	pairPort  int    // dial port a formed pairing code encodes

	// Hardware device plane (all optional; absent → the loopback stand-ins,
	// byte-identical dev/CI behavior). ecpTargets maps entity_id → the LAN
	// Roku its device_commands dispatch to AND its state is polled from
	// (WAIVEO_RELAY_ECP_TARGETS="entity=host[:port],..."). pollInterval is
	// the ECP state-poll period (WAIVEO_RELAY_POLL_MS, default 5000).
	// discoveryOn enables the SSDP client sweep feeding the candidate store
	// (WAIVEO_RELAY_DISCOVERY=1, REL-110/111). mdnsPatterns enables the mDNS
	// listener feeding the SAME candidate store (WAIVEO_RELAY_MDNS_PATTERNS,
	// comma-separated MAN-071 service-type strings e.g. "_waiveo._tcp";
	// empty/unset is off, internal/relay/mdns). ssdpAnnounce enables the
	// SSDP RESPONDER — answering a player's own M-SEARCH for this relay's
	// player/1 pairing surface (WAIVEO_RELAY_SSDP_ANNOUNCE=1, PLY-021/022).
	// keepaliveOn enables the screen keep-alive capability
	// (WAIVEO_RELAY_KEEPALIVE=1, internal/relay/keepalive, player/1
	// PLY-150-154): a second ECP poller over the SAME ecpTargets that
	// re-launches a screen's player channel once it safely idles at Home.
	// All four default off: CI and loopback dev runs must never multicast,
	// and must not dispatch an unrequested launch, so dev/CI stay
	// byte-identical to today.
	ecpTargets   map[string]ecp.Target
	pollInterval time.Duration
	discoveryOn  bool
	mdnsPatterns []string
	ssdpAnnounce bool
	keepaliveOn  bool
}

// loadConfig reads the relay config from env (os.Getenv in main), falling back
// to loopback defaults. Returns an error only on an unparseable pair port, so a
// misconfiguration fails fast at startup rather than emitting an unusable code.
func loadConfig(env func(string) string) (config, error) {
	portStr := envOr(env, "WAIVEO_RELAY_PAIR_PORT", "7421")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return config{}, fmt.Errorf("WAIVEO_RELAY_PAIR_PORT %q is not an integer: %w", portStr, err)
	}
	targets, err := parseECPTargets(env("WAIVEO_RELAY_ECP_TARGETS"))
	if err != nil {
		return config{}, err
	}
	pollMSStr := envOr(env, "WAIVEO_RELAY_POLL_MS", "5000")
	pollMS, err := strconv.Atoi(pollMSStr)
	if err != nil || pollMS <= 0 {
		return config{}, fmt.Errorf("WAIVEO_RELAY_POLL_MS %q is not a positive integer", pollMSStr)
	}
	return config{
		listen:       envOr(env, "WAIVEO_RELAY_LISTEN", "127.0.0.1:7421"),
		feederURL:    envOr(env, "WAIVEO_FEEDER_URL", "https://127.0.0.1:7420"),
		pairHost:     envOr(env, "WAIVEO_RELAY_PAIR_HOST", "127.0.0.1"),
		pairPort:     port,
		ecpTargets:   targets,
		pollInterval: time.Duration(pollMS) * time.Millisecond,
		discoveryOn:  env("WAIVEO_RELAY_DISCOVERY") == "1" || env("WAIVEO_RELAY_DISCOVERY") == "true",
		mdnsPatterns: parseMDNSPatterns(env("WAIVEO_RELAY_MDNS_PATTERNS")),
		ssdpAnnounce: env("WAIVEO_RELAY_SSDP_ANNOUNCE") == "1" || env("WAIVEO_RELAY_SSDP_ANNOUNCE") == "true",
		keepaliveOn:  env("WAIVEO_RELAY_KEEPALIVE") == "1" || env("WAIVEO_RELAY_KEEPALIVE") == "true",
	}, nil
}

// parseMDNSPatterns parses "svc1,svc2" into the mdns package's Config.Patterns
// input list of MAN-071 mdns service-type strings (nil for empty input, or
// input with no non-empty entry after trimming — e.g. "" or ","). Unlike
// parseECPTargets, no entry shape is rejected: any non-empty string is a
// usable service type, so there is nothing here to fail config load over.
func parseMDNSPatterns(raw string) []string {
	if raw == "" {
		return nil
	}
	var patterns []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		patterns = append(patterns, p)
	}
	return patterns
}

// parseECPTargets parses "entity=host[:port],entity2=host2" into the entity →
// ecp.Target map (nil for empty input). A malformed entry fails config load
// fast rather than silently dropping a device.
func parseECPTargets(raw string) (map[string]ecp.Target, error) {
	if raw == "" {
		return nil, nil
	}
	targets := make(map[string]ecp.Target)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		entityID, addr, ok := strings.Cut(entry, "=")
		if !ok || entityID == "" || addr == "" {
			return nil, fmt.Errorf("WAIVEO_RELAY_ECP_TARGETS entry %q is not entity=host[:port]", entry)
		}
		host, portStr, portErr := net.SplitHostPort(addr)
		if portErr != nil {
			// No port present — the whole addr is the host, port defaults in ecp.
			targets[entityID] = ecp.Target{Host: addr}
			continue
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 {
			return nil, fmt.Errorf("WAIVEO_RELAY_ECP_TARGETS entry %q has a bad port", entry)
		}
		targets[entityID] = ecp.Target{Host: host, Port: p}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	return targets, nil
}

func envOr(env func(string) string, key, def string) string {
	if v := env(key); v != "" {
		return v
	}
	return def
}

// enrollRetryBudget/enrollRetryInterval tolerate the feeder not being up
// yet the instant the relay process starts: the Makefile's dev-up backgrounds
// both binaries with no start-up ordering, and scripts/dev-smoke.sh already
// gives the pair up to ~10s to answer /healthz — this matches that budget
// rather than failing the relay outright on the first connection refused.
const (
	enrollRetryBudget   = 10 * time.Second
	enrollRetryInterval = 250 * time.Millisecond
)

func main() {
	versionFlag := flag.Bool("version", false, "print the relay's channel-index/1 build identity (version/channel) and exit")
	flag.Parse()
	if *versionFlag {
		printVersion(os.Stdout, buildVersion, buildChannel)
		return
	}
	log.Printf("waiveo-relay starting: version=%s channel=%s", buildVersion, buildChannel)

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("waiveo-relay: config: %v", err)
	}

	store, err := identity.Open(identity.DefaultPath)
	if err != nil {
		log.Fatalf("waiveo-relay: open identity store: %v", err)
	}
	defer store.Close()

	if err := enrollWithRetry(cfg.feederURL, store); err != nil {
		log.Fatalf("waiveo-relay: enroll: %v", err)
	}

	relayIdent, ok, err := store.Identity()
	if err != nil {
		log.Fatalf("waiveo-relay: read identity after enroll: %v", err)
	}
	if !ok {
		log.Fatalf("waiveo-relay: no persisted identity after enrollment")
	}
	log.Printf("waiveo-relay enrolled (relay_id %s)", relayIdent.RelayID)
	if key, ok, err := store.DesiredStateVerificationKey(); err != nil {
		log.Fatalf("waiveo-relay: read desired_state_verification_key after enroll: %v", err)
	} else if ok {
		log.Printf("waiveo-relay trust anchor learned (desired_state_verification_key %s)", hex.EncodeToString(key))
	}

	// Whether a prior successful pull already left a persisted last-applied
	// generation in the store decides how a boot-time app-peer failure below is
	// handled (REL-055/061): with a persisted snapshot to serve, an unreachable
	// app peer at boot — a restart during a WAN/app-peer disconnection, exactly
	// the case REL-055 offline continuity exists to cover — MUST NOT down the
	// process; the relay serves its persisted last-applied screen_programs
	// offline. With nothing ever persisted, a boot that cannot reach the app
	// peer genuinely has nothing to serve, and hello/Pull failure stays fatal.
	_, _, hasPersisted, err := store.LastAppliedGeneration()
	if err != nil {
		log.Fatalf("waiveo-relay: read last-applied generation: %v", err)
	}

	// Perform the relay/1 connection handshake immediately after enrollment and
	// before the desired-state pull (REL-030): the app peer challenges, this
	// relay signs the nonce with its enrollment key (channel binding, REL-032),
	// declares its version/features/site/subnet/clock, and adopts the app peer's
	// authoritative site_binding from hello-ack (REL-036). The adopted site
	// drives the edge engine's schedule/sun evaluation once the automation stack
	// boots below. A refusal or transport failure here is fatal ONLY when there
	// is no persisted snapshot to fall back on; otherwise the relay proceeds in
	// offline-serve mode (REL-055/061) with no live site adopted.
	site, err := helloWithRetry(cfg, relayIdent)
	helloOK := err == nil
	if err != nil {
		if fatal := offlineServeFallback(err, hasPersisted); fatal != nil {
			log.Fatalf("waiveo-relay: hello: %v", fatal)
		}
		log.Printf("waiveo-relay: hello failed (%v); serving persisted last-applied offline (REL-055/061)", err)
		site = hello.SiteBinding{}
	}

	// Pull + verify the feeder's signed desired-state snapshot against the
	// trust anchor enrollment just persisted. A failure here (app peer
	// unreachable, bad signature, tampered sections, or a regressed generation)
	// is fatal ONLY when there is no persisted snapshot to fall back on. When a
	// prior pull already persisted a last-applied generation, a failed pull is
	// non-fatal: the relay keeps that durable copy and serves it offline
	// (REL-055/061), rather than exiting and leaving every screen unable to
	// pull /player/v1/program.
	//
	// This pull is gated on helloOK: a Pull the app peer would sign is a
	// SEPARATE authorization decision from the hello handshake's own channel-
	// binding proof (REL-030/032), and a hello the peer just REFUSED (a
	// CHANNEL_BINDING_INVALID 403, or any other hello rejection) means this
	// relay does not hold a live, authorized session with that peer right
	// now. Pulling anyway would honor the peer's content signature (integrity
	// survives) while silently overriding the peer's own authorization
	// refusal — exactly the offline-continuity path this function just logged
	// it was taking. So a failed hello skips the live pull entirely and falls
	// straight to serving the persisted last-applied snapshot, instead of
	// reaching the feeder a second time on a session the peer just rejected.
	var applied desiredstate.Applied
	if !helloOK {
		log.Printf("waiveo-relay: skipping desired-state pull — hello was not accepted by the app peer; serving persisted last-applied offline (REL-055/061)")
	} else {
		applied, err = desiredstate.Pull(cfg.feederURL, store)
		if err != nil {
			if fatal := offlineServeFallback(err, hasPersisted); fatal != nil {
				log.Fatalf("waiveo-relay: pull desired state: %v", fatal)
			}
			log.Printf("waiveo-relay: pull desired state failed (%v); serving persisted last-applied offline (REL-055/061)", err)
			applied = desiredstate.Applied{}
		} else {
			log.Printf("waiveo-relay applied desired state generation %d (screen %s, program %s, image %s)",
				applied.Generation, applied.ScreenID, applied.ProgramRevision, applied.Image.AssetRef)
		}
	}

	// Reuse the enrollment identity read above (relayIdent) as the relay's
	// player/1 TLS identity too — the same certificate the handshake channel-
	// bound with.
	relayID := relayIdent

	// Boot the edge-automation stack from the SAME verified Applied value: compile
	// + load the signed edge_rules (REL-062) into the engine, wired to the device
	// plane (command dispatch) and the durable telemetry queue (automation.run,
	// REL-090/093). The four subsystems — engine, device plane, telemetry, and the
	// durable operational store — now run inside this one binary. The device-state
	// INPUT (the observations that fire a rule) is the injectable
	// automationhost.DeviceStateSource seam real polling/ECP will feed; that source
	// is hardware-gated and deferred, so the live binary loads the rules and stands
	// ready rather than driving a synthetic observation (the e2e test drives the
	// synthetic source to prove the wired stack fires).
	// The ONE canonical device-class registry (device-class-registry/1's own
	// built-in media-player content, REG-060-066) this relay resolves every
	// device_command against — the edge engine's (via bootAutomationStack) and
	// the schedule resolver's preset-firing surface (below) alike — so both
	// paths agree on exactly the same command vocabulary (REG-052/REL-113).
	deviceRegistry := deviceclass.Builtin()

	// Select the device plane's controller + resolver pair ONCE and use it for
	// BOTH dispatch paths (edge rules via bootAutomationStack, preset batches
	// via scheduleSink below), so a fired command resolves and dispatches
	// identically regardless of which engine fired it (REL-112/113/115).
	// Configured ECP targets swap in the real hardware adapter; otherwise the
	// loopback stand-ins keep dev/CI behavior byte-identical.
	var (
		devController  deviceplane.DeviceController = loopbackController{}
		baseController deviceplane.DeviceController = loopbackController{}
		devResolver    deviceplane.EntityResolver   = loopbackResolver
	)
	if len(cfg.ecpTargets) > 0 {
		baseController = ecp.New(cfg.ecpTargets)
		devController = loggingController{inner: baseController, source: "automation"}
		targets := cfg.ecpTargets
		devResolver = func(entityID string) (deviceID, deviceClass string, ok bool) {
			// Wave-1 bridge resolver: a configured ECP target IS the adopted
			// entity (device_id = entity_id, class media-player). The real
			// adopted-entity records arrive with the app peer (data-model/1).
			if _, present := targets[entityID]; present {
				return entityID, "media-player", true
			}
			return "", "", false
		}
		log.Printf("waiveo-relay device plane: ECP controller live (%d target(s))", len(cfg.ecpTargets))
	}

	host, err := bootAutomationStack(store, relayID, applied, site, deviceRegistry, devController, devResolver)
	if err != nil {
		log.Fatalf("waiveo-relay: boot automation stack: %v", err)
	}

	// Dev-stack observability demo (gated): the real device-state INPUT that fires
	// an edge rule is hardware polling/ECP, which is deferred, so the live binary
	// otherwise loads the rules and stands ready without ever firing one. When
	// WAIVEO_RELAY_DEMO_OBSERVE is set (the make-dev harness sets it), drive ONE
	// synthetic screen-on observation through the loaded engine so the demo edge
	// rule fires end to end — recording an automation.run into the durable
	// telemetry buffer, which the flush loop below pushes to the app peer. This is
	// exactly the SyntheticSource "dev-stack demo" path automationhost documents;
	// it makes the observability loop (fired rule -> telemetry -> app event log ->
	// /events/v1) demonstrable live without hardware. A failure is non-fatal — the
	// relay serves regardless.
	if os.Getenv("WAIVEO_RELAY_DEMO_OBSERVE") != "" {
		if err := fireDemoObservation(host, deviceRegistry); err != nil {
			log.Printf("waiveo-relay: demo observation did not fire (%v); observability demo skipped", err)
		}
	}

	// Own the connection-layer clock-trust state (internal/relay/clocktrust,
	// REL-132/134/136/038) in one controller: it boots untrusted (the clock_state
	// this relay declared at hello above), owns the runtime clock a clock.hint
	// adjusts, and drives the engine's immediate re-evaluation of time-based rules
	// on an untrusted->trusted transition (REL-134 -> RUL-371) via
	// host.SetClockTrust. A hint NEVER trips that transition (REL-132) —
	// only a VERIFIED time (a desired-state-key-signed timestamp, or authenticated
	// NTP) applied via the controller does. The concrete verified-time source is a
	// deliberate later concern; the controller stands wired for it. This callback
	// runs on a per-request goroutine (the clock.hint HTTP handler), so it MUST
	// reach the engine only through host.SetClockTrust — which serializes on the
	// host's lock against the background re-pull loop's ApplyEdgeRules — never a
	// raw engine pointer.
	clockCtl := clocktrust.NewController(store, clocktrust.NewRuntimeClock(), func(state string) error {
		_, err := host.SetClockTrust(state)
		return err
	})

	cert, certDER, err := relayTLSCertificate(relayID)
	if err != nil {
		log.Fatalf("waiveo-relay: build TLS certificate from enrollment identity: %v", err)
	}

	pairingSrv, err := playerserver.NewServer(relayID.CertPEM, applied.PairingGrants)
	if err != nil {
		log.Fatalf("waiveo-relay: build player/1 pairing server: %v", err)
	}
	logPairingCodes(cfg, applied, certDER)

	// Configure program delivery (GET /player/v1/program) from the relay's
	// OWN persisted last-applied screen_programs (REL-055/061), not directly
	// from the live Applied value — the same durable copy Pull just wrote, so
	// the serve path reads what a restart with a disconnected app peer would
	// read (Offline continuity). ServedProgram's sole input is the operational
	// store; it contacts no app peer. Wave-1 first-photon carries exactly one
	// applied screen-program system-wide, so SetServedProgram takes entry [0].
	// signingKey is the SAME enrollment private key relayID.CertPEM certifies,
	// so a player's PLY-090 signature check against its pinned trust anchor
	// lines up with the cert this listener actually presents.
	served, err := desiredstate.ServedProgram(store)
	if err != nil {
		log.Fatalf("waiveo-relay: read persisted screen_programs for offline serve: %v", err)
	}
	if len(served) == 0 {
		log.Fatalf("waiveo-relay: persisted last-applied snapshot carried no screen_programs to serve")
	}
	pairingSrv.SetServedProgram(applied.Generation, served[0], relayID.PrivateKey)

	// commandSurface is the ONE relay/1 REL-112/113/115 device-command surface
	// this binary's non-edge-rule dispatch paths share — the schedule-preset
	// path (scheduleSink, right below) and the screen keep-alive path
	// (keepalive.Config.Controller, below) both wrap THIS SAME instance
	// (mirroring bootAutomationStack's own construction: same loopback
	// controller, the SAME canonical deviceRegistry as CommandVocab, and
	// entity resolver) so a fired preset batch and a keep-alive recovery
	// launch dispatch through the identical resolve -> serialize -> dispatch
	// path (REL-113/114/115) an edge rule does, and REL-115's per-device lock
	// actually serializes one against the other when both target the same
	// physical device (internal/relay/keepalive's own package doc, "Dispatch
	// is serialized").
	commandSurface := deviceplane.NewCommandSurface(devController, deviceRegistry, devResolver)

	// Boot the schedule resolver (internal/relay/schedulehost, REL-065/DAT-113-118)
	// from the SAME verified Applied value bootAutomationStack read above: it
	// parses applied.Schedule into a data-model/1 RowStore and, for every screen
	// the carried schedule GOVERNS, resolves + serves that screen's Lease over
	// pairingSrv, replacing the app-authored screen_programs baseline
	// SetServedProgram just configured. A screen the schedule does not govern
	// (no carried scope node, or no applicable schedule) is left exactly as
	// SetServedProgram configured it — the additive serving policy.
	scheduleSink := automation.NewCommandSink(commandSurface, relayID.RelayID)

	// rootCtx governs every long-lived background loop this process starts — the
	// per-screen schedule resolve loops and the desired-state re-pull loop — for
	// the life of the binary. The scheduleDriver owns the schedule resolvers'
	// lifecycle across generations: a re-pull that applies a higher generation
	// cancels the prior generation's resolve loops before installing the new
	// ones (REL-056 atomic swap). Because cancellation cannot interrupt a
	// resolver already mid-resolve, a superseded generation's late SetProgram
	// write is fenced by generation at the player server (a strictly-older
	// generation's write is dropped, REL-052/056) rather than by cancel timing.
	rootCtx := context.Background()

	// Real device-state input (hardware): poll every configured ECP target and
	// feed the observation stream into the automation host. The poller's first
	// snapshot per entity arrives as a self-transition seed (sets engine
	// trigger baselines incl. attributes, fires nothing — RUL-300/304/330);
	// real transitions follow on the same stream, so no separate seeding step
	// exists. Host.Run pulls until the poller's stream closes on ctx cancel.
	if len(cfg.ecpTargets) > 0 {
		pollTargets := make(map[string]ecppoll.Target, len(cfg.ecpTargets))
		for entityID, t := range cfg.ecpTargets {
			pollTargets[entityID] = ecppoll.Target{Host: t.Host, Port: t.Port}
		}
		poller := ecppoll.New(pollTargets, cfg.pollInterval)
		go poller.Run(rootCtx)
		go func() {
			if err := host.Run(rootCtx, poller); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("waiveo-relay: device-state drive loop ended: %v", err)
			}
		}()
		log.Printf("waiveo-relay device polling live (%d target(s), every %s)", len(pollTargets), cfg.pollInterval)
	}

	// Screen keep-alive (internal/relay/keepalive, player/1 PLY-150-157): a
	// second, independent ECP poller over the SAME cfg.ecpTargets that
	// re-launches a screen's player channel once it safely idles at Home
	// (power-on settle delay, ≥2-consecutive-poll Home confirmation, never
	// while standby, never while the screen's own active Lease is blank) —
	// see keepalive's own package doc for why this needs its OWN Poller
	// rather than sharing the one host.Run above already exclusively
	// consumes. Off by default (WAIVEO_RELAY_KEEPALIVE unset).
	if cfg.keepaliveOn && len(cfg.ecpTargets) > 0 {
		kaTargets := make(map[string]keepalive.Target, len(cfg.ecpTargets))
		for entityID, t := range cfg.ecpTargets {
			kaTargets[entityID] = keepalive.Target{Host: t.Host, Port: t.Port}
		}
		ka := keepalive.New(keepalive.Config{
			Targets:      kaTargets,
			PollInterval: cfg.pollInterval,
			// The SAME commandSurface the schedule-preset path dispatches
			// through, wrapped exactly as that path wraps it (REL-112/113/115
			// — see commandSurface's own comment above and keepalive's
			// package doc, "Dispatch is serialized"): a keep-alive recovery
			// launch is indistinguishable, from the device plane's side, from
			// an app-peer-, edge-rule-, or preset-batch-issued command, and
			// takes the identical per-device dispatch lock.
			// Keep-alive dispatches through its OWN surface over the same
			// underlying adapter, differing only in the source label its
			// dispatches carry in the journal. The device plane still sees an
			// identical command taking the identical per-device lock; the
			// label exists purely so an operator reading the log can tell a
			// keep-alive recovery from an edge-rule or preset-batch command,
			// which is otherwise impossible once several subsystems drive the
			// same screen.
			Controller: automation.NewCommandSink(
				deviceplane.NewCommandSurface(
					loggingController{inner: baseController, source: "keep-alive"},
					deviceRegistry, devResolver),
				relayID.RelayID),
			// Wave-1 bridge (playerserver.Server.CurrentDisplay's own doc):
			// exactly one screen-program is served system-wide today, so
			// every entityID maps to that SAME currently active Lease
			// display — the real PLY-155 signal (internal/relay/keepalive's
			// package doc, "PLY-155/156" section).
			ActiveDisplay: func(string) string { return pairingSrv.CurrentDisplay() },
		})
		go func() {
			if err := ka.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("waiveo-relay: screen keep-alive ended: %v", err)
			}
		}()
		log.Printf("waiveo-relay screen keep-alive live (%d target(s), every %s)", len(kaTargets), cfg.pollInterval)
	}

	// Discovery (REL-110/111): SSDP client sweep + mDNS listener each mint
	// pattern-hit candidates into ONE SHARED candidate store when both lanes
	// are on — REL-110's device.candidates report is a full-set report per
	// relay, not per discovery lane, so a candidate either lane observes must
	// land in the same Store regardless of which one matched it. When only
	// one lane is configured, candStore is still built here (there is no
	// third lane yet to share it with). The store's device.candidates report
	// rides to the app peer in Wave 2; for now a low-rate log line makes a
	// live sweep's/listener's effect observable on-box. Timestamps are
	// wall-clock Timestamp-ms — the store is in-memory relay state, not
	// persisted evidence, so clock-trust gating does not apply to it.
	if cfg.discoveryOn || len(cfg.mdnsPatterns) > 0 {
		candStore := deviceplane.NewStore(relayID.RelayID)

		if cfg.discoveryOn {
			disc, err := discovery.New(discovery.Config{
				Patterns:  []deviceplane.Match{{SSDP: "roku:ecp"}},
				Store:     candStore,
				NowMillis: func() int64 { return time.Now().UnixMilli() },
			})
			if err != nil {
				log.Fatalf("waiveo-relay: configure SSDP discovery: %v", err)
			}
			go func() {
				if err := disc.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("waiveo-relay: discovery ended: %v", err)
				}
			}()
			log.Printf("waiveo-relay SSDP discovery live (pattern roku:ecp)")
		}

		if len(cfg.mdnsPatterns) > 0 {
			mdnsMatches := make([]deviceplane.Match, len(cfg.mdnsPatterns))
			for i, svcType := range cfg.mdnsPatterns {
				mdnsMatches[i] = deviceplane.Match{MDNS: svcType}
			}
			mdnsListener, err := mdns.New(mdns.Config{
				Patterns:  mdnsMatches,
				Store:     candStore,
				NowMillis: func() int64 { return time.Now().UnixMilli() },
			})
			if err != nil {
				log.Fatalf("waiveo-relay: configure mDNS discovery: %v", err)
			}
			go func() {
				if err := mdnsListener.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("waiveo-relay: mDNS discovery ended: %v", err)
				}
			}()
			log.Printf("waiveo-relay mDNS discovery live (patterns %s)", strings.Join(cfg.mdnsPatterns, ", "))
		}

		go func() {
			tick := time.NewTicker(time.Minute)
			defer tick.Stop()
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-tick.C:
					if n := len(candStore.Report().Body.Candidates); n > 0 {
						log.Printf("waiveo-relay discovery: %d candidate pattern(s) observed", n)
					}
				}
			}
		}()
	}

	// SSDP RESPONDER (player/1 PLY-021/022): answer a player's same-network
	// M-SEARCH for ssdpresponder.SearchTarget with this relay's own
	// resolvable /player/v1 base URL, built from the SAME cfg.pairHost/
	// cfg.pairPort a formed pairing code already dials (REL-126) — so a
	// screen that locates this relay via discovery and one that redeems a
	// pairing code end up dialing the identical address. Off by default
	// (WAIVEO_RELAY_SSDP_ANNOUNCE unset): CI and loopback dev runs must
	// never multicast.
	if cfg.ssdpAnnounce {
		baseURL := fmt.Sprintf("https://%s/player/v1", net.JoinHostPort(cfg.pairHost, strconv.Itoa(cfg.pairPort)))
		responder, err := ssdpresponder.New(ssdpresponder.Config{
			BaseURL: baseURL,
			USN:     fmt.Sprintf("uuid:waiveo-relay:%s", relayID.RelayID),
		})
		if err != nil {
			log.Fatalf("waiveo-relay: configure SSDP responder: %v", err)
		}
		go func() {
			if err := responder.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("waiveo-relay: SSDP responder ended: %v", err)
			}
		}()
		log.Printf("waiveo-relay SSDP responder live (%s -> %s)", ssdpresponder.SearchTarget, baseURL)
	}

	driver := &scheduleDriver{
		srv:        pairingSrv,
		sink:       scheduleSink,
		site:       site,
		signingKey: relayID.PrivateKey,
		tickEvery:  scheduleResolverTickInterval,
	}
	driver.apply(rootCtx, applied, time.Now().UnixMilli())

	// The relay's live loop (relay/1 REL-052/055/056): after the boot pull, it
	// re-pulls desired-state on a bounded interval (POC: a few seconds — relay/1
	// defines no push, so polling is the POC mechanism). A higher generation is
	// applied atomically — the schedule resolvers are re-driven and the automation
	// engine's edge rules reloaded — while a same/lower generation is a no-op and a
	// mid-run pull failure is non-fatal (the last-applied stays served offline).
	// The live loop is gated on the SAME hello outcome the boot pull is gated on.
	// Without this it defeats that gate within one tick: desiredstate.Pull's own
	// verification is a signature check over the snapshot, which succeeds whether
	// or not the app peer accepted this relay's identity, so a relay whose hello
	// was REFUSED would skip the boot pull and then perform the identical pull
	// seconds later — applying live desired state the peer just declined to
	// authorize it for, while the journal reported the opposite. A relay that was
	// not accepted serves its persisted last-applied snapshot offline and nothing
	// more, until an operator re-enrolls it.
	if helloOK {
		puller := &rePuller{
			pull:    func() (desiredstate.Applied, error) { return desiredstate.Pull(cfg.feederURL, store) },
			driver:  driver,
			host:    host,
			nowFn:   func() int64 { return time.Now().UnixMilli() },
			lastGen: applied.Generation,
		}
		rePullTicker := time.NewTicker(rePullInterval)
		go rePullLoop(rootCtx, rePullTicker.C, puller)
	} else {
		log.Printf("waiveo-relay: live desired-state loop NOT started — hello was not accepted by the app peer; serving persisted last-applied offline until re-enrolled (REL-055/061)")
	}

	// Wire the relay's telemetry upstream channel (relay/1 REL-090/092/097): the
	// automation stack records a fired rule's automation.run into the durable
	// telemetry buffer (host.TelemetryBuffer); this Channel pushes that buffer to
	// the co-located app peer's /telemetry/v1/push ingest route over the
	// feeder-trusting TLS client, on a bounded interval. A received ack_through_seq
	// advances the buffer's retention (REL-092); an un-acked batch (app peer down
	// or rejecting) is retained and retried on the next tick (REL-097 — no silent
	// loss). This is the live delivery that carries a fired rule's event off the
	// relay to the app's event log.
	telemetryChannel := telemetry.NewChannel(
		host.TelemetryBuffer(),
		telemetryhttp.New(cfg.feederURL, feederTLSClient()),
		nil, // single-attempt per Flush; an un-acked batch rides the next flush tick
	)
	telemetryFlushTicker := time.NewTicker(telemetryFlushInterval)
	go telemetryFlushLoop(rootCtx, telemetryFlushTicker.C, telemetryChannel)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	pairingSrv.Register(mux)

	// Mount the relay/1 clock.hint receiver (REL-133) on this relay's own
	// listener: the app peer MAY send clock.hint at any time on the established
	// connection, and the relay applies it to a runtime clock only — bounded by
	// this relay's own certificate not_after plus a grace so a hint alone can
	// never make the relay believe its expired credential is still valid, and
	// never touching the persisted floor (REL-132). The relay boots with this
	// runtime clock untrusted (the clock_state it declared at hello, REL-038).
	if err := registerClockHint(mux, certDER, clockCtl); err != nil {
		log.Fatalf("waiveo-relay: register clock.hint receiver: %v", err)
	}

	server := &http.Server{
		Addr:      cfg.listen,
		Handler:   apihttp.WithTraceID(mux),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}

	log.Printf("waiveo-relay listening (HTTPS) on %s (pairing code dial %s:%d)", cfg.listen, cfg.pairHost, cfg.pairPort)
	log.Fatal(server.ListenAndServeTLS("", ""))
}

// relayTLSCertificate builds a crypto/tls.Certificate (for serving player/1
// over HTTPS) from id's enrollment identity, and returns its raw DER
// alongside it — the same DER FormPairingCode computes a REL-126
// fingerprint_commitment over, so the certificate this listener actually
// presents is always the one a formed pairing code commits to.
//
// id is the relay's feeder-issued enrollment identity (internal/relay/enroll,
// internal/relay/identity) — Wave-1 first-photon reuses it as the relay's
// player/1 TLS identity too, rather than minting a separate self-signed
// bootstrap cert: PLY-040's bootstrap fetch is verification-disabled by
// construction, so a player never chain-validates this certificate before
// Out-of-band cert authentication (PLY-052) — only its SubjectPublicKeyInfo,
// via the commitment, matters.
func relayTLSCertificate(id identity.RelayIdentity) (tls.Certificate, []byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(id.PrivateKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(id.CertPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	block, _ := pem.Decode(id.CertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return tls.Certificate{}, nil, fmt.Errorf("waiveo-relay: identity cert did not PEM-decode to a CERTIFICATE block")
	}

	return cert, block.Bytes, nil
}

// registerClockHint mounts the relay/1 clock.hint receiver (REL-133) on mux over
// the clock-trust controller's own runtime clock (internal/relay/clocktrust), so
// a wire clock.hint adjusts the very clock the controller tracks — never the
// persisted floor and never the trust state (REL-132). Accepted hints are bounded
// to this relay's own certificate not_after (parsed from certDER) plus
// clocktrust.DefaultBoundedGraceMs, so a hint alone can never make the relay
// believe its expired credential is still valid. This is the connection-layer
// path that makes the runtime clock reachable end-to-end: a real clock.hint on
// the wire reaches AcceptHint here, never a direct call.
func registerClockHint(mux *http.ServeMux, certDER []byte, ctl *clocktrust.Controller) error {
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("parse enrollment cert for clock.hint bound: %w", err)
	}
	recv := clocktrust.NewHintReceiver(ctl.Clock(), leaf.NotAfter.UnixMilli(), clocktrust.DefaultBoundedGraceMs)
	mux.HandleFunc("/relay/v1/clock-hint", recv.ServeHTTP)
	return nil
}

// logPairingCodes forms and logs (dev-console-only stand-in for a real
// display surface, out of player/1's own scope) a REL-126 pairing code for
// every applied pairing grant, so a developer can read one off the relay's
// own log and hand it to a later player/1 client task.
func logPairingCodes(cfg config, applied desiredstate.Applied, relayCertDER []byte) {
	for _, grant := range applied.PairingGrants {
		code, err := playerserver.FormPairingCode(cfg.pairHost, cfg.pairPort, grant, relayCertDER)
		if err != nil {
			log.Printf("waiveo-relay: form pairing code for grant %s: %v", grant.GrantID, err)
			continue
		}
		log.Printf("waiveo-relay pairing code (grant %s): %s", grant.GrantID, code)
	}
}

// offlineServeFallback decides, per REL-055/061, whether a boot-time app-peer
// failure (a hello handshake or a desired-state Pull that could not complete)
// should down the relay process or degrade to serving the persisted
// last-applied snapshot offline. A failure is survivable ONLY when a prior
// successful pull left a persisted last-applied generation in the store: that
// durable {generation, hash, screen_programs} copy is exactly what the relay
// serves without a live app-peer connection (REL-055 offline continuity), so a
// restart during a disconnection must reach the player/1 listener rather than
// exit. With nothing persisted, a boot that cannot reach the app peer has no
// program to serve at all, so the failure stays fatal.
//
// It returns nil when the caller should continue in offline-serve mode, or the
// original bootErr (unchanged) when the caller should treat it as fatal. A nil
// bootErr always returns nil.
func offlineServeFallback(bootErr error, hasPersisted bool) error {
	if bootErr == nil {
		return nil
	}
	if hasPersisted {
		return nil
	}
	return bootErr
}

// enrollWithRetry calls enroll.Run against the co-located feeder, retrying
// on failure (e.g. the feeder's listener not up yet) until
// enrollRetryBudget elapses. enroll.Run is idempotent for a valid persisted
// identity — a store that already holds one with an unexpired certificate
// returns immediately without a network call — but if that certificate has
// expired, Run drives the Expired-certificate re-enrollment path
// (internal/relay/reenroll, REL-020/027): the relay recovers its identity by
// proving possession of the expired certificate's own retained private key,
// under the same relay_id, re-anchoring its trust anchor (REL-014/017).
func enrollWithRetry(feederURL string, store *identity.Store) error {
	deadline := time.Now().Add(enrollRetryBudget)
	var lastErr error
	for {
		if err := enroll.Run(feederURL, store); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(enrollRetryInterval)
	}
}

// bootAutomationStack builds the relay's edge-automation Host over the
// operational store and loads the verified desired-state generation's signed
// edge_rules into it (REL-062), logging how many edge rules loaded. The caller
// selects the DeviceController + EntityResolver pair (real ECP when targets are
// configured, loopback stand-ins otherwise) so both dispatch paths share it. dc
// is the relay's ONE canonical device-class registry (device-class-registry/1's
// own built-in, REG-060-066) — automationhost.New wires it directly into the
// device plane's CommandVocab and, adapted, into the engine's registry.Registry.
func bootAutomationStack(store *identity.Store, relayID identity.RelayIdentity, applied desiredstate.Applied, site hello.SiteBinding, dc deviceclass.Registry, controller deviceplane.DeviceController, resolveEntity deviceplane.EntityResolver) (*automationhost.Host, error) {
	host, err := automationhost.New(store, dc, controller, resolveEntity, relayID.RelayID)
	if err != nil {
		return nil, err
	}
	// Adopt the app peer's authoritative site_binding (REL-036) into the engine
	// before loading rules, so the edge engine's schedule/sun triggers evaluate
	// against the site's real timezone and coordinates from the first tick.
	if err := host.SetLocation(site.TZ, site.Lat, site.Long); err != nil {
		return nil, fmt.Errorf("adopt site_binding tz %q into engine: %w", site.TZ, err)
	}
	if err := host.ApplyEdgeRules(applied.EdgeRules, int(applied.Generation)); err != nil {
		return nil, err
	}
	log.Printf("waiveo-relay automation engine loaded: %d edge rule(s); device plane + durable telemetry ready", host.EdgeRuleCount())
	return host, nil
}

// demoObserveEntityID is the entity the dev-stack observability demo drives a
// synthetic screen-on transition on. It MUST match the seeded demo edge rule's
// trigger entity (the app store's seedRuleEntityID) so the rising edge actually
// matches the loaded rule and fires it; any other entity would observe a
// transition the rule does not trigger on, producing no automation.run.
const demoObserveEntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"

// fireDemoObservation drives one synthetic "off -> on" transition on
// demoObserveEntityID through the loaded engine (RUL-300/330), firing the demo
// edge rule and recording its automation.run into the durable telemetry buffer
// (REL-090/093). It first establishes a durable "off" baseline (no transition, no
// firing) then the rising edge that fires. dc is the relay's canonical
// device-class registry, adapted to the rules/1 registry surface only to classify
// the observation's (absent) attribute changes — a pure state transition consults
// no attribute declarations, so the media-player class resolves trivially. It
// returns the host's drive error, if any (never fatal to the caller).
func fireDemoObservation(host *automationhost.Host, dc deviceclass.Registry) error {
	reg := registry.FromDeviceClass(dc, registry.Overlay{})
	off := state.Entity{ID: demoObserveEntityID, DeviceClass: "media-player", State: "off"}
	on := state.Entity{ID: demoObserveEntityID, DeviceClass: "media-player", State: "on"}
	src := automationhost.NewSyntheticSource(
		state.NewObservation(reg, off, off),
		state.NewObservation(reg, off, on),
	)
	if err := host.Run(context.Background(), src); err != nil {
		return err
	}
	log.Printf("waiveo-relay observability demo: synthetic screen-on fired the edge rule; automation.run buffered for telemetry push (REL-090)")
	return nil
}

// scheduleResolverTickInterval is the cadence the running relay re-resolves
// each governed screen's effective state at (internal/relay/schedulehost's
// Loop), catching daypart boundaries (DAT-119: a daypart is a holding STATE,
// re-resolved every tick, never a one-shot event). It is comfortably tighter
// than a daypart's own minute-granularity boundaries while staying well
// clear of a busy-loop, matching the coarse cadence player/1's own program
// poll already runs at.
const scheduleResolverTickInterval = 30 * time.Second

// bootScheduleResolverAt is the boot-path entry to the schedule resolver: it
// resolves + serves applied.Schedule once at nowMs and starts each governed
// screen's background re-resolve loop under context.Background() at the standard
// scheduleResolverTickInterval. It is a thin wrapper over resolveAndServe — which
// takes the loop context and tick cadence explicitly, so the live re-pull path
// (scheduleDriver, livepull.go) can cancel a superseded generation's loops and
// tests can drive a deterministic instant — preserving the boot-only signature
// the existing boot tests exercise.
func bootScheduleResolverAt(applied desiredstate.Applied, srv *playerserver.Server, sink *automation.CommandSink, site hello.SiteBinding, signingKey ed25519.PrivateKey, nowMs int64) []*schedulehost.Resolver {
	return resolveAndServe(context.Background(), applied, srv, sink, site, signingKey, scheduleResolverTickInterval, nowMs)
}

// resolveAndServe parses applied.Schedule into a data-model/1 RowStore
// (schedulehost.BuildStore — degrade-safe: a parse/validation error is logged
// but never fatal) and, for every carried scope node of kind "screen" the
// schedule GOVERNS (schedulehost.Governs, the stated additive serving
// policy), builds a schedulehost.Resolver serving it over srv: one resume
// tick (schedulehost.Resolver.TickBoot) at nowMs runs immediately, resolving
// and serving the current Lease and firing the effective daypart's rising-edge
// preset through sink UNLESS its effective misfire is "skip" (DAT-075/076/
// 094/121 — this is the boot-or-generation-apply resume edge those clauses name,
// not an ordinary tick) — and a background ticker keeps re-resolving at
// tickEvery so a later daypart boundary is caught without a restart, firing
// unconditionally on every ordinary rising edge from then on.
//
// ctx governs every background resolve loop this call starts: cancelling it stops
// them (and stops their tickers). This is what lets a re-pulled generation
// atomically replace a prior one (REL-056) — the caller cancels the prior
// generation's ctx before resolveAndServe installs the new generation's loops.
// Cancellation cannot interrupt a resolver already mid-resolve, so the residual
// hazard — a superseded generation's late write landing after the new serve — is
// closed at the write point: each resolver stamps applied.Generation onto
// playerserver.Server.SetProgram, which drops a strictly-older generation's write
// (REL-052/056), so a stale in-flight resolver can never revert the new serve.
//
// A screen the schedule does not govern (no carried scope node for it, or no
// applicable schedule) is left exactly as it was: the app-authored
// screen_programs SetServedProgram already configured on srv stays served,
// unchanged — this function builds no Resolver for it and calls SetProgram
// for it not at all. The same holds, vacuously, when applied.Schedule carries
// no scope nodes at all (today's first-photon empty-schedule state): the
// returned slice is empty and every screen's serving is untouched.
//
// site is the app peer's authoritative site_binding (REL-036, the same value
// bootAutomationStack adopts into the edge engine) — carried through only for
// the boot log line's context; the resolved schedule's own effective tz comes
// from the carried scope tree exclusively (datamodel.EffectiveTZ via
// datamodel.Resolve), never from site or any box-local clock (DAT-034/118).
func resolveAndServe(ctx context.Context, applied desiredstate.Applied, srv *playerserver.Server, sink *automation.CommandSink, site hello.SiteBinding, signingKey ed25519.PrivateKey, tickEvery time.Duration, nowMs int64) []*schedulehost.Resolver {
	store, errs := schedulehost.BuildStore(applied.Schedule)
	for _, e := range errs {
		log.Printf("waiveo-relay: schedule section: %s: %s: %s", e.Field, e.Code, e.Message)
	}

	var resolvers []*schedulehost.Resolver
	for _, screenID := range scheduleScreenNodeIDs(applied.Schedule) {
		if !schedulehost.Governs(store, screenID) {
			continue
		}

		display, _, content, _, err := schedulehost.ProjectLease(store, screenID, nowMs, applied.ContentOrigin)
		if err != nil {
			// An unresolvable effective tz (DAT-034) degrades to the app-authored
			// program already served — never a box-local substitution.
			log.Printf("waiveo-relay: schedule resolver: screen %s: resolve at boot: %v; serving app-authored program", screenID, err)
			continue
		}

		r := schedulehost.NewResolver(store, screenID, srv, signingKey, applied.Generation, applied.ContentOrigin)
		r.TickBoot(nowMs, sink) // the level-triggered STATE projection + the misfire-governed boot resume-edge preset (DAT-075/076/094/119/121).
		resolvers = append(resolvers, r)

		if display == "content" && len(content) > 0 {
			log.Printf("SCHEDULE RESOLVER OK (screen %s: display:content, asset %s; site tz %s)", screenID, content[0].AssetRef, site.TZ)
		} else {
			log.Printf("SCHEDULE RESOLVER OK (screen %s: display:%s; site tz %s)", screenID, display, site.TZ)
		}

		ticker := time.NewTicker(tickEvery)
		go func(res *schedulehost.Resolver) {
			defer ticker.Stop()
			res.Loop(ctx, ticker.C, sink)
		}(r)
	}

	if len(resolvers) == 0 {
		log.Printf("SCHEDULE RESOLVER OK (no governing schedule; serving app-authored program)")
	}
	return resolvers
}

// scheduleScreenNodeIDs returns the id of every carried scope node of kind
// "screen" in sec — the candidate screens bootScheduleResolverAt checks
// schedulehost.Governs against. A node that fails to unmarshal is skipped
// (schedulehost.BuildStore already reports it as a ROW_MALFORMED error above)
// rather than aborting the whole scan.
func scheduleScreenNodeIDs(sec wire.ScheduleSection) []string {
	var ids []string
	for _, raw := range sec.ScopeNodes {
		var n datamodel.ScopeNode
		if err := json.Unmarshal(raw, &n); err != nil {
			continue
		}
		if n.Kind == "screen" {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

// helloWithRetry performs the relay/1 connection handshake against the
// co-located app peer, retrying a transport failure (e.g. the feeder's
// listener not up yet, or its handshake routes mid-registration) until
// enrollRetryBudget elapses — mirroring enrollWithRetry's tolerance of the
// dev harness starting both binaries with no ordering. A typed *hello.RefusedError
// (a channel-binding or protocol-version refusal) is decisive and returned
// immediately, never retried: the app peer answered and declined.
func helloWithRetry(cfg config, relayIdent identity.RelayIdentity) (hello.SiteBinding, error) {
	decl := hello.Declaration{
		ProtocolVersion: "1.0",
		Features:        []string{"telemetry.latest_only_v1"},
		SiteBinding:     hello.SiteBinding{}, // no cached site pre-pull; the relay adopts the app peer's authoritative copy
		SubnetMetadata:  hello.SubnetMetadata{AdvertisedAddress: cfg.listen},
		ClockState:      hello.ClockState{State: "untrusted", Source: "cold_boot"},
	}

	deadline := time.Now().Add(enrollRetryBudget)
	var lastErr error
	for {
		ack, err := hello.PerformHello(cfg.feederURL, relayIdent.PrivateKey, relayIdent.RelayID, decl)
		if err == nil {
			log.Printf("waiveo-relay hello negotiated version %s; site %s", ack.Body.NegotiatedVersion, ack.Body.SiteBinding.TZ)
			return ack.Body.SiteBinding, nil
		}
		var refused *hello.RefusedError
		if errors.As(err, &refused) {
			return hello.SiteBinding{}, err // decisive refusal, not a transport hiccup
		}
		lastErr = err
		if time.Now().After(deadline) {
			return hello.SiteBinding{}, lastErr
		}
		time.Sleep(enrollRetryInterval)
	}
}

// loopbackController is the Wave-1 stand-in DeviceController: it accepts every
// resolved device command and logs it, standing in for the real ECP/Roku adapter
// (hardware, deferred) so the wired automation stack can be exercised without a
// physical device. It never fails, so a fired rule's dispatch always succeeds.
type loopbackController struct{}

func (loopbackController) Dispatch(entityID, command string, params map[string]any) error {
	log.Printf("waiveo-relay automation dispatch (loopback): %s %s", entityID, command)
	return nil
}

// loggingController wraps a DeviceController so every dispatch leaves an
// operator-visible trace naming WHICH subsystem issued it.
//
// The device adapters themselves are deliberately silent (REL-114 credential
// hygiene: an adapter that logs is an adapter that can leak a parameter), and
// with several subsystems — the edge-rules engine, schedule preset batches, and
// screen keep-alive — all dispatching through the same surface, a command
// arriving at a device was previously indistinguishable at the journal from any
// other. That was found the first time a real screen was driven end-to-end: the
// screen moved, and nothing on the box could say what moved it.
//
// Only the command NAME and the parameter KEYS are logged, never parameter
// values, so a credential passed as a param can never reach the journal.
type loggingController struct {
	inner  deviceplane.DeviceController
	source string
}

func (c loggingController) Dispatch(entityID, command string, params map[string]any) error {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	err := c.inner.Dispatch(entityID, command, params)
	if err != nil {
		log.Printf("waiveo-relay dispatch [%s]: %s %s params=%v FAILED: %v", c.source, entityID, command, keys, err)
		return err
	}
	log.Printf("waiveo-relay dispatch [%s]: %s %s params=%v ok", c.source, entityID, command, keys)
	return nil
}

// loopbackResolver is the Wave-1 stand-in entity resolver: it maps every
// entity_id to a single media-player loopback device so a loaded edge rule's
// device_command resolves against the fixture registry's vocabulary (REL-112/113).
// The real resolver reads the device plane's adopted-entity records (data-model/1)
// and is a later concern.
func loopbackResolver(entityID string) (deviceID, deviceClass string, ok bool) {
	return "01J8Z3K4N5P6Q7R8S9T0V1DEVA", "media-player", true
}

// telemetryFlushInterval is the POC cadence the running relay pushes its
// buffered telemetry upstream at (relay/1 REL-090): a couple of seconds, bounded
// so a fired rule's automation.run reaches the app peer promptly without a
// busy-loop. relay/1 defines no server push, so a periodic Flush is the
// deliberate POC delivery mechanism. The live binary drives the loop from a
// time.Ticker at this cadence; tests inject a manual channel, so nothing here
// sleeps on the wall clock.
const telemetryFlushInterval = 2 * time.Second

// telemetryPushTimeout bounds one telemetry.push POST so a stalled app peer
// cannot wedge the flush loop; a timed-out push is a non-fatal transport error —
// the batch stays buffered and rides the next tick (REL-097).
const telemetryPushTimeout = 5 * time.Second

// telemetryFlushLoop drives ch.Flush once per tick delivered on ticks — a
// time.Ticker's channel in the binary, a manual channel in tests, so nothing
// here sleeps on the wall clock. Each Flush pushes the buffered telemetry batch
// to the app peer and, on a received ack_through_seq, advances the buffer's
// retention (REL-092); a push that fails (app peer unreachable or rejecting) is
// logged and the batch left buffered for the next tick (REL-097 — an un-acked
// batch is retried across reconnects, its durable entries never discarded). It
// returns when ctx is cancelled or ticks is closed.
func telemetryFlushLoop(ctx context.Context, ticks <-chan time.Time, ch *telemetry.Channel) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if _, err := ch.Flush(); err != nil {
				log.Printf("waiveo-relay telemetry push: flush failed (%v); batch retained for retry (REL-097)", err)
			}
		}
	}
}

// feederTLSClient returns the relay's feeder-trusting HTTP client for the
// telemetry upstream push (REL-090): server-authenticated TLS with no separate
// trust anchor to validate the co-located feeder/app-peer's self-signed listener
// certificate against, mirroring the relay's existing enroll / desired-state /
// hello bootstrap clients (REL-010/011 bootstrap exception, made concrete for
// the Wave-1 co-located feeder+relay loopback deployment). It is independent of
// the telemetry retention/ack logic, which the Channel owns; this client only
// carries the batch on the wire.
func feederTLSClient() *http.Client {
	return &http.Client{
		Timeout: telemetryPushTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // REL-010/011 co-located bootstrap exception, see doc above
		},
	}
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"component": "waiveo-relay",
		"status":    "ok",
	})
}
