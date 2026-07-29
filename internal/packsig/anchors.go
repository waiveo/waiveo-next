package packsig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// AnchorsFormat is the trust-anchors file's format discriminant.
const AnchorsFormat = "pack-trust-anchors/1"

// anchorsFile is the on-disk trust-anchors document: the host-provisioned local
// anchor set marketplace/1 MKT-009b requires while the external, root-signed
// trust root (channel-index/1 CHI-012's publisher-namespace delegation) is
// still a pending owner ceremony. It carries PUBLIC key material only.
type anchorsFile struct {
	Format  string        `json:"format"`
	Anchors []anchorEntry `json:"anchors"`
}

type anchorEntry struct {
	Namespace string `json:"namespace"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"` // base64 std, 32-byte ed25519 public key
}

// FileAnchors is a TrustAnchors source backed by an on-disk anchors document.
// It re-reads the file on every resolution: the file is tiny, and reading per
// call means a provisioning step (or the eventual root-signed replacement
// writing through the same path) takes effect without a process restart — and
// means a deleted anchors file immediately refuses every install rather than
// serving a cached ghost of revoked trust.
//
// A MISSING file resolves to no anchors (every signed artifact refuses
// untrusted — an unprovisioned host fails closed); an unreadable or malformed
// file is an error the verifier also fails closed on, but loudly, so
// misconfiguration is distinguishable from "not provisioned".
type FileAnchors struct {
	Path string
}

func (f FileAnchors) KeysFor(namespace string) ([]TrustedKey, error) {
	// The anchors document IS the pack-provenance trust root: anyone who can
	// write it can name themselves a trusted publisher. A file the world or
	// the group can write is therefore refused outright rather than read —
	// fail closed on a trust root whose integrity the host is not enforcing,
	// instead of silently trusting whatever it currently says.
	info, err := os.Stat(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("packsig: stat trust anchors %s: %w", f.Path, err)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return nil, fmt.Errorf("packsig: trust anchors %s are group- or world-writable (mode %04o) — refusing to treat them as a trust root", f.Path, mode)
	}
	// The file's own mode is not sufficient. Anyone who can write the DIRECTORY
	// can rename their own 0644 anchors document over this path, and the check
	// above then passes on a file they authored — so the trust root becomes
	// whatever they say it is, at the mode they chose. Checking the file alone
	// protects a document an attacker simply replaces.
	//
	// The sticky bit is honoured: a sticky directory (1777, /tmp's mode) already
	// forbids non-owners from renaming or unlinking files they do not own, which
	// is precisely the attack this closes, so refusing there would be a false
	// refusal.
	dir := filepath.Dir(f.Path)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("packsig: the directory holding trust anchors %s cannot be examined: %w", dir, err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o022 != 0 && dirInfo.Mode()&os.ModeSticky == 0 {
		return nil, fmt.Errorf("packsig: the directory holding trust anchors, %s, is group- or world-writable (mode %04o) and not sticky — anyone who can write it can substitute the trust root, so it is refused rather than read", dir, perm)
	}

	raw, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("packsig: read trust anchors %s: %w", f.Path, err)
	}
	var doc anchorsFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("packsig: trust anchors %s: not valid JSON: %w", f.Path, err)
	}
	if doc.Format != AnchorsFormat {
		return nil, fmt.Errorf("packsig: trust anchors %s: format %q, want %q", f.Path, doc.Format, AnchorsFormat)
	}
	var out []TrustedKey
	for _, a := range doc.Anchors {
		if a.Namespace != namespace {
			continue
		}
		pub, err := base64.StdEncoding.DecodeString(a.PublicKey)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("packsig: trust anchors %s: anchor %q for %q carries an invalid public key",
				f.Path, a.KeyID, a.Namespace)
		}
		out = append(out, TrustedKey{KeyID: a.KeyID, PublicKey: ed25519.PublicKey(pub)})
	}
	return out, nil
}

// ---- make-dev-local publisher provisioning ---------------------------------

// devSigningKeyFile is the dev publisher's ed25519 private key filename
// (PKCS8 PEM, 0600) inside the key directory DevProvision manages.
const devSigningKeyFile = "signing_key.pem"

