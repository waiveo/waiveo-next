// Package signing manages the feeder's persistent relay/1 identity: the
// ed25519 keypair it signs desired-state snapshots and leases with, plus a
// self-signed TLS certificate it can serve them over. A relay learns the
// signing public key at enrollment and verifies every subsequent
// snapshot/lease against it (#28 enrollment-anchored trust) — so the
// feeder must present the SAME key across restarts, not mint a new one
// every run. LoadOrCreate makes that persistence concrete: it generates the
// identity once and reuses it from disk on every later call against the
// same dir.
//
// Key material is written under a make-dev-local, git-ignored directory
// (see DefaultDir) and MUST never be committed. Private key files land with
// 0600 permissions.
package signing

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
)

// DefaultDir is the make-dev-local directory the feeder's identity persists
// under, relative to the repo root. It sits under .dev/ — the Makefile's
// RUNDIR, and already git-ignored wholesale — but is also called out by
// name in .gitignore for a self-documenting paper trail.
const DefaultDir = ".dev/feeder-keys"

const (
	signingKeyFile = "signing_key.pem" // ed25519 desired-state signing private key, PKCS8
	tlsCertFile    = "tls_cert.pem"    // self-signed TLS leaf certificate
	tlsKeyFile     = "tls_key.pem"     // TLS certificate's private key, PKCS8
)

// Identity is the feeder's persistent relay/1 identity: the ed25519
// keypair it signs desired-state snapshots and leases with, plus the
// self-signed TLS certificate it serves them over.
type Identity struct {
	signingPub  ed25519.PublicKey
	signingPriv ed25519.PrivateKey
	certPEM     []byte
	certKeyPEM  []byte
}

// SigningPub returns the identity's ed25519 desired-state signing public
// key — the value a relay learns at enrollment and verifies future
// snapshots/leases against.
func (id *Identity) SigningPub() ed25519.PublicKey {
	return id.signingPub
}

// SigningPriv returns the identity's ed25519 desired-state signing private
// key, used to sign snapshots and leases handed to a relay.
func (id *Identity) SigningPriv() ed25519.PrivateKey {
	return id.signingPriv
}

// TLSCertPEM returns the identity's self-signed TLS leaf certificate, PEM
// encoded.
func (id *Identity) TLSCertPEM() []byte {
	return id.certPEM
}

// TLSKeyPEM returns the private key for the identity's TLS certificate,
// PEM encoded.
func (id *Identity) TLSKeyPEM() []byte {
	return id.certKeyPEM
}

// LoadOrCreate loads the feeder's identity (signing keypair + self-signed
// TLS cert) from dir, generating and persisting a fresh one if dir is
// empty or missing its key files. A second LoadOrCreate call against the
// same dir returns the SAME public key as the first — the persistence
// property a relay's enrollment-anchored trust depends on.
//
// dir is created (mode 0700) if it does not already exist. Private key
// material is written with 0600 permissions; the (non-secret) TLS
// certificate is written 0644.
func LoadOrCreate(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("signing: create dir %s: %w", dir, err)
	}

	signingKeyPath := filepath.Join(dir, signingKeyFile)
	certPath := filepath.Join(dir, tlsCertFile)
	certKeyPath := filepath.Join(dir, tlsKeyFile)

	if fileExists(signingKeyPath) {
		return load(signingKeyPath, certPath, certKeyPath)
	}

	return create(signingKeyPath, certPath, certKeyPath)
}

// create generates a fresh signing keypair and self-signed TLS cert,
// persists all three files, and returns the resulting Identity.
func create(signingKeyPath, certPath, certKeyPath string) (*Identity, error) {
	pub, priv := signhash.GenerateKey()
	certPEM, certKeyPEM := tlsboot.GenSelfSigned()

	signingKeyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal signing key: %w", err)
	}
	signingKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: signingKeyDER})

	if err := os.WriteFile(signingKeyPath, signingKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("signing: write signing key: %w", err)
	}
	if err := os.WriteFile(certKeyPath, certKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("signing: write TLS key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("signing: write TLS cert: %w", err)
	}

	return &Identity{
		signingPub:  pub,
		signingPriv: priv,
		certPEM:     certPEM,
		certKeyPEM:  certKeyPEM,
	}, nil
}

