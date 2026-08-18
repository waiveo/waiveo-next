// Command waiveo-relay is the Wave-1 relay component: a Go process speaking
// the relay/1 protocol (contracts/relay-1.md) to an app peer and the player
// over its own channels. On start, it opens its persistent operational
// identity store (internal/relay/identity), enrolls against the co-located
// feeder (internal/relay/enroll, relay/1 REL-010–014) if it hasn't already —
// persisting the feeder's own desired-state signing key as its
// enrollment-anchored trust anchor (REL-071, `#28`) — then opens ONE
// mutually authenticated persistent connection to the app peer's /relay/v1
// (internal/relay/relayconn: challenge → hello → hello-ack inside Dial,
// REL-030–041) and pulls + verifies the feeder's signed desired-state
// snapshot over it (state.pull → desiredstate.VerifyAndApply,
// REL-050–056/071/072), persisting last-applied and holding the resulting
// applied screen-program in memory. The connection then stays up for the
// life of the process: server-initiated state.changed nudges (REL-057)
// trigger an immediate pull-and-apply, and a reconnect supervisor re-dials
// a dropped connection with backoff, pulling once on every reconnect so a
// relay offline through N generations converges without waiting for a
// nudge. If the app peer is unreachable at boot but a prior pull already
// persisted a last-applied generation, the connect/pull failure is NOT
// fatal: the relay proceeds and serves that durable copy offline
// (REL-055/061 offline continuity), so a restart during a disconnection
// still serves screens. It then serves player/1's pairing surface
// (internal/relay/playerserver, PLY-030–037) over its own HTTPS listener,
// using the exact same certificate — the enrollment identity persisted in
// its identity store — that FormPairingCode's commitment (REL-126) is
// computed over, so a player's local PLY-052 comparison is always checking
// the cert this listener actually presents.
package main

import (
	"context"
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
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/arpsweep"
	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/devicetargets"
	"github.com/maaxton/waiveo-next/internal/relay/discovery"
	"github.com/maaxton/waiveo-next/internal/relay/ecp"
	"github.com/maaxton/waiveo-next/internal/relay/ecppoll"
	"github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/hostmdns"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/keepalive"
	"github.com/maaxton/waiveo-next/internal/relay/mdns"
	"github.com/maaxton/waiveo-next/internal/relay/neighbor"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/relay/portscan"
	"github.com/maaxton/waiveo-next/internal/relay/reenroll"
	"github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
	"github.com/maaxton/waiveo-next/internal/relay/ssdpresponder"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/relay/telemetryhttp"
	"github.com/maaxton/waiveo-next/internal/relay/vitals"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/slidelive"
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
	// identityPath is the relay's own operational store (its enrollment-anchored
	// pin among other things). Overridable so two relays can coexist on one
	// machine — see the envOr call for why that matters beyond tidiness.
	identityPath string
	pairPort     int // dial port a formed pairing code encodes

	// Hardware device plane (all optional). ecpTargets is the deployment
	// OVERRIDE map, entity_id → the LAN Roku its device_commands dispatch to
	// AND its state is polled from
	// (WAIVEO_RELAY_ECP_TARGETS="entity=host[:port],...").
	//
	// It is no longer what turns device control on. The drivable set is the
	// adoption gate's (internal/relay/devicetargets): what the app peer adopted
	// in `device_inventory` intersected with what this relay's discovery can
	// locate. These entries are the out-of-band escape hatch on top of it — a
	// device on a subnet SSDP does not cross, one whose LOCATION is wrong, or a
	// bring-up before any adoption record exists. An empty map is the normal
	// deployment, not a disabled device plane. pollInterval is
	// the ECP state-poll period (WAIVEO_RELAY_POLL_MS, default 5000).
	// discoveryOn enables the SSDP client sweep feeding the candidate store
	// (REL-110/111). mdnsPatterns enables the mDNS listener feeding the SAME
	// candidate store (WAIVEO_RELAY_MDNS_PATTERNS, comma-separated MAN-071
	// service-type strings e.g. "_waiveo._tcp"; empty/unset is off,
	// internal/relay/mdns). ssdpAnnounce enables the SSDP RESPONDER —
	// answering a player's own M-SEARCH for this relay's player/1 pairing
	// surface (WAIVEO_RELAY_SSDP_ANNOUNCE=1, PLY-021/022). keepaliveOn enables
	// the screen keep-alive capability (internal/relay/keepalive, player/1
	// PLY-150-154): a second ECP poller over the SAME ecpTargets that
	// re-launches a screen's player channel once it safely idles at Home.
	//
	// # Which of these default on, and why they split the way they do
	//
	// TWO of the four default ON and are switched off explicitly:
	// discoveryOn (WAIVEO_RELAY_DISCOVERY=0, and only for a LAN-bound
	// deployment — see discoveryEnabled) and keepaliveOn
	// (WAIVEO_RELAY_KEEPALIVE=0). The other two — mdnsPatterns and
	// ssdpAnnounce — default OFF and are switched on explicitly.
	//
	// The line between them is not "safe vs. unsafe", it is what the box does
	// to a network or a device that did not ask for it:
	//
	//   - mdnsPatterns and ssdpAnnounce make this box ANNOUNCE itself or bind
	//     a well-known multicast port. That is intrusive on a network that did
	//     not ask and, in mDNS's case, collides outright with the host's own
	//     avahi daemon. CI and loopback dev runs must never multicast, so both
	//     stay off until a deployment states otherwise.
	//   - discoveryOn is the opposite posture: an M-SEARCH is a control point
	//     ASKING who is out there, which is the entire job of a signage
	//     appliance that has to find the TVs it drives. Off by default meant a
	//     fresh box discovered nothing at all until somebody knew to set an
	//     environment variable — the appliance shipped unable to see its own
	//     hardware, and the legacy system it replaces swept always-on. It is
	//     nonetheless bound by the SAME invariant as the two above: CI and
	//     loopback dev runs must never multicast. Both hold at once because
	//     the default is read off the LISTEN address rather than being a
	//     constant — a loopback-bound relay serves no screen on any LAN and so
	//     has no fleet to discover, and does not sweep. discoveryEnabled has
	//     the full reasoning.
	//   - keepaliveOn drives an ADOPTED screen back to its channel. A screen
	//     sitting at Home is a screen showing nothing, and it stays that way
	//     until somebody notices — which, on an unattended wall, is the next
	//     time a human walks past. The legacy stack ran its equivalent
	//     unconditionally for exactly that reason, and shipping the capability
	//     switched off reproduced the outage it exists to end. What makes
	//     on-by-default safe is the ADOPTION GATE it carries
	//     (keepalive.AdoptionSet): the relay drives only screens the app peer's
	//     signed `device_inventory` says this deployment has adopted, so a
	//     relay that can reach a Roku the legacy stack still owns does nothing
	//     to it.
	//
	// Dev/CI behavior is unchanged by either flip: a loopback dev run does not
	// sweep at all (discoveryEnabled), and with nothing discovered and no
	// override configured the adoption gate is empty, so keepalive watches
	// nothing.
	ecpTargets   map[string]ecp.Target
	pollInterval time.Duration
	discoveryOn  bool
	mdnsPatterns []string
	ssdpAnnounce bool
	keepaliveOn  bool

	// powerOnLaunchOn enables the keep-alive's POWER-ON AUTO-LAUNCH rule
	// (keepalive.Config.PowerOnLaunch, parity row 5.6):
	// WAIVEO_RELAY_POWERON_LAUNCH, a THIRD opt-out switch on the same off-list
	// as the two above.
	//
	// It defaults ON for the reason keepaliveOn does, and is the same class of
	// decision: the legacy stack foregrounded the channel on every power-on,
	// unconditionally, and a screen that comes back up inside whatever app it
	// was last on shows the wrong thing until a human notices. Its blast radius
	// is bounded by exactly the same adoption gate — the rule lives inside
	// keepalive and is evaluated after that gate — so a relay that can reach a
	// Roku it has not adopted still does nothing to it.
	//
	// It is a switch of its own rather than being folded into keepaliveOn
	// because the two capabilities fail differently and an operator may
	// legitimately want one without the other: rule 2 only ever recovers a
	// screen sitting idle at Home (it never interrupts anyone), while this rule
	// deliberately foregrounds over whatever app the screen resumed into. A
	// deployment that shares its TVs with people has a real reason to disable
	// this one and keep the other.
	powerOnLaunchOn bool
}

// dialAddress is the address a player is told to dial to reach this relay's
// player/1 surface — the value a formed pairing code encodes (REL-126) and the
// value hello advertises to the app peer, which forms codes from it too
// (REL-037). It is "" when this relay can tell the address cannot work, and
// every consumer treats "" as "no code can be formed" rather than forming one.
//
// The case it catches is the deployment default drifting apart from the
// deployment. pairHost defaults to loopback, which is exactly right while the
// listener is also on loopback — a dev run or CI, where the player is on this
// same host — and becomes a footgun the moment an on-box deployment overrides
// only the LISTEN address. The relay then binds somewhere a screen on the LAN
// can reach and hands that screen a code saying "dial 127.0.0.1", which is the
// screen's OWN loopback: a code that cannot work, printed with the same
// confidence as one that can, failing at the far end as an ordinary "cannot
// reach the server" with nothing pointing back here.
//
// So this refuses precisely the mismatch — a loopback dial address behind a
// non-loopback listener — and nothing else. It does NOT guess a LAN address:
// which interface a screen should reach this relay on is a deployment fact
// nobody here knows (a multi-homed box, NAT, a VIP fronting several relays,
// REL-121c), so the remedy is for the deployment to state it in
// WAIVEO_RELAY_PAIR_HOST. A loopback listener with a loopback dial address is
// left exactly as it was.
func (c config) dialAddress() string {
	if c.pairHost == "" {
		return ""
	}
	if isLoopbackHost(c.pairHost) && !isLoopbackHost(hostOf(c.listen)) {
		return ""
	}
	return net.JoinHostPort(c.pairHost, strconv.Itoa(c.pairPort))
}

// isLoopbackHost reports whether host names the local machine's own loopback.
// A non-address hostname is NOT treated as loopback: "waiveo.local" or a real
// DNS name is a dialable name this relay has no business second-guessing, and
// resolving it here would make a boot-time config check depend on a resolver.
// "localhost" is spelled out because it is the one name whose meaning is fixed.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostOf is the host component of a "host:port" bind address, or the whole
// string when it does not split — a bind address with no port is malformed and
// net.Listen will reject it later; this predicate just needs something to
// classify. An empty host (":7421", every interface) is deliberately NOT
// loopback: binding every interface includes the LAN ones.
func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
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
	listen := envOr(env, "WAIVEO_RELAY_LISTEN", "127.0.0.1:7421")
	return config{
		listen:    listen,
		feederURL: envOr(env, "WAIVEO_FEEDER_URL", "https://127.0.0.1:7420"),
		pairHost:  envOr(env, "WAIVEO_RELAY_PAIR_HOST", "127.0.0.1"),
		// The relay's own operational store. Every other path either binary reads
		// is env-overridable; this one was a hardcoded constant, which made the
		// identity store a hidden GLOBAL — two relays on one machine shared it
		// however differently they were otherwise configured.
		//
		// That is not a tidiness point. The store holds the enrollment-anchored
		// pin REL-137 checks, so a relay started against a second app peer picks
		// up the identity enrolled with the FIRST and refuses the connection it
		// was just told to make: "the app peer at this address is NOT the one
		// this relay enrolled with". The relay says exactly that and names the
		// two causes, and one of them — "a second process holding the same
		// address" — is the one this constant made unavoidable, because the
		// isolated stack that would have ruled it out could not be started.
		identityPath: envOr(env, "WAIVEO_RELAY_IDENTITY_PATH", identity.DefaultPath),
		pairPort:     port,
		ecpTargets:   targets,
		pollInterval: time.Duration(pollMS) * time.Millisecond,
		discoveryOn:  discoveryEnabled(env("WAIVEO_RELAY_DISCOVERY"), listen),
		mdnsPatterns: parseMDNSPatterns(env("WAIVEO_RELAY_MDNS_PATTERNS")),
		ssdpAnnounce: env("WAIVEO_RELAY_SSDP_ANNOUNCE") == "1" || env("WAIVEO_RELAY_SSDP_ANNOUNCE") == "true",
		keepaliveOn:  keepaliveEnabled(env("WAIVEO_RELAY_KEEPALIVE")),
		// Read through the same opt-out helper the keep-alive switch uses, so
		// the two honor an identical off-list (see offValue's own doc for why
		// one list rather than one per switch).
		powerOnLaunchOn: keepaliveEnabled(env("WAIVEO_RELAY_POWERON_LAUNCH")),
	}, nil
}

