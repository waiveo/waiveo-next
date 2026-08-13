package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/keepalive"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/relay/telemetryhttp"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestRegisterClockHintReceivesWireHint proves the relay/1 clock.hint receiver
// (REL-133) is wired into the relay's own listener and reachable end-to-end: a
// POSTed clock.hint bounded by the relay's own cert not_after is applied to the
// runtime clock, and one past not_after+grace is declined.
func TestRegisterClockHintReceivesWireHint(t *testing.T) {
	notAfter := time.UnixMilli(1784073600000)
	certDER := selfSignedCertDER(t, notAfter)

	mux := http.NewServeMux()
	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctl := clocktrust.NewController(store, clocktrust.NewRuntimeClock(), nil)
	if err := registerClockHint(mux, certDER, ctl); err != nil {
		t.Fatalf("registerClockHint: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(wire string) (accepted bool, status int) {
		resp, err := http.Post(srv.URL+"/relay/v1/clock-hint", "application/json", bytes.NewBufferString(wire))
		if err != nil {
			t.Fatalf("POST clock.hint: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Accepted bool `json:"accepted"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return body.Accepted, resp.StatusCode
	}

	// Within not_after + grace -> accepted.
	if accepted, status := post(`{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1752537600000}}`); !accepted || status != http.StatusOK {
		t.Errorf("in-bound clock.hint: accepted=%v status=%d, want accepted=true status=200", accepted, status)
	}
	// Past not_after + grace -> declined (still 200).
	if accepted, status := post(`{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1784074200001}}`); accepted || status != http.StatusOK {
		t.Errorf("out-of-bound clock.hint: accepted=%v status=%d, want accepted=false status=200", accepted, status)
	}
}

func selfSignedCertDER(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return der
}

// TestOfflineServeFallback pins the REL-055/061 boot policy the relay applies
// when the app peer is unreachable at startup: a hello or Pull failure is
// survivable (degrade to serving the persisted last-applied snapshot offline)
// IFF a prior successful pull already persisted a last-applied generation;
// otherwise it stays fatal because there is nothing to serve. This is the
// regression guard for the defect where a boot-time app-peer failure downed the
// whole process even though a valid persisted {generation, hash,
// screen_programs} sat in the store — a restart during a disconnection that
// REL-055 exists to keep serving.
func TestOfflineServeFallback(t *testing.T) {
	bootErr := errors.New("app peer unreachable")

	// Persisted snapshot present: a boot-time failure must NOT be fatal — the
	// relay serves the persisted last-applied copy offline.
	if fatal := offlineServeFallback(bootErr, true); fatal != nil {
		t.Errorf("offlineServeFallback(err, hasPersisted=true) = %v, want nil (serve persisted offline, REL-055/061)", fatal)
	}

	// Nothing persisted: the same failure IS fatal — no program to serve.
	if fatal := offlineServeFallback(bootErr, false); fatal == nil {
		t.Error("offlineServeFallback(err, hasPersisted=false) = nil, want the original error (nothing to serve)")
	} else if !errors.Is(fatal, bootErr) {
		t.Errorf("offlineServeFallback returned %v, want the original bootErr unchanged", fatal)
	}

	// A nil bootErr is never fatal regardless of persisted state.
	if fatal := offlineServeFallback(nil, false); fatal != nil {
		t.Errorf("offlineServeFallback(nil, false) = %v, want nil", fatal)
	}
	if fatal := offlineServeFallback(nil, true); fatal != nil {
		t.Errorf("offlineServeFallback(nil, true) = %v, want nil", fatal)
	}
}

func TestLoadConfigDefaultsAreLoopback(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadConfig(defaults): %v", err)
	}
	if cfg.listen != "127.0.0.1:7421" {
		t.Errorf("default listen = %q, want 127.0.0.1:7421", cfg.listen)
	}
	if cfg.feederURL != "https://127.0.0.1:7420" {
		t.Errorf("default feederURL = %q, want https://127.0.0.1:7420", cfg.feederURL)
	}
	if cfg.pairHost != "127.0.0.1" {
		t.Errorf("default pairHost = %q, want 127.0.0.1", cfg.pairHost)
	}
	if cfg.pairPort != 7421 {
		t.Errorf("default pairPort = %d, want 7421", cfg.pairPort)
	}
	// The identity store defaults to the make-dev path, so `make dev-up` — which
	// clears exactly that path to force a re-enroll — keeps working untouched.
	if cfg.identityPath != identity.DefaultPath {
		t.Errorf("default identityPath = %q, want %q", cfg.identityPath, identity.DefaultPath)
	}
}

// The identity store used to be a hardcoded constant, which made it a hidden
// GLOBAL: every relay on one machine opened the same file however differently it
// was otherwise configured.
//
// That is a correctness problem, not a tidiness one. The store holds the
// enrollment-anchored pin REL-137 checks, so a relay pointed at a SECOND app peer
// picked up the identity enrolled with the FIRST and refused the connection it
// had just been told to make — reporting, accurately, that "the app peer at this
// address is NOT the one this relay enrolled with". One of the two causes the
// relay names for that is "a second process holding the same address", and the
// isolated stack that would rule it in or out could not be started at all while
// this path was fixed.
func TestLoadConfigIdentityPathIsOverridable(t *testing.T) {
	env := map[string]string{"WAIVEO_RELAY_IDENTITY_PATH": "/tmp/second-stack/relay.db"}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig(override): %v", err)
	}
	if cfg.identityPath != "/tmp/second-stack/relay.db" {
		t.Errorf("identityPath = %q, want the override", cfg.identityPath)
	}
	// …and it is genuinely a different file from the default, which is the whole
	// point: two relays that resolve to one store are one relay with two names.
	if cfg.identityPath == identity.DefaultPath {
		t.Error("the override resolved back to the default path")
	}
}

func TestLoadConfigOnBoxOverride(t *testing.T) {
	// The on-box first-photon shape: bind LAN-reachable, tell the screen to dial
	// the box's LAN IP, keep the feeder loopback (co-located).
	env := map[string]string{
		"WAIVEO_RELAY_LISTEN":    "0.0.0.0:7421",
		"WAIVEO_RELAY_PAIR_HOST": "192.0.2.12",
		"WAIVEO_RELAY_PAIR_PORT": "7421",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig(override): %v", err)
	}
	if cfg.listen != "0.0.0.0:7421" {
		t.Errorf("listen = %q, want 0.0.0.0:7421", cfg.listen)
	}
	if cfg.pairHost != "192.0.2.12" {
		t.Errorf("pairHost = %q, want the LAN IP a screen must dial", cfg.pairHost)
	}
	if cfg.feederURL != "https://127.0.0.1:7420" {
		t.Errorf("feederURL = %q, want the loopback default (feeder is co-located)", cfg.feederURL)
	}
}

func TestLoadConfigRejectsNonIntegerPairPort(t *testing.T) {
	// A bad port must fail fast at startup, not silently emit a pairing code no
	// screen can dial.
	env := map[string]string{"WAIVEO_RELAY_PAIR_PORT": "not-a-port"}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("loadConfig accepted a non-integer WAIVEO_RELAY_PAIR_PORT, want error")
	}
}

func TestLoadConfigECPTargetsAndPolling(t *testing.T) {
	// The on-box hardware shape: entity → LAN Roku mapping, custom poll period,
	// discovery on. Port defaults to 0 in the parsed Target (ecp/ecppoll apply
	// the 8060 ECP default themselves).
	env := map[string]string{
		"WAIVEO_RELAY_ECP_TARGETS":   "01J8Z3K4N5P6Q7R8S9T0V1SCRN=192.0.2.51, second=192.0.2.52:9060",
		"WAIVEO_RELAY_POLL_MS":       "2500",
		"WAIVEO_RELAY_DISCOVERY":     "1",
		"WAIVEO_RELAY_MDNS_PATTERNS": "_waiveo._tcp",
		"WAIVEO_RELAY_SSDP_ANNOUNCE": "1",
		"WAIVEO_RELAY_KEEPALIVE":     "1",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig(hardware shape): %v", err)
	}
	if got := cfg.ecpTargets["01J8Z3K4N5P6Q7R8S9T0V1SCRN"]; got.Host != "192.0.2.51" || got.Port != 0 {
		t.Errorf("target[SCRN] = %+v, want host 192.0.2.51 port 0 (ECP default applies downstream)", got)
	}
	if got := cfg.ecpTargets["second"]; got.Host != "192.0.2.52" || got.Port != 9060 {
		t.Errorf("target[second] = %+v, want host 192.0.2.52 port 9060", got)
	}
	if cfg.pollInterval != 2500*time.Millisecond {
		t.Errorf("pollInterval = %s, want 2.5s", cfg.pollInterval)
	}
	if !cfg.discoveryOn {
		t.Error("discoveryOn = false, want true for WAIVEO_RELAY_DISCOVERY=1")
	}
	if len(cfg.mdnsPatterns) != 1 || cfg.mdnsPatterns[0] != "_waiveo._tcp" {
		t.Errorf("mdnsPatterns = %v, want [_waiveo._tcp]", cfg.mdnsPatterns)
	}
	if !cfg.ssdpAnnounce {
		t.Error("ssdpAnnounce = false, want true for WAIVEO_RELAY_SSDP_ANNOUNCE=1")
	}
	if !cfg.keepaliveOn {
		t.Error("keepaliveOn = false, want true for WAIVEO_RELAY_KEEPALIVE=1")
	}
}

