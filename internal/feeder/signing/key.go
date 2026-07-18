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
	"crypto/ed25519"
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
