package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
)

// helloRecoveryInterval is the POC cadence a relay whose boot-time hello was
// refused for a recoverable reason (helloRefusalIsRecoverable) retries the
// full handshake at, in the background, until the app peer accepts it.
// relay/1 defines no push/notification for "the app peer is ready to accept
// you now", so polling is the deliberate POC mechanism here too — mirroring
// rePullInterval's own POC polling for desired-state re-pull (livepull.go).
const helloRecoveryInterval = 5 * time.Second

// helloRefusalIsRecoverable reports whether err — a hello.PerformHello (or
// helloWithRetry) failure — is one relay/1's own Error taxonomy directs a
// relay to recover from by reconnecting and retrying the handshake, as
// opposed to one that needs an operator's intervention (a fresh claim
// credential or a software upgrade) and so is left permanent:
//
//   - A transport-level failure (err is not a decisive *hello.RefusedError at
//     all — the app peer unreachable, mid-restart, etc.) is always worth
//     retrying.
//   - CHANNEL_BINDING_INVALID and RELAY_IDENTITY_MISMATCH are the two typed
//     refusals the taxonomy itself annotates "reconnect and retry the
//     handshake" (contracts/relay-1.md, Error taxonomy) — CHANNEL_BINDING_INVALID
//     in particular is exactly the refusal a feeder restart's enrollment-
//     registry amnesia produces (internal/feeder/enroll.Server.EnablePersistence's
//     own doc), so once that peer-side amnesia clears (its own fix; or any
//     other transient cause of the same refusal clears), a later attempt is
//     expected to succeed.
//   - PROTOCOL_VERSION_UNSUPPORTED gets no such guidance from the taxonomy:
//     retrying against an unchanged software pairing would only repeat the
//     identical refusal forever, so that one is left exactly as permanent as
//     it is today, requiring an operator/software fix instead of a retry
//     loop.
func helloRefusalIsRecoverable(err error) bool {
	if err == nil {
		return false
	}
	var refused *hello.RefusedError
	if !errors.As(err, &refused) {
		return true
	}
	switch refused.Code {
	case "CHANNEL_BINDING_INVALID", "RELAY_IDENTITY_MISMATCH":
		return true
	default:
		return false
	}
}

// helloRecoverer retries the relay/1 connection handshake against the app
// peer (hello) once per tick until it is accepted, at which point onAccepted
// is invoked exactly once with the negotiated site_binding and the loop
// stops retrying. It never gives up on a recoverable refusal or a transport
// failure — see helloRefusalIsRecoverable's own doc for what "recoverable"
// excludes.
//
// This is what keeps a relay whose boot-time hello was refused
// CHANNEL_BINDING_INVALID from degrading permanently: absent this, the live
// desired-state loop simply never started for the rest of the process's
// life, and only an operator wiping the relay's identity dir and restarting
// it could bring it back online (the exact manual recovery the field defect
// this closes required).
type helloRecoverer struct {
	hello      func() (hello.SiteBinding, error) // one hello attempt against the app peer
	onAccepted func(hello.SiteBinding)           // invoked exactly once, when hello is finally accepted
}

// attempt performs one hello attempt, reporting whether the recovery loop
// should stop: true when hello was just accepted (onAccepted has already run)
// or when the refusal is not recoverable (further attempts cannot change the
// outcome, helloRefusalIsRecoverable); false when it should keep retrying on
// the next tick.
func (r *helloRecoverer) attempt() bool {
	site, err := r.hello()
	if err != nil {
		if !helloRefusalIsRecoverable(err) {
			log.Printf("waiveo-relay: hello recovery stopping — %v is not a recoverable refusal (relay/1 Error taxonomy); an operator must intervene", err)
			return true
		}
		log.Printf("waiveo-relay: hello recovery attempt failed (%v); retrying", err)
		return false
	}
	log.Printf("waiveo-relay: hello recovery succeeded — the app peer accepted this relay again")
	r.onAccepted(site)
	return true
}

// helloRecoveryLoop drives r.attempt once per tick delivered on ticks — a
// time.Ticker's channel in the binary, a manual channel in tests, so nothing
// here sleeps on the wall clock (mirroring rePullLoop's own shape,
// livepull.go). It returns when ctx is cancelled, ticks is closed, or
// r.attempt reports done (accepted, or a non-recoverable refusal).
func helloRecoveryLoop(ctx context.Context, ticks <-chan time.Time, r *helloRecoverer) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if r.attempt() {
				return
			}
		}
	}
}