func TestLoadConfigDeviceDefaultsOff(t *testing.T) {
	// No hardware env → nil targets and the loopback stand-ins stay in, and
	// NOTHING on this box touches the network: not the announcing lanes, and
	// not the SSDP client sweep either. This is the plain `make dev` / CI shape
	// (see discoveryEnabled and TestDiscoveryFollowsTheDeploymentPosture).
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadConfig(defaults): %v", err)
	}
	if cfg.ecpTargets != nil {
		t.Errorf("ecpTargets = %v, want nil", cfg.ecpTargets)
	}
	if cfg.discoveryOn {
		t.Error("discoveryOn = true for a loopback-bound relay, want false — CI and loopback dev runs must never " +
			"multicast, and a relay no screen can reach has no fleet to discover")
	}
	if cfg.mdnsPatterns != nil {
		t.Errorf("mdnsPatterns = %v, want nil by default (CI/dev loopback must not multicast)", cfg.mdnsPatterns)
	}
	if cfg.ssdpAnnounce {
		t.Error("ssdpAnnounce = true, want false by default (CI/dev loopback must not multicast)")
	}
	// keepaliveOn defaults ON, as discoveryOn above does — see config's own
	// doc, and TestLoadConfigKeepaliveDefaultsOnAndOptsOut below. It is inert
	// here regardless: with no ecpTargets there is nothing
	// to watch, which is what keeps dev/CI byte-identical.
	if !cfg.keepaliveOn {
		t.Error("keepaliveOn = false, want true by default (a screen stuck at Home must self-heal)")
	}
	if cfg.pollInterval != 5*time.Second {
		t.Errorf("pollInterval = %s, want the 5s default", cfg.pollInterval)
	}
}

// TestDiscoveryFollowsTheDeploymentPosture pins BOTH invariants the SSDP client
// sweep has to satisfy at once, and they pull in opposite directions:
//
//   - A deployed appliance sweeps without being told to. Off-by-default meant a
//     fresh box discovered nothing until somebody set an environment variable,
//     and the failure looked like "there are no devices" rather than like a
//     configuration gap.
//   - CI and loopback dev runs never multicast. `make dev` on a laptop on an
//     office LAN must not M-SEARCH strangers and then probe everything that
//     answers.
//
// The listen address is what separates them, so the table below is keyed by it.
// If either half regresses this is where it shows: a constant default cannot
// satisfy both, and the previous attempt at "on by default" satisfied only the
// first while deleting the second from the comment that carried it.
func TestDiscoveryFollowsTheDeploymentPosture(t *testing.T) {
	const lan = "0.0.0.0:7421"
	const loopback = "127.0.0.1:7421"

	cases := []struct {
		name   string
		listen string
		raw    string
		want   bool
	}{
		// Unset: the posture decides.
		{"deployed appliance, unset", lan, "", true},
		{"deployed appliance on a LAN IP, unset", "192.168.50.12:7421", "", true},
		{"loopback dev/CI, unset", loopback, "", false},
		{"loopback by name, unset", "localhost:7421", "", false},
		{"whitespace is not a value", loopback, "   ", false},
		{"whitespace is not a value, LAN", lan, "   ", true},

		// Stated: the operator decides, in both directions.
		{"explicitly on from a laptop, sweeping a real LAN", loopback, "1", true},
		{"explicitly on, any truthy spelling", loopback, "yes", true},
		{"an unrecognized value is not an off value", loopback, "anything-at-all", true},
		{"explicitly off on a deployed box", lan, "0", false},
		{"off spellings, all of them", lan, "false", false},
		{"off spellings, case-insensitive", lan, "FALSE", false},
		{"off spellings, padded", lan, " Off ", false},
		{"off spellings, no", lan, "no", false},
		{"off spellings, disabled", lan, "disabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"WAIVEO_RELAY_LISTEN": tc.listen, "WAIVEO_RELAY_DISCOVERY": tc.raw}
			cfg, err := loadConfig(func(k string) string { return env[k] })
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.discoveryOn != tc.want {
				t.Errorf("listen=%q WAIVEO_RELAY_DISCOVERY=%q → discoveryOn = %v, want %v",
					tc.listen, tc.raw, cfg.discoveryOn, tc.want)
			}
		})
	}
}

// The screen keep-alive capability is an OPT-OUT, and the on/off decision is
// pinned here because getting it wrong is invisible: a relay with the flag
// silently off looks completely healthy right up until a screen sits at Home
// for a weekend. The legacy stack ran its equivalent unconditionally, and
// shipping this switched off reproduced the outage it exists to end.
func TestLoadConfigKeepaliveDefaultsOnAndOptsOut(t *testing.T) {
	for raw, want := range map[string]bool{
		"":         true, // unset — the default that matters
		"1":        true,
		"true":     true,
		"0":        false,
		"false":    false,
		"off":      false,
		"no":       false,
		"disabled": false,
		"DISABLED": false, // case-insensitive: an operator's intent is not a spelling test
		// A value that means nothing keeps the capability running rather than
		// silently disabling self-healing on a typo.
		"yes-please": true,
	} {
		env := map[string]string{"WAIVEO_RELAY_KEEPALIVE": raw}
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("loadConfig(WAIVEO_RELAY_KEEPALIVE=%q): %v", raw, err)
		}
		if cfg.keepaliveOn != want {
			t.Errorf("WAIVEO_RELAY_KEEPALIVE=%q → keepaliveOn = %v, want %v", raw, cfg.keepaliveOn, want)
		}
	}
}

// The POWER-ON AUTO-LAUNCH rule (parity row 5.6) is the third opt-out switch,
// and its default is the whole point of the row: legacy foregrounded the
// channel on every power-on, so a like-for-like relay must too, out of the box,
// with no environment variable to remember. Pinned separately from
// keepaliveOn's own default because they are separately switchable — a
// deployment that shares its TVs with people has a real reason to keep the
// home-only recovery and drop this one, and that combination must be reachable.
func TestLoadConfigPowerOnLaunchDefaultsOnAndOptsOut(t *testing.T) {
	for raw, want := range map[string]bool{
		"":           true, // unset — the parity default
		"1":          true,
		"true":       true,
		"0":          false,
		"off":        false,
		"disabled":   false,
		"yes-please": true, // a typo never silently disables a parity behaviour
	} {
		env := map[string]string{"WAIVEO_RELAY_POWERON_LAUNCH": raw}
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("loadConfig(WAIVEO_RELAY_POWERON_LAUNCH=%q): %v", raw, err)
		}
		if cfg.powerOnLaunchOn != want {
			t.Errorf("WAIVEO_RELAY_POWERON_LAUNCH=%q → powerOnLaunchOn = %v, want %v", raw, cfg.powerOnLaunchOn, want)
		}
	}

	// The two switches are independent in BOTH directions: switching the
	// power-on launch off must leave the keep-alive running, and vice versa.
	env := map[string]string{"WAIVEO_RELAY_POWERON_LAUNCH": "0"}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.keepaliveOn {
		t.Error("keepaliveOn = false with only WAIVEO_RELAY_POWERON_LAUNCH=0 set, want true (the two switches are independent)")
	}
	env = map[string]string{"WAIVEO_RELAY_KEEPALIVE": "0"}
	cfg, err = loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.powerOnLaunchOn {
		t.Error("powerOnLaunchOn = false with only WAIVEO_RELAY_KEEPALIVE=0 set, want true (the config values are independent; the capability is unreachable anyway because keepalive itself is not constructed)")
	}
}

func TestLoadConfigMDNSPatterns(t *testing.T) {
	// Comma-separated service types, tolerating stray whitespace and empty
	// entries from a trailing/doubled comma.
	env := map[string]string{"WAIVEO_RELAY_MDNS_PATTERNS": " _waiveo._tcp, _googlecast._tcp ,,"}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig(mdns patterns): %v", err)
	}
	if len(cfg.mdnsPatterns) != 2 || cfg.mdnsPatterns[0] != "_waiveo._tcp" || cfg.mdnsPatterns[1] != "_googlecast._tcp" {
		t.Errorf("mdnsPatterns = %v, want [_waiveo._tcp _googlecast._tcp]", cfg.mdnsPatterns)
	}
}

func TestLoadConfigRejectsMalformedECPTargets(t *testing.T) {
	for _, raw := range []string{"justanentity", "=192.0.2.51", "e=", "e=h:notaport"} {
		env := map[string]string{"WAIVEO_RELAY_ECP_TARGETS": raw}
		if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
			t.Errorf("loadConfig accepted malformed WAIVEO_RELAY_ECP_TARGETS %q, want error", raw)
		}
	}
	env := map[string]string{"WAIVEO_RELAY_POLL_MS": "0"}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Error("loadConfig accepted WAIVEO_RELAY_POLL_MS=0, want error")
	}
}

