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
// in memory. It then serves player/1's pairing surface
// (internal/relay/playerserver, PLY-030–037) over its own HTTPS listener,
// using the exact same certificate — the enrollment identity persisted in
// its identity store — that FormPairingCode's commitment (REL-126) is
// computed over, so a player's local PLY-052 comparison is always checking
// the cert this listener actually presents.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

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
	return config{
		listen:    envOr(env, "WAIVEO_RELAY_LISTEN", "127.0.0.1:7421"),
		feederURL: envOr(env, "WAIVEO_FEEDER_URL", "https://127.0.0.1:7420"),
		pairHost:  envOr(env, "WAIVEO_RELAY_PAIR_HOST", "127.0.0.1"),
		pairPort:  port,
	}, nil
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

	// Perform the relay/1 connection handshake immediately after enrollment and
	// before the desired-state pull (REL-030): the app peer challenges, this
	// relay signs the nonce with its enrollment key (channel binding, REL-032),
	// declares its version/features/site/subnet/clock, and adopts the app peer's
	// authoritative site_binding from hello-ack (REL-036). The adopted site
	// drives the edge engine's schedule/sun evaluation once the automation stack
	// boots below. A refusal here — a channel binding the app peer will not
	// accept, or an unsupported protocol version — is fatal: the relay has no
	// authenticated connection to proceed on.
	site, err := helloWithRetry(cfg, relayIdent)
	if err != nil {
		log.Fatalf("waiveo-relay: hello: %v", err)
	}

	// Pull + verify the feeder's signed desired-state snapshot against the
	// trust anchor enrollment just persisted. A failure here (bad
	// signature, tampered sections, or a regressed generation) is fatal —
	// Wave-1 first-photon's relay has nothing useful to serve without a
	// verified desired-state generation applied.
	applied, err := desiredstate.Pull(cfg.feederURL, store)
	if err != nil {
		log.Fatalf("waiveo-relay: pull desired state: %v", err)
	}
	log.Printf("waiveo-relay applied desired state generation %d (screen %s, program %s, image %s)",
		applied.Generation, applied.ScreenID, applied.ProgramRevision, applied.Image.AssetRef)

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
	if err := bootAutomationStack(store, relayID, applied, site); err != nil {
		log.Fatalf("waiveo-relay: boot automation stack: %v", err)
	}

	cert, certDER, err := relayTLSCertificate(relayID)
	if err != nil {
		log.Fatalf("waiveo-relay: build TLS certificate from enrollment identity: %v", err)
	}

	pairingSrv, err := playerserver.NewServer(relayID.CertPEM, applied.PairingGrants)
	if err != nil {
		log.Fatalf("waiveo-relay: build player/1 pairing server: %v", err)
	}
	logPairingCodes(cfg, applied, certDER)

	// Task 10: configure program delivery (GET /player/v1/program) from the
	// SAME verified Applied value pairing already sourced its grants from —
	// Wave-1 first-photon carries exactly one content kind (image), so the
	// relay/1 -> player/1 `type` annotation (relay/1's own ContentRef has
	// no `type` field, player/1's Content reference requires one, PLY-083)
	// is a constant here, not a lookup. signingKey is the SAME enrollment
	// private key relayID.CertPEM certifies, so a player's PLY-090
	// signature check against its pinned trust anchor lines up with the
	// cert this listener actually presents.
	pairingSrv.SetProgram(applied.ProgramRevision, applied.Priority, applied.Display, []wire.LeaseContent{{
		Type:      "image",
		AssetRef:  applied.Image.AssetRef,
		URL:       applied.Image.URL,
		ExpiresAt: applied.Image.ExpiresAt,
	}}, relayID.PrivateKey)

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
	if _, err := registerClockHint(mux, certDER); err != nil {
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

// registerClockHint mounts the relay/1 clock.hint receiver (REL-133) on mux
// over a fresh, untrusted runtime clock (internal/relay/clocktrust), bounding
// accepted hints to this relay's own certificate not_after (parsed from
// certDER) plus clocktrust.DefaultBoundedGraceMs. It returns the receiver so
// the wiring is observable in tests. This is the connection-layer path that
// makes the runtime clock reachable end-to-end: a real clock.hint on the wire
// reaches AcceptHint here, never a direct call.
func registerClockHint(mux *http.ServeMux, certDER []byte) (*clocktrust.HintReceiver, error) {
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse enrollment cert for clock.hint bound: %w", err)
	}
	recv := clocktrust.NewHintReceiver(clocktrust.NewRuntimeClock(), leaf.NotAfter.UnixMilli(), clocktrust.DefaultBoundedGraceMs)
	mux.HandleFunc("/relay/v1/clock-hint", recv.ServeHTTP)
	return recv, nil
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
// edge_rules into it (REL-062), logging how many edge rules loaded. It wires a
// loopback device controller and a loopback entity resolver for now: the real
// ECP DeviceController and the real adopted-entity resolver (reading the device
// plane's own store) are hardware/data-model concerns that land later. The
// registry is the media-player FixtureRegistry for now — the real
// device-class-registry/1 content is a later data-model concern.
func bootAutomationStack(store *identity.Store, relayID identity.RelayIdentity, applied desiredstate.Applied, site hello.SiteBinding) error {
	host, err := automationhost.New(store, registry.FixtureRegistry{}, loopbackController{}, loopbackResolver, relayID.RelayID)
	if err != nil {
		return err
	}
	// Adopt the app peer's authoritative site_binding (REL-036) into the engine
	// before loading rules, so the edge engine's schedule/sun triggers evaluate
	// against the site's real timezone and coordinates from the first tick.
	if err := host.SetLocation(site.TZ, site.Lat, site.Long); err != nil {
		return fmt.Errorf("adopt site_binding tz %q into engine: %w", site.TZ, err)
	}
	if err := host.ApplyEdgeRules(applied.EdgeRules, int(applied.Generation)); err != nil {
		return err
	}
	log.Printf("waiveo-relay automation engine loaded: %d edge rule(s); device plane + durable telemetry ready", host.EdgeRuleCount())
	return nil
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
		ClockState:      hello.ClockState{State: "untrusted", Source: "none"},
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

// loopbackResolver is the Wave-1 stand-in entity resolver: it maps every
// entity_id to a single media-player loopback device so a loaded edge rule's
// device_command resolves against the fixture registry's vocabulary (REL-112/113).
// The real resolver reads the device plane's adopted-entity records (data-model/1)
// and is a later concern.
func loopbackResolver(entityID string) (deviceID, deviceClass string, ok bool) {
	return "01J8Z3K4N5P6Q7R8S9T0V1DEVA", "media-player", true
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"component": "waiveo-relay",
		"status":    "ok",
	})
}
