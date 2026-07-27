package reenroll

// This file holds the relay (client) side of PROACTIVE, ahead-of-expiry
// certificate renewal (REL-015): the path a healthy relay drives on the
// reconnect supervisor's cadence once its leaf enters the renewal window,
// as opposed to client.go's ReEnroll — the after-the-fact recovery for a
// leaf that already expired during a long power-off (REL-020).
//
// Until the in-band `renew`/`renew-ack` verb lands on the authenticated
// connection (REL-015's own transport; an additive REL-004 minor), renewal
// rides the SAME bootstrap exchange re-enrollment does — the contract's
// REL-015 draft-note blesses exactly this: the app peer's eligibility check
// is issuance-record-based (REL-021), never expiry-gated, so a
// most-recently-issued, unrevoked certificate renews identically before or
// after its not_after.
//
// Renew deliberately REUSES the enrollment keypair where ReEnroll rotates
// it: the enrollment key doubles as the player/1-facing trust anchor —
// paired players pin the leaf's SubjectPublicKeyInfo via the pairing code's
// fingerprint_commitment (REL-126/PLY-052, tlsboot.Commitment is computed
// over the SPKI) and verify every lease signature against that same key
// (PLY-090) — so rotating it on a routine renewal would strand every paired
// player after the relay's next restart. Issuing a fresh certificate over
// the SAME key keeps the SPKI byte-identical: commitments, lease
// verification, and the player TLS pin all survive. REL-015 mandates no
// rotation; possession is proven by the current leaf's own key either way.

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// parseCertPEM decodes and parses a PEM-encoded certificate — the shared
// parse ExpiresWithin (and, via internal/relay/enroll's certExpired, the
// boot-time expiry check) evaluates NotAfter from.
func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("certificate did not decode to a CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

// ExpiresWithin reports whether certPEM's NotAfter falls at or within window
// of now — the certificate is already expired, or will expire in less than
// window. window=0 degenerates to the plain "already expired" check. A
// certPEM that does not decode/parse is an error (a corrupt store), never
// silently treated as "not due": the caller decides how loudly to fail, but
// a corrupt certificate must never quietly suppress renewal.
//
// This is the relay-side proactive-renewal predicate's core (REL-015): the
// caller supplies now from a clock a backwards jump cannot regress (the
// persisted clock floor, REL-130/132 — never a bare wall reading), so a
// wrong relay clock can delay renewal at most until the floor catches up,
// and the window is chosen wide enough that a failed attempt has thousands
// of retry opportunities before hard expiry.
func ExpiresWithin(certPEM []byte, now time.Time, window time.Duration) (bool, error) {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return false, fmt.Errorf("reenroll: ExpiresWithin: %w", err)
	}
	return !now.Before(cert.NotAfter.Add(-window)), nil
}

// Renew drives one proactive certificate renewal (REL-015) for the relay
// whose identity store holds a valid, most-recently-issued certificate,
// against the feeder at feederBaseURL. It presents the current certificate
// over the bootstrap exchange (package doc: the REL-015 draft-note's blessed
// transport until the in-band verb lands), proves possession with that
// certificate's OWN private key over the challenge nonce bound to a fresh
// CSR (REL-026/027) — a CSR over the SAME keypair, never a rotated one (see
// package doc) — and, on success, persists the fresh certificate under the
// SAME relay_id (REL-014) and re-anchors the desired-state verification key
// delivered in the ack.
//
// Per REL-015's cutover sentence, a successful Renew changes only the
// PERSISTED identity: the fresh leaf takes effect at the next connection
// attempt (relayconn.Dial re-reads the store on every dial); an established
// connection is never torn down or re-keyed here, and the superseded
// certificate is not revoked. On ANY error the persisted identity is left
// untouched (never-wipe, REL-142) — the current leaf may still be valid,
// and the renewal window leaves the caller's cadence plenty of retries.
//
// A typed refusal from the app peer (CERT_EXPIRED_INELIGIBLE,
// RE_ENROLL_POP_INVALID, RE_ENROLL_RATE_LIMITED) is returned as a
// *ReEnrollError, so a caller can branch on errors.As.
func Renew(feederBaseURL string, store *identity.Store) error {
	if store == nil {
		return errors.New("reenroll: Renew: store must not be nil")
	}

	id, ok, err := store.Identity()
	if err != nil {
		return fmt.Errorf("reenroll: Renew: read persisted identity: %w", err)
	}
	if !ok {
		return errors.New("reenroll: Renew: no persisted identity to renew")
	}

	// The fresh CSR is over the CURRENT keypair — key reuse is the point
	// (package doc); only the certificate is renewed.
	csrPEM, csrDER, err := buildCSR(id.PrivateKey, id.RelayID)
	if err != nil {
		return fmt.Errorf("reenroll: Renew: build CSR: %w", err)
	}

	client := bootstrapClient()

	nonce, err := fetchChallenge(client, feederBaseURL)
	if err != nil {
		return fmt.Errorf("reenroll: Renew: fetch challenge: %w", err)
	}

	// Proof of possession (REL-027): the presented certificate's own private
	// key over the nonce bound to this CSR. With key reuse, the "presented
	// certificate's key" and the CSR's key are one and the same.
	popSig := SignPoP(id.PrivateKey, nonce, csrDER)

	ack, err := postRenew(client, feederBaseURL, RenewRequest{
		Type:    "renew",
		ID:      ulid.New(),
		RelayID: id.RelayID,
		Body: RenewBody{
			PresentedCert: string(id.CertPEM),
			CSR:           csrPEM,
			PoPSignature:  popSig,
		},
	})
	if err != nil {
		return err
	}

	verKey, err := decodeVerificationKey(ack.Body.DesiredStateVerificationKey)
	if err != nil {
		return fmt.Errorf("reenroll: Renew: %w", err)
	}

	// Persist the fresh certificate under the SAME relay_id and keypair
	// (REL-014), then re-anchor the verification key — identity first, the
	// same crash-ordering ReEnroll uses (client.go): a crash between the two
	// leaves a usable cert to retry re-anchoring against.
	if err := store.SetIdentity(id.RelayID, []byte(ack.Body.Cert), id.PrivateKey); err != nil {
		return fmt.Errorf("reenroll: Renew: persist renewed identity: %w", err)
	}
	if err := store.SetDesiredStateVerificationKey(verKey); err != nil {
		return fmt.Errorf("reenroll: Renew: re-anchor desired_state_verification_key: %w", err)
	}
	return nil
}