// newTestPlayerServer builds a real playerserver.Server with its own ed25519
// Lease-signing identity already installed, plus a redeemable pairing
// grant's selector — a byte-exact duplicate of
// internal/relay/schedulehost's own test helper of the same name (this
// codebase's established cross-package pattern for small test collaborators,
// e.g. demoScreenScopeNodeID's own doc comment there).
//
// The cert is minted ed25519 directly (not via tlsboot.GenSelfSigned, which now
// serves the browser-facing ECDSA P-256 leaf): the relay's lease-signing
// identity — the cert whose public key a player verifies Leases against
// (PLY-090) — is ed25519, a distinct key from the feeder's TLS serving leaf.
func newTestPlayerServer(t *testing.T) (*playerserver.Server, string) {
	t.Helper()
	certPEM, priv := newTestRelayIdentity(t)

	srv, err := playerserver.NewServer(certPEM, []wire.PairingGrant{testPlayerServerGrant()}, playerserver.WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// The relay's own Lease-signing identity, installed exactly as the boot path
	// installs it (main's own pairingSrv.SetSigningKey) and independently of any
	// program: a program write carries no key, so nothing else on this server can
	// establish one and every pull would answer 500 without this.
	srv.SetSigningKey(priv)
	return srv, testPlayerServerGrantID
}

// newTestRelayIdentity mints one self-signed relay certificate and its private
// key — the pair a player/1 server presents as its sole trust anchor and signs
// every Lease with. Hoisted out of newTestPlayerServer so a case that builds
// TWO servers across a simulated restart can give both the SAME relay identity,
// as a restarted process would have (it reads its persisted one back).
func newTestRelayIdentity(t *testing.T) ([]byte, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "waiveo-relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), priv
}

// testPlayerServerGrantID is newTestPlayerServer's own boot-time REL-121
// pairing-grant id — hoisted to package scope so buildRePullContentApplied
// (below) can carry the SAME grant forward as every re-pull fixture's own
// desiredstate.Applied.PairingGrants, mirroring what a real relay/1 pull
// actually returns: a FULL current pairing_grants list, not a delta — a
// grant an app peer never touched keeps riding every successive generation's
// snapshot, identical, until something actually changes it.
const testPlayerServerGrantID = "grant-schedresolver-test-01"

// testPlayerServerGrant builds the wire.PairingGrant record NewServer boots
// srv with (testPlayerServerGrantID), freshly timestamped so its TTL is
// always live for the calling test's own real-time duration.
// testPlayerServerScreenID is the SCREEN IDENTITY ROW (data-model/1 DAT-004a)
// these cases' pairing grant redeems into — the same id the feeder's fixture
// snapshot puts on its one screen_programs entry (snapshot.FixtureScreenID),
// so a test player pairing here is served that entry rather than the terminal
// default a relay serves an unknown screen.
const testPlayerServerScreenID = snapshot.FixtureScreenID

func testPlayerServerGrant() wire.PairingGrant {
	return wire.PairingGrant{
		GrantID:                testPlayerServerGrantID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		ScreenID:               testPlayerServerScreenID,
		TTL:                    900,
		RedemptionMode:         "one-time",
		IssuedAt:               time.Now().UnixMilli(),
	}
}

// pairAndPull pairs a player against srv (redeeming grantID) and pulls its
// served program back over player/1's own pair -> program HTTP surface — the
// black-box way to observe what the boot path actually configured
// (SetProgram/SetServedProgram), not an internal field peek.
func pairAndPull(t *testing.T, srv *playerserver.Server, grantID string) playerserver.LeaseResponse {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(apihttp.WithTraceID(mux))
	t.Cleanup(ts.Close)

	pairBody, err := json.Marshal(playerserver.PairingRequest{
		HardwareID:    "hw-schedresolver-0001",
		GrantSelector: grantID,
		Capabilities:  playerserver.Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	pairResp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(pairBody))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	defer pairResp.Body.Close()
	var pr playerserver.PairingResponse
	if err := json.NewDecoder(pairResp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode pairing response: %v", err)
	}
	if pr.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pr)
	}

	pullBody, err := json.Marshal(playerserver.ProgramPullRequest{
		Capabilities: playerserver.Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal program pull: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/player/v1/program", bytes.NewReader(pullBody))
	if err != nil {
		t.Fatalf("build program request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+pr.ChannelToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /player/v1/program: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("program pull status = %d, want 200", resp.StatusCode)
	}
	var lease playerserver.LeaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	return lease
}

// buildDemoAppliedForTest runs the real feeder Build path (the same demo
// schedule Task 2 authored, REL-065) and adapts its output into a
// desiredstate.Applied value — the exact shape bootScheduleResolverAt receives
// from a verified pull, without standing up a live feeder + pull-over-frames
// round trip (already covered by internal/relay/desiredstate's own tests).
func buildDemoAppliedForTest(t *testing.T) desiredstate.Applied {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	snap, err := snapshot.Build([]byte("fixture-image-bytes"), contenturl.Signer{Base: "https://origin.example"}, id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	return desiredstate.Applied{
		Generation:      snap.Generation,
		ScreenID:        snap.Sections.ScreenPrograms[0].ScreenID,
		ProgramRevision: snap.Sections.ScreenPrograms[0].ProgramRevision,
		Priority:        snap.Sections.ScreenPrograms[0].Priority,
		Display:         snap.Sections.ScreenPrograms[0].Display,
		ScreenPrograms:  snap.Sections.ScreenPrograms,
		Schedule:        snap.Sections.Schedule,
	}
}

// fakeScheduleSink builds a real *automation.CommandSink over a loopback
// device-command surface — the SAME reused constructor
// (automation.NewCommandSink over a deviceplane.CommandSurface) preset
// firing dispatches through in the running binary; a no-op controller is
// enough here since these tests assert on the served Lease, not on
// dispatched device commands.
func fakeScheduleSink() *automation.CommandSink {
	surface := deviceplane.NewCommandSurface(
		loopbackController{},
		registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID + "-device", "media-player", true },
	)
	return automation.NewCommandSink(surface, "01J8ZTESTSCHEDSINKRELAYID1")
}

// demoContentHourInstant is a fixed Unix-ms instant inside the feeder's demo
// content daypart (06:00-22:00 America/Chicago, REL-065) — a deterministic
// "now" so the boot resolve lands on display:content regardless of wall-clock
// time when this test runs.
func demoContentHourInstant(t *testing.T) int64 {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return time.Date(2026, time.January, 15, 12, 0, 0, 0, loc).UnixMilli()
}

// TestBootScheduleResolverServesResolvedProgramForGovernedScreen is the
// Task-6 end-to-end proof: with the feeder's real demo schedule (Task 2)
// present, booting the schedule resolver at a content-hour instant resolves
// the governed screen to display:content and the player server's served
// Lease reflects the schedule-resolved program (its own programRevision,
// DAT-113) — not the raw app-authored one SetServedProgram configured
// moments before, exactly as main() configures it ahead of booting the
// resolver.
func TestBootScheduleResolverServesResolvedProgramForGovernedScreen(t *testing.T) {
	applied := buildDemoAppliedForTest(t)

	srv, grantID := newTestPlayerServer(t)
	srv.SetServedProgram(applied.Generation, applied.ScreenPrograms[0])
	appAuthoredRevision := applied.ScreenPrograms[0].ProgramRevision

	sink := fakeScheduleSink()
	site := hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298}

	resolvers := bootScheduleResolverAt(applied, srv, sink, site, demoContentHourInstant(t))
	if len(resolvers) != 1 {
		t.Fatalf("bootScheduleResolverAt returned %d resolver(s), want 1 (the demo schedule governs exactly one screen)", len(resolvers))
	}

	lease := pairAndPull(t, srv, grantID)
	if lease.Display != "content" {
		t.Errorf("served display = %q, want content (the schedule-resolved program)", lease.Display)
	}
	if lease.Priority != "scheduled" {
		t.Errorf("served priority = %q, want scheduled", lease.Priority)
	}
	if lease.ProgramRevision == appAuthoredRevision {
		t.Errorf("served program_revision = %q, still the app-authored one — the schedule resolver did not take over serving", lease.ProgramRevision)
	}
	if len(lease.Content) != 1 {
		t.Fatalf("served content has %d item(s), want 1", len(lease.Content))
	}
}

// TestBootScheduleResolverEmptyScheduleLeavesAppAuthoredProgramUnchanged
// asserts the stated additive serving policy (Global Constraints): with an
// empty (never-carried) schedule section, bootScheduleResolverAt builds no
// resolvers and the app-authored screen_programs SetServedProgram already
// configured is served completely unchanged — today's first-photon
// behavior, preserved.
func TestBootScheduleResolverEmptyScheduleLeavesAppAuthoredProgramUnchanged(t *testing.T) {
	appAuthored := wire.ScreenProgram{
		ScreenID:        "01J8Z9DEM0SCREENR0WF1RSTPH", // a screen identity row's id (DAT-004a), never a scope node's
		ProgramRevision: "rev-1",
		Priority:        "scheduled",
		Display:         "content",
		Content:         []wire.ContentRef{{AssetRef: "sha256:deadbeef", URL: "https://origin.example/content/deadbeef"}},
	}
	srv, grantID := newTestPlayerServer(t)
	srv.SetServedProgram(1, appAuthored)

	applied := desiredstate.Applied{} // never-carried schedule (today's first-photon state)
	sink := fakeScheduleSink()

	resolvers := bootScheduleResolverAt(applied, srv, sink, hello.SiteBinding{}, time.Now().UnixMilli())
	if len(resolvers) != 0 {
		t.Fatalf("bootScheduleResolverAt returned %d resolver(s) for an empty schedule, want 0", len(resolvers))
	}

	lease := pairAndPull(t, srv, grantID)
	if lease.ProgramRevision != appAuthored.ProgramRevision {
		t.Errorf("served program_revision = %q, want unchanged app-authored %q", lease.ProgramRevision, appAuthored.ProgramRevision)
	}
	if lease.Display != appAuthored.Display || lease.Priority != appAuthored.Priority {
		t.Errorf("served display/priority = %q/%q, want unchanged app-authored %q/%q", lease.Display, lease.Priority, appAuthored.Display, appAuthored.Priority)
	}
}

// recordingController records every physical dispatch it receives, in order —
// the same fake-controller pattern internal/relay/schedulehost's own tests use
// (recordController) — so a test can observe whether a boot-time preset batch
// actually reached the device plane.
type recordingController struct {
	mu   sync.Mutex
	seen []string // "entity/command", in dispatch order
}

func (c *recordingController) Dispatch(entityID, command string, params map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, entityID+"/"+command)
	return nil
}

func (c *recordingController) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.seen))
	copy(out, c.seen)
	return out
}

