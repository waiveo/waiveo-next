// Command waiveo-relay is the Wave-1 skeleton for the relay component: a Go
// process that will speak the relay/1 protocol (contracts/relay-1.md) to an
// app peer and the player over its own future channels. On start, it opens
// its persistent operational identity store (internal/relay/identity) and
// enrolls against the co-located feeder (internal/relay/enroll, relay/1
// REL-010–014) if it hasn't already — persisting the feeder's own
// desired-state signing key as its enrollment-anchored trust anchor
// (REL-071, `#28`). It otherwise only exposes a /healthz probe so the dev
// loop (`make dev`) has something real to build and run in place of the
// Wave-0 stub.
package main

import (
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

const addr = "127.0.0.1:7421"

// feederBaseURL is the co-located feeder's own HTTPS listener
// (cmd/waiveo-feeder's addr) — Wave-1 first-photon's loopback deployment
// (REL-011's co-located claim credential MAY leave app_endpoint implicit).
const feederBaseURL = "https://127.0.0.1:7420"

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
	store, err := identity.Open(identity.DefaultPath)
	if err != nil {
		log.Fatalf("waiveo-relay: open identity store: %v", err)
	}
	defer store.Close()

	if err := enrollWithRetry(store); err != nil {
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)

	log.Printf("waiveo-relay listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// enrollWithRetry calls enroll.Run against the co-located feeder, retrying
// on failure (e.g. the feeder's listener not up yet) until
// enrollRetryBudget elapses. enroll.Run itself is idempotent — a store that
// already holds a persisted identity returns immediately without a network
// call — so a retry here only ever costs real work on a genuinely fresh
// store.
func enrollWithRetry(store *identity.Store) error {
	deadline := time.Now().Add(enrollRetryBudget)
	var lastErr error
	for {
		if err := enroll.Run(feederBaseURL, store); err == nil {
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