// KeyIDFor derives a stable key id from an ed25519 public key:
// "ed25519:<first-16-hex of sha256(pub)>". Content-derived so the dev
// provisioning needs no separate id file; marketplace/1 treats key_id as an
// opaque string, so a registry-minted id slots into the same field later.
func KeyIDFor(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "ed25519:" + hex.EncodeToString(sum[:8])
}

// DevProvision is the make-dev-local trust provisioning: it loads (or creates,
// once) the dev publisher's ed25519 signing keypair under keyDir, and ensures
// the trust-anchors document at anchorsPath authorizes that key for namespace —
// the "explicitly host-provisioned local anchor set" MKT-009b requires of a
// deployment whose external trust root does not exist yet. The private key
// lands 0600 under a 0700 directory and is NEVER committed (keyDir lives under
// the git-ignored .dev/); the anchors file carries public material only.
//
// This is dev/tooling plumbing on the SIGNER side plus a convenience write of
// the verifier's anchor file so `make dev` needs no manual ceremony. The
// verifier itself never calls this — it only ever reads anchors.
func DevProvision(keyDir, anchorsPath, namespace string) (keyID string, priv ed25519.PrivateKey, err error) {
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("packsig: create key dir %s: %w", keyDir, err)
	}
	keyPath := filepath.Join(keyDir, devSigningKeyFile)

	if _, statErr := os.Stat(keyPath); statErr == nil {
		priv, err = readSigningKey(keyPath)
		if err != nil {
			return "", nil, err
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		_, priv = signhash.GenerateKey()
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return "", nil, fmt.Errorf("packsig: marshal signing key: %w", err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
			return "", nil, fmt.Errorf("packsig: write signing key: %w", err)
		}
	} else {
		return "", nil, fmt.Errorf("packsig: stat %s: %w", keyPath, statErr)
	}

	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return "", nil, fmt.Errorf("packsig: %s: public half is %T, want ed25519.PublicKey", keyPath, priv.Public())
	}
	keyID = KeyIDFor(pub)

	if err := ensureAnchor(anchorsPath, namespace, keyID, pub); err != nil {
		return "", nil, err
	}
	return keyID, priv, nil
}

// ensureAnchor upserts one {namespace, keyID, pub} entry into the anchors
// document at path, creating the document (and its parent directory) if absent.
// Existing entries for other namespaces/keys are preserved.
func ensureAnchor(path, namespace, keyID string, pub ed25519.PublicKey) error {
	doc := anchorsFile{Format: AnchorsFormat}
	if raw, err := os.ReadFile(path); err == nil {
		if jerr := json.Unmarshal(raw, &doc); jerr != nil || doc.Format != AnchorsFormat {
			return fmt.Errorf("packsig: trust anchors %s exist but are not a readable %s document — refusing to overwrite", path, AnchorsFormat)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("packsig: read trust anchors %s: %w", path, err)
	}

	encoded := base64.StdEncoding.EncodeToString(pub)
	for _, a := range doc.Anchors {
		if a.Namespace == namespace && a.KeyID == keyID && a.PublicKey == encoded {
			return nil // already provisioned
		}
	}
	doc.Anchors = append(doc.Anchors, anchorEntry{Namespace: namespace, KeyID: keyID, PublicKey: encoded})

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("packsig: create anchors dir: %w", err)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("packsig: marshal trust anchors: %w", err)
	}
	// Written temp-then-rename: a truncate-in-place write that dies midway
	// leaves a partial document, and a partial trust root refuses every
	// install until a human deletes it by hand (this function will not
	// overwrite what it cannot parse, on purpose). The rename is atomic, so a
	// concurrent reader sees either the old document or the new one.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("packsig: write trust anchors %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("packsig: install trust anchors %s: %w", path, err)
	}
	return nil
}

func readSigningKey(path string) (ed25519.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("packsig: read signing key %s: %w", path, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("packsig: %s did not decode to a PRIVATE KEY PEM block", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("packsig: parse signing key %s: %w", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("packsig: %s parsed as %T, want ed25519.PrivateKey", path, key)
	}
	return priv, nil
}
