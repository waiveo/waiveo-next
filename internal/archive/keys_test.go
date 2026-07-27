package archive

import (
	"bytes"
	"testing"
)

// TestDefaultKDFParams pins the PRODUCTION cost profile. An export container is
// an offline artifact an attacker grinds against with no rate limit, so the
// passphrase's whole protection is the cost of one guess — a "harmless"
// reduction here would be a silent security regression that no functional test
// would ever catch, which is why the numbers are asserted rather than trusted.
func TestDefaultKDFParams(t *testing.T) {
	got := DefaultKDFParams()
	want := KDFParams{MemoryKiB: 262144, Iterations: 3, Parallelism: 4}
	if got != want {
		t.Fatalf("DefaultKDFParams() = %+v, want %+v (256 MiB / 3 passes / 4 lanes)", got, want)
	}
	if KDFAlgorithm != "argon2id" {
		t.Fatalf("KDFAlgorithm = %q, want %q — a memory-hard KDF is what ARC-010 requires", KDFAlgorithm, "argon2id")
	}
}

// TestKDFParamsOrDefault confirms the zero value resolves to the production
// profile — a caller that leaves Options.KDF unset must not get a KDF with zero
// memory and zero iterations — and that a partially specified profile is left
// exactly as its caller chose rather than half-filled from a different one.
func TestKDFParamsOrDefault(t *testing.T) {
	if got := (KDFParams{}).orDefault(); got != DefaultKDFParams() {
		t.Errorf("KDFParams{}.orDefault() = %+v, want %+v", got, DefaultKDFParams())
	}
	explicit := KDFParams{MemoryKiB: 8, Iterations: 1, Parallelism: 1}
	if got := explicit.orDefault(); got != explicit {
		t.Errorf("explicit.orDefault() = %+v, want it unchanged (%+v)", got, explicit)
	}
}

// TestKDFParamsValidate confirms a profile argon2id cannot run with is refused at
// the edge rather than panicking deep inside the derivation, where the
// diagnostic would be far worse.
func TestKDFParamsValidate(t *testing.T) {
	tests := map[string]struct {
		params  KDFParams
		wantErr bool
	}{
		"the production profile":     {DefaultKDFParams(), false},
		"the suite's light profile":  {KDFParams{MemoryKiB: 8, Iterations: 1, Parallelism: 1}, false},
		"zero iterations":            {KDFParams{MemoryKiB: 1024, Iterations: 0, Parallelism: 1}, true},
		"zero parallelism":           {KDFParams{MemoryKiB: 1024, Iterations: 1, Parallelism: 0}, true},
		"too little memory per lane": {KDFParams{MemoryKiB: 8, Iterations: 1, Parallelism: 4}, true},
		"zero everything":            {KDFParams{}, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := tc.params.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("KDFParams%+v.validate() = %v, wantErr = %v", tc.params, err, tc.wantErr)
			}
		})
	}
}

// TestDeriveKeysSubKeysAreSeparated is ARC-011: two independent sub-keys derived
// from the one export root key under DISTINCT fixed context labels, so the same
// key material is never used for two cryptographic purposes. The two sub-keys
// differing is the whole observable content of that requirement.
func TestDeriveKeysSubKeysAreSeparated(t *testing.T) {
	salt := bytes.Repeat([]byte{0x2A}, saltSize)

	keys, err := DeriveKeys(testPassphrase, lightKDF(), salt)
	if err != nil {
		t.Fatalf("DeriveKeys() = %v, want nil", err)
	}
	if len(keys.Body) != subKeySize {
		t.Errorf("body key is %d bytes, want %d", len(keys.Body), subKeySize)
	}
	if len(keys.DataKeyWrap) != subKeySize {
		t.Errorf("data-key-wrap key is %d bytes, want %d", len(keys.DataKeyWrap), subKeySize)
	}
	if bytes.Equal(keys.Body, keys.DataKeyWrap) {
		t.Fatal("the body key and the data-key-wrap key are identical — ARC-011 requires two independent sub-keys under distinct labels")
	}
	if labelBodyKey == labelDataKeyWrapKey {
		t.Fatal("the two context labels are the same string; ARC-011 requires them distinct")
	}
}

