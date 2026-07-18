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
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
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

	if id, ok, err := store.Identity(); err != nil {
		log.Fatalf("waiveo-relay: read identity after enroll: %v", err)
	} else if ok {
		log.Printf("waiveo-relay enrolled (relay_id %s)", id.RelayID)
	}
	if key, ok, err := store.DesiredStateVerificationKey(); err != nil {
		log.Fatalf("waiveo-relay: read desired_state_verification_key after enroll: %v", err)
	} else if ok {
		log.Printf("waiveo-relay trust anchor learned (desired_state_verification_key %s)", hex.EncodeToString(key))
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

	relayID, ok, err := store.Identity()
	if err != nil {
		log.Fatalf("waiveo-relay: read identity for TLS certificate: %v", err)
	}
	if !ok {
		log.Fatalf("waiveo-relay: no persisted identity after enrollment — cannot serve player/1")
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
// enrollRetryBudget elapses. enroll.Run itself is idempotent — a store that
// already holds a persisted identity returns immediately without a network
// call — so a retry here only ever costs real work on a genuinely fresh
// store.
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

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"component": "waiveo-relay",
		"status":    "ok",
	})
}