// recordingScheduleSink builds a real *automation.CommandSink — the SAME
// constructor the running binary and fakeScheduleSink both use — wired to a
// recordingController so a test can observe exactly which preset commands a
// boot resolve dispatched.
func recordingScheduleSink(ctrl *recordingController) *automation.CommandSink {
	surface := deviceplane.NewCommandSurface(
		ctrl,
		registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID + "-device", "media-player", true },
	)
	return automation.NewCommandSink(surface, "01J8ZTESTSCHEDSINKRECORD01")
}

// marshalRows marshals each value to json.RawMessage — building a
// wire.ScheduleSection's opaquely-carried row arrays by hand (REL-065),
// mirroring internal/feeder/snapshot's own unexported marshalEachRow helper
// (which this package cannot reach directly).
func marshalRows(t *testing.T, vs ...any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(vs))
	for _, v := range vs {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal row %+v: %v", v, err)
		}
		out = append(out, b)
	}
	return out
}

// skipMisfire fixture constants: a single schedule + one all-day daypart
// (00:00:00-23:59:59, so ANY boot instant lands inside it — modeling a relay
// restart landing inside an already-active daypart's window) declaring
// misfire:"skip" and bound to a preset batch.
const (
	skipMisfireSiteID     = "01J8ZBOOTSKIPSITE0000001"
	skipMisfireScreenID   = "01J8ZBOOTSKIPSCREEN000001"
	skipMisfireScheduleID = "01J8ZBOOTSKIPSCHEDULE0001"
	skipMisfireDaypartID  = "01J8ZBOOTSKIPDAYPART00001"
	skipMisfirePresetID   = "01J8ZBOOTSKIPPRESET000001"
	skipMisfireEntity     = "01J8ZBOOTSKIPENTITY000001"
)

// buildSkipMisfireAppliedForTest builds a minimal desiredstate.Applied whose
// carried schedule section governs one screen with a single all-day daypart
// declaring misfire:"skip" and bound to a preset batch — the DAT-075/076/
// 094/121 boot-resume regression fixture for bootScheduleResolverAt itself
// (the exact call site the fix touches).
func buildSkipMisfireAppliedForTest(t *testing.T) desiredstate.Applied {
	t.Helper()
	tz := "America/Chicago"
	lat := 41.8781
	long := -87.6298
	orgBound := "01J8ZBOOTSKIPORGBOUND0001"
	siteParent := skipMisfireSiteID

	siteNode := datamodel.ScopeNode{ID: skipMisfireSiteID, Kind: "site", ParentID: &orgBound, Name: "Skip Misfire Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	screenNode := datamodel.ScopeNode{ID: skipMisfireScreenID, Kind: "screen", ParentID: &siteParent, Name: "Skip Misfire Screen", Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	schedule := datamodel.Schedule{ID: skipMisfireScheduleID, ScopeNode: skipMisfireScreenID, Name: "Skip Misfire Schedule", Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	daypart := datamodel.Daypart{
		ID: skipMisfireDaypartID, ScheduleID: skipMisfireScheduleID, ScopeNode: skipMisfireScreenID,
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "00:00:00", EndTime: "23:59:59",
		DisplayPower: "on", PresetBatchID: skipMisfirePresetID, Misfire: "skip", Name: "All Day",
		Revision: 1, CreatedAt: 1, UpdatedAt: 1,
	}
	presetBatch := datamodel.PresetBatch{
		PresetID: skipMisfirePresetID, ScopeNode: skipMisfireScreenID, Name: "Skip Misfire Preset",
		Commands: []datamodel.PresetCommand{{EntityID: skipMisfireEntity, Command: "home"}},
		Revision: 1, CreatedAt: 1, UpdatedAt: 1,
	}

	sec := wire.ScheduleSection{
		ScopeNodes:    marshalRows(t, siteNode, screenNode),
		Schedules:     marshalRows(t, schedule),
		Dayparts:      marshalRows(t, daypart),
		PresetBatches: marshalRows(t, presetBatch),
	}.Normalized()

	return desiredstate.Applied{Schedule: sec}
}

// TestBootScheduleResolverSkipMisfireDoesNotResumeFire is the Task-6
// DAT-075/076/094/121 regression at the exact call site the fix touches
// (bootScheduleResolverAt): a daypart declaring misfire:"skip" MUST NOT
// re-dispatch its bound preset batch's device commands on a boot resolve that
// lands inside its already-active window — a site declares "skip" precisely
// so a relay restart (redeploy, crash-loop, box reboot) never re-toggles a
// device. The resolver is still built and governs the screen (the STATE
// projection, DAT-119, is unaffected); only the resume-edge preset dispatch
// is suppressed.
func TestBootScheduleResolverSkipMisfireDoesNotResumeFire(t *testing.T) {
	applied := buildSkipMisfireAppliedForTest(t)
	srv, _ := newTestPlayerServer(t)
	ctrl := &recordingController{}
	sink := recordingScheduleSink(ctrl)
	site := hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298}

	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	nowMs := time.Date(2026, time.January, 15, 12, 0, 0, 0, loc).UnixMilli() // inside the all-day daypart

	resolvers := bootScheduleResolverAt(applied, srv, sink, site, nowMs)
	if len(resolvers) != 1 {
		t.Fatalf("bootScheduleResolverAt returned %d resolver(s), want 1 (the fixture schedule governs exactly one screen)", len(resolvers))
	}

	if calls := ctrl.calls(); len(calls) != 0 {
		t.Fatalf("boot resolve with misfire:skip dispatched %v to the device plane, want nothing (DAT-075/076/121: a skip misfire suppresses the boot resume-edge fire)", calls)
	}
}

// --- Task 5: relay periodic desired-state re-pull + the live loop -------------

// Re-pull fixture identities: one site + screen + schedule governed by a single
// all-day content daypart sourcing a one-item playlist. Two of these at
// successive generations, differing only in the playlist's asset_ref, model an
// authored schedule edit A->B the re-pull loop must apply live.
const (
	rePullOrgBoundID  = "01J8ZREPULLORGBOUND000001"
	rePullSiteID      = "01J8ZREPULLSITE000000001"
	rePullScreenID    = "01J8ZREPULLSCREEN00000001"
	rePullScheduleID  = "01J8ZREPULLSCHEDULE000001"
	rePullDaypartID   = "01J8ZREPULLDAYPARTALLDAY01"
	rePullPlaylistID  = "01J8ZREPULLPLAYLIST000001"
	rePullTestRelayID = "01J8ZREPULLRELAYIDENTITY01"
	rePullAssetA      = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rePullAssetB      = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// buildRePullContentApplied builds a desiredstate.Applied at generation gen whose
// carried schedule governs one screen resolving (at a content-hour instant) to
// display:content sourced from a one-item playlist carrying assetRef — the exact
// shape a verified pull (desiredstate.VerifyAndApply) produces. Two at successive generations,
// differing only in assetRef, are the A->B authored-schedule edit the re-pull
// loop applies. It mirrors internal/feeder/snapshot's own demo authoring, using
// a single all-day daypart so any instant lands inside its window.
func buildRePullContentApplied(t *testing.T, gen int64, assetRef string) desiredstate.Applied {
	t.Helper()
	tz := "America/Chicago"
	lat := 41.8781
	long := -87.6298
	orgParent := rePullOrgBoundID
	siteParent := rePullSiteID

	siteNode := datamodel.ScopeNode{ID: rePullSiteID, Kind: "site", ParentID: &orgParent, Name: "Re-pull Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	screenNode := datamodel.ScopeNode{ID: rePullScreenID, Kind: "screen", ParentID: &siteParent, Name: "Re-pull Screen", Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	schedule := datamodel.Schedule{ID: rePullScheduleID, ScopeNode: rePullScreenID, Name: "Re-pull Schedule", Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	daypart := datamodel.Daypart{
		ID: rePullDaypartID, ScheduleID: rePullScheduleID, ScopeNode: rePullScreenID,
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "00:00:00", EndTime: "23:59:59",
		DisplayPower: "on", PlaylistID: rePullPlaylistID, Name: "All Day", Revision: 1, CreatedAt: 1, UpdatedAt: 1,
	}
	playlist := datamodel.Playlist{
		ID: rePullPlaylistID, ScopeNode: rePullScreenID, Name: "Re-pull Playlist",
		Items:    []datamodel.PlaylistItem{{Source: "asset", AssetRef: assetRef}},
		Revision: 1, CreatedAt: 1, UpdatedAt: 1,
	}

	sec := wire.ScheduleSection{
		ScopeNodes: marshalRows(t, siteNode, screenNode),
		Schedules:  marshalRows(t, schedule),
		Dayparts:   marshalRows(t, daypart),
		Playlists:  marshalRows(t, playlist),
	}.Normalized()

	// Carry testPlayerServerGrantID's own grant forward on every generation
	// this fixture builds — the same "a real pull returns the FULL current
	// pairing_grants list" reasoning testPlayerServerGrant's own doc gives:
	// this fixture's schedule-only tests never intend to exercise a grant
	// CHANGING across generations, so their fixture should not accidentally
	// supersede newRePullFixture's own boot grant to empty on every tick. A
	// test that DOES want to exercise a superseding grant set (a new or
	// dropped grant_id) overwrites this field explicitly on the returned
	// value, same as it already does for other fields.
	// One screen_programs entry, for the screen testPlayerServerGrant redeems
	// into. It is what tells the relay WHICH screen this schedule's resolution
	// is served to: resolution happens at a scope node and a program is served
	// to a screen identity row (DAT-004a), and with no screen carried at all
	// there is nobody to attribute the resolution to. Its own display is blank
	// so a test observing display:content can only be seeing the resolver's
	// output, never this baseline.
	programs := []wire.ScreenProgram{{
		ScreenID:        testPlayerServerScreenID,
		ProgramRevision: "app-authored-baseline",
		Priority:        "scheduled",
		Display:         "blank",
	}}

	return desiredstate.Applied{
		Generation:     gen,
		Schedule:       sec,
		ScreenPrograms: programs,
		PairingGrants:  []wire.PairingGrant{testPlayerServerGrant()},
	}
}

// newRePullFixture wires the live serving collaborators a re-pull tick drives —
// a real playerserver.Server, a scheduleDriver over it, and a real automationhost
// over an in-memory operational store — plus the deterministic content-hour
// instant the resolvers resolve at. It is the black-box harness the re-pull tests
// observe the served program through (pairAndPull).
func newRePullFixture(t *testing.T) (*scheduleDriver, *automationhost.Host, *playerserver.Server, string, int64) {
	t.Helper()
	srv, grantID := newTestPlayerServer(t)

	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host, err := automationhost.New(store, deviceclass.Builtin(), loopbackController{}, loopbackResolver, rePullTestRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}

	driver := &scheduleDriver{
		srv:       srv,
		sink:      fakeScheduleSink(),
		site:      hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298},
		tickEvery: scheduleResolverTickInterval,
	}
	return driver, host, srv, grantID, demoContentHourInstant(t)
}

