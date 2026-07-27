// Package workspacekey holds the workspace signing key `security-model.md`
// SEC-046 defines: the per-workspace asymmetric keypair whose private half is
// "the sole key material used to produce the signature `archive/1` ARC-021
// requires over an export's outer header", and whose public half is what an
// archive's `signer_key_id` resolves against.
//
// It is its own package, and its own key file, for the reason SEC-044 states
// outright: the platform operates three distinct, unrelated trust roots, and "a
// conformant implementation MUST NOT cross-use key material between them." The
// feeder's relay/1 desired-state signing key (internal/feeder/signing) is root
// (3); `channel-index/1`'s artifact bundle is root (2); this key belongs to root
// (1), the wrapped-secret hierarchy. Reusing the feeder's key to sign an archive
// would be exactly the cross-use that rule forbids, so this package deliberately
// does not import it, and the two persist to different files.
//
// # What this package does NOT implement, and why that is stated rather than
// # implied
//
// SEC-047 requires the private half be held "in the same root-owned keyfile as
// the box key (SEC-041), whether as an independently persisted secret or
// deterministically derived from the box key under a fixed, distinct context
// label." No BOX key exists in this tree — SEC-041's root-owned keyfile and
// SEC-042's tenant KMS are both unimplemented — so this package takes SEC-047's
// FIRST option: an independently persisted secret, in its own 0600 file under a
// 0700 directory. When the box key lands, the correct move is to derive both
// keys from it under distinct context labels and delete the standalone files;
// the persisted-secret form is a way station, not a competing design.
//
// The DATA key (SEC-040) is here too, established in the same call, because
// SEC-046 requires the signing key be "established at the same time as the
// workspace's data key" and because `archive/1` ARC-071 makes an export
// impossible without one to re-wrap. What is NOT implemented is everything the
// data key is supposed to protect: no secret stub exists in this tree, so the
// key today wraps nothing but itself into an export. That is a gap in the
// secrets surface, not in this key's custody.
package workspacekey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

const (
	privateKeyFile = "workspace_signing_key.pem" // ed25519 private half, PKCS8, 0600
	keyIDFile      = "workspace_signing_key_id"  // the ULID `signer_key_id` carries
	dataKeyFile    = "workspace_data_key"        // the per-workspace data key (SEC-040), 0600
)

// dataKeySize is the per-workspace data key's length: 32 bytes, the width every
// AEAD in this tree takes and the width `archive/1`'s own data-key-wrapping
// sub-key is derived at (ARC-011).
const dataKeySize = 32

// Key is a workspace signing key loaded from (or freshly established in) a
// directory: the ed25519 private half an export signs with, and the stable
// identifier an archive's `signer_key_id` field records so a reader knows which
// public half to resolve the signature against (ARC-002/021, SEC-046).
type Key struct {
	priv  ed25519.PrivateKey
	keyID string
	// data is the per-workspace DATA key (SEC-040): the key that wraps every
	// secret stub a workspace holds. SEC-046 requires the signing key be
	// "established at the same time as the workspace's data key", so the two are
	// established, loaded and destroyed together here rather than by two
	// mechanisms that could disagree about whether a workspace has one.
	//
	// It wraps no stub yet — nothing in this tree mints one — but it exists, and
	// its existence is not decorative: `archive/1` ARC-071 requires every
	// container to carry `data_key_wrap`, "the source workspace's own data key,
	// re-wrapped under the sub-key ARC-011 derives for that purpose ... never the
	// raw, unwrapped data key". An export with no data key to re-wrap cannot
	// produce a conformant manifest at all.
	data []byte
	dir  string
}

// Private returns the private half — the only key material ARC-021's signature
// is produced from.
func (k *Key) Private() ed25519.PrivateKey { return k.priv }

// Public returns the public half, the value `signer_key_id` resolves against
// (SEC-046) and the one a reader verifies an archive's header signature with.
func (k *Key) Public() ed25519.PublicKey {
	pub, _ := k.priv.Public().(ed25519.PublicKey)
	return pub
}

// KeyID returns the archive header's `signer_key_id` value: a ULID minted once,
// when the key was established, and persisted beside it.
//
// It is an identifier rather than a fingerprint on purpose. ARC-002 fixes only
// that the field exists and that a reader resolves the verifying key through it;
// making it a minted id keeps rotation expressible — a rotated key gets a new id
// while every already-written archive keeps naming the id it was actually signed
// under, which a content-derived fingerprint would also give but which a
// fingerprint would additionally couple to the key's encoding.
func (k *Key) KeyID() string { return k.keyID }