// TestDeriveKeysIsDeterministicAndSaltBound confirms the two properties a
// restore depends on: the same passphrase, parameters, and salt reproduce the
// same keys (or no container could ever be reopened), and a different salt or
// passphrase produces different ones (or ARC-010's per-archive salt would buy
// nothing).
func TestDeriveKeysIsDeterministicAndSaltBound(t *testing.T) {
	saltA := bytes.Repeat([]byte{0x2A}, saltSize)
	saltB := bytes.Repeat([]byte{0x2B}, saltSize)

	first, err := DeriveKeys(testPassphrase, lightKDF(), saltA)
	if err != nil {
		t.Fatalf("DeriveKeys() = %v", err)
	}
	again, err := DeriveKeys(testPassphrase, lightKDF(), saltA)
	if err != nil {
		t.Fatalf("DeriveKeys() = %v", err)
	}
	if !bytes.Equal(first.Body, again.Body) || !bytes.Equal(first.DataKeyWrap, again.DataKeyWrap) {
		t.Fatal("DeriveKeys() is not deterministic for the same passphrase, params, and salt")
	}

	otherSalt, err := DeriveKeys(testPassphrase, lightKDF(), saltB)
	if err != nil {
		t.Fatalf("DeriveKeys() = %v", err)
	}
	if bytes.Equal(first.Body, otherSalt.Body) {
		t.Error("a different salt produced the same body key (ARC-010)")
	}

	otherPass, err := DeriveKeys("a different export passphrase", lightKDF(), saltA)
	if err != nil {
		t.Fatalf("DeriveKeys() = %v", err)
	}
	if bytes.Equal(first.Body, otherPass.Body) {
		t.Error("a different passphrase produced the same body key")
	}
}

// TestDeriveKeysRejectsUnusableInput confirms the edge refusals: an empty salt
// means the header never recorded one (ARC-010), and an unrunnable cost profile
// is refused rather than passed to argon2id.
func TestDeriveKeysRejectsUnusableInput(t *testing.T) {
	if _, err := DeriveKeys(testPassphrase, lightKDF(), nil); err == nil {
		t.Error("DeriveKeys() with no salt = nil, want an error")
	}
	if _, err := DeriveKeys(testPassphrase, KDFParams{}, bytes.Repeat([]byte{1}, saltSize)); err == nil {
		t.Error("DeriveKeys() with a zero cost profile = nil, want an error")
	}
}

// TestDeriveFrameNonce pins the nonce derivation ARC-013 requires and ARC-017
// leans on: the per-archive base nonce combined with the frame's own sequence
// number, such that no nonce repeats under the same key.
//
// The scheme is an XOR of the big-endian sequence number over the last 8 bytes,
// which makes the derivation INJECTIVE in seq — distinct sequence numbers give
// distinct nonces with certainty, not merely with high probability. The cases
// below check exactly that: frame 0 is the base nonce, the leading 16 random
// bytes are never disturbed, and a large sample of sequence numbers collides
// nowhere.
func TestDeriveFrameNonce(t *testing.T) {
	base := make([]byte, 24)
	for i := range base {
		base[i] = byte(0xC0 + i)
	}

	t.Run("frame 0 is the base nonce", func(t *testing.T) {
		if got := deriveFrameNonce(base, 0); !bytes.Equal(got, base) {
			t.Fatalf("deriveFrameNonce(base, 0) = %x, want the base nonce %x", got, base)
		}
	})

	t.Run("the random prefix is preserved", func(t *testing.T) {
		got := deriveFrameNonce(base, 0xFFFFFFFFFFFFFFFF)
		if !bytes.Equal(got[:16], base[:16]) {
			t.Fatalf("deriveFrameNonce disturbed the leading 16 random bytes: %x, want %x", got[:16], base[:16])
		}
	})

	t.Run("the base nonce is not mutated", func(t *testing.T) {
		before := append([]byte{}, base...)
		deriveFrameNonce(base, 12345)
		if !bytes.Equal(base, before) {
			t.Fatalf("deriveFrameNonce mutated its base nonce argument: %x, want %x", base, before)
		}
	})

	t.Run("distinct sequence numbers give distinct nonces", func(t *testing.T) {
		seen := make(map[string]uint64, 4096)
		for _, seq := range sequenceSample() {
			key := string(deriveFrameNonce(base, seq))
			if prev, dup := seen[key]; dup {
				t.Fatalf("frames %d and %d derive the same nonce — a nonce reuse under one key", prev, seq)
			}
			seen[key] = seq
		}
	})
}

// sequenceSample is a spread of frame sequence numbers: every small one (the
// only ones a real archive reaches), plus boundary and high-bit values that
// would expose an off-by-one in the XOR window.
func sequenceSample() []uint64 {
	seen := make(map[uint64]bool, 4096)
	seqs := make([]uint64, 0, 4096)
	add := func(v uint64) {
		if !seen[v] {
			seen[v] = true
			seqs = append(seqs, v)
		}
	}
	for i := uint64(0); i < 4000; i++ {
		add(i)
	}
	for _, v := range []uint64{
		1 << 8, 1 << 16, 1 << 24, 1 << 32, 1 << 40, 1 << 48, 1 << 56, 1 << 63,
		0xFFFFFFFF, 0xFFFFFFFFFFFFFFFF, 0x0123456789ABCDEF,
	} {
		add(v)
	}
	return seqs
}