// TestRePullAppliesHigherGenerationLive is the Task-5 oracle (REL-056): after a
// boot apply of generation N (schedule A), a re-pull tick that pulls generation
// N+1 (schedule B — a different authored playlist asset) re-resolves the governed
// screen so the served program reflects B, NOT A. This is the "an API edit MUST
// change the resolved program" guarantee: a green loop that fails to propagate
// the new generation is exactly what this test guards against.
func TestRePullAppliesHigherGenerationLive(t *testing.T) {
	driver, host, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedA := buildRePullContentApplied(t, 7, rePullAssetA)
	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)

	// Boot apply of generation N (schedule A): the screen now serves A's asset
	// (the "SCHEDULE RESOLVER OK ... asset ...aaaa" log line). The one-time
	// pairing grant is redeemed exactly once, after the re-pull, so the served
	// program observed IS the post-tick program — proving the transition A->B.
	driver.apply(ctx, appliedA, nowMs)

	// The tick must claim the caller's last-applied generation as REL-050's
	// since_generation, so an unchanged app peer can answer state.unchanged
	// instead of a full snapshot.
	var claimedSince int64 = -1
	puller := &rePuller{
		pull: func(since int64) (desiredstate.Applied, error) {
			claimedSince = since
			return appliedB, nil
		},
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: appliedA.Generation,
	}

	if applied := puller.tick(ctx); !applied {
		t.Fatal("re-pull tick of a higher generation returned applied=false, want true (REL-056 apply)")
	}
	if claimedSince != appliedA.Generation {
		t.Errorf("tick claimed since_generation %d, want the last-applied %d (REL-050)", claimedSince, appliedA.Generation)
	}
	if puller.lastGen != appliedB.Generation {
		t.Errorf("after applying gen 8, lastGen = %d, want 8", puller.lastGen)
	}

	lease := pairAndPull(t, srv, grantID)
	if lease.Display != "content" || lease.Priority != "scheduled" {
		t.Errorf("served display/priority = %q/%q, want content/scheduled (schedule-resolved)", lease.Display, lease.Priority)
	}
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetB {
		t.Fatalf("served content = %+v, want one item asset %s (the N+1 schedule) — the re-pull did not re-resolve live", lease.Content, rePullAssetB)
	}
}

