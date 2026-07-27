// Package secretseal is this tree's one authenticated-encryption construction
// for a secret that must be RECOVERED rather than merely compared.
//
// It exists because the platform holds two structurally different kinds of
// secret and they need opposite primitives:
//
//   - A secret only ever COMPARED against a presented value — a password, a
//     bearer token — is hashed. Nothing ever needs the original back, so a KDF
//     (internal/app/auth's Argon2id) or a plain digest is both sufficient and
//     strictly safer: a database thief gets no plaintext at any price.
//   - A secret the server must READ to do its job — a TOTP shared secret, a
//     pack's stored credential stub — cannot be hashed, because verifying a
//     TOTP code means recomputing HMAC over that exact secret. The honest
//     protection is encryption under a key held somewhere the database is not,
//     which is what this package provides and what `security-model.md` SEC-040
//     designates the per-workspace data key for.
//
// The construction is XChaCha20-Poly1305 with a fresh random 24-byte nonce
// prefixed to the ciphertext, the whole rendered base64. XChaCha's extended
// nonce is the reason it is chosen over plain ChaCha20-Poly1305 or AES-GCM
// here: a 192-bit random nonce has no practical birthday bound, so a caller
// sealing an unbounded number of secrets under one key needs no counter, no
// per-key sequence, and no coordination between processes to stay safe. A
// 96-bit-nonce AEAD would need all three.
//
// Additional authenticated data is REQUIRED, not optional, on both Seal and
// Open. A sealed blob is a database column, and a database column can be COPIED
// — moving another principal's sealed TOTP secret onto your own row would
// otherwise let you authenticate as yourself with their authenticator. Binding
// the ciphertext to the row's own identity makes that copy fail to open rather
// than succeed quietly, so the AAD parameter is positional and unskippable.
package secretseal

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the sealing key's required length: 32 bytes, XChaCha20-Poly1305's
// key width and the width every other AEAD in this tree takes.
const KeySize = chacha20poly1305.KeySize

// ErrOpenFailed is what Open returns for any input it cannot authenticate: a
// corrupt blob, one sealed under a different key, one whose AAD does not match,
// or one that was tampered with. It is deliberately ONE error for all four —
// distinguishing them would tell an attacker which of their guesses was closest,
// and no caller has a different action for any of them.
var ErrOpenFailed = errors.New("secretseal: the sealed value did not open under this key and context")

// Sealer seals and opens secrets under one key. It is safe for concurrent use:
// the underlying AEAD is stateless and every Seal draws its own nonce.
type Sealer struct {
	aead interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
		NonceSize() int
		Overhead() int
	}
}

// New returns a Sealer over key, which MUST be exactly KeySize bytes.
//
// The key is expected to be a DERIVED sub-key under a purpose-specific context
// label — never a top-level key used directly for several purposes. That is
// SEC-044's no-cross-use rule applied one level down and the same technique
// `archive/1` ARC-011 uses for its own two export sub-keys.
func New(key []byte) (*Sealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretseal: key is %d bytes, want %d", len(key), KeySize)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("secretseal: init aead: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext under this Sealer's key, binding it to aad, and
// returns the base64 form a database column carries.
//
// aad MUST be non-empty — see the package doc for why binding is mandatory
// rather than a caller's option.
func (s *Sealer) Seal(plaintext, aad []byte) (string, error) {
	if s == nil || s.aead == nil {
		return "", errors.New("secretseal: nil sealer")
	}
	if len(aad) == 0 {
		return "", errors.New("secretseal: sealing requires non-empty context (aad) to bind the ciphertext to its row")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secretseal: read nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, plaintext, aad)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal, returning ErrOpenFailed for anything that does not
// authenticate under this key and this aad.
func (s *Sealer) Open(sealed string, aad []byte) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, errors.New("secretseal: nil sealer")
	}
	if len(aad) == 0 {
		return nil, errors.New("secretseal: opening requires the same non-empty context (aad) the value was sealed under")
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, ErrOpenFailed
	}
	n := s.aead.NonceSize()
	if len(raw) < n+s.aead.Overhead() {
		return nil, ErrOpenFailed
	}
	out, err := s.aead.Open(nil, raw[:n], raw[n:], aad)
	if err != nil {
		return nil, ErrOpenFailed
	}
	return out, nil
}