// offValue reports whether raw spells an explicit "off" for one of this
// binary's two OPT-OUT switches (WAIVEO_RELAY_DISCOVERY, WAIVEO_RELAY_KEEPALIVE
// — see config's own doc for why those two default on and the multicast
// announce switches do not).
//
// Written as an explicit off-list rather than as `!= "0"` so a typo — the
// classic `=disabled` — does not silently mean "on" for an operator who plainly
// intended otherwise. Anything NOT on the list (including a typo the list does
// not anticipate) leaves the capability on, which is the safe direction for a
// default that exists so the box works out of the box.
//
// One shared list rather than one per switch, deliberately: the two flags are
// spelled the same way in the same deployment file, and an operator who learns
// that WAIVEO_RELAY_KEEPALIVE=disabled works is entitled to expect
// WAIVEO_RELAY_DISCOVERY=disabled to work too. The tracks that introduced these
// two switches independently arrived at off-lists differing by exactly that one
// spelling; unifying on the LONGER list keeps every spelling either track
// already honored.
func offValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no", "disabled":
		return true
	default:
		return false
	}
}

// discoveryEnabled decides whether this process sweeps, from
// WAIVEO_RELAY_DISCOVERY and — when that says nothing — from the deployment
// posture the listen address states.
//
// A STATED value wins in both directions: any explicit off value disables the
// sweep anywhere, and any other explicit value enables it anywhere (including
// on a loopback-bound run, which is how a developer deliberately sweeps a real
// LAN from a laptop).
//
// UNSET is where the two invariants this function has to satisfy at once meet:
//
//   - A deployed appliance must sweep. Off-by-default meant a fresh box
//     discovered nothing until somebody knew to set an environment variable —
//     it shipped unable to see the TVs it exists to drive, while the legacy
//     stack it replaces swept always-on.
//   - CI and loopback dev runs must never multicast. `make dev` on a laptop
//     on an office or café LAN must not M-SEARCH strangers and then HTTP-GET
//     /query/device-info on everything that answers, and a CI runner must not
//     either.
//
// The listen address separates them, and it is the honest discriminator rather
// than a proxy for one: a relay bound to loopback is by construction serving
// nothing but this machine — no screen on any LAN can reach it — so there is
// no fleet for it to discover. A relay bound anywhere else is reachable by the
// devices it is supposed to find.
//
// Deriving it from the binary's own configuration is deliberate, and is the
// second half of this fix. The invariant previously lived in a
// WAIVEO_RELAY_DISCOVERY=0 on one line of one make target, which is a guard
// rail that protects exactly the one command someone remembered to edit; the
// dev target it was supposed to protect had already lost it. A rule the
// process enforces about itself cannot be left off a launcher.
func discoveryEnabled(raw, listen string) bool {
	if strings.TrimSpace(raw) == "" {
		return !isLoopbackHost(hostOf(listen))
	}
	return !offValue(raw)
}

// keepaliveEnabled reads WAIVEO_RELAY_KEEPALIVE (and, on the same off-list,
// WAIVEO_RELAY_POWERON_LAUNCH) as an OPT-OUT: unset, or any value other than an
// explicit off, leaves the capability running (see config.keepaliveOn and
// config.powerOnLaunchOn for why each defaults on).
func keepaliveEnabled(raw string) bool {
	return !offValue(raw)
}

// onOff renders a boot-log capability flag as the word an operator scanning the
// log is looking for, rather than as `true`/`false`.
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
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
// relayRenewalWindow is how far ahead of the leaf's not_after the relay
// begins attempting proactive certificate renewal (REL-015; the contract
// draft-note's proposed default) — ~8% of the feeder's 365-day leaf
// lifetime, so a renewal that keeps failing still has thousands of paced
// retry opportunities (the supervisor's loop cadence + hourly connected
// ticker) before hard expiry ever threatens. relayRenewalWindowJitter
// widens each process's effective window by a random slice so a fleet of
// relays enrolled the same day does not converge on the feeder's
// re-enroll rate limit (REL-025) in lockstep.
const (
	relayRenewalWindow       = 30 * 24 * time.Hour
	relayRenewalWindowJitter = 3 * 24 * time.Hour
)

// flooredNowMs is THE reading every time-windowed decision in this binary makes
// — the latest of three values:
//
//   - the OS wall clock;
//   - the persisted clock floor (REL-130), which is advance-only, so a
//     backwards-jumped OS clock can never walk this reading behind a time the
//     relay has already verified; and
//   - the hint-adjusted runtime clock (REL-133-bounded; before any accepted hint
//     it reads as bare uptime and never wins the max).
//
// It exists as a named function, rather than inline at each caller, because
// every caller that reads a different clock quietly opts out of REL-130. That
// is what "on restart it MUST NOT adopt a wall-clock reading earlier than this
// persisted floor" is for: a rolled-back host clock must not be able to re-open
// a window that has closed — a certificate renewal that is due (REL-015), a
// pairing grant whose ttl elapsed (REL-121), a channel token past its
// expires_at (PLY-072). A floor store that cannot be read degrades to the other
// two readings rather than failing: the floor raises this value, never lowers
// it, so its absence can only make the reading more conservative.
func flooredNowMs(store *identity.Store, clock *clocktrust.RuntimeClock) int64 {
	nowMs := time.Now().UnixMilli()
	if floorMs, ok, err := store.ClockFloor(); err == nil && ok && floorMs > nowMs {
		nowMs = floorMs
	}
	if runtimeMs := clock.WallMillis(); runtimeMs > nowMs {
		nowMs = runtimeMs
	}
	return nowMs
}

// blankSuppressionReader is the PLY-155 display reading keep-alive suppresses
// recovery on, and the place this relay says when it cannot make that reading.
//
// PLY-155 gates on the TARGET screen's own active Lease; keep-alive polls DEVICE
// ENTITIES; and nothing on the wire binds one to the other (REL-063's
// device_inventory carries no screen reference). Where a relay serves exactly one
// screen the binding is not needed — every polled entity belongs to that screen,
// because there is no other. Where it serves several, an entity cannot be
// attributed, and this answers "" rather than a DIFFERENT screen's display.
//
// # The cost of that answer, and why it is now audible
//
// "" is not blank, so suppression does not fire: on a multi-screen site the
// relay will relaunch the channel on a screen an operator deliberately blanked.
// That is the accepted degrade — the alternative, reporting some other screen's
// display, would ALSO suppress recovery on a live screen, and would be wrong
// without any way to notice.
//
// What was not acceptable is that it happened SILENTLY. An operator watching a
// blanked lobby screen come back to life had nothing anywhere connecting that to
// a missing wire binding. This logs the condition once, so the behaviour is
// diagnosable without changing it.
//
// Once, not per poll: keep-alive polls continuously, and a line per poll would
// bury the fact rather than report it. The condition is a property of the
// deployment's shape rather than of any one poll. A relay whose screen set later
// collapses to one and grows again does not log a second time, which is the
// price of not being noisy.
func blankSuppressionReader(srv *playerserver.Server) func(string) string {
	var once sync.Once
	return func(string) string {
		screenID, sole := srv.SoleServedScreen()
		if !sole {
			once.Do(func() {
				log.Printf("waiveo-relay: keep-alive cannot attribute a polled entity to a screen on this relay, " +
					"so PLY-155 blank-display suppression is INACTIVE here: a screen left blank by its schedule may " +
					"have its channel relaunched by recovery. This needs an entity-to-screen binding the wire does " +
					"not carry (REL-063)")
			})
			return ""
		}
		return srv.CurrentDisplay(screenID)
	}
}