// attemptPair POSTs a bare pairing attempt against srv for grantSelector and
// returns only the HTTP status code — the black-box way this file's grant-
// supersession test observes whether a given selector is currently
// redeemable, without asserting on a successful lease (pairAndPull already
// covers the success path end to end).
func attemptPair(t *testing.T, srv *playerserver.Server, grantSelector string) int {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(apihttp.WithTraceID(mux))
	t.Cleanup(ts.Close)

	body, err := json.Marshal(playerserver.PairingRequest{
		HardwareID:    "hw-repull-grants-0001",
		GrantSelector: grantSelector,
		Capabilities:  playerserver.Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	resp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestRePullSupersedesPairingGrantsWithNewerGeneration is the relay/1 REL-122
// oracle for the live loop's OWN redeemable grant set, mirroring
// TestRePullAppliesHigherGenerationLive's own shape for the served program: a
// re-pull tick that applies a strictly higher generation carrying a DIFFERENT
// pairing grant must make that new grant redeemable, and must retire the boot
// grant it replaces. Before playerserver.Server.SetPairingGrants existed,
// there was no call anywhere in the re-pull path that ever touched the grant
// set NewServer built once at boot — this is the regression test that fails
// without it (the new grant would stay unredeemable, PAIRING_CODE_INVALID,
// forever, and the superseded boot grant would stay redeemable forever too).
func TestRePullSupersedesPairingGrantsWithNewerGeneration(t *testing.T) {
	driver, host, srv, bootGrantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedA := buildRePullContentApplied(t, 7, rePullAssetA)
	driver.apply(ctx, appliedA, nowMs) // boot apply of generation 7; srv's grant set is still newRePullFixture's own boot grant (bootGrantID)

	const newGrantID = "grant-repull-superseding-01"
	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	appliedB.PairingGrants = []wire.PairingGrant{{
		GrantID:                newGrantID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		// The same screen the boot grant is bound to (REL-121a): the assertion
		// below is about the program a redemption is served, and a program is
		// served per screen, so a grant redeeming into some other screen would
		// be served the terminal default no matter what the resolver did.
		ScreenID:       testPlayerServerScreenID,
		TTL:            900,
		RedemptionMode: "one-time",
		IssuedAt:       time.Now().UnixMilli(),
	}}

	puller := &rePuller{
		pull:    func(int64) (desiredstate.Applied, error) { return appliedB, nil },
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: appliedA.Generation,
	}
	if applied := puller.tick(ctx); !applied {
		t.Fatal("re-pull tick of a higher generation returned applied=false, want true (REL-056 apply)")
	}

	// The boot grant is superseded — REL-122's own boundary condition ("until
	// a newer generation supersedes it") — so it must no longer redeem.
	if status := attemptPair(t, srv, bootGrantID); status != http.StatusBadRequest {
		t.Errorf("pairing against the superseded boot grant = %d, want 400 PAIRING_CODE_INVALID (REL-122 superseded)", status)
	}

	// The new generation's OWN grant must now be redeemable — REL-122's
	// affirmative half, and the whole point of a live re-pull being able to
	// refresh what a screen can pair against without a relay restart.
	lease := pairAndPull(t, srv, newGrantID)
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetB {
		t.Fatalf("served content after redeeming the superseding generation's own grant = %+v, want one item asset %s", lease.Content, rePullAssetB)
	}
}

// TestRePullSameGenerationIsNoOp pins REL-052/070: a re-pull that returns the
// already-applied generation is a no-op — tick reports it did not apply (no
// re-resolve churn) and the served program is left exactly as the last-applied
// generation set it.
func TestRePullSameGenerationIsNoOp(t *testing.T) {
	driver, host, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	driver.apply(ctx, appliedB, nowMs) // boot apply of gen N+1 (schedule B)

	puller := &rePuller{
		pull:    func(int64) (desiredstate.Applied, error) { return appliedB, nil }, // same generation returned again
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: appliedB.Generation,
	}

	if applied := puller.tick(ctx); applied {
		t.Fatal("re-pull tick of the same generation returned applied=true, want false (REL-070 no-op, no re-resolve churn)")
	}
	if puller.lastGen != appliedB.Generation {
		t.Errorf("lastGen after a same-generation no-op = %d, want unchanged 8", puller.lastGen)
	}
	lease := pairAndPull(t, srv, grantID)
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetB {
		t.Fatalf("served content after a no-op = %+v, want unchanged asset %s", lease.Content, rePullAssetB)
	}
}

// TestRePullRefreshesTheKeepaliveAdoptionGate pins the LIVE half of the screen
// keep-alive adoption gate: an operator who adopts a screen this afternoon
// gets it kept alive this afternoon.
//
// The failure this guards is quiet in the worst way. Adoption wired only at
// boot leaves the relay serving the new generation's content perfectly while
// refusing to re-launch the very screen the operator just adopted — no error,
// no log, nothing to notice until that screen sits at Home. The inverse
// matters just as much: a screen REMOVED from the inventory must stop being
// driven live, because that is how a screen is handed back to the legacy
// stack, and a relay that kept driving it is one half of the two-controller
// flapping failure.
func TestRePullRefreshesTheKeepaliveAdoptionGate(t *testing.T) {
	driver, host, _, _, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		adoptedScreen = "screen.hanger"
		laterScreen   = "screen.lobby"
	)
	inventoryWith := func(entityIDs ...string) wire.DeviceInventory {
		inv := wire.DeviceInventory{Devices: []json.RawMessage{}, PackMatchPatterns: []json.RawMessage{}}
		for _, id := range entityIDs {
			raw, err := json.Marshal(wire.DeviceEntry{
				DeviceID: "device." + id,
				Driver:   "roku",
				NativeID: id,
				Entities: []wire.DeviceEntity{{EntityID: id, DeviceClass: "media-player", Enabled: true, Category: "primary"}},
			})
			if err != nil {
				t.Fatalf("marshal device entry: %v", err)
			}
			inv.Devices = append(inv.Devices, raw)
		}
		return inv
	}

	appliedA := buildRePullContentApplied(t, 7, rePullAssetA)
	appliedA.DeviceInventory = inventoryWith(adoptedScreen)
	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	appliedB.DeviceInventory = inventoryWith(laterScreen)

	adoption := keepalive.NewAdoptionSet()
	adoption.Apply(appliedA.Generation, appliedA.DeviceInventory) // the boot seed main.go performs
	driver.apply(ctx, appliedA, nowMs)

	if !adoption.IsAdopted(adoptedScreen) {
		t.Fatalf("the boot generation's adopted screen %q is not adopted", adoptedScreen)
	}
	if adoption.IsAdopted(laterScreen) {
		t.Fatalf("%q is adopted before the generation that adopts it was applied", laterScreen)
	}

	puller := &rePuller{
		pull:     func(int64) (desiredstate.Applied, error) { return appliedB, nil },
		driver:   driver,
		host:     host,
		adoption: adoption,
		nowFn:    func() int64 { return nowMs },
		lastGen:  appliedA.Generation,
	}
	if applied := puller.tick(ctx); !applied {
		t.Fatal("re-pull tick of a higher generation returned applied=false")
	}

	if !adoption.IsAdopted(laterScreen) {
		t.Errorf("%q was adopted by the applied generation but the live path never refreshed the gate — it would stay un-driven until a restart", laterScreen)
	}
	if adoption.IsAdopted(adoptedScreen) {
		t.Errorf("%q was dropped from the applied generation's inventory but is still driven — un-adoption must take effect live", adoptedScreen)
	}
}

// A relay with keep-alive disabled has no adoption set at all, and the live
// path must tolerate that rather than panic on the first applied generation.
func TestRePullToleratesNoAdoptionSet(t *testing.T) {
	driver, host, _, _, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedA := buildRePullContentApplied(t, 7, rePullAssetA)
	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	driver.apply(ctx, appliedA, nowMs)

	puller := &rePuller{
		pull:    func(int64) (desiredstate.Applied, error) { return appliedB, nil },
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: appliedA.Generation,
	} // adoption deliberately nil
	if applied := puller.tick(ctx); !applied {
		t.Fatal("re-pull tick with no adoption set returned applied=false")
	}
}

// TestRePullRejectsRegressedGeneration pins REL-052: a re-pull that regresses —
// whether the pull itself rejects it as desiredstate.ErrGenerationRegressed, or
// returns a lower generation the loop's own monotonic guard rejects — is a no-op,
// and the last-applied generation stays served.
func TestRePullRejectsRegressedGeneration(t *testing.T) {
	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	appliedRegressed := buildRePullContentApplied(t, 7, rePullAssetA) // a lower generation than last-applied

	cases := []struct {
		name string
		pull pullFunc
	}{
		{"pull rejects as ErrGenerationRegressed", func(int64) (desiredstate.Applied, error) {
			return desiredstate.Applied{}, desiredstate.ErrGenerationRegressed
		}},
		{"pull returns a lower generation", func(int64) (desiredstate.Applied, error) {
			return appliedRegressed, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			driver, host, srv, grantID, nowMs := newRePullFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			driver.apply(ctx, appliedB, nowMs) // last-applied = gen 8 (schedule B)
			puller := &rePuller{pull: tc.pull, driver: driver, host: host, nowFn: func() int64 { return nowMs }, lastGen: appliedB.Generation}

			if applied := puller.tick(ctx); applied {
				t.Fatal("re-pull tick of a regressed generation returned applied=true, want false (REL-052 rejected)")
			}
			if puller.lastGen != appliedB.Generation {
				t.Errorf("lastGen after a regressed pull = %d, want unchanged 8", puller.lastGen)
			}
			lease := pairAndPull(t, srv, grantID)
			if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetB {
				t.Fatalf("served content after a regressed pull = %+v, want unchanged asset %s (last-applied stays)", lease.Content, rePullAssetB)
			}
		})
	}
}

// TestRePullFailureIsNonFatal pins REL-055: a mid-run pull failure (app peer
// unreachable, transport error) is non-fatal — the tick reports no apply and the
// last-applied program keeps being served offline, never blanked.
func TestRePullFailureIsNonFatal(t *testing.T) {
	driver, host, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	driver.apply(ctx, appliedB, nowMs)

	puller := &rePuller{
		pull: func(int64) (desiredstate.Applied, error) {
			return desiredstate.Applied{}, errors.New("app peer unreachable")
		},
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: appliedB.Generation,
	}

	if applied := puller.tick(ctx); applied {
		t.Fatal("re-pull tick with a failing pull returned applied=true, want false (REL-055 non-fatal, keep last-applied)")
	}
	lease := pairAndPull(t, srv, grantID)
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetB {
		t.Fatalf("served content after a failed pull = %+v, want unchanged asset %s (last-applied served offline)", lease.Content, rePullAssetB)
	}
}

// TestNudgeSinkDeliversToInstalledHandlerAndTickApplies proves the nudge
// wiring that replaced the legacy poll loop: a state.changed delivery through
// the nudgeSink drives rePuller.tick (applying a strictly higher generation
// live), and a nudge arriving BEFORE any handler is installed is dropped
// harmlessly (REL-057 best-effort — the supervisor's pull-on-reconnect
// recovers it), never a nil-handler panic.
func TestNudgeSinkDeliversToInstalledHandlerAndTickApplies(t *testing.T) {
	driver, host, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedA := buildRePullContentApplied(t, 7, rePullAssetA)
	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	driver.apply(ctx, appliedA, nowMs)

	puller := &rePuller{
		pull:    func(int64) (desiredstate.Applied, error) { return appliedB, nil },
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: appliedA.Generation,
	}

	nudges := &nudgeSink{}
	nudges.deliver(8) // no handler installed yet: dropped, no panic (REL-057 best-effort)

	nudges.set(func(int64) { puller.tick(ctx) })
	nudges.deliver(8) // handler installed: pulls + applies gen 8 synchronously

	lease := pairAndPull(t, srv, grantID)
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetB {
		t.Fatalf("after a delivered nudge, served content = %+v, want asset %s", lease.Content, rePullAssetB)
	}
}

// TestTelemetryFlushLoopPushesBufferedTelemetryOnTick proves the flush wiring:
// telemetryFlushLoop drives telemetry.Channel.Flush once per tick delivered on
// its channel (a manual channel here, a time.Ticker in the binary), pushing the
// buffered automation.run to the app peer over the concrete telemetryhttp
// transport, and advancing retention on the received ack_through_seq (REL-090/
// 092/097); and it returns cleanly when its context is cancelled. Two sends on
// the unbuffered channel act as a barrier — the second returns only once the
// loop has finished processing the first — so the assertion is race-free.
func TestTelemetryFlushLoopPushesBufferedTelemetryOnTick(t *testing.T) {
	var pushedEntries atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch telemetry.PushBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("app peer: decode telemetry.push: %v", err)
		}
		var through int64
		for _, e := range batch.Entries {
			if e.Seq > through {
				through = e.Seq
			}
		}
		pushedEntries.Add(int64(len(batch.Entries)))
		_ = json.NewEncoder(w).Encode(telemetry.Ack{AckThroughSeq: through})
	}))
	defer srv.Close()

	buf := telemetry.NewBuffer(16)
	buf.Record(telemetry.SchemaAutomationRun, json.RawMessage(`{"rule_id":"01J8Z3K4N5RULEA"}`), "", 1)
	ch := telemetry.NewChannel(buf, telemetryhttp.New(srv.URL, srv.Client()), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		telemetryFlushLoop(ctx, ticks, ch)
		close(done)
	}()

	ticks <- time.Now() // tick 1: pushes the buffered automation.run
	ticks <- time.Now() // barrier: returns only after tick 1 is fully processed

	if got := pushedEntries.Load(); got != 1 {
		t.Errorf("app peer received %d telemetry entries after a flush tick, want 1", got)
	}
	if pending := buf.Pending(); len(pending) != 0 {
		t.Errorf("after an acked flush, buffer has %d pending entries, want 0 (retention advanced)", len(pending))
	}

	cancel() // the loop must return on context cancellation
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("telemetryFlushLoop did not return after context cancellation")
	}
}

// TestLivePullRidesTheAuthenticatedConnectionOnly pins the authorization
// property the HTTP era enforced with a helloOK gate: a relay whose hello the
// app peer refused must not acquire live desired state by any path. On the
// persistent transport this is structural — state.pull exists ONLY as a frame
// on the mutually authenticated connection relayconn.Dial returns, and this
// binary's one pull implementation reads the supervisor's current connection
// holder, erroring (non-fatally) while disconnected. This test guards against
// the bypass being reintroduced: main.go must contain no HTTP-era pull path
// (desiredstate.Pull is deleted from the codebase) and the pull closure must
// go through the connection holder.
func TestLivePullRidesTheAuthenticatedConnectionOnly(t *testing.T) {
	for _, file := range []string{"main.go", "conn.go", "livepull.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(src), "desiredstate.Pull(") {
			t.Errorf("%s reintroduces an HTTP-era desiredstate.Pull call — live desired state must ride the authenticated connection only", file)
		}
	}

	// While disconnected (no live connection in the holder), a pull errors
	// non-fatally instead of reaching the app peer by another route.
	holder := &connHolder{}
	if c := holder.get(); c != nil {
		t.Fatalf("fresh connHolder holds %v, want nil while disconnected", c)
	}
}