// LoadOrCreate loads the workspace signing key from dir, establishing and
// persisting a fresh one if dir holds none. A second call against the same dir
// returns the SAME key and the same id — an archive signed yesterday must still
// verify today, which a key regenerated per process could not promise.
//
// dir is created 0700 if absent; the private key lands 0600. newID mints the
// key's own identifier and MUST produce a valid ULID (DAT-005a) — it is the same
// injected-id-source seam every other id in this tree comes from, never a
// package-level generator, so a test pins it exactly as it pins a clock.
func LoadOrCreate(dir string, newID func() string) (*Key, error) {
	if dir == "" {
		return nil, errors.New("workspacekey: empty key directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("workspacekey: create dir %s: %w", dir, err)
	}
	keyPath := filepath.Join(dir, privateKeyFile)
	idPath := filepath.Join(dir, keyIDFile)

	if _, err := os.Stat(keyPath); err == nil {
		return load(dir, keyPath, idPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("workspacekey: stat %s: %w", keyPath, err)
	}
	return create(dir, keyPath, idPath, newID)
}

// WrapDataKey wraps this workspace's data key under wrapKey and returns the
// opaque value an `archive/1` manifest carries as `data_key_wrap.wrapped_value`
// (ARC-071).
//
// The wrap lives HERE, not in internal/archive, and the split is the contract's
// own: archive/1's Scope puts "the key hierarchy's own wrap/unwrap algorithm and
// key-custody model" explicitly out of its scope — "this contract carries
// wrapped, opaque key material and references the hierarchy that produces and
// consumes it, never redefines it". So archive/1 derives the wrapping sub-key
// (ARC-011's second label) and hands it here; this hierarchy decides what
// wrapping means and returns bytes archive/1 only carries.
//
// wrapKey MUST be the sub-key ARC-011 derives for this purpose and no other —
// passing the body-encryption sub-key would put the wrap's output under the same
// key the body is encrypted with, which is the exact reuse those two distinct
// context labels exist to prevent.
//
// The construction is XChaCha20-Poly1305 with a random nonce prefixed to the
// ciphertext, base64-encoded. The nonce is random rather than derived because
// this key material is wrapped once per export under a fresh per-archive
// sub-key, so there is no counter to derive from and no sequence to collide
// within.
func (k *Key) WrapDataKey(wrapKey []byte) (string, error) {
	if len(k.data) != dataKeySize {
		return "", fmt.Errorf("workspacekey: data key is %d bytes, want %d — this workspace has no data key to wrap (SEC-040)", len(k.data), dataKeySize)
	}
	aead, err := chacha20poly1305.NewX(wrapKey)
	if err != nil {
		return "", fmt.Errorf("workspacekey: init data-key wrap: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("workspacekey: read wrap nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, k.data, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DataKeyPresent reports whether this workspace holds a data key at all.
func (k *Key) DataKeyPresent() bool { return len(k.data) == dataKeySize }

func create(dir, keyPath, idPath string, newID func() string) (*Key, error) {
	if newID == nil {
		return nil, errors.New("workspacekey: no id source supplied")
	}
	keyID := newID()
	if !ulid.Valid(keyID) {
		return nil, fmt.Errorf("workspacekey: signer_key_id %q is not a valid ULID (DAT-005a)", keyID)
	}
	_, priv := signhash.GenerateKey()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("workspacekey: marshal private key: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyPath, block, 0o600); err != nil {
		return nil, fmt.Errorf("workspacekey: write %s: %w", keyPath, err)
	}
	if err := os.WriteFile(idPath, []byte(keyID), 0o600); err != nil {
		return nil, fmt.Errorf("workspacekey: write %s: %w", idPath, err)
	}
	// SEC-046: the signing key is "established at the same time as the
	// workspace's data key". Same call, same directory, same failure path — so a
	// workspace can never come into existence holding one and not the other.
	data := make([]byte, dataKeySize)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return nil, fmt.Errorf("workspacekey: generate data key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataKeyFile), data, 0o600); err != nil {
		return nil, fmt.Errorf("workspacekey: write %s: %w", dataKeyFile, err)
	}
	return &Key{priv: priv, keyID: keyID, data: data, dir: dir}, nil
}

func load(dir, keyPath, idPath string) (*Key, error) {
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("workspacekey: read %s: %w", keyPath, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("workspacekey: %s did not decode to a PRIVATE KEY PEM block", keyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("workspacekey: parse %s: %w", keyPath, err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("workspacekey: %s parsed as %T, want ed25519.PrivateKey", keyPath, parsed)
	}
	idBytes, err := os.ReadFile(idPath)
	if err != nil {
		return nil, fmt.Errorf("workspacekey: read %s: %w", idPath, err)
	}
	keyID := string(idBytes)
	if !ulid.Valid(keyID) {
		return nil, fmt.Errorf("workspacekey: persisted signer_key_id %q is not a valid ULID (DAT-005a)", keyID)
	}
	data, err := os.ReadFile(filepath.Join(dir, dataKeyFile))
	if err != nil {
		return nil, fmt.Errorf("workspacekey: read %s: %w", dataKeyFile, err)
	}
	if len(data) != dataKeySize {
		return nil, fmt.Errorf("workspacekey: persisted data key is %d bytes, want %d", len(data), dataKeySize)
	}
	return &Key{priv: priv, keyID: keyID, data: data, dir: dir}, nil
}

// Destroy removes the persisted key material — the workspace-signing-key clause
// of SEC-121's factory-reset destruction ("the workspace signing key,
// SEC-046–047").
//
// It removes the FILES, and it also zeroes the in-memory private half so a
// caller holding this value cannot keep signing with a key the deployment has
// declared destroyed. Removing a file that is already absent is not an error:
// destruction is idempotent by nature, and a second delete request must not fail
// merely because the first one succeeded.
func (k *Key) Destroy() error {
	for i := range k.priv {
		k.priv[i] = 0
	}
	for i := range k.data {
		k.data[i] = 0
	}
	for _, name := range []string{privateKeyFile, keyIDFile, dataKeyFile} {
		if err := os.Remove(filepath.Join(k.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workspacekey: destroy %s: %w", name, err)
		}
	}
	return nil
}