// renewalDue reports whether the persisted leaf is inside its proactive
// renewal window (REL-015), evaluated on a floor-aware clock: the LATEST of
// the OS wall clock, the persisted clock floor (REL-130 — advance-only, so
// a backwards-jumped OS clock can never suppress renewal past a time the
// relay already verified; REL-135's discipline that an untrusted relay
// clock must not block credential recovery), and the hint-adjusted runtime
// clock (REL-133-bounded; before any accepted hint it reads as bare uptime
// and never wins the max). A store with no persisted identity, or one whose
// certificate cannot be parsed, is reported not-due — enrollment (or the
// operator) owns those states, and a renewal loop must not spin on them.
func renewalDue(store *identity.Store, clock *clocktrust.RuntimeClock, window time.Duration) bool {
	id, ok, err := store.Identity()
	if err != nil {
		log.Printf("waiveo-relay: renewal predicate: read persisted identity: %v", err)
		return false
	}
	if !ok {
		return false
	}
	due, err := reenroll.ExpiresWithin(id.CertPEM, time.UnixMilli(flooredNowMs(store, clock)), window)
	if err != nil {
		log.Printf("waiveo-relay: renewal predicate: %v", err)
		return false
	}
	return due
}

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

	store, err := identity.Open(cfg.identityPath)
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

	// Open the relay/1 persistent connection immediately after enrollment and
	// before the desired-state pull: relayconn.Dial runs the whole handshake on
	// ONE mutually authenticated connection — the app peer challenges with a
	// nonce derived from THIS TLS session's exporter keying material (REL-040),
	// this relay signs it with its enrollment key (channel binding, REL-032),
	// declares its version/features/site/subnet/clock (REL-031), and adopts the
	// app peer's authoritative site_binding from hello-ack (REL-036). The
	// adopted site drives the edge engine's schedule/sun evaluation once the
	// automation stack boots below. Dial reads the persisted identity and the
	// enrollment-captured app-peer trust pin from the store on EVERY dial
	// (REL-011/137), so identity persistence carries across every reconnect. A
	// refusal or transport failure here is fatal ONLY when there is no
	// persisted snapshot to fall back on; otherwise the relay proceeds in
	// offline-serve mode (REL-055/061) with no live site adopted — and the
	// reconnect supervisor below keeps redialing in the background when the
	// failure is one the Error taxonomy marks recoverable.
	//
	// nudges decouples the connection's state.changed dispatcher (wired into
	// every Dial, including redials) from the live apply path installed further
	// down — see nudgeSink's own doc.
	// commands is the same decoupling for the device plane: relayconn.Config's
	// OnDeviceCommand has to be supplied at dial time, and the automation host
	// that executes a command does not exist until several boot steps later —
	// see deviceCommandSink's own doc for why that ordering is what left the
	// callback nil in the shipped binary.
	nudges := &nudgeSink{}
	commands := &deviceCommandSink{}
	// scans is the same late-wiring decoupling for the on-demand ACTIVE scan:
	// OnDiscoveryScan is needed at dial time, the discoverer that performs one
	// does not exist until the discovery block far below.
	scans := &discoveryScanSink{}
	dialConn := func() (*relayconn.Client, error) {
		return relayconn.Dial(relayDialConfig(cfg, store, nudges, commands, scans))
	}
	client, err := dialWithRetry(dialConn)
	helloOK := err == nil
	// connErr is captured under its own name (rather than read back from err
	// later) because err itself is reused by several unrelated `:=` statements
	// further down this function — by the time the supervisor gate below wants
	// to know WHY the connection failed (to decide whether background redial is
	// worth attempting, relayconn.RefusalIsRecoverable), the shared err variable
	// no longer holds this value.
	connErr := err
	var site hello.SiteBinding
	liveConn := &connHolder{}
	if helloOK {
		liveConn.set(client)
		site = client.HelloAck().SiteBinding
		log.Printf("waiveo-relay hello negotiated version %s; site %s", client.HelloAck().NegotiatedVersion, site.TZ)
	} else {
		if fatal := offlineServeFallback(err, hasPersisted); fatal != nil {
			log.Fatalf("waiveo-relay: connect to app peer: %v", fatal)
		}
		log.Printf("waiveo-relay: connect to app peer failed (%v); serving persisted last-applied offline (REL-055/061)", err)
	}

	// Pull + verify the feeder's signed desired-state snapshot over the
	// authenticated connection, against the trust anchor enrollment just
	// persisted. A failure here (bad signature, tampered sections, or a
	// regressed generation) is fatal ONLY when there is no persisted snapshot
	// to fall back on. When a prior pull already persisted a last-applied
	// generation, a failed pull is non-fatal: the relay keeps that durable copy
	// and serves it offline (REL-055/061), rather than exiting and leaving
	// every screen unable to pull /player/v1/program.
	//
	// A relay whose handshake the app peer REFUSED structurally cannot pull at
	// all on this transport: state.pull only exists on the authenticated
	// connection the refusal just denied, so the HTTP era's separate
	// "hello-refused relay must not pull anyway" gate is now enforced by
	// construction — an unaccepted relay serves its persisted last-applied
	// snapshot offline and nothing more, until the supervisor's redial is
	// accepted or an operator re-enrolls it.
	var applied desiredstate.Applied
	if helloOK {
		applied, err = pullOverFrames(client, store, 0)
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

	// The candidate store the discovery lanes below Observe into and the
	// device.candidates report rides from (REL-110/111). It is built HERE,
	// ahead of the resolver selection, because it is also an EntityResolver: a
	// device this relay discovered but nobody has adopted yet still has to
	// resolve when the app peer addresses a command to one of its entities, and
	// only the store knows what it discovered. Both peers derive those entity
	// ids from the same identity tuple (REL-110b), so the resolution below is a
	// comparison against values this relay derived itself.
	candStore := deviceplane.NewStore(relayID.RelayID)

	// The relay's device plane: the adoption gate, the ECP controller reading
	// its targets through it, and the entity resolver behind both dispatch paths
	// (edge rules and preset batches). Built by newDevicePlane so the wiring the
	// binary runs is the wiring a test can hold — see its own doc.
	plane := newDevicePlane(cfg.ecpTargets, candStore)
	deviceTargets, devController, baseController, devResolver := plane.targets, plane.controller, plane.base, plane.resolve
	log.Printf("waiveo-relay device plane: ECP controller live, adoption-gated (%d configured override(s); adopted devices resolve from device_inventory)",
		len(cfg.ecpTargets))

	host, err := bootAutomationStack(store, relayID, applied, site, deviceRegistry, devController, devResolver, commands)
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

	// The player/1 surface reads the floor-aware clock, not the host's: a
	// pairing grant's ttl and a channel token's expires_at are exactly the
	// time windows REL-130's persisted floor exists to keep a rolled-back host
	// clock from re-opening.
	pairingSrv, err := playerserver.NewServer(relayID.CertPEM, applied.PairingGrants, func() int64 {
		return flooredNowMs(store, clockCtl.Clock())
	})
	if err != nil {
		log.Fatalf("waiveo-relay: build player/1 pairing server: %v", err)
	}
	// The relay's own Lease-signing identity (PLY-090), installed before any
	// route is mounted. It is the private half of the keypair relayID.CertPEM
	// certifies — the very certificate this listener presents — so a player's
	// pinned-anchor signature check lines up with what it connected to.
	//
	// Installed here, once, rather than as a side effect of installing a program:
	// a relay with no program at all still has to answer a paired screen's pull
	// with data-model/1's terminal default (DAT-118), and that Lease needs this
	// key like any other. A site whose screen_programs is empty (REL-060's
	// placeholder), or whose entries were all derived away by an unresolvable
	// effective tz, is exactly that relay.
	pairingSrv.SetSigningKey(relayID.PrivateKey)
	// Point the pairing/session surface at the SAME durable operational store
	// enrollment identity and last-applied generation already persist into, so
	// a minted channel token and a one-time pairing grant's redemption both
	// survive this process's own restart (REL-120's sole-issuer/sole-verifier
	// role; see playerserver.Server.EnablePersistence's own doc). Must happen
	// before pairingSrv.Register mounts routes onto mux below.
	pairingSrv.EnablePersistence(store)

	// Wire the RETURN PATH — a viewer's press on an interactive slide layer into
	// the durable telemetry buffer, and from there to the app peer's event log
	// and any automation watching for it. Installed BEFORE pairingSrv.Register
	// mounts the routes below: with no recorder a press is refused 503, and
	// refusing a real press is exactly what this ordering avoids. See
	// wireInteractionRecorder (interactionwiring.go) for the whole argument.
	wireInteractionRecorder(pairingSrv, host.TelemetryBuffer())

	// Install the live data a native slide's server-resolved widgets are filled
	// from as this relay signs each Lease (internal/slidelive):
	//
	//   - Weather comes from the keyless Open-Meteo source, asked at the site's
	//     OWN coordinates — the app peer's authoritative site_binding (REL-036),
	//     which is the site scope node's DAT-033 effective geo already resolved
	//     by the app. That is the same value bootAutomationStack adopts into the
	//     engine for sun/schedule triggers, so a slide's weather and a rule's
	//     sunset agree about where this relay is by construction.
	//   - Entity state comes from the automation host, which records every
	//     device-plane observation that flows through it (Host.EntityState). A
	//     relay with no polled devices simply never populates it and every
	//     entity widget shows the unavailable placeholder.
	//
	// Installed through siteGeo rather than written here once, because `site` is
	// the zero binding on an offline boot (REL-055/061) and this relay must
	// self-correct when the real one arrives on a later hello-ack instead of
	// asking the weather at (0,0) until someone restarts the process — see
	// siteGeo's own doc, and slidelive.Sources.HasGeo for why (0,0) draws a dash
	// rather than the Gulf of Guinea's forecast. The same value is adopted into
	// the automation engine's location, so a slide's weather and a rule's sunset
	// agree about where this relay is by construction, on every adoption and not
	// just the first. The forecast pull itself is background and never blocks a
	// Lease.
	geo := &siteGeo{
		setSlideLive: pairingSrv.SetSlideLive,
		setLocation:  host.SetLocation,
		weather:      slidelive.NewOpenMeteo(slidelive.OpenMeteoConfig{}),
		entity:       slidelive.EntitySourceFunc(host.EntityState),
	}
	geo.adopt(site)

	logPairingCodes(cfg, applied, certDER, relayID.RelayID)

	// Configure the serving surface from the relay's OWN durable last-applied
	// row — its programs AND its revocation set (REL-055/061/066) — rather than
	// from the live Applied value, so a boot whose pull failed serves and
	// enforces exactly what one whose pull succeeded does. See
	// installPersistedServingState's own doc.
	applied, err = installPersistedServingState(store, pairingSrv, applied)
	if err != nil {
		log.Fatalf("waiveo-relay: install persisted serving state: %v", err)
	}

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
	commandSurface := deviceplane.NewCommandSurface(devController, deviceRegistry, devResolver,
		// The label an unresolved command's log line carries. A preset batch
		// firing at an entity this relay cannot resolve — a renamed or
		// unadopted device, which is the ordinary shape of the failure — is
		// refused inside the surface, ahead of the loggingController wrapper
		// below, so this is the only thing that names it (deviceplane's
		// logUnresolved).
		deviceplane.WithCommandSource("preset"))

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
	//
	// The poller runs UNCONDITIONALLY and takes its target set from the same
	// adoption gate the controller dispatches through, refreshed on every
	// generation apply (installInventory below). It used to start only when
	// WAIVEO_RELAY_ECP_TARGETS was set, which meant the default deployment
	// observed nothing: no rule could fire on a real device, and no entity ever
	// reported a state. With zero targets the loop is a no-op tick, so starting
	// it always costs nothing and removes the "polling exists but only for the
	// env-var deployment" split.
	poller := ecppoll.New(nil, cfg.pollInterval)

	// installInventory is the ONE place a generation's adopted-device set
	// (REL-063) becomes drivable: it replaces the gate's adopted set and
	// re-points the poller at the resulting target set, so "what this relay
	// commands" and "what this relay observes" can never be two different lists.
	// It runs once here for the boot generation and then on every live apply
	// (rePuller.applyInventory).
	installInventory := func(inv wire.DeviceInventory) {
		adopted := deviceTargets.SetInventory(inv.Devices)
		targets := pollTargetsFor(deviceTargets)
		poller.SetTargets(targets)
		log.Printf("waiveo-relay device plane: %d adopted+enabled entit(ies) in device_inventory, %d currently drivable (adopted and locatable, plus overrides)",
			adopted, len(targets))
	}
	installInventory(applied.DeviceInventory)

	go poller.Run(rootCtx)

	// Start the edge engine's TWO drive loops — the observation loop over the
	// poller's stream and the time loop that advances `delay` resumption,
	// `for`-holds, the stabilization release and every wall-clock trigger. They
	// start together, from one named call, because an engine driven by only one
	// of them is silently broken rather than merely reduced; see
	// startAutomationDriveLoops' own doc for what shipped dead when the time
	// half was missing.
	startAutomationDriveLoops(rootCtx, host, poller, automationTickInterval)
	log.Printf("waiveo-relay device polling live (every %s; targets follow the adopted set); edge engine time drive live (every %s)",
		cfg.pollInterval, automationTickInterval)

	// Screen keep-alive (internal/relay/keepalive, player/1 PLY-150-157): a
	// second, independent ECP poller over the adopted set that re-launches a
	// screen's player channel once it safely idles at Home (power-on settle
	// delay, ≥2-consecutive-poll Home confirmation, never while standby, never
	// while the screen's own active Lease is blank) — see keepalive's own
	// package doc for why this needs its OWN Poller rather than sharing the one
	// host.Run above already exclusively consumes. ON by default
	// (WAIVEO_RELAY_KEEPALIVE=0 disables it) — see config.keepaliveOn's own doc.
	//
	// Its target set is the ADOPTION GATE's, refreshed on every devicePlaneSync
	// tick like the state poller's, and that is what makes the on-by-default
	// flip mean anything. It used to be cfg.ecpTargets, fixed at construction —
	// and cfg.ecpTargets is, since the device-control track, an out-of-band
	// escape hatch rather than the normal path. So the capability was switched
	// on in a deployment where the map it watched was empty: it self-healed
	// nothing, silently, in exactly the power-cut scenario it was turned on for.
	// It is started whenever the capability is on, with whatever the gate
	// resolves at boot (possibly nothing), because the set it watches is now
	// something that arrives later rather than something known here.
	//
	// Widening what it WATCHES does not widen what it may drive: adoption is
	// enforced separately, per dispatch, by the Adopted gate wired below, and
	// the target set is itself already the adopted-and-locatable intersection.
	//
	// keepaliveAdoption is declared out here, above the block, because two
	// places need it: this wiring reads it on every poll, and the live
	// re-pull path (rePuller, livepull.go) REFRESHES it on every applied
	// generation. It stays nil when the capability is off, which is the
	// signal the puller uses to skip refreshing a set nothing consults.
	var keepaliveAdoption *keepalive.AdoptionSet
	var keepaliveTargetSink keepaliveTargets
	if cfg.keepaliveOn {
		kaTargets := make(map[string]keepalive.Target)
		for entityID, ep := range deviceTargets.Targets() {
			kaTargets[entityID] = keepalive.Target{Host: ep.Host, Port: ep.Port}
		}

		// The adoption gate, seeded from the generation this boot applied and
		// refreshed by every later one. Seeding here rather than leaving it
		// empty until the first live pull matters on the path this whole
		// capability exists for: after a power cut the relay boots, applies
		// its persisted or freshly-pulled generation, and the screens are
		// already sitting at Home — waiting for a nudge that may be minutes
		// away would be waiting through exactly the outage.
		//
		// "Its persisted OR freshly-pulled generation" is load-bearing, and was
		// for a while simply false. A boot whose pull fails leaves `applied`
		// the zero value (see the pull site above), so this line seeded the
		// gate with an EMPTY device_inventory and keep-alive relaunched
		// nothing — on the offline boot, which is the power-cut boot, which is
		// the scenario. It is true now because `device_inventory` is persisted
		// in the last-applied row beside screen_programs and revoked, and
		// installPersistedServingState restores it onto `applied` before this
		// point (REL-055/061/063).
		keepaliveAdoption = keepalive.NewAdoptionSet()
		keepaliveAdoption.Apply(applied.Generation, applied.DeviceInventory)

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
					deviceRegistry, devResolver,
					// The same label the wrapper above stamps on a DISPATCHED
					// command, so a keep-alive recovery that never reached a
					// device reads as one line in the same grammar rather than
					// as an unattributed refusal.
					deviceplane.WithCommandSource("keep-alive")),
				relayID.RelayID),
			// PLY-155 gates on "the TARGET screen's own currently active
			// Lease", and keepalive polls DEVICE ENTITIES: answering needs an
			// entity -> screen binding the relay does not have. relay/1's
			// `device_inventory` (REL-063) carries no screen reference on an
			// adopted entity, and ecpTargets is keyed by entity id alone.
			//
			// Where this relay serves exactly ONE screen the binding is not
			// needed to answer — every polled entity belongs to that screen,
			// because there is no other — and this reports its real display.
			// Where it serves several, an entity cannot be attributed to one of
			// them, so this reports "" (keepalive's documented not-blank
			// degrade) rather than a DIFFERENT screen's display, which would
			// both suppress recovery on a live screen and relaunch an
			// intentionally blanked one.
			ActiveDisplay: blankSuppressionReader(pairingSrv),
			// The adoption gate (keepalive.AdoptionSet): reachability is not
			// permission. This relay can reach every Roku on the LAN,
			// including the ones the legacy stack is still watchdogging
			// during coexistence, and two controllers re-launching one Roku
			// makes it flap. Only entities the app peer's signed
			// `device_inventory` marks adopted + enabled are driven.
			Adopted: keepaliveAdoption.IsAdopted,
			// Power-on auto-launch (parity row 5.6, keepalive's rule 1b): on by
			// default, off with WAIVEO_RELAY_POWERON_LAUNCH=0. It rides INSIDE
			// keepalive rather than as a second relauncher of its own precisely
			// so it inherits this same adoption gate, this same settle delay,
			// this same blank-Lease suppression and this same serialized
			// dispatch surface — two independent things launching one Roku is
			// the flap this deployment has already been bitten by.
			PowerOnLaunch: cfg.powerOnLaunchOn,
		})
		keepaliveTargetSink = ka
		go func() {
			if err := ka.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("waiveo-relay: screen keep-alive ended: %v", err)
			}
		}()
		log.Printf("waiveo-relay screen keep-alive live (%d target(s) at boot, following the adopted set, every %s; power-on auto-launch %s)",
			len(kaTargets), cfg.pollInterval, onOff(cfg.powerOnLaunchOn))
	}

	// The periodic join across the device plane: re-derive the drivable set,
	// re-point both pollers at it, and copy what the state poller has observed
	// onto the candidates the device.candidates report is built from. It is a
	// named unit (deviceplanesync.go) rather than a closure here because it is
	// the sole mechanism behind two user-facing behaviours and, as a closure,
	// could be deleted whole with every gate still green — and it is started
	// through a named function, rather than assembled and `go`-ed here, so that
	// deleting THIS line cannot be green either (TestMainStartsTheDevicePlaneSync).
	startDevicePlaneSync(rootCtx, deviceTargets, poller, candStore, keepaliveTargetSink, cfg.pollInterval)

	// Discovery (REL-110/111): SSDP client sweep + mDNS listener each mint
	// per-DEVICE candidates into ONE SHARED candidate store when both lanes
	// are on — REL-110's device.candidates report is a full-set report per
	// relay, not per discovery lane, so a candidate either lane observes must
	// land in the same Store regardless of which one matched it. Timestamps are
	// wall-clock Timestamp-ms — the store is in-memory relay state, not
	// persisted evidence, so clock-trust gating does not apply to it.
	//
	// Each lane is configured with WATCHES rather than bare match patterns: a
	// response identifies WHICH device answered (its USN, or the mDNS instance
	// name), and the watch supplies the driver, device class and entity fan-out
	// that identity is reported under (REL-110a). Those are declaration-side
	// facts — a pack's device contribution, manifest/1 MAN-070 — not something
	// a discovery response could state.
	// The BUILTIN watch sets: what this deployment sweeps for before (and
	// beside) any pack declaration. The roku:ecp watch is the one piece of
	// device knowledge still hardcoded here — its extraction into a pack
	// declaration is the Discovery programme's D2/D3, and mergeSSDPWatches'
	// builtin-first precedence is what lets it move without a window where a
	// colliding pack declaration degrades it.
	builtinSSDP := []discovery.Watch{{
		Match:       deviceplane.Match{SSDP: rokuSearchTarget},
		Driver:      rokuDriver,
		DeviceClass: mediaPlayerClass,
		DefaultPort: rokuECPPort,
		Entities:    []deviceplane.CandidateEntity{{Key: mainEntityKey, DeviceClass: mediaPlayerClass}},
	}}
	builtinMDNS := make([]mdns.Watch, len(cfg.mdnsPatterns))
	for i, svcType := range cfg.mdnsPatterns {
		builtinMDNS[i] = mdns.Watch{
			Match:       deviceplane.Match{MDNS: svcType},
			Driver:      mdnsDriver,
			DeviceClass: mediaPlayerClass,
			Entities:    []deviceplane.CandidateEntity{{Key: mainEntityKey, DeviceClass: mediaPlayerClass}},
		}
	}
	// The boot generation's pack patterns seed the lanes' INITIAL sets — the
	// lanes are constructed after the boot inventory applies, so without this
	// a pack-declared watch would not exist until the first LIVE apply.
	bootSSDPW, bootMDNSW, _, _ := patternWatchSets(applied.DeviceInventory.PackMatchPatterns)

	var disc *discovery.Discoverer
	var mdnsListener *mdns.Listener
	if cfg.discoveryOn || len(cfg.mdnsPatterns) > 0 {
		if cfg.discoveryOn {
			// The neighbour lane (Discovery spec §4.1): every L2-present host in
			// the relay host's own kernel neighbour table becomes an
			// unclassified MAC-keyed candidate — the enumerate-all FOUNDATION
			// the protocol lanes merge onto. It is a LOCAL READ, not a scan
			// (the kernel populated the table from ordinary traffic), so it runs
			// with discovery by default; it is why the box lists every host on
			// the segment where watch-driven discovery listed only the one Roku
			// a built-in pattern named.
			neighborLane, err := neighbor.New(neighbor.Config{
				Store:     candStore,
				NowMillis: func() int64 { return time.Now().UnixMilli() },
			})
			if err != nil {
				log.Fatalf("waiveo-relay: configure neighbour discovery: %v", err)
			}
			go func() {
				if err := neighborLane.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("waiveo-relay: neighbour discovery ended: %v", err)
				}
			}()
			log.Printf("waiveo-relay neighbour discovery live (kernel neighbour table; every L2-present host, unclassified until a pattern claims it)")

			// The host-avahi mDNS lane (spec §11 L6): read the HOST avahi
			// daemon's cache (a local read, not a bind) and merge the human
			// name of every mDNS-advertising host onto its neighbour candidate.
			// It names a device the neighbour lane found — including one that is
			// mDNS-advertising while SSDP-silent — rather than leaving it an
			// anonymous MAC. Needs the neighbour resolver to correlate a service
			// to a MAC, so it is wired here beside the lane that provides it.
			hostMDNSLane, err := hostmdns.New(hostmdns.Config{
				Store:      candStore,
				NowMillis:  func() int64 { return time.Now().UnixMilli() },
				ResolveMAC: neighborLane.MAC,
			})
			if err != nil {
				log.Fatalf("waiveo-relay: configure host-avahi mDNS discovery: %v", err)
			}
			go func() {
				if err := hostMDNSLane.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("waiveo-relay: host-avahi mDNS discovery ended: %v", err)
				}
			}()
			log.Printf("waiveo-relay host-avahi mDNS discovery live (host avahi cache; names every mDNS-advertising host, incl. SSDP-silent ones)")

			// One HTTP client for every identification probe: connections to a
			// device are pooled and reused across sweeps instead of being dialed
			// fresh each time (see ecp.NewIdentifyClient for the timeout).
			identifyClient := ecp.NewIdentifyClient()
			disc, err = discovery.New(discovery.Config{
				Watches: mergeSSDPWatches(builtinSSDP, bootSSDPW),
				Store:   candStore,
				// Resolve an SSDP responder's IP to the MAC the neighbour lane
				// saw it at, so a device both lanes found is ONE candidate under
				// the canonical MAC identity (spec §4.1), not a double-counted
				// row — the Roku shows once, not as both a raw host and a Roku.
				ResolveMAC: neighborLane.MAC,
				NowMillis:  func() int64 { return time.Now().UnixMilli() },
				// The identification probe: a discovered address that answers
				// Roku's ECP device-info query IS a Roku, and says which one.
				// Without it a sweep reports opaque USNs an operator cannot map
				// onto the TVs in front of them (internal/relay/ecp).
				Identify: func(ctx context.Context, address string) (discovery.Identity, bool) {
					info, err := ecp.QueryDeviceInfo(ctx, identifyClient, address)
					if err != nil {
						return discovery.Identity{}, false
					}
					return discovery.Identity{Name: info.Name, Model: info.Model, Serial: info.SerialNumber}, true
				},
			})
			if err != nil {
				log.Fatalf("waiveo-relay: configure SSDP discovery: %v", err)
			}
			go func() {
				if err := disc.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("waiveo-relay: discovery ended: %v", err)
				}
			}()
			// The ACTIVE half is reachable only from here: an operator's
			// `discovery.scan` runs disc.Scan once. Run itself is passive and
			// never searches (Discovery spec §4, owner 2026-08-17).
			//
			// Single-flight: one scan at a time per relay. A second request while
			// one is running is answered as an accepted no-op carrying the
			// RUNNING scan's id rather than starting a concurrent sweep — an
			// operator double-click must not double the traffic on the segment.
			// The scan runs on its own goroutine so the handler can answer
			// immediately with acceptance; findings ride the ordinary
			// device.candidates report.
			var scanMu sync.Mutex
			var scanRunning string
			scanDisc := disc
			scans.set(func(body wire.DiscoveryScanBody) wire.DiscoveryScanResultBody {
				scanMu.Lock()
				if scanRunning != "" {
					id := scanRunning
					scanMu.Unlock()
					return wire.NewDiscoveryScanBusy(id)
				}
				id := ulid.New()
				scanRunning = id
				scanMu.Unlock()

				go func() {
					defer func() {
						scanMu.Lock()
						scanRunning = ""
						scanMu.Unlock()
					}()
					startedAt := time.Now().UnixMilli()
					log.Printf("waiveo-relay: discovery scan %s starting (operator-requested)", id)
					reportScanStatus(liveConn, wire.DiscoveryScanStatusBody{
						State: wire.DiscoveryScanStateScanning, ScanID: id, StartedAt: startedAt,
						Candidates: len(candStore.Report().Body.Candidates),
					})
					// ACTIVE LANE 1 — the ARP sweep. It mints nothing itself: it
					// pokes each address on this relay's own private subnet so the
					// kernel learns hosts that have not spoken recently, and the
					// PASSIVE neighbour lane then reads them. Refresh() is what
					// makes them appear now rather than up to a sweep interval
					// later. A refusal (a segment larger than the cap) is logged
					// and the scan continues with its other lanes — one lane that
					// cannot run is not a failed scan.
					if res, err := arpsweep.Sweep(rootCtx, arpsweep.Config{}); err != nil {
						log.Printf("waiveo-relay: discovery scan %s: ARP sweep skipped: %v", id, err)
					} else if res.Probed > 0 {
						log.Printf("waiveo-relay: discovery scan %s: ARP sweep probed %d address(es) across %v", id, res.Probed, res.Subnets)
						neighborLane.Refresh()
					}
					// ACTIVE LANE 2 — the port scan. It runs AFTER the sweep and
					// its Refresh, so it scans the hosts this relay can actually
					// see, including the ones the sweep just revealed. The findings
					// attach to the SAME MAC-keyed candidate the passive lanes
					// maintain (ObservePorts) rather than minting a second row.
					if hosts := neighborLane.Hosts(); len(hosts) > 0 {
						open := portscan.Scan(rootCtx, hosts, portscan.Config{})
						attached := 0
						for ip, ports := range open {
							if neighborLane.ObservePorts(ip, ports, time.Now().UnixMilli()) {
								attached++
							}
						}
						log.Printf("waiveo-relay: discovery scan %s: port scan found open ports on %d of %d host(s), attached to %d candidate(s)",
							id, len(open), len(hosts), attached)
					}
					scanDisc.Scan(rootCtx)
					log.Printf("waiveo-relay: discovery scan %s complete", id)
					reportScanStatus(liveConn, wire.DiscoveryScanStatusBody{
						State: wire.DiscoveryScanStateIdle, ScanID: id, StartedAt: startedAt,
						FinishedAt: time.Now().UnixMilli(), Candidates: len(candStore.Report().Body.Candidates),
					})
				}()
				return wire.NewDiscoveryScanAccepted(id)
			})
			log.Printf("waiveo-relay SSDP discovery live — PASSIVE listen only; active M-SEARCH runs on an operator scan (builtin %s; pack patterns follow desired state, REL-064)", rokuSearchTarget)
		}

		// The mDNS lane stays a deployment OPT-IN (env patterns set), even now
		// that pack-declared mDNS patterns could otherwise feed it: constructing
		// it means BINDING 5353 multicast, which the config doc above draws as
		// the intrusiveness line — it collides outright with a host's own avahi,
		// and CI/loopback runs must never multicast. A pack declaration must not
		// be able to force that bind on a deployment that did not opt in; when
		// mDNS patterns arrive with no lane to land on, the watch applier SAYS
		// so instead of silently absorbing them.
		if len(cfg.mdnsPatterns) > 0 {
			var err error
			mdnsListener, err = mdns.New(mdns.Config{
				Watches:   mergeMDNSWatches(builtinMDNS, bootMDNSW),
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
			log.Printf("waiveo-relay mDNS discovery live (env patterns [%s]; pack patterns follow desired state, REL-064)", strings.Join(cfg.mdnsPatterns, ", "))
		}

		// The full-set report rides upward on the discovery cadence (REL-110/111
		// make every report a complete replace, so a periodic one is idempotent
		// and a lost one is recovered by the next). Reconnects report
		// immediately from OnConnected below rather than waiting for this tick.
		go func() {
			tick := time.NewTicker(candidateReportInterval)
			defer tick.Stop()
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-tick.C:
					reportCandidates(liveConn.get(), candStore)
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
	if cfg.ssdpAnnounce && cfg.dialAddress() == "" {
		// Multicasting "reach me at 127.0.0.1" to the LAN is the same defect a
		// formed pairing code would carry (dialAddress's own doc), and louder:
		// every player that hears it records an address pointing at itself.
		log.Printf("waiveo-relay: SSDP responder NOT started: listening on %s but the pairing dial host is %q (loopback), so the announced base URL would point a screen at its own loopback. Set WAIVEO_RELAY_PAIR_HOST.",
			cfg.listen, cfg.pairHost)
	} else if cfg.ssdpAnnounce {
		baseURL := fmt.Sprintf("https://%s/player/v1", cfg.dialAddress())
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
		srv:       pairingSrv,
		sink:      scheduleSink,
		site:      site,
		tickEvery: scheduleResolverTickInterval,
	}
	driver.apply(rootCtx, applied, time.Now().UnixMilli())

	// The relay's live path (relay/1 REL-050/052/055/056/057): the persistent
	// connection replaces the HTTP era's 3s poll ticker AND its separate hello
	// recovery loop with one mechanism. A server-initiated state.changed nudge
	// triggers an immediate pull-and-apply (puller.tick — a strictly higher
	// generation re-drives the schedule resolvers and reloads the edge rules; a
	// same/lower one is a no-op; a mid-run failure is non-fatal, the
	// last-applied stays served offline). The reconnect supervisor keeps the
	// connection alive for the life of the process: when it dies, it re-dials
	// under exponential backoff for every failure the Error taxonomy marks
	// recoverable (transport failures, CHANNEL_BINDING_INVALID,
	// RELAY_IDENTITY_MISMATCH — exactly the classification the HTTP era's hello
	// recovery applied), adopts each fresh hello-ack's authoritative site
	// (REL-036), and pulls once immediately so a relay offline through N
	// generations converges without waiting for a nudge (a lost nudge's
	// recovery path, REL-057).
	//
	// A relay whose handshake was refused cannot pull by construction — the
	// pull only exists on the authenticated connection — so the HTTP era's
	// "live loop gated on hello acceptance" invariant needs no separate gate
	// here: an unaccepted relay serves its persisted last-applied snapshot
	// offline and nothing more, until a redial is accepted.
	// Live applies drive BOTH inventory consumers: the adoption gate
	// (installInventory) and the discovery watch sets (REL-064). One composed
	// hook rather than a second rePuller field, so the two can never be
	// refreshed from different generations.
	applyDiscoveryWatches := discoveryWatchApplier(disc, mdnsListener, builtinSSDP, builtinMDNS)
	// Boot generation, once, for the log line: the lanes were SEEDED with these
	// watches at construction (they must not sweep even once without them), so
	// this re-install is idempotent — its value is the operator-visible count of
	// what the boot generation declared, including what could NOT be delivered
	// (macOui, malformed), which the seeding path has no place to say.
	applyDiscoveryWatches(applied.DeviceInventory)
	puller := &rePuller{
		nowFn:    func() int64 { return time.Now().UnixMilli() },
		driver:   driver,
		host:     host,
		lastGen:  applied.Generation,
		lastHash: applied.Hash,
		applyInventory: func(inv wire.DeviceInventory) {
			installInventory(inv)
			applyDiscoveryWatches(inv)
		},
		adoption: keepaliveAdoption,
		geo:      geo,
	}
	puller.pull = func(since int64) (desiredstate.Applied, error) {
		c := liveConn.get()
		if c == nil {
			return desiredstate.Applied{}, errors.New("no live app-peer connection")
		}
		return pullOverFrames(c, store, since)
	}
	nudges.set(func(int64) { puller.tick(rootCtx) })

	if helloOK || relayconn.RefusalIsRecoverable(connErr) {
		// Seed the supervisor with the boot connection when one is up, so its
		// first "connect" adopts the live connection instead of dialing a
		// second one; every later connect is a fresh dial.
		var seedMu sync.Mutex
		seed := client // nil when the boot dial failed
		// Proactive certificate renewal rides the supervisor's own cadence
		// (REL-015): renewalDue watches the persisted leaf's not_after on a
		// floor-aware clock, and reenroll.Renew reuses the enrollment
		// keypair (its own doc: the SPKI is the player-pinned trust anchor,
		// REL-126/PLY-052, and the lease-signing key — rotation would
		// strand every paired player at the next restart). A successful
		// renewal touches ONLY the store: this process keeps serving
		// players from the in-memory relayID leaf it booted with, the live
		// app-peer connection continues under the leaf it authenticated
		// with (REL-015's cutover sentence), and the fresh leaf rides the
		// next redial — relayconn.Dial re-reads the store on every dial.
		renewWindow := relayRenewalWindow + rand.N(relayRenewalWindowJitter)
		// The operator's account of this connection's life (connReporter's
		// own doc for why it exists at all).
		connReport := newConnReporter(log.Printf, time.Now)
		relayconn.StartSupervisor(relayconn.SupervisorConfig{
			NeedsRenewal: func() bool { return renewalDue(store, clockCtl.Clock(), renewWindow) },
			Renew: func() error {
				if err := reenroll.Renew(cfg.feederURL, store); err != nil {
					log.Printf("waiveo-relay: proactive certificate renewal failed (retrying on the supervisor cadence): %v", err)
					return err
				}
				log.Printf("waiveo-relay: certificate renewed ahead of expiry (REL-015) — fresh leaf persisted; it takes effect on the next redial")
				return nil
			},
			Connect: func() (*relayconn.Client, error) {
				seedMu.Lock()
				c := seed
				seed = nil
				seedMu.Unlock()
				if c != nil {
					return c, nil
				}
				return dialConn()
			},
			// The dead connection must stop being this process's live
			// connection the instant the supervisor says it died — before
			// the first redial, and without waiting for a SUCCESSFUL one to
			// overwrite it, which is precisely the event that does not
			// happen during an outage (connHolder.clear's doc, HV-22).
			OnDisconnected: func(err error, connectedFor time.Duration) {
				liveConn.clear()
				connReport.disconnected(err, connectedFor)
			},
			OnConnectFailed: connReport.connectFailed,
			OnConnected: func(c *relayconn.Client) {
				connReport.connected()
				liveConn.set(c)
				puller.adoptSite(c.HelloAck().SiteBinding)
				// Adopt the app peer's authoritative site (REL-036) into the
				// candidate store: it is the third member of the identity tuple
				// both peers derive device/entity ids from (REL-153/110b), so
				// until it is adopted this relay cannot resolve a command
				// addressed to a device it discovered. Then report the full
				// current set immediately — a reconnecting relay's app peer has
				// no view of it until it does (REL-110/111).
				candStore.SetSite(c.HelloAck().SiteBinding.ScopeNode)
				reportCandidates(c, candStore)
				// Same immediacy for screen liveness (parity row 5.8): until
				// this relay says something, the app peer's view of every
				// screen behind it is whatever it held before the disconnect,
				// ageing.
				reportScreenStatus(c, pairingSrv)
				// REL-124's "next connection opportunity", taken literally:
				// every pairing-grant redemption performed while this relay was
				// disconnected (REL-122) — or before its last restart — is owed
				// upstream, and this is the first moment it can be paid.
				reportRedemptions(c, pairingSrv)
				// Pull-on-reconnect immediacy (and, for the seeded boot
				// connection, a catch-up for anything authored between the
				// boot pull and this point): a same-generation answer is a
				// no-op state.unchanged.
				puller.tick(rootCtx)
			},
			OnPermanentRefusal: func(r *relayconn.Refusal) {
				log.Printf("waiveo-relay: app peer refused the connection permanently (%v) — supervision ended; an operator must intervene (relay/1 Error taxonomy)", r)
			},
		})
		log.Printf("waiveo-relay: persistent-connection supervisor started — state.changed nudges drive live desired-state applies (REL-057)")
	} else {
		log.Printf("waiveo-relay: connection supervisor NOT started — %v is not a recoverable refusal (relay/1 Error taxonomy); serving persisted last-applied offline until an operator intervenes", connErr)
	}

	// REL-124/REL-124d: a redemption performed WHILE connected is owed upstream
	// too, and the redemption path itself never touches the connection — a
	// player's POST /player/v1/pair must not block on the app peer, and REL-122
	// makes redemption legal with no app peer at all. So the owed-report ledger
	// is drained on this cadence as well as on connect, and a report stays owed
	// until its own ack arrives.
	go func() {
		tick := time.NewTicker(redemptionReportInterval)
		defer tick.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-tick.C:
				reportRedemptions(liveConn.get(), pairingSrv)
			}
		}
	}()

	// The LIVE SCREEN STATUS report (parity row 5.8): what every screen behind
	// this relay has actually been observed doing, pushed upward on its own short
	// cadence so a console's screens page is describing the fleet as it is rather
	// than as it was authored.
	//
	// Its own loop rather than a rider on the candidate loop above, because the
	// two are gated differently and answer different questions: candidate
	// reporting only runs when DISCOVERY is on (a relay with discovery disabled
	// still serves screens and must still report their liveness), and a candidate
	// set changes on the minute scale while a screen's liveness is only useful on
	// the ten-second scale.
	//
	// It is also reported immediately on connect (OnConnected above), for the
	// same reason candidates are: a reconnecting relay's app peer holds nothing
	// about it until it says something, and waiting out a tick would leave every
	// screen behind a just-recovered relay reading as stale for that whole tick.
	go func() {
		tick := time.NewTicker(screenStatusReportInterval)
		defer tick.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-tick.C:
				reportScreenStatus(liveConn.get(), pairingSrv)
			}
		}
	}()

	// Wire the relay's telemetry upstream channel (relay/1 REL-090/092/097): the
	// automation stack records a fired rule's automation.run into the durable
	// telemetry buffer (host.TelemetryBuffer); this Channel pushes that buffer to
	// the co-located app peer's /telemetry/v1/push ingest route over the
	// feeder-trusting TLS client, on a bounded interval. A received ack_through_seq
	// advances the buffer's retention (REL-092); an un-acked batch (app peer down
	// or rejecting) is retained and retried on the next tick (REL-097 — no silent
	// loss). This is the live delivery that carries a fired rule's event off the
	// relay to the app's event log.
	//
	// The push is mutually authenticated: the client presents this relay's
	// enrollment-issued leaf, re-read from the persisted identity on every
	// handshake so a renewed certificate (REL-015) takes effect without a
	// restart, falling back to the leaf loaded at boot if the store cannot be
	// re-read (that leaf is still valid until its own expiry, and refusing to
	// push telemetry over a transient store read error would discard
	// observability for no safety gain — the app peer still verifies whatever is
	// presented).
	currentLeaf := func() (*tls.Certificate, error) {
		id, enrolled, err := store.Identity()
		if err != nil || !enrolled {
			return &cert, nil
		}
		renewed, _, err := relayTLSCertificate(id)
		if err != nil {
			return &cert, nil
		}
		return &renewed, nil
	}
	telemetryChannel := telemetry.NewChannel(
		host.TelemetryBuffer(),
		telemetryhttp.New(cfg.feederURL, feederTLSClient(currentLeaf)),
		nil, // single-attempt per Flush; an un-acked batch rides the next flush tick
	)
	telemetryFlushTicker := time.NewTicker(telemetryFlushInterval)
	go telemetryFlushLoop(rootCtx, telemetryFlushTicker.C, telemetryChannel)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzWithVitals(relayID.RelayID, cfg.identityPath))
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

	log.Print(listeningLine(cfg))
	log.Fatal(server.ListenAndServeTLS("", ""))
}