// TestRenewalDue pins the proactive-renewal predicate's clock discipline
// (REL-015 + REL-130/135): due exactly when the persisted leaf is inside
// the renewal window, evaluated on the LATEST of the OS wall clock, the
// persisted clock floor, and the hint-adjusted runtime clock — so a
// backwards-jumped OS clock can never suppress renewal past a time the
// relay already verified, and a store with no identity is simply not due
// (enrollment's problem, not renewal's).
func TestRenewalDue(t *testing.T) {
	const window = 30 * 24 * time.Hour

	newStoreWithCert := func(t *testing.T, notAfter time.Time) *identity.Store {
		t.Helper()
		store, err := identity.Open(":memory:")
		if err != nil {
			t.Fatalf("identity.Open(:memory:): %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: selfSignedCertDER(t, notAfter)})
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		if err := store.SetIdentity("relay-test", certPEM, priv); err != nil {
			t.Fatalf("SetIdentity: %v", err)
		}
		return store
	}

	t.Run("far from expiry is not due", func(t *testing.T) {
		store := newStoreWithCert(t, time.Now().Add(400*24*time.Hour))
		if renewalDue(store, clocktrust.NewRuntimeClock(), window) {
			t.Error("renewalDue = true for a leaf 400 days from expiry, want false")
		}
	})

	t.Run("inside the window is due", func(t *testing.T) {
		store := newStoreWithCert(t, time.Now().Add(10*24*time.Hour))
		if !renewalDue(store, clocktrust.NewRuntimeClock(), window) {
			t.Error("renewalDue = false for a leaf 10 days from expiry with a 30-day window, want true")
		}
	})

	t.Run("persisted floor overrides a lagging wall clock", func(t *testing.T) {
		// The leaf is far from expiry by the OS wall clock, but the relay
		// has VERIFIED a later time (the persisted floor sits past
		// NotAfter-window): the floor must win, or a backwards-jumped OS
		// clock could suppress renewal past real expiry (REL-130/132).
		notAfter := time.Now().Add(400 * 24 * time.Hour)
		store := newStoreWithCert(t, notAfter)
		if advanced, err := store.SetClockFloor(notAfter.Add(-time.Hour).UnixMilli()); err != nil || !advanced {
			t.Fatalf("SetClockFloor: advanced=%v err=%v", advanced, err)
		}
		if !renewalDue(store, clocktrust.NewRuntimeClock(), window) {
			t.Error("renewalDue = false with the persisted floor inside the window, want true (floor-aware clock)")
		}
	})

	t.Run("no persisted identity is not due", func(t *testing.T) {
		store, err := identity.Open(":memory:")
		if err != nil {
			t.Fatalf("identity.Open(:memory:): %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if renewalDue(store, clocktrust.NewRuntimeClock(), window) {
			t.Error("renewalDue = true for an unenrolled store, want false")
		}
	})
}

// twoScreenGrant builds a live screen-bound pairing grant (REL-121a) for
// screenID with its own grant_id, so one player server can carry a redeemable
// grant per screen.
func twoScreenGrant(screenID string) wire.PairingGrant {
	return wire.PairingGrant{
		GrantID:                "grant-offline-" + screenID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		ScreenID:               screenID,
		TTL:                    900,
		RedemptionMode:         "one-time",
		IssuedAt:               time.Now().UnixMilli(),
	}
}

// TestOfflineBootServesEveryPersistedScreenProgram is the offline-continuity
// oracle at the granularity that actually matters (REL-055/061): the boot path
// reads the relay's OWN durable copy of screen_programs — no app peer in the
// picture at all — and every screen it names comes back served ITS OWN entry.
//
// The path under test is the one a restart with the app peer down takes:
// serveAppAuthoredPrograms over what desiredstate.ServedProgram returns from
// the store. Serving only the first entry left every other screen on the site
// showing the first screen's content, which is not a degraded continuity but a
// wrong one.
func TestOfflineBootServesEveryPersistedScreenProgram(t *testing.T) {
	const (
		lobbyScreen = "01J8Z9DEM0SCREENR0WL0BBY01"
		cafeScreen  = "01J8Z9DEM0SCREENR0WCAFE001"
		darkScreen  = "01J8Z9DEM0SCREENR0WDARK001"
	)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "waiveo-relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	srv, err := playerserver.NewServer(certPEM, []wire.PairingGrant{
		twoScreenGrant(lobbyScreen), twoScreenGrant(cafeScreen), twoScreenGrant(darkScreen),
	}, playerserver.WallClockMs)
	if err != nil {
		t.Fatalf("playerserver.NewServer: %v", err)
	}
	srv.SetSigningKey(priv)

	// The persisted last-applied section, exactly as desiredstate.ServedProgram
	// hands it back from the relay's own durable store: three screens, three
	// different programs, and one malformed entry naming no screen at all.
	served := []wire.ScreenProgram{
		{ScreenID: lobbyScreen, ProgramRevision: "rev-lobby", Priority: "scheduled", Display: "content",
			Content: []wire.ContentRef{{AssetRef: "sha256:aa", URL: "https://origin.example/content/lobby"}}},
		{ScreenID: cafeScreen, ProgramRevision: "rev-cafe", Priority: "preempt", Display: "content",
			Content: []wire.ContentRef{{AssetRef: "sha256:bb", URL: "https://origin.example/content/cafe"}}},
		{ScreenID: darkScreen, ProgramRevision: "rev-dark", Priority: "scheduled", Display: "blank"},
		{ScreenID: "", ProgramRevision: "rev-orphan", Priority: "scheduled", Display: "content"},
	}
	serveAppAuthoredPrograms(srv, 12, served)

	lobby := pairAndPull(t, srv, "grant-offline-"+lobbyScreen)
	cafe := pairAndPull(t, srv, "grant-offline-"+cafeScreen)
	dark := pairAndPull(t, srv, "grant-offline-"+darkScreen)

	if lobby.ProgramRevision != "rev-lobby" {
		t.Errorf("lobby screen served program_revision %q, want rev-lobby", lobby.ProgramRevision)
	}
	if cafe.ProgramRevision != "rev-cafe" {
		t.Errorf("cafe screen served program_revision %q, want rev-cafe — a later screen_programs entry was never installed, so this screen is showing the first entry's program", cafe.ProgramRevision)
	}
	if dark.ProgramRevision != "rev-dark" || dark.Display != "blank" {
		t.Errorf("dark screen served %q/%q, want rev-dark/blank", dark.ProgramRevision, dark.Display)
	}
	if cafe.Priority != "preempt" {
		t.Errorf("cafe screen served priority %q, want preempt (PLY-108, from its own entry)", cafe.Priority)
	}
	if len(lobby.Content) != 1 || len(cafe.Content) != 1 {
		t.Fatalf("content counts = lobby:%d cafe:%d, want 1 each", len(lobby.Content), len(cafe.Content))
	}
	if lobby.Content[0].URL == cafe.Content[0].URL {
		t.Errorf("lobby and cafe were served the same content url %q — every paired screen is showing one screen's program", lobby.Content[0].URL)
	}
	if len(dark.Content) != 0 {
		t.Errorf("dark screen served content %+v, want none (display:blank)", dark.Content)
	}
}

// TestDialAddressRefusesALoopbackCodeBehindALANListener covers the deployment
// footgun in the pairing dial address. pairHost defaults to loopback, which is
// right while the listener is also on loopback (a dev run, CI, the in-process
// harness — the player is on this same host). It becomes wrong the instant an
// on-box deployment overrides only WAIVEO_RELAY_LISTEN: the relay then binds
// where a screen on the LAN can reach it and forms a code telling that screen to
// dial 127.0.0.1, which is the SCREEN's own loopback.
//
// A code that cannot work is worse than no code — the same judgement the
// REL-121b relay-binding skip already makes — so the mismatch yields no dial
// address at all, and every consumer (the printed code, the SSDP announcement,
// and the address hello advertises for the app peer to form codes from) declines
// rather than publishing a wrong one.
//
// It deliberately refuses ONLY the mismatch. Guessing which LAN address a screen
// should use is a deployment fact this binary does not have.
func TestDialAddressRefusesALoopbackCodeBehindALANListener(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		host    string
		wantErr bool // true = no dial address may be formed
	}{
		{"loopback listener, loopback dial (dev/CI default)", "127.0.0.1:7421", "127.0.0.1", false},
		{"localhost listener, localhost dial", "localhost:7421", "localhost", false},
		{"LAN listener, LAN dial (a configured deployment)", "192.168.50.12:7421", "192.168.50.12", false},
		{"LAN listener, loopback dial (the footgun)", "192.168.50.12:7421", "127.0.0.1", true},
		{"all-interfaces listener, loopback dial", "0.0.0.0:7421", "127.0.0.1", true},
		{"portless all-interfaces listener, loopback dial", ":7421", "127.0.0.1", true},
		{"LAN listener, localhost dial", "192.168.50.12:7421", "localhost", true},
		{"LAN listener, ipv6 loopback dial", "192.168.50.12:7421", "::1", true},
		{"LAN listener, hostname dial (not second-guessed)", "192.168.50.12:7421", "waiveo.local", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{listen: tc.listen, pairHost: tc.host, pairPort: 7421}
			got := cfg.dialAddress()
			if tc.wantErr && got != "" {
				t.Errorf("dialAddress() = %q, want \"\" — a code formed from this pairing dial host tells a screen to dial its own loopback", got)
			}
			if !tc.wantErr && got == "" {
				t.Errorf("dialAddress() = \"\", want a dial address — this configuration is dialable and must keep forming codes")
			}
			if !tc.wantErr && got != net.JoinHostPort(tc.host, "7421") {
				t.Errorf("dialAddress() = %q, want %q", got, net.JoinHostPort(tc.host, "7421"))
			}
		})
	}
}

