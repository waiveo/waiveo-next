package hello

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// channelBindingSigPrefix is the on-wire grammar for HelloBody's
// channel_binding_signature (REL-032): "ed25519-sig:" + hex(raw ed25519
// signature bytes) — the exact grammar contracts/relay-1.md's own worked
// Hello example uses. Make this encoding exactly once here; both the signing
// side (a relay, via SignChannelBinding) and the verifying side (the app
// peer, via VerifyChannelBinding) must agree on it byte-for-byte.
const channelBindingSigPrefix = "ed25519-sig:"

// ErrChannelBindingInvalid is returned by VerifyChannelBinding whenever a
// hello's channel_binding_signature does not prove possession of the
// relay's enrollment private key over the challenge nonce (REL-032):
// absent, empty, malformed, or cryptographically wrong all collapse to this
// one typed refusal (CHANNEL_BINDING_INVALID, Error taxonomy). Callers MUST
// refuse the connection on any non-nil error from VerifyChannelBinding
// regardless of whether the connection's own mutual-TLS handshake already
// succeeded — transport-level authentication alone is never sufficient
// proof of peer identity (REL-032).
var ErrChannelBindingInvalid = errors.New("hello: channel_binding_signature invalid (CHANNEL_BINDING_INVALID)")

// SignChannelBinding computes a hello's channel_binding_signature (REL-032):
// an ed25519 signature, computed with the relay's own enrollment private
// key, over the raw bytes of the hex-encoded challenge nonce the app peer
// supplied in `challenge` (REL-030). nonceHex is the exact string carried in
// ChallengeBody.Nonce for this connection attempt.
func SignChannelBinding(priv ed25519.PrivateKey, nonceHex string) (string, error) {
	nonce, err := hex.DecodeString(nonceHex)
	if err != nil {
		return "", fmt.Errorf("hello: SignChannelBinding: nonce %q is not valid hex: %w", nonceHex, err)
	}
	sig := signhash.Sign(priv, nonce)
	return channelBindingSigPrefix + hex.EncodeToString(sig), nil
}

// VerifyChannelBinding verifies a hello's channel_binding_signature against
// pub — the public key the app peer learned for this relay at enrollment
// (never a key asserted by the connection itself) — over nonceHex, the
// exact challenge nonce the app peer issued on this connection attempt
// (REL-032).
//
// It returns nil only when signature is well-formed
// ("ed25519-sig:"+hex(sig)) and verifies against pub over the decoded
// nonce bytes. Every other case — an absent/empty signature, a malformed
// prefix or hex body, undecodable nonceHex, or a well-formed signature that
// simply does not verify (wrong key, wrong nonce, tampered bytes) — returns
// ErrChannelBindingInvalid, never a panic and never a different error, so a
// caller can refuse uniformly (CHANNEL_BINDING_INVALID) on any non-nil
// return. This holds even when pub is not a valid ed25519 public key size:
// signhash.Verify fails closed rather than panicking on a malformed key,
// exactly as it must here since pub ultimately derives from a wire-learned
// value at enrollment time.
func VerifyChannelBinding(pub ed25519.PublicKey, nonceHex, signature string) error {
	sigHex, ok := strings.CutPrefix(signature, channelBindingSigPrefix)
	if !ok || sigHex == "" {
		return ErrChannelBindingInvalid
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return ErrChannelBindingInvalid
	}
	nonce, err := hex.DecodeString(nonceHex)
	if err != nil {
		return ErrChannelBindingInvalid
	}
	if !signhash.Verify(pub, nonce, sig) {
		return ErrChannelBindingInvalid
	}
	return nil
}
