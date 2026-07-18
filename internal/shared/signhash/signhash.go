// Package signhash holds the ed25519 signing and sha256 content-addressing
// primitives shared by the feeder (cmd/waiveo-feeder) and the relay
// (cmd/waiveo-relay): the feeder signs desired-state snapshots and leases
// with an ed25519 key, the relay verifies them, and content throughout the
// system is addressed by sha256.
//
// This package is deliberately small and stdlib-only (crypto/ed25519,
// crypto/sha256, encoding/hex) — no protocol behavior (envelope framing,
// handshake sequencing, key distribution) lives here, only the primitives.
package signhash

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// GenerateKey returns a new ed25519 key pair, generated with crypto/rand.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		// crypto/rand.Reader failing is a fatal environment problem
		// (entropy source unavailable); ed25519.GenerateKey has no
		// other failure mode, so there is no meaningful error to
		// propagate to callers.
		panic("signhash: GenerateKey: " + err.Error())
	}
	return pub, priv
}

// Sign returns the ed25519 signature of msg under priv.
func Sign(priv ed25519.PrivateKey, msg []byte) []byte {
	return ed25519.Sign(priv, msg)
}

// Verify reports whether sig is a valid ed25519 signature of msg under pub.
// It returns false (never panics) when pub is not exactly
// ed25519.PublicKeySize bytes — crypto/ed25519.Verify panics on a
// wrong-length key, and callers here include the relay verifying an
// untrusted, wire-decoded `desired_state_verification_key` (relay/1
// REL-012, REL-071), which must fail closed rather than crash the process.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// ContentID returns the sha256 content address of b in the exact
// "sha256:<lowercase-hex>" form the asset_ref grammar uses (player/1
// PLY-083, data-model/1 DAT-041).
func ContentID(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
