package enroll

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// enrollmentStateFile is the filename EnablePersistence's on-disk registry
// snapshot is written under, inside the directory the caller supplies.
const enrollmentStateFile = "enrollment.json"

// persistedIssuance is the on-disk form of one issuance record: the same
// four fields issuance (enroll.go) carries, with the ed25519 public key
// hex-encoded so the whole record round-trips through JSON.
type persistedIssuance struct {
	Serial   string `json:"serial"`
	Pub      string `json:"pub"`
	IssuedAt int64  `json:"issued_at"`
	Revoked  bool   `json:"revoked"`
}

// persistedState is the on-disk form of everything a restarted feeder
// process needs in order to keep recognizing every relay it already
// enrolled: the enrollment public key RelayEnrollmentKey looks up for the
// connection handshake's channel-binding verification (REL-032, via
// internal/relay/hello), the issuance record the Expired-certificate
// re-enrollment path evaluates eligibility against (REL-021/022), the
// claim-token bookkeeping (REL-013), and the CA keypair certificates are
// issued under — so a certificate issued after a restart chains under the
// SAME CA as one issued before it.
type persistedState struct {
	CACertPEM string                         `json:"ca_cert_pem"`
	CAKeyPEM  string                         `json:"ca_key_pem"`
	Pending   string                         `json:"pending"`
	Redeemed  map[string]bool                `json:"redeemed"`
	RelayKeys map[string]string              `json:"relay_keys"`
	Issuances map[string][]persistedIssuance `json:"issuances"`
}

// EnablePersistence points s at dir as its durable enrollment-registry
// store: a restarted feeder process (same identity, same dir) loads back
// exactly the relay_id -> enrollment-key, issuance, claim-token, and CA
// state it held before the restart, instead of a fresh process forgetting
// every relay it ever enrolled.
//
// This closes the amnesia a feeder restart otherwise causes: NewServer
// always builds an empty registry and a fresh in-memory CA, so — absent
// this call — an already-enrolled relay's next hello fails channel-binding
// verification (CHANNEL_BINDING_INVALID, REL-032) purely because the app
// peer process restarted, even though nothing about the relay's own
// identity changed.
//
// dir is created (mode 0700) if it does not already exist. When dir holds
// no prior state file, s's freshly-generated in-memory registry (built by
// NewServer) is persisted as the initial state. When it holds one, s's
// in-memory registry is REPLACED by the persisted one — the loaded CA,
// relay keys, issuance records, and claim-token bookkeeping become
// authoritative, discarding the fresh generateCA() state NewServer built
// before this call. Must be called before Register/serving traffic; every
// later mutating operation (enrollment, re-enrollment, claim-token
// issuance) persists its update back to the same file.
func (s *Server) EnablePersistence(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("enroll: EnablePersistence: create dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, enrollmentStateFile)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistPath = path

	if !fileExistsEnroll(path) {
		return s.persistLocked()
	}
	return s.loadLocked(path)
}

func fileExistsEnroll(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// loadLocked reads path's persisted registry and replaces s's in-memory
// fields with it. The caller holds s.mu.
func (s *Server) loadLocked(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("enroll: load persisted enrollment state %s: %w", path, err)
	}
	var ps persistedState
	if err := json.Unmarshal(raw, &ps); err != nil {
		return fmt.Errorf("enroll: parse persisted enrollment state %s: %w", path, err)
	}

	caCert, caKey, err := decodeCA(ps.CACertPEM, ps.CAKeyPEM)
	if err != nil {
		return fmt.Errorf("enroll: decode persisted CA in %s: %w", path, err)
	}

	relayKeys := make(map[string]ed25519.PublicKey, len(ps.RelayKeys))
	for relayID, pubHex := range ps.RelayKeys {
		pub, err := hex.DecodeString(pubHex)
		if err != nil {
			return fmt.Errorf("enroll: decode persisted relay key for %s: %w", relayID, err)
		}
		relayKeys[relayID] = ed25519.PublicKey(pub)
	}

	issuances := make(map[string][]issuance, len(ps.Issuances))
	for relayID, list := range ps.Issuances {
		decoded := make([]issuance, 0, len(list))
		for _, pi := range list {
			pub, err := hex.DecodeString(pi.Pub)
			if err != nil {
				return fmt.Errorf("enroll: decode persisted issuance key for %s: %w", relayID, err)
			}
			decoded = append(decoded, issuance{
				serial:   pi.Serial,
				pub:      ed25519.PublicKey(pub),
				issuedAt: pi.IssuedAt,
				revoked:  pi.Revoked,
			})
		}
		issuances[relayID] = decoded
	}

	redeemed := ps.Redeemed
	if redeemed == nil {
		redeemed = map[string]bool{}
	}

	s.caCert = caCert
	s.caKey = caKey
	s.pending = ps.Pending
	s.redeemed = redeemed
	s.relayKeys = relayKeys
	s.issuances = issuances
	return nil
}

// persistLocked writes s's current in-memory registry to s.persistPath,
// replacing its previous contents atomically (write-then-rename) so a crash
// mid-write never leaves a truncated/corrupt file behind. The caller holds
// s.mu. A no-op when persistence was never enabled (s.persistPath == "").
func (s *Server) persistLocked() error {
	if s.persistPath == "" {
		return nil
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.caCert.Raw})
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(s.caKey)
	if err != nil {
		return fmt.Errorf("enroll: marshal CA key: %w", err)
	}
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER})

	relayKeys := make(map[string]string, len(s.relayKeys))
	for relayID, pub := range s.relayKeys {
		relayKeys[relayID] = hex.EncodeToString(pub)
	}

	issuances := make(map[string][]persistedIssuance, len(s.issuances))
	for relayID, list := range s.issuances {
		encoded := make([]persistedIssuance, 0, len(list))
		for _, iss := range list {
			encoded = append(encoded, persistedIssuance{
				Serial:   iss.serial,
				Pub:      hex.EncodeToString(iss.pub),
				IssuedAt: iss.issuedAt,
				Revoked:  iss.revoked,
			})
		}
		issuances[relayID] = encoded
	}

	ps := persistedState{
		CACertPEM: string(caCertPEM),
		CAKeyPEM:  string(caKeyPEM),
		Pending:   s.pending,
		Redeemed:  s.redeemed,
		RelayKeys: relayKeys,
		Issuances: issuances,
	}

	raw, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return fmt.Errorf("enroll: marshal enrollment state: %w", err)
	}

	tmp := s.persistPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("enroll: write enrollment state %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.persistPath); err != nil {
		return fmt.Errorf("enroll: rename enrollment state into place %s: %w", s.persistPath, err)
	}
	return nil
}

// decodeCA parses a CA certificate/key pair back out of the PEM strings
// persistLocked wrote.
func decodeCA(certPEMStr, keyPEMStr string) (*x509.Certificate, ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(certPEMStr))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("CA certificate did not PEM-decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode([]byte(keyPEMStr))
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		return nil, nil, fmt.Errorf("CA key did not PEM-decode")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("CA key parsed as %T, want ed25519.PrivateKey", key)
	}
	return cert, priv, nil
}