// TestListeningLineAgreesWithPairingCodeFormation: the boot log must not assert
// two contradictory things about the same configuration.
//
// The listening line used to print the raw pairHost/pairPort regardless of
// whether a code could be formed from them, so a box that had overridden only
// the listen address logged "pairing code dial 127.0.0.1:7421" and then "NOT
// forming pairing codes" — the first line naming an address no code would ever
// carry, and naming it first.
func TestListeningLineAgreesWithPairingCodeFormation(t *testing.T) {
	dialable := config{listen: "192.168.50.12:7421", pairHost: "192.168.50.12", pairPort: 7421}
	line := listeningLine(dialable)
	if !strings.Contains(line, "192.168.50.12:7421") {
		t.Errorf("listeningLine(dialable) = %q, want it to name the dial address a formed code carries", line)
	}

	// The footgun: a LAN listener behind a loopback dial host. No code is formed
	// for this configuration, so this line must not name a dial address either.
	mismatched := config{listen: "192.168.50.12:7421", pairHost: "127.0.0.1", pairPort: 7421}
	line = listeningLine(mismatched)
	if strings.Contains(line, "127.0.0.1:7421") {
		t.Errorf("listeningLine(mismatched) = %q — it advertises a dial address for a configuration that forms no pairing code at all", line)
	}
	if !strings.Contains(line, "no dialable pairing address") {
		t.Errorf("listeningLine(mismatched) = %q, want it to say no code is formed, matching what logPairingCodes then reports", line)
	}
}

// TestRePullSameHashHigherGenerationIsANoOp pins REL-070's actual condition: a
// snapshot whose hash equals the last-applied hash MUST be treated as a no-op
// and MUST NOT re-run any apply-time side effect — and that holds "regardless of
// whether `generation` itself advanced".
//
// A higher generation carrying byte-identical sections is the ONLY case the
// requirement is about that the generation guard cannot catch, and it is the
// case that was reaching the apply path. What re-ran there is not cosmetic:
// scheduleDriver.apply cancels the prior generation's in-flight schedule-resolve
// loops, which REL-070 names outright ("no in-flight rule run is canceled on
// account of it").
//
// The generation still advances. REL-070 suppresses the apply-time EFFECTS, not
// the record of what was applied — a relay that kept reporting the stale
// generation would look permanently behind to its app peer.
func TestRePullSameHashHigherGenerationIsANoOp(t *testing.T) {
	driver, host, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	appliedB.Hash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	driver.apply(ctx, appliedB, nowMs)

	// Generation 9, same hash, and — deliberately — DIFFERENT content. If the
	// no-op is honoured, that content must never reach the served program: the
	// test would pass just as well with identical content, but then it could not
	// tell a real no-op from an apply that happened to change nothing.
	repeat := buildRePullContentApplied(t, 9, rePullAssetA)
	repeat.Hash = appliedB.Hash

	applies := 0
	puller := &rePuller{
		pull: func(int64) (desiredstate.Applied, error) {
			applies++
			return repeat, nil
		},
		driver:   driver,
		host:     host,
		nowFn:    func() int64 { return nowMs },
		lastGen:  appliedB.Generation,
		lastHash: appliedB.Hash,
	}

	if applied := puller.tick(ctx); applied {
		t.Fatal("a higher generation carrying the SAME section hash reported applied=true; REL-070 makes it a no-op regardless of whether the generation advanced")
	}
	if applies != 1 {
		t.Fatalf("pull was called %d times, want 1 — the fixture did not exercise a tick", applies)
	}
	// The generation advanced even though nothing re-ran.
	if puller.lastGen != 9 {
		t.Errorf("lastGen after a same-hash no-op = %d, want 9 — the apply-time effects are suppressed, not the record of what was applied", puller.lastGen)
	}
	// And the serving side is untouched: still B's asset, never A's.
	lease := pairAndPull(t, srv, grantID)
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetB {
		t.Fatalf("served content after a same-hash no-op = %+v, want unchanged asset %s — the apply re-ran and replaced the served program", lease.Content, rePullAssetB)
	}
}

// TestRePullDifferentHashHigherGenerationApplies is the control. Without it, a
// fence that refused EVERY apply — the easiest way to break this — satisfies the
// no-op test above while making the relay permanently unable to take new state.
func TestRePullDifferentHashHigherGenerationApplies(t *testing.T) {
	driver, host, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	appliedB.Hash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	driver.apply(ctx, appliedB, nowMs)

	next := buildRePullContentApplied(t, 9, rePullAssetA)
	next.Hash = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	puller := &rePuller{
		pull:     func(int64) (desiredstate.Applied, error) { return next, nil },
		driver:   driver,
		host:     host,
		nowFn:    func() int64 { return nowMs },
		lastGen:  appliedB.Generation,
		lastHash: appliedB.Hash,
	}

	if applied := puller.tick(ctx); !applied {
		t.Fatal("a higher generation carrying a DIFFERENT section hash was not applied; REL-070's no-op is hash equality, not a blanket refusal")
	}
	if puller.lastHash != next.Hash {
		t.Errorf("lastHash after an apply = %q, want the applied snapshot's hash %q — a stale comparison value makes the very next repeat undetectable", puller.lastHash, next.Hash)
	}
	lease := pairAndPull(t, srv, grantID)
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetA {
		t.Fatalf("served content after a real apply = %+v, want the new asset %s", lease.Content, rePullAssetA)
	}
}

// relayTestCertDER mints a self-signed leaf for pairing-code formation, which
// commits to the certificate a screen will pin.
func relayTestCertDER(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "waiveo-relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return der
}

// TestBootPairingCodesSkipGrantsBoundToAnotherRelay pins REL-121b on the boot
// log, which is a dev-console surface and was covered by nothing: deleting the
// skip entirely leaves the whole tree green.
//
// Every relay of a site applies the SAME signed snapshot, so relay A holds
// relay B's grants in memory whether or not it may redeem them. The skip is
// what keeps them out of A's LOG. Two distinct reasons, and the second is the
// one that makes this more than cosmetic:
//
//   - A code A forms encodes A's OWN dial address against a selector only B can
//     redeem, so anyone who typed it would be refused. A code that cannot work
//     is worse than no code.
//   - It writes a selector redeemable AT RELAY B into relay A's log. Both boxes
//     hold the grant, but only one of them can spend it, and a log is read by
//     more people and kept longer than process memory.
func TestBootPairingCodesSkipGrantsBoundToAnotherRelay(t *testing.T) {
	const (
		thisRelay  = "01J8ZRELAYSELF000000000001"
		otherRelay = "01J8ZRELAYOTHER0000000002"
	)
	grants := []wire.PairingGrant{
		{GrantID: "grant-mine-000000000000001", RelayID: thisRelay, ScreenID: "01J8ZSCREEN0000000000000A"},
		{GrantID: "grant-theirs-00000000000002", RelayID: otherRelay, ScreenID: "01J8ZSCREEN0000000000000B"},
		{GrantID: "grant-unbound-0000000000003", ScreenID: "01J8ZSCREEN0000000000000C"},
	}
	cfg := config{listen: "192.168.1.50:7421", pairHost: "192.168.1.50", pairPort: 7421}

	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	logPairingCodes(cfg, desiredstate.Applied{PairingGrants: grants}, relayTestCertDER(t), thisRelay)
	out := buf.String()

	if !strings.Contains(out, "grant-mine-000000000000001") {
		t.Errorf("no pairing code was logged for this relay's OWN grant; the surface is doing nothing.\nlog:\n%s", out)
	}
	// An unbound grant is redeemable at any relay of the site (REL-121b's
	// exemption), so this relay may display it.
	if !strings.Contains(out, "grant-unbound-0000000000003") {
		t.Errorf("no pairing code was logged for an UNBOUND grant, which any relay may redeem.\nlog:\n%s", out)
	}
	if strings.Contains(out, "grant-theirs-00000000000002") {
		t.Errorf("this relay logged a pairing code for a grant bound to another relay (REL-121b). "+
			"The code encodes THIS relay's dial address against a selector only the bound relay can redeem, and it "+
			"puts that selector in this box's log.\nlog:\n%s", out)
	}
}

// TestBootPairingCodesFormNoneWithoutADialableAddress: with no address a screen
// could reach, the surface says so and forms nothing.
//
// This is the control. Without it a skip that dropped EVERY grant — the easiest
// way to break the test above — passes it.
func TestBootPairingCodesFormNoneWithoutADialableAddress(t *testing.T) {
	grants := []wire.PairingGrant{{GrantID: "grant-mine-000000000000001", ScreenID: "01J8ZSCREEN0000000000000A"}}
	// Loopback pair host behind a non-loopback listener: a formed code would
	// tell a screen to dial its own loopback.
	cfg := config{listen: "192.168.1.50:7421", pairHost: "127.0.0.1", pairPort: 7421}

	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	logPairingCodes(cfg, desiredstate.Applied{PairingGrants: grants}, relayTestCertDER(t), "01J8ZRELAYSELF000000000001")
	out := buf.String()

	if strings.Contains(out, "grant-mine-000000000000001") {
		t.Errorf("a pairing code was formed with no dialable address; it would tell a screen to dial its own loopback.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "NOT forming pairing codes") {
		t.Errorf("no codes were formed and nothing said why — an operator debugging a screen that cannot reach the "+
			"server has nothing connecting it to this box's configuration.\nlog:\n%s", out)
	}
}
