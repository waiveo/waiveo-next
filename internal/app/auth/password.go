package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id is the platform's password KDF. It is the same primitive the
// `archive/1` export/emergency-kit key derivation already commits this codebase
// to, so a deployment carries one memory-hard KDF rather than two.
//
// The parameters below are the OWASP-recommended second-choice profile
// (64 MiB / t=3 / p=4) — deliberately memory-heavy rather than iteration-heavy,
// which is what makes a GPU or ASIC attacker pay. A login is a rare, interactive
// operation on an appliance-class box, so ~50ms and 64 MiB per verification is
// affordable; the box never verifies passwords in bulk.
//
// The parameters are stored ALONGSIDE each hash in the PHC string (encodeHash),
// never assumed from these constants at verification time, so raising them later
// re-hashes lazily on next login instead of invalidating every stored credential.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// argonAlgorithm is the PHC identifier this package writes and is the only one
// it accepts: argon2id specifically, never argon2i or argon2d.
const argonAlgorithm = "argon2id"

// ErrPasswordMismatch is returned by VerifyPassword when the presented password
// does not match the stored hash. It is deliberately indistinguishable, to a
// caller, from "no such credential" at the login handler level — the handler
// maps both onto one generic failure, and reaches this comparison on BOTH paths
// (an unknown identifier is verified against DummyPasswordHash), so neither the
// response body nor the elapsed time can be used to enumerate which identifiers
// exist. See the login handler's own doc for the measurement.
var ErrPasswordMismatch = errors.New("auth: password does not match")

// DummyPasswordHash is the PHC-encoded Argon2id hash the login path verifies a
// presented password against when the presented IDENTIFIER resolves to no
// credential at all. Its whole purpose is that the KDF runs either way: without
// it the unknown-identifier path returns before Argon2id and answers three
// orders of magnitude sooner than a wrong password does, which enumerates
// identifiers over the network just as surely as a distinguishable response body
// would.
//
// Two properties make it a faithful stand-in rather than a token gesture:
//
//   - It carries THIS BUILD's parameters, the same ones HashPassword writes, so
//     the dummy verification does the same memory-hard work a real one does. A
//     cheaper dummy would reintroduce exactly the gap it exists to close, which
//     is why the timing test compares its decoded parameters against a really
//     hashed credential's rather than trusting this comment.
//   - Its key is 32 fresh crypto/rand bytes rather than the hash of some fixed
//     password. No password verifies against it — not by policy, but because
//     nothing derives to those bytes — so it cannot be turned into a usable
//     credential by any later coding error, and constructing it costs no KDF
//     work at process start.
//
// A hash whose parameters were read from a STORED credential would be the only
// way to also match a credential hashed under superseded parameters (the PHC
// string carries its own, so an old hash verifies at its own cost). Reading one
// would mean picking some real credential to imitate, which is a lookup an
// unknown identifier must not perform; this build's parameters are what a
// credential hashed today costs, which is the case that matters.
var DummyPasswordHash = newDummyPasswordHash()

func newDummyPasswordHash() string {
	salt := make([]byte, argonSaltLen)
	key := make([]byte, argonKeyLen)
	if _, err := rand.Read(salt); err != nil {
		panic("auth: read dummy password salt: " + err.Error())
	}
	if _, err := rand.Read(key); err != nil {
		panic("auth: read dummy password key: " + err.Error())
	}
	return encodeHash(salt, key, argonMemory, argonTime, argonThreads)
}

// HashPassword derives an Argon2id hash of password and returns it in the
// standard PHC string format:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<b64-salt>$<b64-hash>
//
// The salt is 16 fresh crypto/rand bytes per call, so two principals sharing a
// password never share a hash. The encoded string carries its own parameters, so
// VerifyPassword re-derives with the parameters the hash was MADE with rather
// than whatever this build's constants currently say.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return encodeHash(salt, key, argonMemory, argonTime, argonThreads), nil
}

// VerifyPassword reports whether password matches the PHC-encoded Argon2id hash
// in encoded, returning ErrPasswordMismatch when it does not and a distinct
// error when encoded itself is malformed (a corrupt row, not a wrong password).
// The comparison is constant-time, so the response time carries no information
// about how many leading bytes matched.
func VerifyPassword(encoded, password string) error {
	salt, want, memory, time, threads, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// encodeHash renders salt/key and the parameters they were derived under as the
// PHC string format Argon2's own reference implementation defines.
func encodeHash(salt, key []byte, memory, time uint32, threads uint8) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonAlgorithm, argon2.Version, memory, time, threads,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decodeHash parses a PHC string back into the salt, the stored key, and the
// parameters the key was derived under. It refuses any algorithm other than
// argon2id and any version other than the one this build's argon2 package
// implements, so a hash this package cannot faithfully reproduce is a hard
// error rather than a silent mismatch reported as a wrong password.
func decodeHash(encoded string) (salt, key []byte, memory, time uint32, threads uint8, err error) {
	parts := strings.Split(encoded, "$")
	// "$argon2id$v=19$m=...,t=...,p=...$salt$key" splits into 6 fields, the
	// first of which is the empty string before the leading '$'.
	if len(parts) != 6 || parts[0] != "" {
		return nil, nil, 0, 0, 0, fmt.Errorf("auth: malformed password hash (%d fields)", len(parts))
	}
	if parts[1] != argonAlgorithm {
		return nil, nil, 0, 0, 0, fmt.Errorf("auth: password hash algorithm %q is not %s", parts[1], argonAlgorithm)
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("auth: malformed password hash version: %w", err)
	}
	if version != argon2.Version {
		return nil, nil, 0, 0, 0, fmt.Errorf("auth: password hash version %d is not %d", version, argon2.Version)
	}
	m, t, p, err := parseParams(parts[3])
	if err != nil {
		return nil, nil, 0, 0, 0, err
	}
	b64 := base64.RawStdEncoding
	salt, err = b64.DecodeString(parts[4])
	if err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("auth: decode password salt: %w", err)
	}
	key, err = b64.DecodeString(parts[5])
	if err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("auth: decode password hash: %w", err)
	}
	if len(salt) == 0 || len(key) == 0 {
		return nil, nil, 0, 0, 0, errors.New("auth: password hash carries an empty salt or key")
	}
	return salt, key, m, t, p, nil
}

// parseParams reads the "m=<mem>,t=<time>,p=<threads>" field of a PHC string.
// It is written by hand rather than with Sscanf so a field in the wrong order,
// or an extra field, is rejected instead of partially absorbed.
func parseParams(field string) (memory, time uint32, threads uint8, err error) {
	kv := strings.Split(field, ",")
	if len(kv) != 3 {
		return 0, 0, 0, fmt.Errorf("auth: malformed password hash parameters %q", field)
	}
	var m, t, p uint64
	for _, pair := range kv {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return 0, 0, 0, fmt.Errorf("auth: malformed password hash parameter %q", pair)
		}
		n, perr := strconv.ParseUint(value, 10, 32)
		if perr != nil {
			return 0, 0, 0, fmt.Errorf("auth: password hash parameter %q: %w", name, perr)
		}
		switch name {
		case "m":
			m = n
		case "t":
			t = n
		case "p":
			p = n
		default:
			return 0, 0, 0, fmt.Errorf("auth: unknown password hash parameter %q", name)
		}
	}
	if m == 0 || t == 0 || p == 0 || p > 255 {
		return 0, 0, 0, fmt.Errorf("auth: out-of-range password hash parameters %q", field)
	}
	return uint32(m), uint32(t), uint8(p), nil
}
