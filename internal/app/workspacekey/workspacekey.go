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
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/maaxton/waiveo-next/internal/shared/secretfile"
	"github.com/maaxton/waiveo-next/internal/shared/secretseal"
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

// ErrDestroyed is what every operation on a destroyed key returns. It is a
// sentinel so a caller can tell "this workspace's key material was destroyed"
// (SEC-121 ran) from "this key cannot wrap right now" — the first is a permanent
// state no retry changes, the second might not be.
var ErrDestroyed = errors.New("workspacekey: the workspace signing key has been destroyed (SEC-121)")

// Key is a workspace signing key loaded from (or freshly established in) a
// directory: the ed25519 private half an export signs with, and the stable
// identifier an archive's `signer_key_id` field records so a reader knows which
// public half to resolve the signature against (ARC-002/021, SEC-046).
//
// A Key is safe for concurrent use. That is not decoration: the export operation
// and the delete operation are separate api/1 Jobs on separate goroutines, and
// the delete's whole purpose is to destroy the key material the export is
// reading. Without the lock below, "the export signs while the delete zeroes"
// is a data race on the key's own bytes.
type Key struct {
	mu sync.RWMutex
	// destroyed records that Destroy has run. It is the state EVERY accessor
	// consults, because the alternative — inferring destruction from the key
	// material's shape — is exactly the bug this field exists to kill: zeroed
	// key material has the same LENGTH as live key material, so a length test
	// reports a destroyed key as present and usable.
	destroyed bool
	priv      ed25519.PrivateKey
	keyID     string
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
// is produced from — or NIL once the key has been destroyed.
//
// nil rather than zeroed bytes is the whole point. Every writer of a signature
// in this tree checks its signer's length (archive.Create refuses a signer that
// is not ed25519.PrivateKeySize), so a destroyed key that returned 64 zero bytes
// would sail through that check and sign; one that returns nothing cannot.
//
// The returned slice is a COPY. Destroy zeroes the key's own storage, and a
// caller holding an alias of it would watch its key material turn to zeros
// underneath a signature already in progress.
func (k *Key) Private() ed25519.PrivateKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.destroyed || len(k.priv) != ed25519.PrivateKeySize {
		return nil
	}
	out := make(ed25519.PrivateKey, len(k.priv))
	copy(out, k.priv)
	return out
}

// Public returns the public half, the value `signer_key_id` resolves against
// (SEC-046) and the one a reader verifies an archive's header signature with —
// or nil once the key has been destroyed.
func (k *Key) Public() ed25519.PublicKey {
	priv := k.Private()
	if priv == nil {
		return nil
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	return pub
}

// Destroyed reports whether this key's material has been destroyed (SEC-121).
//
// It is exported so a consumer can ASK, rather than discover the answer by
// attempting an operation and reading the failure. Destruction is a state of the
// workspace, and a state nothing can observe is a state nothing can be honest
// about.
func (k *Key) Destroyed() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.destroyed
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
//
// A destroyed key returns the empty string: the id file is gone, no public half
// exists for it to resolve to, and archive.Create refuses an empty
// `signer_key_id` (ARC-002) — a second boundary at which a destroyed key cannot
// produce a container.
func (k *Key) KeyID() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.destroyed {
		return ""
	}
	return k.keyID
}

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
	if err := secretfile.EnsureDir(dir); err != nil {
		return nil, fmt.Errorf("workspacekey: prepare key directory: %w", err)
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
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.destroyed {
		// Loudly, and before any wrapping happens. The alternative this replaced
		// was silent and catastrophic: a destroyed key's zeroed data key is still
		// dataKeySize bytes long, so a length test passed and the wrap SUCCEEDED,
		// putting 32 zero bytes into a manifest's `data_key_wrap` and letting the
		// export report success.
		return "", ErrDestroyed
	}
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

// labelSecretSeal is the fixed context label the secret-sealing sub-key is
// derived from the data key under. It is the same per-purpose separation
// technique SEC-047 names and `archive/1` ARC-011 uses for its own two export
// sub-keys: the data key itself is never used as an encryption key directly, so
// a weakness in one construction cannot reach another purpose's material.
//
// This string is effectively frozen. Change it and every already-sealed secret
// stub in a deployment becomes unopenable — the wrapped value carries no label
// of its own to negotiate with.
const labelSecretSeal = "waiveo-security-model/1 secret-stub-seal"

// SecretSealer returns the sealer every recoverable secret stub this workspace
// holds is protected under (SEC-040: the data key is "the per-workspace
// symmetric key that wraps every secret stub").
//
// It answers the question a TOTP shared secret forces: a second-factor secret
// cannot be hashed the way a password is, because verifying a code means
// recomputing HMAC over the secret itself. So it must be recoverable, and
// "recoverable" without a key is just "stored in the clear beside the password
// hash" — one database read from being an attacker's second factor too. Sealing
// it under a sub-key of the data key puts the material behind a file
// (`workspace_data_key`, 0600 under a 0700 directory) that lives outside the
// database, so a stolen or leaked auth database alone yields nothing usable.
//
// A DESTROYED key returns ErrDestroyed rather than a sealer over zeroed bytes.
// That is the same trap WrapDataKey documents: zeroed key material is exactly as
// long as live key material, so a length check would hand back a perfectly
// functional sealer keyed on 32 zero bytes and every secret sealed after a
// factory reset would be protected by a constant every reader of this source
// knows.
func (k *Key) SecretSealer() (*secretseal.Sealer, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.destroyed {
		return nil, ErrDestroyed
	}
	if len(k.data) != dataKeySize {
		return nil, fmt.Errorf("workspacekey: data key is %d bytes, want %d — this workspace has no data key to seal under (SEC-040)", len(k.data), dataKeySize)
	}
	sub, err := hkdf.Expand(sha256.New, k.data, labelSecretSeal, secretseal.KeySize)
	if err != nil {
		return nil, fmt.Errorf("workspacekey: derive secret-seal sub-key: %w", err)
	}
	return secretseal.New(sub)
}

// DataKeyPresent reports whether this workspace holds a data key at all.
//
// A destroyed key holds none, whatever the length of the bytes still sitting in
// its field — which is precisely what the length test alone got wrong.
func (k *Key) DataKeyPresent() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return !k.destroyed && len(k.data) == dataKeySize
}