// load reads a previously-persisted identity back from disk.
func load(signingKeyPath, certPath, certKeyPath string) (*Identity, error) {
	priv, err := readSigningKey(signingKeyPath)
	if err != nil {
		return nil, err
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("signing: read TLS cert %s: %w", certPath, err)
	}
	certKeyPEM, err := os.ReadFile(certKeyPath)
	if err != nil {
		return nil, fmt.Errorf("signing: read TLS key %s: %w", certKeyPath, err)
	}

	// Self-heal a stale serving leaf. An identity persisted before the
	// browser-compat fix carries an Ed25519 TLS leaf (the old GenSelfSigned
	// output), which Chrome/Safari/Firefox and macOS LibreSSL curl reject at
	// handshake — leaving the embedded-SPA browser->feeder HTTPS path
	// unreachable. .dev/feeder-keys is designed to persist and the Makefile's
	// dev-up never clears it (unlike .dev/relay-identity), so without this a
	// dev checkout that ran the stack once pre-fix would reload and re-serve
	// the stale leaf forever. Reissue an ECDSA P-256 serving cert in place when
	// the on-disk leaf is not already one, persisting it so the next restart
	// reuses it. Only the TLS-serving cert/key change; the enrollment-anchored
	// Ed25519 SIGNING key (read above) is untouched — and the relay re-pins the
	// leaf's SPKI at its next (fresh-per-dev-up) enrollment, so reissuing here
	// breaks no commitment.
	if !servingLeafIsECDSAP256(certPEM) {
		certPEM, certKeyPEM, err = reissueTLSLeaf(certPath, certKeyPath)
		if err != nil {
			return nil, err
		}
	}

	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("signing: %s: public half is %T, want ed25519.PublicKey", signingKeyPath, priv.Public())
	}

	return &Identity{
		signingPub:  pub,
		signingPriv: priv,
		certPEM:     certPEM,
		certKeyPEM:  certKeyPEM,
	}, nil
}

// servingLeafIsECDSAP256 reports whether the PEM-encoded certificate certPEM
// carries an ECDSA P-256 public key — the algorithm real browsers and macOS
// LibreSSL require of the feeder's HTTPS serving leaf. It returns false for an
// Ed25519 leaf (the pre-fix material), a non-P-256 curve, or anything that does
// not decode/parse as a certificate, so load() fails closed toward reissuing a
// fresh, correct leaf.
func servingLeafIsECDSAP256(certPEM []byte) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	return pub.Curve == elliptic.P256()
}

// reissueTLSLeaf mints a fresh ECDSA P-256 self-signed serving cert and
// overwrites the persisted TLS cert/key files (never the signing key),
// returning the new PEM material. Used by load() to repair a stale serving
// leaf in place so the fix takes effect on the next feeder start without a
// manual key-directory wipe.
func reissueTLSLeaf(certPath, certKeyPath string) (certPEM, certKeyPEM []byte, err error) {
	certPEM, certKeyPEM = tlsboot.GenSelfSigned()
	if err := os.WriteFile(certKeyPath, certKeyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("signing: reissue TLS key %s: %w", certKeyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("signing: reissue TLS cert %s: %w", certPath, err)
	}
	return certPEM, certKeyPEM, nil
}

func readSigningKey(path string) (ed25519.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("signing: read signing key %s: %w", path, err)
	}

	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("signing: %s did not decode to a PRIVATE KEY PEM block", path)
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("signing: parse signing key %s: %w", path, err)
	}

	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing: %s parsed as %T, want ed25519.PrivateKey", path, key)
	}

	return priv, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