// listeningLine is the line the relay logs once its player/1 listener is
// configured: where it listens, and the address a pairing code formed here tells
// a screen to dial.
//
// It reads cfg.dialAddress() rather than the raw pairHost/pairPort, because
// those two disagree exactly when the deployment is misconfigured — and that is
// the moment an operator is reading this line. Printing "pairing code dial
// 127.0.0.1:7421" from the raw fields while logPairingCodes prints "NOT forming
// pairing codes" from the same configuration left a boot log asserting both, and
// the wrong one first.
func listeningLine(cfg config) string {
	if addr := cfg.dialAddress(); addr != "" {
		return fmt.Sprintf("waiveo-relay listening (HTTPS) on %s (pairing code dial %s)", cfg.listen, addr)
	}
	return fmt.Sprintf("waiveo-relay listening (HTTPS) on %s (no dialable pairing address: pairing dial host is %q behind this listener, so no pairing code is formed — set WAIVEO_RELAY_PAIR_HOST)",
		cfg.listen, cfg.pairHost)
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
// every applied pairing grant THIS relay could actually redeem, so a developer
// can read one off the relay's own log and hand it to a later player/1 client
// task. A grant bound to a different relay (REL-121b) is skipped — see below.
func logPairingCodes(cfg config, applied desiredstate.Applied, relayCertDER []byte, relayID string) {
	if cfg.dialAddress() == "" {
		// No code is better than a code that dials the reader's own loopback —
		// the same judgement the REL-121b skip below makes, for the same reason.
		// The message names the remedy, because the alternative is an operator
		// debugging a screen that says "cannot reach the server" with nothing
		// connecting that back to this box's configuration.
		log.Printf("waiveo-relay: NOT forming pairing codes: listening on %s but the pairing dial host is %q (loopback), so a formed code would tell a screen to dial its OWN loopback. Set WAIVEO_RELAY_PAIR_HOST to the address a screen reaches this relay on.",
			cfg.listen, cfg.pairHost)
		return
	}
	for _, grant := range applied.PairingGrants {
		// A grant bound to another relay (REL-121b) is not this relay's to
		// display: the code would encode THIS relay's dial address for a
		// selector only the bound relay can redeem, so anyone who typed it
		// would be refused — a code that cannot work is worse than no code.
		if grant.RelayID != "" && grant.RelayID != relayID {
			continue
		}
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
// selects the DeviceController + EntityResolver pair (newDevicePlane) so both
// dispatch paths share it. dc is the relay's ONE canonical device-class registry
// (device-class-registry/1's own built-in, REG-060-066) — automationhost.New
// wires it directly into the device plane's CommandVocab and, adapted, into the
// engine's registry.Registry.
//
// commands is the connection's inbound `device.command` sink, and this function
// ARMS it with the host it just built (REL-112). That is deliberately not the
// caller's job: this is the only function in the binary that produces a Host, so
// arming here makes "the wire can reach the device plane" a property of building
// the device plane at all, rather than a separate line a boot sequence can omit
// — which is exactly what it did omit, leaving every app-issued command answered
// "this relay has no device plane wired" for the life of the process.
func bootAutomationStack(store *identity.Store, relayID identity.RelayIdentity, applied desiredstate.Applied, site hello.SiteBinding, dc deviceclass.Registry, controller deviceplane.DeviceController, resolveEntity deviceplane.EntityResolver, commands *deviceCommandSink) (*automationhost.Host, error) {
	host, err := automationhost.New(store, dc, controller, resolveEntity, relayID.RelayID)
	if err != nil {
		return nil, err
	}
	// Armed BEFORE the rule load below, not after: ApplyEdgeRules can take a
	// moment on a large generation, and a command arriving in that window should
	// execute rather than be told the device plane is not up.
	commands.set(host.DeviceCommand)
	// Adopt the app peer's authoritative site_binding (REL-036) into the engine
	// before loading rules, so the edge engine's schedule/sun triggers evaluate
	// against the site's real timezone and coordinates from the first tick.
	if err := host.SetLocation(site.TZ, site.Lat, site.Long); err != nil {
		return nil, fmt.Errorf("adopt site_binding tz %q into engine: %w", site.TZ, err)
	}
	scheduleFloorMs := seedScheduleFloor(host, store, time.Now().UnixMilli())
	if err := host.ApplyEdgeRules(applied.EdgeRules, int(applied.Generation)); err != nil {
		return nil, err
	}
	// The schedule floor is logged because it is the one input to the first tick
	// an operator cannot otherwise see, and it decides how far back that tick
	// catches missed schedule occurrences up (RUL-350/370).
	log.Printf("waiveo-relay automation engine loaded: %d edge rule(s); schedule resume cursor at %s; device plane + durable telemetry ready",
		host.EdgeRuleCount(), time.UnixMilli(scheduleFloorMs).Format(time.RFC3339))
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
func bootScheduleResolverAt(applied desiredstate.Applied, srv *playerserver.Server, sink *automation.CommandSink, site hello.SiteBinding, nowMs int64) []*schedulehost.Resolver {
	// nil carried state: a boot has no prior generation's resolvers to carry a
	// rising-edge baseline from, which is exactly what makes the boot resolve a
	// genuine resume edge (schedulehost.Resolver.AdoptCarriedState).
	return resolveAndServe(context.Background(), applied, srv, sink, site, scheduleResolverTickInterval, nowMs, nil)
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
// for it not at all. Because a resolver's write now names the ONE screen it
// serves, that additive policy holds for real rather than by luck: before,
// every resolver wrote the server's single shared program, so resolving ONE
// governed screen replaced what every OTHER screen was being served. The same
// holds, vacuously, when applied.Schedule carries no scope nodes at all: the
// returned slice is empty and every screen's serving is untouched.
//
// # Which screen a governed scope node's resolution is served to
//
// Resolution happens at a SCOPE NODE; a program is served to a SCREEN IDENTITY
// ROW; data-model/1 DAT-004a makes those distinct rows with distinct ids. The
// join between them is a screen row's own `scope_node` placement — which the
// app peer has (internal/feeder/snapshot derives `screen_programs` from exactly
// it) and the relay does NOT: REL-065's carried `schedule` section is keyed by
// scope node and carries scheduling-core rows plus scope nodes, no screen
// identity rows. So this function can only serve a resolution to a screen where
// the answer is forced rather than guessed: the carried schedule governs exactly
// one scope node AND the relay serves exactly one screen, in which case that
// node's resolution is that screen's and there is no other candidate either way.
//
// Otherwise every resolver is built with no served screen: it still resolves and
// still fires its node's preset batches (DAT-075 is a scope-node concern needing
// no screen identity), and every screen keeps the app-authored per-screen
// `screen_programs` baseline that generation carried — correct at the instant
// the generation was built, just not re-resolved locally at a daypart boundary.
// What moves those screens is a NEW generation, whose own baseline the apply
// path re-installs before calling this (scheduleDriver.apply); this function
// leaves them untouched by design, and must not be read as promising that
// anything else here will refresh them. Serving one screen's schedule resolution
// to another because the relay cannot tell them apart is the failure this
// refuses.
//
// site is the app peer's authoritative site_binding (REL-036, the same value
// bootAutomationStack adopts into the edge engine) — carried through only for
// the boot log line's context; the resolved schedule's own effective tz comes
// from the carried scope tree exclusively (datamodel.EffectiveTZ via
// datamodel.Resolve), never from site or any box-local clock (DAT-034/118).
//
// # carried: why a rebuild is not a boot
//
// carried maps a scope node to the rising-edge baseline the resolver being
// REPLACED for that node had reached (schedulehost.Resolver.CarryState); nil,
// or an absent entry, means nothing was resolving that node — a boot, or an
// apply that begins governing it. Each new resolver adopts its entry before its
// one resume-governed tick, so an apply over a node the relay was already
// resolving is not mistaken for a resume and does not re-dispatch that node's
// preset batch to real devices — unless THIS generation re-authored that
// daypart or its bound preset batch, which the adopting resolver checks and
// which is the one case an apply must still dispatch on.
// schedulehost.Resolver.AdoptCarriedState owns the full argument;
// scheduleDriver.apply is what harvests it.
func resolveAndServe(ctx context.Context, applied desiredstate.Applied, srv *playerserver.Server, sink *automation.CommandSink, site hello.SiteBinding, tickEvery time.Duration, nowMs int64, carried map[string]*schedulehost.CarriedBaseline) []*schedulehost.Resolver {
	store, errs := schedulehost.BuildStore(applied.Schedule)
	for _, e := range errs {
		log.Printf("waiveo-relay: schedule section: %s: %s: %s", e.Field, e.Code, e.Message)
	}

	var governed []string
	for _, nodeID := range scheduleScreenNodeIDs(applied.Schedule) {
		if schedulehost.Governs(store, nodeID) {
			governed = append(governed, nodeID)
		}
	}
	servedScreenID := soleServedScreenID(governed, applied.ScreenPrograms)

	var resolvers []*schedulehost.Resolver
	for _, nodeID := range governed {
		display, _, content, _, err := schedulehost.ProjectLease(store, nodeID, nowMs, applied.ContentOrigin, applied.ContentURLKey)
		if err != nil {
			// An unresolvable effective tz (DAT-034) degrades to the app-authored
			// program already served — never a box-local substitution.
			log.Printf("waiveo-relay: schedule resolver: scope node %s: resolve at boot: %v; serving app-authored program", nodeID, err)
			continue
		}

		r := schedulehost.NewResolver(store, nodeID, servedScreenID, srv, applied.Generation, applied.ContentOrigin, applied.ContentURLKey)
		// BEFORE TickBoot, always: the carried baseline is what decides whether
		// that one tick is a resume edge at all. Adopting it afterwards would be
		// adopting it too late — TickBoot would already have fired.
		r.AdoptCarriedState(carried[nodeID])
		r.TickBoot(nowMs, sink) // the level-triggered STATE projection + the misfire-governed resume-edge preset (DAT-075/076/094/119/121).
		resolvers = append(resolvers, r)

		switch {
		case servedScreenID == "":
			log.Printf("SCHEDULE RESOLVER OK (scope node %s: display:%s, presets only — no screen placement carried to attribute this resolution to a screen; app-authored program stays served; site tz %s)", nodeID, display, site.TZ)
		case display == "content" && len(content) > 0:
			log.Printf("SCHEDULE RESOLVER OK (scope node %s -> screen %s: display:content, asset %s; site tz %s)", nodeID, servedScreenID, content[0].AssetRef, site.TZ)
		default:
			log.Printf("SCHEDULE RESOLVER OK (scope node %s -> screen %s: display:%s; site tz %s)", nodeID, servedScreenID, display, site.TZ)
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

// serveAppAuthoredPrograms installs EVERY `screen_programs` entry in served on
// srv, each under its own `screen_id` (REL-061) — the app-authored per-screen
// baseline a relay serves with no app peer reachable (REL-055/061, Offline
// continuity).
//
// Every entry, not entry [0]: the section is one entry per screen identity row
// (data-model/1 DAT-004a) and a paired player presents a channel token naming
// exactly which row it is, so installing one of them served whichever screen
// sorted first to every paired player on the site.
//
// It is called on EVERY generation apply, not once at boot — from the boot path
// over the relay's own durable copy (desiredstate.ServedProgram) and from
// scheduleDriver.apply over the applied generation's own array, whose doc owns
// why. generation is that apply's own generation, carried into SetProgram's
// fence: the baseline is what a same-generation schedule resolution then
// replaces where the relay can attribute one to a screen, and what stands
// unchallenged where it cannot.
//
// An empty served installs nothing and is not a fault: it is REL-060's stated
// empty placeholder, and every screen is then served data-model/1's terminal
// default (DAT-118, display:blank) rather than a fabricated program. Reporting
// that belongs to the caller, which is the only one that knows whether an empty
// array means "no screens authored yet" (boot, reading the durable copy) or
// "this generation carried none" (a live apply).
//
// It installs no signing key: the relay's Lease-signing identity is established
// once, separately (playerserver.Server.SetSigningKey), and a per-screen program
// write has no authority over it.
//
// It returns how many entries were actually INSTALLED, which is not len(served)
// — an entry naming no screen is skipped. A caller reporting the size of the
// input instead would claim to have installed programs it discarded.
func serveAppAuthoredPrograms(srv *playerserver.Server, generation int64, served []wire.ScreenProgram) int {
	installed := 0
	for _, sp := range served {
		if sp.ScreenID == "" {
			// An entry naming no screen cannot be served to any screen — no
			// channel token resolves to an empty screen_id — so it is reported
			// rather than installed where nothing could ever read it.
			log.Printf("waiveo-relay: screen_programs entry (program_revision %q) carries no screen_id; not served", sp.ProgramRevision)
			continue
		}
		srv.SetServedProgram(generation, sp)
		installed++
	}
	return installed
}

// installPersistedServingState configures srv from the relay's OWN durable
// last-applied row — the app-authored per-screen programs (REL-055/061), the
// generation's `revocation_and_site.revoked` set (REL-066) and its
// `device_inventory` adopted set (REL-063/064) — and returns applied with its
// Revoked and DeviceInventory filled from that same row. It performs no network
// I/O and contacts no app peer: its sole input is the operational store, so a
// boot during an app-peer outage installs exactly what a boot with a live
// connection does (Offline continuity).
//
// # The programs
//
// Read from the durable copy rather than from the live Applied value, because
// that is the copy a restart with a disconnected app peer would read — the boot
// path exercises the offline path even when it is online. EVERY entry is
// installed, each under its own screen_id: `screen_programs` is one entry per
// screen identity row (REL-061), and a paired player presents a channel token
// naming exactly which row it is (PLY-035), so the serve path selects by that.
// No signing key travels with these writes — the Lease-signing identity is
// established separately (playerserver.Server.SetSigningKey).
//
// # The revocation set, and why it is RETURNED rather than installed here
//
// The set is carried onto the returned applied instead of installed directly,
// so the ONE apply seam (scheduleDriver.apply) installs it. It has to be: a boot
// whose pull failed — or that never had a live connection at all — leaves
// applied the zero value, and driver.apply installs applied.Revoked WHOLESALE,
// a set-replace by REL-066's own shape. An install at this line would therefore
// be wiped moments later by the empty set the zero value carries, and the relay
// would come up serving its persisted programs to its persisted channel tokens
// with nothing revoked.
//
// That is precisely what REL-123 forbids — a SYNCED revocation MUST be enforced
// "regardless of connectivity" — and a restart during an app-peer outage is
// exactly when a relay has only its own last-synced copy to go on. The programs
// and the revocation ride one durable row committed by one atomic write
// (identity.Store.ApplyGeneration), so reading them together here is what keeps
// the relay from serving one generation's programs under another generation's
// credential rules.
//
// On a boot whose pull SUCCEEDED this changes nothing: VerifyAndApply persisted
// exactly the set it returned, in that same row write, so the durable copy and
// applied.Revoked are the same list — and reading the durable one keeps both
// boot paths on a single source of truth rather than two that can drift.
//
// # The adopted set, and why it is read here at all
//
// `device_inventory` is returned the same way and for a strictly worse failure
// than the revocation set's. It is the ONLY statement of which devices this
// relay may drive; every consumer of it fails CLOSED (keepalive.AdoptionSet
// adopts nothing on an empty set, devicetargets makes nothing drivable). So a
// boot that left it at the zero value's empty section did not degrade loudly —
// it came up healthy, connected to nothing, driving nothing, and keeping no
// screen alive.
//
// Which is the power-cut boot. Both the app peer and the relay come back at
// once, the relay's pull loses that race, and the ONE capability whose entire
// job is "a screen idling at Home shows NOTHING until a human walks past"
// (screen keep-alive, PLY-150-157) had an empty gate to consult. It relaunched
// nothing, in the exact scenario it exists for. Reading the adopted set from
// the same durable row as the programs it applies to is what makes this
// function's own claim — that an offline boot installs what an online one does
// — true of the device plane and not only of the serve path.
func installPersistedServingState(store *identity.Store, srv *playerserver.Server, applied desiredstate.Applied) (desiredstate.Applied, error) {
	served, err := desiredstate.ServedProgram(store)
	if err != nil {
		return applied, fmt.Errorf("read persisted screen_programs for offline serve: %w", err)
	}
	if len(served) == 0 {
		// REL-060's empty placeholder, not a fault — and still a fully served
		// state: the relay's own signing key is not any screen's, so a screen
		// that pairs here is answered with DAT-118's terminal default rather
		// than a configuration error.
		log.Printf("waiveo-relay: persisted last-applied snapshot carried no screen_programs; every screen serves the terminal default until one is authored")
	}
	serveAppAuthoredPrograms(srv, applied.Generation, served)

	revoked, err := desiredstate.ServedRevocation(store)
	if err != nil {
		return applied, fmt.Errorf("read persisted revoked set for offline enforcement: %w", err)
	}
	applied.Revoked = revoked
	if len(revoked) > 0 {
		log.Printf("waiveo-relay: persisted last-applied snapshot revokes %d screen(s); enforced from boot against every credential decision, app peer reachable or not (REL-123)", len(revoked))
	}

	inventory, err := desiredstate.ServedDeviceInventory(store)
	if err != nil {
		return applied, fmt.Errorf("read persisted device_inventory for offline device plane: %w", err)
	}
	applied.DeviceInventory = inventory
	log.Printf("waiveo-relay: persisted last-applied snapshot adopts %d device(s); the device plane and screen keep-alive come up on that set with no app peer needed (REL-055/061/063)", len(inventory.Devices))

	return applied, nil
}

// soleServedScreenID returns the screen identity row a governed scope node's
// schedule resolution may be served to, or "" when the relay cannot say which
// screen that is without guessing.
//
// It is "" unless BOTH sides of the join are singular: exactly one governed
// scope node, and exactly one screen in the relay's own `screen_programs`
// (REL-061). Then there is no other node the screen's program could come from
// and no other screen the node's resolution could be for, so the attribution is
// forced by the inputs rather than assumed. With several of either, the join is
// a screen row's own `scope_node` placement (data-model/1 DAT-004a), which the
// relay never receives — REL-065's `schedule` section carries scheduling-core
// rows and scope nodes, no screen identity rows — and this returns "" so every
// resolver serves nobody rather than serving one screen's schedule to another.
//
// A `screen_programs` entry with no screen_id is not a candidate: it names no
// screen, so counting it would make a one-real-screen site look ambiguous.
//
// What is counted is DISTINCT screen ids, not entries: two entries naming the
// same screen still describe one screen, and reading that as ambiguous would
// cost a genuinely single-screen site its schedule attribution over a duplicate
// in an array nothing here controls.
//
// # A PINNED program is never served over
//
// A screen whose app-authored program is PINNED (wire.ScreenProgram.Pinned —
// the app peer projected it from that screen's own program override,
// data-model/1 DAT-004c) is excluded, and the exclusion is the whole reason an
// override survives on a single-screen site. DAT-004d states it directly: a
// consumer re-resolving a screen's program locally MUST NOT replace a program
// an override produced, because the relay has no input that could contradict
// the app peer's statement about one specific screen. Without this, exactly the
// deployment where the attribution IS forced — one governed node, one screen —
// would revert every play_cast and every alert at the next resolver tick: the
// write lands, the projection is right, the screen still shows its schedule.
//
// Excluding the screen (rather than suppressing the whole resolver) keeps the
// resolver running for its OTHER job: a scope node's daypart rising-edge preset
// batches (DAT-075) are a scope-node concern that needs no screen identity, and
// an alert on a screen must not also stop the lights from coming on.
func soleServedScreenID(governedNodeIDs []string, programs []wire.ScreenProgram) string {
	if len(governedNodeIDs) != 1 {
		return ""
	}
	screenID := ""
	pinned := map[string]bool{}
	for _, sp := range programs {
		if sp.ScreenID == "" {
			continue
		}
		if sp.Pinned {
			pinned[sp.ScreenID] = true
		}
		if sp.ScreenID == screenID {
			continue
		}
		if screenID != "" {
			return "" // more than one screen: the node -> screen attribution is not forced.
		}
		screenID = sp.ScreenID
	}
	if pinned[screenID] {
		return ""
	}
	return screenID
}

// scheduleScreenNodeIDs returns the id of every carried scope node of kind
// "screen" in sec — the candidate scope nodes bootScheduleResolverAt checks
// schedulehost.Governs against. These are scope nodes (data-model/1 DAT-001),
// NOT screen identity rows (DAT-004a); soleServedScreenID above is what decides
// which screen, if any, a governed one's resolution is served to. A node that
// fails to unmarshal is skipped (schedulehost.BuildStore already reports it as a
// ROW_MALFORMED error above) rather than aborting the whole scan.
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

// relayHelloDeclaration builds this relay's hello Declaration (relay/1
// REL-031), shared by every hello attempt this binary makes — the boot
// handshake (helloWithRetry) and the background recovery loop's retries
// (helloRecoverer, hellorecovery.go) alike — so they declare identically
// regardless of which call site is asking.
func relayHelloDeclaration(cfg config) hello.Declaration {
	return hello.Declaration{
		ProtocolVersion: "1.0",
		Features:        []string{"telemetry.latest_only_v1"},
		SiteBinding:     hello.SiteBinding{}, // no cached site pre-pull; the relay adopts the app peer's authoritative copy
		// REL-037: the canonical advertised address is "the same address it
		// advertises in its own discovery/pairing responses" — the pairing
		// dial address (cfg.pairHost/cfg.pairPort, exactly what
		// FormPairingCode encodes, REL-126), NOT the bind address, which on
		// a multi-homed or 0.0.0.0-bound relay is not dialable at all. The
		// app peer forms pairing codes from this value (relayconn
		// ConnectedRelays), so drift here would mint codes that dial nowhere.
		//
		// A dial address this relay knows cannot work is advertised as NO
		// address at all (dialAddress, below): the app peer's own pairing-code
		// formation already degrades that to "the connected relay advertised no
		// dialable address" and mints the grant anyway, which is a far better
		// outcome than a code that dials the reader's own loopback.
		SubnetMetadata: hello.SubnetMetadata{AdvertisedAddress: cfg.dialAddress()},
		ClockState:     hello.ClockState{State: "untrusted", Source: "cold_boot"},
	}
}

// pollTargetsFor projects the drivable-device gate's current answer onto the
// ECP poller's own Target type. The two packages deliberately declare separate
// address types (ecp.Target, ecppoll.Target) so neither driver depends on the
// other; this is the one place the gate's answer is adapted for the poller, so
// the poll set is BY CONSTRUCTION the command set and no drift is possible.
func pollTargetsFor(targets *devicetargets.Registry) map[string]ecppoll.Target {
	resolved := targets.Targets()
	out := make(map[string]ecppoll.Target, len(resolved))
	for entityID, ep := range resolved {
		out[entityID] = ecppoll.Target{Host: ep.Host, Port: ep.Port}
	}
	return out
}

// loopbackController is a TEST-ONLY stand-in DeviceController: it refuses every
// dispatch with a typed error, so a test can exercise the automation stack's
// wiring (rules load, fire, and reach the device plane) with no hardware and no
// ECP server.
//
// It is no longer the production default and no longer stands in for anything
// deferred. The relay now installs the real ECP adapter unconditionally and
// gates it on adoption (deviceplane.go's newDevicePlane); this remains only as
// the double the serving-side tests hand to bootAutomationStack when the device
// plane is not what they are testing.
type loopbackController struct{}

// Dispatch REFUSES rather than reporting success.
//
// It used to log and return nil, which meant that on a relay with no configured
// ECP targets — the DEFAULT — an operator issuing a real command against a real
// discovered device received `{"ok":true}` while nothing was sent to any
// hardware. That is the same shape this codebase has shipped three times before
// (a 202 that mutated nothing, a webhook loop with no caller, a secret-install
// path with no secrets), and it is worse here because it wears a success
// response: an operator watching a screen that did not change has been told the
// command worked.
//
// A stand-in must be honest about standing in. The refusal is typed, so the api
// surface renders it as a refusal an operator can act on rather than a silent
// nothing, and the automation stack a loopback exists to exercise still runs —
// it just reports that no device plane is attached, which is true.
func (loopbackController) Dispatch(entityID, command string, params map[string]any) error {
	log.Printf("waiveo-relay dispatch refused (no device adapter configured): %s %s", entityID, command)
	return &deviceplane.ControllerError{
		Code:    "COMMAND_UNRESOLVED",
		Message: "this relay has no device adapter configured, so the command reached no hardware (set WAIVEO_RELAY_ECP_TARGETS)",
	}
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

// Discovery-watch facts for the deployment's own declared device lanes. A
// discovery response says WHICH device answered; these say what a device found
// that way is (REL-110a) — the driver that speaks to it, the class whose
// vocabulary its commands resolve against, and the entity handle it exposes.
// They stand in for a pack's own device contribution (manifest/1 MAN-070) until
// installed packs drive the watch set.
const (
	rokuSearchTarget = "roku:ecp"
	rokuDriver       = "roku-ecp"
	mdnsDriver       = "mdns"
	mediaPlayerClass = "media-player"
	mainEntityKey    = "main"
	// rokuECPPort is Roku's well-known ECP port, declared on the watch so a
	// NOTIFY whose LOCATION header is missing or malformed can still be turned
	// into a dialable address from the packet's own source IP
	// (internal/relay/discovery/address.go). A Roku's LOCATION normally names
	// this port itself; this is the fallback, not the usual path.
	rokuECPPort = 8060
)

// candidateReportInterval is how often the relay re-reports its full candidate
// set while connected. Every report is a complete replace (REL-111), so this is
// idempotent and a lost report costs at most one interval; a reconnect reports
// immediately rather than waiting for the tick.
const candidateReportInterval = time.Minute

// reportCandidates sends the store's full current candidate set upward
// (REL-110/111). A nil client is the ordinary offline case — the relay keeps
// discovering while disconnected and the next connection reports what it found,
// which is exactly what a full-set report makes safe. A send failure is logged
// and dropped: the connection is already dying, its supervisor will redial, and
// OnConnected reports again.
func reportCandidates(c *relayconn.Client, store *deviceplane.Store) {
	if c == nil {
		return
	}
	report := store.Report()
	if err := c.SendDeviceCandidates(wire.DeviceCandidatesBody{
		Candidates: toWireCandidates(report.Body.Candidates),
	}); err != nil {
		log.Printf("waiveo-relay: reporting %d device candidate(s) failed (retried on the next report): %v",
			len(report.Body.Candidates), err)
		return
	}
	if n := len(report.Body.Candidates); n > 0 {
		log.Printf("waiveo-relay discovery: reported %d device candidate(s) to the app peer", n)
	}
}

// screenStatusReportInterval is how often a connected relay re-reports what its
// screens have been observed doing (parity row 5.8). Every report is a full-set
// replace, so this is idempotent and a lost one costs at most one interval.
//
// It is NOT a number chosen here. It is wire.ScreenStatusReportIntervalMs,
// declared beside the player's measured pull cadence and the console's
// live/stale window, because the app peer's staleness arithmetic ADDS this
// interval to every age it reports: a report sits in the app peer until the
// next one replaces it, so a perfectly healthy screen's worst honest age is one
// pull cadence plus one of these. Slowing this reporter down without widening
// that window is exactly how a healthy fleet starts reading `stale` (W2-18),
// and the shared constant is what makes the two impossible to change apart.
const screenStatusReportInterval = time.Duration(wire.ScreenStatusReportIntervalMs) * time.Millisecond

// reportScreenStatus sends the player server's full current per-screen
// observation set upward (parity row 5.8, wire.ScreenStatusBody).
//
// A nil client is the ordinary offline case, handled exactly as reportCandidates
// handles it: the relay keeps serving screens and observing them while
// disconnected, and the next connection reports the accumulated truth — which is
// what a full-set report makes safe. A send failure is logged at most once per
// interval and dropped; the connection is already dying and its supervisor will
// redial.
func reportScreenStatus(c *relayconn.Client, srv *playerserver.Server) {
	if c == nil {
		return
	}
	entries := screenStatusEntries(srv)
	if err := c.SendScreenStatus(wire.ScreenStatusBody{Screens: entries}); err != nil {
		log.Printf("waiveo-relay: reporting %d screen status entr(ies) failed (retried on the next report): %v", len(entries), err)
	}
}

// screenStatusEntries projects the player server's own per-screen observation
// snapshot onto the wire entries a report carries.
//
// It is split out from reportScreenStatus — which needs a live *relayconn.Client
// and therefore a real connection to drive — so this mapping can be exercised
// directly. That is worth a function boundary because the mapping is the one
// place a field can be silently DROPPED on the way upstream, and a dropped field
// is indistinguishable at the console from a screen that never reported it: a
// wall rendering perfectly, described as having never rendered anything.
func screenStatusEntries(srv *playerserver.Server) []wire.ScreenStatusEntry {
	statuses := srv.ScreenStatuses()
	entries := make([]wire.ScreenStatusEntry, 0, len(statuses))
	for _, st := range statuses {
		entries = append(entries, wire.ScreenStatusEntry{
			ScreenID:                st.ScreenID,
			Paired:                  st.Paired,
			LastPullAgeMs:           st.LastPullAgeMs,
			LastAckAgeMs:            st.LastAckAgeMs,
			LastRenderStartAgeMs:    st.LastRenderStartAgeMs,
			UnackedPulls:            st.UnackedPulls,
			ProgramRevision:         st.ProgramRevision,
			Priority:                st.Priority,
			Display:                 st.Display,
			ContentCount:            st.ContentCount,
			AckedProgramRevision:    st.AckedProgramRevision,
			AckedDisplay:            st.AckedDisplay,
			AckedContentCount:       st.AckedContentCount,
			Rejected:                st.Rejected,
			RejectedProgramRevision: st.RejectedProgramRevision,
			RejectReason:            st.RejectReason,
			RenderAssetRef:          st.RenderAssetRef,
		})
	}
	return entries
}

// redemptionReportInterval is how often a connected relay drains its owed
// pairing-grant redemption reports upstream (REL-124). Short relative to a
// grant's own 900s ttl: the report drives the app peer to retire a spent grant
// from later generations, and a spent grant still riding snapshots is exactly
// the thing this shrinks. It is deliberately NOT load-bearing — the site-wide
// at-most-once property is the grant's own relay binding (REL-121b/REL-124c) —
// so nothing breaks if a tick is missed or the app peer is down for hours.
const redemptionReportInterval = 30 * time.Second

// reportRedemptions drains the relay's owed redemption reports onto c, oldest
// first, retiring each from the ledger only once its own ack has arrived
// (REL-124a/REL-124d). A nil client (disconnected) is a no-op: the ledger is
// durable, so everything owed is still owed at the next connection.
//
// Two failure shapes, deliberately different:
//   - PAIRING_REPORT_UNAUTHORIZED (REL-124b) — the app peer says this grant is
//     not this relay's to report, and the Error taxonomy marks that refusal
//     non-retryable. The report is retired anyway and logged loudly: re-sending
//     would draw the identical refusal forever and wedge every LATER report
//     behind it. It also means something is wrong that an operator should see —
//     this relay believes it redeemed a grant the app peer says belongs to
//     someone else.
//   - anything else (write failure, timeout, INTERNAL) — the batch STOPS and
//     everything from this report on stays owed. Stopping rather than skipping
//     ahead keeps the ledger in order and avoids hammering a peer that is
//     already failing.
func reportRedemptions(c *relayconn.Client, srv *playerserver.Server) {
	if c == nil {
		return
	}
	owed, err := srv.PendingRedemptionReports()
	if err != nil {
		log.Printf("waiveo-relay: reading owed pairing-redemption reports failed (retried on the next tick): %v", err)
		return
	}
	for _, r := range owed {
		err := c.SendPairingRedeemed(wire.PairingRedeemedBody{GrantID: r.GrantID, RedeemedAt: r.RedeemedAt})
		var refusal *relayconn.Refusal
		switch {
		case err == nil:
		case errors.As(err, &refusal) && refusal.Code == "PAIRING_REPORT_UNAUTHORIZED":
			log.Printf("waiveo-relay: the app peer refused this relay's redemption report for grant %s as bound to a DIFFERENT relay (REL-124b) — dropping the report; an operator should investigate", r.GrantID)
		default:
			log.Printf("waiveo-relay: reporting redemption of grant %s upstream failed (%v); it stays owed for the next connection opportunity (REL-124d)", r.GrantID, err)
			return
		}
		if err := srv.MarkRedemptionReported(r.Seq); err != nil {
			log.Printf("waiveo-relay: clearing the reported redemption of grant %s from the ledger failed (%v); it will be re-sent, which the app peer takes as a no-op (REL-124b)", r.GrantID, err)
			return
		}
	}
}

// toWireCandidates projects the store's own candidates onto the relay/1 wire
// shape. The two are separate types on purpose (internal/shared/wire's own
// note): the app peer decodes into its own definition, and this projection is
// where the producing side states, field by field, what it is claiming.
func toWireCandidates(cands []deviceplane.Candidate) []wire.DeviceCandidate {
	out := make([]wire.DeviceCandidate, 0, len(cands))
	for _, c := range cands {
		match, err := json.Marshal(c.Match)
		if err != nil {
			// A candidate whose match cannot be encoded has no MAN-071 form to
			// report; dropping it is better than reporting a candidate whose
			// provenance is unstatable.
			continue
		}
		ents := make([]wire.CandidateEntity, 0, len(c.Entities))
		for _, e := range c.Entities {
			ents = append(ents, wire.CandidateEntity{Key: e.Key, DeviceClass: e.DeviceClass, State: e.State, Attributes: e.Attributes})
		}
		out = append(out, wire.DeviceCandidate{
			Match:        match,
			Provenance:   string(c.Provenance),
			Status:       string(c.Status),
			IgnoredUntil: c.IgnoredUntil,
			FirstSeen:    c.FirstSeen,
			LastSeen:     c.LastSeen,
			Driver:       c.Driver,
			NativeID:     c.NativeID,
			DeviceClass:  c.DeviceClass,
			Name:         c.Name,
			Address:      c.Address,
			Model:        c.Model,
			Serial:       c.Serial,
			Entities:     ents,
		})
	}
	return out
}

// loopbackResolver is a TEST-ONLY stand-in entity resolver: it maps every
// entity_id to a single media-player loopback device so a loaded edge rule's
// device_command resolves against the fixture registry's vocabulary
// (REL-112/113) without a candidate store or an adopted inventory.
//
// It is deliberately NOT wired in production, and the reason is the whole point
// of this track: resolving every id to one device means every id is a device
// this relay claims, which is a blanket claim over a shared LAN. The production
// resolver (deviceplane.go) resolves only what this relay discovered or what the
// app peer adopted.
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

// feederTLSClient returns the relay's HTTP client for the telemetry upstream
// push (REL-090). Its server half is unchanged: no separate trust anchor to
// validate the co-located feeder/app-peer's self-signed listener certificate
// against, mirroring the relay's existing enroll / desired-state / hello
// bootstrap clients (REL-010/011 bootstrap exception, made concrete for the
// co-located feeder+relay loopback deployment).
//
// Its CLIENT half now presents this relay's enrollment-issued leaf, exactly as
// the /relay/v1 connection does (REL-003/041) — the app peer's telemetry ingest
// identifies the pusher by that certificate and checks it against the enrollment
// registry's revocation record (REL-016). The certificate is supplied through
// GetClientCertificate rather than a fixed Certificates slice so a proactive
// renewal (REL-015) is picked up on the next handshake, the same way the relay
// connection picks it up on the next redial, instead of pinning this client to
// the leaf that happened to be persisted at boot.
//
// This client is independent of the telemetry retention/ack logic, which the
// Channel owns; it only carries the batch on the wire.
func feederTLSClient(currentLeaf func() (*tls.Certificate, error)) *http.Client {
	return &http.Client{
		Timeout: telemetryPushTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify:   true, //nolint:gosec // REL-010/011 co-located bootstrap exception, see doc above
				GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return currentLeaf() },
			},
		},
	}
}

// healthzWithVitals answers /healthz with this relay's real operational health
// rather than a constant.
//
// The previous body was `{"component":"waiveo-relay","status":"ok"}` — a literal,
// returned whether or not the box was overheating, throttled, undervolted or out
// of disk. A probe that cannot fail is a probe that reports nothing, and the
// deploy tooling treats a 200 here as "the relay is fine".
//
// `vitals` is events/1's box.vitals payload (EVT-070/071) rather than a shape
// invented for this route, so the numbers an operator reads here are the same
// ones a fleet consumer will receive when emission is wired — the cadence for
// that is still an open question in the contract's own draft-note, which is why
// this reads on demand and emits nothing.
//
// The disk read is the relay's OWN operational storage (the configured identity
// path),
// which is what EVT-070's disk_headroom is about — not whatever filesystem the
// process happened to start in.
//
// `status` stays "ok" while the process is answering: this route reports health,
// and deciding which readings constitute unhealthy is a Repairs-detector question
// with its own thresholds, not something to invent inside a liveness probe.
func healthzWithVitals(relayID, identityPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reading := vitals.Read(filepath.Dir(identityPath))
		body := map[string]any{
			"component": "waiveo-relay",
			"status":    "ok",
			"vitals":    vitals.Payload(relayID, reading),
		}
		// Named plainly rather than left to be inferred from absent members: a
		// consumer that sees no cpu_temp should be able to tell "this platform has
		// no thermal sensor" from "the field was dropped".
		if missing := reading.Unavailable(); len(missing) > 0 {
			body["vitals_unavailable"] = missing
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}
