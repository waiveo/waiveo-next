package hello

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// nonceBytes is 16 bytes (128 bits) — REL-030's minimum challenge nonce
// size.
const nonceBytes = 16

// NewChallenge mints a fresh Challenge carrying a freshly generated,
// crypto/rand-sourced nonce of at least 128 bits (REL-030), hex-encoded.
//
// Single-use enforcement per connection attempt is the caller's
// responsibility (the issuer tracks outstanding nonces per connection, per
// the package doc); NewChallenge's own job is only to mint a fresh,
// unpredictable value on every call.
func NewChallenge() (Challenge, error) {
	b := make([]byte, nonceBytes)
	if _, err := rand.Read(b); err != nil {
		return Challenge{}, fmt.Errorf("hello: NewChallenge: %w", err)
	}
	return Challenge{
		Type: "challenge",
		Body: ChallengeBody{Nonce: hex.EncodeToString(b)},
	}, nil
}
