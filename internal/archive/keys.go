package archive

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// KDFAlgorithm is the only key-derivation algorithm this implementation
// accepts in a header's `kdf.algorithm` (ARC-010). The contract leaves the exact
// KDF a draft-note proposal; argon2id is that proposal, and this constant is
// where a future revision would change it. A container naming anything else is
// refused rather than guessed at — a wrong guess about how a key was stretched
// produces a wrong key, which surfaces as DECRYPT_FAILED and sends the operator
// hunting for a passphrase problem that does not exist.
const KDFAlgorithm = "argon2id"

const (
	// saltSize is the per-archive random KDF salt's length (ARC-010). 16 bytes
	// is argon2's own recommendation and is comfortably beyond birthday-collision
	// range across any plausible number of archives.
	saltSize = 16

	// rootKeySize is the export root key's length in bytes. 32 bytes feeds both
	// the HKDF expansions below at full strength.
	rootKeySize = 32

	// subKeySize is each derived sub-key's length: 32 bytes, the
	// XChaCha20-Poly1305 key size, and a sensible width for the data-key
	// wrapping key too.
	subKeySize = 32
)

// The two DISTINCT, FIXED context labels ARC-011 requires. Two sub-keys are
// derived from the one export root key under these labels so the same key
// material is never used for two different cryptographic purposes: reusing one
// key for both body encryption and data-key wrapping would mean a weakness found
// in either construction compromises the other, and would put the wrapping
// operation's output under the same key stream the body is encrypted with.
//
// These strings are WIRE-VISIBLE in effect even though they never appear on the
// wire: change one and every previously written container becomes undecryptable
// (body label) or its data key unrewrappable (wrap label). They are frozen.
const (
	labelBodyKey        = "waiveo-archive/1 body-encryption"
	labelDataKeyWrapKey = "waiveo-archive/1 data-key-wrap"
)

// KDFParams are the argon2id cost parameters recorded in the outer header
// (ARC-010). They travel WITH the container because a reader cannot repeat the
// derivation without them — and they are inside the signature's scope (ARC-021)
// because a reader that honored them before verifying would let an attacker
// dictate its memory and CPU allocation.
type KDFParams struct {
	// MemoryKiB is argon2id's memory cost, in kibibytes.
	MemoryKiB uint32
	// Iterations is argon2id's time cost (passes over memory).
	Iterations uint32
	// Parallelism is argon2id's lane count.
	Parallelism uint8
}

// DefaultKDFParams returns the production cost parameters: 256 MiB of memory,
// 3 passes, 4 lanes. These are deliberately EXPENSIVE — an export container is
// an offline artifact an attacker can grind against forever with no rate limit
// from us, so the passphrase's protection is exactly the cost of one guess.
// Tests override these with token values (see the test suite) purely for speed;
// nothing but a test should.
func DefaultKDFParams() KDFParams {
	return KDFParams{MemoryKiB: 256 * 1024, Iterations: 3, Parallelism: 4}
}

// orDefault resolves the zero value to DefaultKDFParams, so a caller that
// leaves Options.KDF unset gets production costs rather than a KDF with zero
// memory and zero iterations. Partial specification is NOT filled in field by
// field: a caller that sets some parameters has thought about them, and quietly
// completing the rest from a different profile would produce a cost mix nobody
// chose.
func (p KDFParams) orDefault() KDFParams {
	if p == (KDFParams{}) {
		return DefaultKDFParams()
	}
	return p
}

// validate rejects parameters argon2id itself cannot run with. argon2.IDKey
// panics on a zero lane count and misbehaves below its documented memory floor,
// and a panic deep inside a derivation is a far worse diagnostic than a refusal
// at the edge.
func (p KDFParams) validate() error {
	if p.Iterations == 0 {
		return fmt.Errorf("archive: kdf iterations must be >= 1")
	}
	if p.Parallelism == 0 {
		return fmt.Errorf("archive: kdf parallelism must be >= 1")
	}
	if p.MemoryKiB < 8*uint32(p.Parallelism) {
		return fmt.Errorf("archive: kdf memory_kib (%d) must be at least 8 KiB per lane (%d lanes)", p.MemoryKiB, p.Parallelism)
	}
	return nil
}

// Keys are the two sub-keys ARC-011 derives from one export root key.
type Keys struct {
	// Body encrypts the container body's frames (ARC-012/013).
	Body []byte
	// DataKeyWrap is the key `data_key_wrap` is wrapped under (ARC-071). This
	// package never uses it: it carries `data_key_wrap` as opaque bytes
	// (ARC-070/072) and has no business unwrapping a data key. It is derived and
	// returned so the restorer that DOES re-wrap the data key under the
	// destination's own hierarchy (ARC-073) gets it from this one derivation
	// rather than re-deriving it — and, more importantly, so the two-label
	// separation is realized in exactly one place.
	DataKeyWrap []byte
}

// DeriveKeys stretches passphrase into the export root key with argon2id
// (ARC-010) and expands that root into the two purpose-separated sub-keys
// (ARC-011).
//
// The split is argon2id-then-HKDF rather than two argon2id runs on purpose: the
// memory-hard stretch is the expensive part and buys the same protection once,
// while HKDF's Expand is what gives cryptographic domain separation between the
// two purposes. Running argon2id twice would double a 256 MiB derivation's cost
// for no additional security.
//
// The salt MUST be the per-archive random salt recorded in the header, and
// callers MUST NOT pass parameters read from an unverified header — ARC-021
// requires the header signature to verify first, and Open enforces that
// ordering before it reaches this function.
func DeriveKeys(passphrase string, p KDFParams, salt []byte) (Keys, error) {
	if err := p.validate(); err != nil {
		return Keys{}, err
	}
	if len(salt) == 0 {
		return Keys{}, fmt.Errorf("archive: kdf salt is empty")
	}

	root := argon2.IDKey([]byte(passphrase), salt, p.Iterations, p.MemoryKiB, p.Parallelism, rootKeySize)

	body, err := hkdf.Expand(sha256.New, root, labelBodyKey, subKeySize)
	if err != nil {
		return Keys{}, fmt.Errorf("archive: derive body key: %w", err)
	}
	wrap, err := hkdf.Expand(sha256.New, root, labelDataKeyWrapKey, subKeySize)
	if err != nil {
		return Keys{}, fmt.Errorf("archive: derive data-key-wrap key: %w", err)
	}

	return Keys{Body: body, DataKeyWrap: wrap}, nil
}