// TestFrameNonceLength confirms the derived nonce is XChaCha20-Poly1305's
// 24-byte width — the wide nonce that makes this derivation trivially
// collision-free, and the reason the contract proposes that cipher (ARC-013).
func TestFrameNonceLength(t *testing.T) {
	base := make([]byte, 24)
	if got := len(deriveFrameNonce(base, 7)); got != 24 {
		t.Fatalf("deriveFrameNonce returned %d bytes, want 24", got)
	}
}

// TestCanonicalSignedHeaderDropsSignature pins the signed scope ARC-021 defines:
// the entire cleartext header EXCEPT `signature`. Two headers differing only in
// their signature field must canonicalize identically (or verification could
// never reproduce the signed bytes), and a header differing anywhere else must
// not (or the signature would not actually cover that field).
func TestCanonicalSignedHeaderDropsSignature(t *testing.T) {
	const withSigA = `{"format":"waiveo-archive","archive_format_version":"1.0","digest":"ab","signature":"AAAA"}`
	const withSigB = `{"format":"waiveo-archive","archive_format_version":"1.0","digest":"ab","signature":"BBBB"}`
	const noSig = `{"format":"waiveo-archive","archive_format_version":"1.0","digest":"ab"}`
	const otherDigest = `{"format":"waiveo-archive","archive_format_version":"1.0","digest":"cd","signature":"AAAA"}`

	a, err := canonicalSignedHeader([]byte(withSigA))
	if err != nil {
		t.Fatalf("canonicalSignedHeader = %v", err)
	}
	b, err := canonicalSignedHeader([]byte(withSigB))
	if err != nil {
		t.Fatalf("canonicalSignedHeader = %v", err)
	}
	none, err := canonicalSignedHeader([]byte(noSig))
	if err != nil {
		t.Fatalf("canonicalSignedHeader = %v", err)
	}
	other, err := canonicalSignedHeader([]byte(otherDigest))
	if err != nil {
		t.Fatalf("canonicalSignedHeader = %v", err)
	}

	if !bytes.Equal(a, b) {
		t.Errorf("two headers differing only in `signature` canonicalized differently:\n%s\n%s", a, b)
	}
	if !bytes.Equal(a, none) {
		t.Errorf("`signature` is not being dropped from the signed scope:\n%s\n%s", a, none)
	}
	if bytes.Equal(a, other) {
		t.Error("two headers differing in `digest` canonicalized identically — the signature would not cover the digest")
	}
}

// TestCanonicalSignedHeaderIsKeyOrderIndependent confirms the canonicalization
// is a canonicalization: the same fields in a different textual order produce
// the same signed bytes. Without this, a header re-serialized anywhere along the
// way — by a proxy, a tool, a future writer — would stop verifying.
func TestCanonicalSignedHeaderIsKeyOrderIndependent(t *testing.T) {
	const orderA = `{"format":"waiveo-archive","kdf":{"algorithm":"argon2id","memory_kib":262144},"digest":"ab"}`
	const orderB = `{"digest":"ab","kdf":{"memory_kib":262144,"algorithm":"argon2id"},"format":"waiveo-archive"}`

	a, err := canonicalSignedHeader([]byte(orderA))
	if err != nil {
		t.Fatalf("canonicalSignedHeader = %v", err)
	}
	b, err := canonicalSignedHeader([]byte(orderB))
	if err != nil {
		t.Fatalf("canonicalSignedHeader = %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("key order changed the canonical form:\n%s\n%s", a, b)
	}
	// And numbers keep their literal form rather than round-tripping through a
	// float, which would turn 262144 into 262144e+05 or similar and break every
	// previously written container's signature.
	if !bytes.Contains(a, []byte("262144")) {
		t.Errorf("canonical form lost the integer literal: %s", a)
	}
}

// TestCanonicalSignedHeaderRejectsNonObjects confirms a header that is not a
// JSON object cannot be canonicalized into signed bytes — the failure Open turns
// into a refusal rather than a nil-map panic.
func TestCanonicalSignedHeaderRejectsNonObjects(t *testing.T) {
	for _, doc := range []string{`null`, `[]`, `"header"`, `{}{}`, `{`} {
		if _, err := canonicalSignedHeader([]byte(doc)); err == nil {
			t.Errorf("canonicalSignedHeader(%q) = nil error, want one", doc)
		}
	}
}
