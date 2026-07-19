package hello

import (
	"encoding/hex"
	"testing"
)

// TestNewChallengeNonceIsAtLeast128Bits asserts NewChallenge mints a
// hex-encoded nonce decoding to at least 16 bytes (128 bits, REL-030's
// floor).
func TestNewChallengeNonceIsAtLeast128Bits(t *testing.T) {
	c, err := NewChallenge()
	if err != nil {
		t.Fatalf("NewChallenge: %v", err)
	}

	if c.Type != "challenge" {
		t.Errorf("Type = %q, want %q", c.Type, "challenge")
	}

	raw, err := hex.DecodeString(c.Body.Nonce)
	if err != nil {
		t.Fatalf("Body.Nonce %q is not valid hex: %v", c.Body.Nonce, err)
	}
	if len(raw) < 16 {
		t.Errorf("Body.Nonce decoded to %d bytes (%d bits), want >= 16 bytes (128 bits)", len(raw), len(raw)*8)
	}
}

// TestNewChallengeNoncesAreDistinct asserts two calls mint different nonces
// (REL-030: a fresh nonce unique to that connection attempt) — a
// crypto/rand source that were broken/fixed would otherwise go unnoticed.
func TestNewChallengeNoncesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		c, err := NewChallenge()
		if err != nil {
			t.Fatalf("NewChallenge: %v", err)
		}
		if seen[c.Body.Nonce] {
			t.Fatalf("NewChallenge produced a repeated nonce %q across %d calls", c.Body.Nonce, i+1)
		}
		seen[c.Body.Nonce] = true
	}
}