// StrayKeyFiles reports which of this package's files exist in dir, for a dir
// that is NOT supposed to be a key directory.
//
// It exists for one specific hazard: key material and export output once shared
// a directory, and a deployment that ran such a build still has a private
// signing key and a cleartext data key sitting among files an operator is
// encouraged to copy off the box. Moving the key on a new boot does not move the
// old copy, and deleting it automatically is not this code's call — those bytes
// may be the only remaining way to verify archives an operator already holds. So
// the honest action is to detect and say so.
//
// An unreadable or absent dir yields nothing: this is a warning path, and it has
// no business turning a missing directory into a failure.
func StrayKeyFiles(dir string) []string {
	if dir == "" {
		return nil
	}
	var found []string
	for _, name := range []string{privateKeyFile, keyIDFile, dataKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			found = append(found, name)
		}
	}
	return found
}

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
	// All three files go through the shared secret discipline
	// (internal/shared/secretfile): 0600 from the instant they exist, installed
	// by rename, so a crash between two of these writes cannot leave a key
	// directory holding a private half with no id or an id with no key.
	if err := secretfile.Write(keyPath, block); err != nil {
		return nil, fmt.Errorf("workspacekey: write %s: %w", keyPath, err)
	}
	if err := secretfile.Write(idPath, []byte(keyID)); err != nil {
		return nil, fmt.Errorf("workspacekey: write %s: %w", idPath, err)
	}
	// SEC-046: the signing key is "established at the same time as the
	// workspace's data key". Same call, same directory, same failure path — so a
	// workspace can never come into existence holding one and not the other.
	data := make([]byte, dataKeySize)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return nil, fmt.Errorf("workspacekey: generate data key: %w", err)
	}
	if err := secretfile.Write(filepath.Join(dir, dataKeyFile), data); err != nil {
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
// It removes the FILES, and it does three things in memory, all three of which
// are needed and none of which substitutes for the others:
//
//   - It MARKS the key destroyed. This is the observable part: Destroyed,
//     DataKeyPresent, KeyID, Private and WrapDataKey all consult the flag, so
//     every consumer sees the destruction rather than only the one that happens
//     to call the operation that would have failed. Marking is what a length
//     test could never do — zeroed key material is exactly as long as live key
//     material, so "is it there" and "is it usable" had silently become
//     different questions with the same answer.
//   - It ZEROES the key material, so the bytes do not linger in the process's
//     heap after the deployment has declared them destroyed.
//   - It DROPS the references, so nothing can reach the zeroed bytes at all —
//     the flag makes an attempt fail loudly, and the nil makes there be nothing
//     to fail with.
//
// Removing a file that is already absent is not an error: destruction is
// idempotent by nature, and a second delete request must not fail merely because
// the first one succeeded.
func (k *Key) Destroy() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.destroyed = true
	for i := range k.priv {
		k.priv[i] = 0
	}
	for i := range k.data {
		k.data[i] = 0
	}
	k.priv = nil
	k.data = nil
	k.keyID = ""
	for _, name := range []string{privateKeyFile, keyIDFile, dataKeyFile} {
		if err := os.Remove(filepath.Join(k.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workspacekey: destroy %s: %w", name, err)
		}
	}
	return nil
}
