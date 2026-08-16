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
// serveHosts are the non-loopback names the serving certificate must also
// cover — the feeder's configured listen host, when it binds a specific
// address. Loopback is always covered: pack processes are exec'd children on
// this host and verify the leaf with stock X.509, where names are what count.
//
// dir is created (mode 0700) if it does not already exist. Private key
// material is written with 0600 permissions; the (non-secret) TLS
// certificate is written 0644.
func LoadOrCreate(dir string, serveHosts ...string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("signing: create dir %s: %w", dir, err)
	}

	signingKeyPath := filepath.Join(dir, signingKeyFile)
	certPath := filepath.Join(dir, tlsCertFile)
	certKeyPath := filepath.Join(dir, tlsKeyFile)

	if fileExists(signingKeyPath) {
		return load(signingKeyPath, certPath, certKeyPath, serveHosts)
	}

	return create(signingKeyPath, certPath, certKeyPath, serveHosts)
}

// create generates a fresh signing keypair and self-signed TLS cert,
// persists all three files, and returns the resulting Identity.
func create(signingKeyPath, certPath, certKeyPath string, serveHosts []string) (*Identity, error) {
	pub, priv := signhash.GenerateKey()
	certPEM, certKeyPEM := tlsboot.GenSelfSignedFor(serveHosts...)

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
func load(signingKeyPath, certPath, certKeyPath string, serveHosts []string) (*Identity, error) {
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

	// Self-heal a stale serving leaf. Two generations of staleness exist, and
	// they are repaired DIFFERENTLY because they break different things:
	//
	//   - An identity persisted before the browser-compat fix carries an
	//     Ed25519 leaf, which browsers reject at handshake. The KEY is the
	//     problem, so the reissue mints a new keypair — accepting that every
	//     enrolled relay's SPKI pin breaks and re-enrollment follows (the
	//     dev-up flow re-enrolls fresh; that trade was taken knowingly).
	//   - An identity persisted before the pack runtime carries a SAN-less
	//     leaf — pack processes verify with stock X.509, where a name match is
	//     required and the CommonName is never consulted, so a SAN-less anchor
	//     can never verify no matter how correctly it is handed over. Here the
	//     key is FINE, so the reissue mints a new certificate OVER THE SAME
	//     KEY: the SPKI is unchanged by construction and every enrolled
	//     relay's pin survives. Healing a certificate's shape must never cost
	//     a persisted deployment its fleet (REL-137 is exactly that outage).
	//
	// The same key-preserving path covers a leaf that no longer covers the
	// configured listen host. The enrollment-anchored Ed25519 SIGNING key
	// (read above) is untouched by either repair.
	if !servingLeafIsECDSAP256(certPEM) {
		certPEM, certKeyPEM, err = reissueTLSLeaf(certPath, certKeyPath, serveHosts)
		if err != nil {
			return nil, err
		}
	} else if !servingLeafCoversHosts(certPEM, serveHosts) {
		certPEM, err = reissueCertOverSameKey(certPath, certKeyPEM, serveHosts)
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

// servingLeafCoversHosts reports whether the PEM-encoded certificate covers
// loopback and every host in serveHosts, by the same name-matching rules a
// verifying client applies (VerifyHostname — SANs, never the CommonName). It
// returns false for anything that does not parse, failing closed toward a
// reissue.
func servingLeafCoversHosts(certPEM []byte, serveHosts []string) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	for _, h := range append([]string{"127.0.0.1", "::1", "localhost"}, serveHosts...) {
		if h == "" {
			continue
		}
		if cert.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

// reissueTLSLeaf mints a fresh ECDSA P-256 self-signed serving cert AND KEY,
// overwriting the persisted TLS cert/key files (never the signing key). The
// key-replacing repair: correct only when the existing key itself is unusable,
// because a new key is a new SPKI and every enrolled relay's pin breaks.
func reissueTLSLeaf(certPath, certKeyPath string, serveHosts []string) (certPEM, certKeyPEM []byte, err error) {
	certPEM, certKeyPEM = tlsboot.GenSelfSignedFor(serveHosts...)
	if err := os.WriteFile(certKeyPath, certKeyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("signing: reissue TLS key %s: %w", certKeyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("signing: reissue TLS cert %s: %w", certPath, err)
	}
	return certPEM, certKeyPEM, nil
}

// reissueCertOverSameKey mints a fresh serving cert over the EXISTING TLS key
// and overwrites only the persisted certificate. The SPKI — the thing every
// pin in this system is over — is unchanged by construction, so enrolled
// relays keep trusting the feeder across the repair.
func reissueCertOverSameKey(certPath string, certKeyPEM []byte, serveHosts []string) ([]byte, error) {
	certPEM, err := tlsboot.ReissueCertOver(certKeyPEM, serveHosts...)
	if err != nil {
		return nil, fmt.Errorf("signing: reissue serving cert over the existing key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("signing: reissue TLS cert %s: %w", certPath, err)
	}
	return certPEM, nil
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

// HasIdentity reports whether dir already holds this deployment's identity.
//
// It exists so a caller can tell a LOAD from a MINT, which LoadOrCreate cannot
// report and which is the difference between a routine boot and a fleet-wide
// outage: minting invalidates the certificate every enrolled relay pinned. The
// check is the same one LoadOrCreate branches on — the signing key's presence —
// named once so the two cannot drift into disagreeing about what "already has
// an identity" means.
func HasIdentity(dir string) bool {
	return fileExists(filepath.Join(dir, signingKeyFile))
}
