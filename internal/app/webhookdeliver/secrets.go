package webhookdeliver

import (
	"errors"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// secrets.go is the at-rest protection for a webhook endpoint's signing secret.
//
// The secret is RECOVERABLE, not comparable: the platform is the signer, so it
// must read the exact bytes back to compute the HMAC EVT-151 defines. That rules
// out hashing and puts it under internal/shared/secretseal, keyed from the
// per-workspace data key's secret-seal sub-key — the same shape and the same
// key the TOTP shared secret uses, for the same reason.
//
// The AAD binds each blob to the endpoint id AND to which of the two slots it
// occupies. Two different contexts rather than one is deliberate, exactly as the
// TOTP secret uses different contexts for its pending and armed lives: a sealed
// blob copied from one endpoint's row to another's fails to open, and a prior
// blob lifted into the current slot fails to open too, so the only way a secret
// becomes the CURRENT secret is through a real rotation.

// Sealer is the authenticated-encryption seam, satisfied by
// *secretseal.Sealer. It is an interface so this package depends on the
// construction rather than on the workspace key's plumbing — and so a test
// binds the real sealer over a fixed key rather than a no-op double.
type Sealer interface {
	Seal(plaintext, aad []byte) (string, error)
	Open(sealed string, aad []byte) ([]byte, error)
}

// ErrNoSealer is returned by every path that would have to seal or open a
// signing secret without a sealer. It is a hard refusal, never a fallback to
// storing the secret in the clear.
var ErrNoSealer = errors.New("webhookdeliver: no secret sealer is configured, so a webhook signing secret can be neither stored nor read")

// Secrets seals and opens endpoint signing secrets.
type Secrets struct {
	sealer Sealer
}

// NewSecrets returns a Secrets over sealer. A nil sealer is accepted here and
// refused at every use, so a wiring mistake surfaces as a refusal at the moment
// a secret would have been written rather than as a silently unsealed column.
func NewSecrets(sealer Sealer) *Secrets { return &Secrets{sealer: sealer} }

// currentAAD / priorAAD are the contexts a signing secret is sealed under in
// each of its two lives.
func currentAAD(endpointID string) []byte { return []byte("webhook-secret:" + endpointID) }
func priorAAD(endpointID string) []byte   { return []byte("webhook-prior-secret:" + endpointID) }

// Seal seals a freshly supplied signing secret for endpointID, ready to be
// installed as the current secret.
func (s *Secrets) Seal(endpointID string, secret []byte) (string, error) {
	if s == nil || s.sealer == nil {
		return "", ErrNoSealer
	}
	if endpointID == "" {
		return "", errors.New("webhookdeliver: sealing a signing secret needs the endpoint id it binds to")
	}
	if len(secret) == 0 {
		return "", errors.New("webhookdeliver: refusing to seal an empty signing secret")
	}
	sealed, err := s.sealer.Seal(secret, currentAAD(endpointID))
	if err != nil {
		// The plaintext is not echoed — it is credential material.
		return "", fmt.Errorf("webhookdeliver: seal this endpoint's signing secret: %w", err)
	}
	return sealed, nil
}

// Reseal converts a blob sealed under the CURRENT context into one sealed under
// the PRIOR context — the one legitimate promotion path between the two slots.
//
// It exists because the store demotes the current column into the prior column
// on rotation, and a blob whose AAD still says "current" would then never open
// again. Doing the re-seal here, above the store, is what keeps the store
// holding opaque bytes and holding no key.
func (s *Secrets) Reseal(endpointID, sealedCurrent string) (string, error) {
	if s == nil || s.sealer == nil {
		return "", ErrNoSealer
	}
	if sealedCurrent == "" {
		return "", nil
	}
	secret, err := s.sealer.Open(sealedCurrent, currentAAD(endpointID))
	if err != nil {
		return "", errors.New("webhookdeliver: this endpoint's current signing secret did not open under the workspace's key")
	}
	sealed, err := s.sealer.Seal(secret, priorAAD(endpointID))
	if err != nil {
		return "", fmt.Errorf("webhookdeliver: re-seal the superseded signing secret: %w", err)
	}
	return sealed, nil
}

// Open returns the endpoint's current and prior signing secrets in plaintext.
//
// The returned strings are live credential material: callers hand them straight
// to the signer and drop them. Nothing here caches them, and nothing anywhere
// may log them, wrap them into an error string, or return them over the wire.
// An empty current secret means the endpoint has none yet — not an error, just
// an endpoint nothing can be signed for.
func (s *Secrets) Open(endpointID string, st store.WebhookDeliveryState) (current, prior string, err error) {
	if s == nil || s.sealer == nil {
		return "", "", ErrNoSealer
	}
	if st.SealedSecret == "" {
		return "", "", nil
	}
	cur, err := s.sealer.Open(st.SealedSecret, currentAAD(endpointID))
	if err != nil {
		return "", "", errors.New("webhookdeliver: this endpoint's signing secret did not open under the workspace's key")
	}
	if st.SealedPriorSecret == "" {
		return string(cur), "", nil
	}
	pri, err := s.sealer.Open(st.SealedPriorSecret, priorAAD(endpointID))
	if err != nil {
		// A prior secret that will not open is not a reason to stop delivering:
		// the CURRENT secret is what every delivery is signed with, and the prior
		// one only widens what a receiver may still accept. Report the loss of
		// the overlap by returning no prior secret rather than refusing the
		// endpoint outright.
		return string(cur), "", nil
	}
	return string(cur), string(pri), nil
}
