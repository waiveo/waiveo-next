// Package identity manages the relay's persistent operational SQLite
// store: its own enrollment identity (relay_id, issued certificate, and
// the private key it generated), the feeder's `desired_state_verification_key`
// learned at enrollment (relay/1 REL-012, REL-071 — the enrollment-anchored
// trust anchor, `#28`), and its last-applied desired-state generation
// (REL-073: persisted beside the verification key, so both survive a power
// cycle and remain usable for offline verification without contacting the
// app peer).
//
// This is deliberately a narrow, operational store — relay/1 REL-142 scopes
// a relay's durable local state to exactly this identity/trust/progress
// data (plus a bounded telemetry buffer this package does not yet hold) and
// nothing else. In particular, `#52`'s gateway posture means the relay MUST
// NOT cache asset/media bytes anywhere: the schema this package creates has
// no table capable of holding them, and TestOpenCreatesExactOperationalTableSet
// pins the table set to guard against one ever being added by accident.
//
// Backed by modernc.org/sqlite — a pure-Go SQLite driver (no cgo), keeping
// the relay binary's pure-Go, no-`.node` release-gate posture (Wave-0's
// pure-Go invariant) intact.
package identity

import (
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DefaultPath is the make-dev-local path the relay's identity store lives
// under, relative to the repo root — mirroring the feeder's
// internal/feeder/signing.DefaultDir convention. Already covered by the
// wholesale .dev/ .gitignore entry; never commit this file.
const DefaultPath = ".dev/relay-identity/relay.db"

// Store is the relay's operational SQLite store. Safe for concurrent use
// (delegates to database/sql's own connection-pool locking); callers should
// still Close it exactly once when done.
type Store struct {
	db *sql.DB
}

// schema creates exactly the three operational tables REL-142 scopes a
// relay's durable local state to. Each is a singleton (a single row keyed
// by the fixed id=1), since a relay holds exactly one identity, one
// verification key, and one last-applied generation at a time.
const schema = `
CREATE TABLE IF NOT EXISTS relay_identity (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	relay_id         TEXT NOT NULL,
	cert_pem         BLOB NOT NULL,
	private_key_pem  BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS desired_state_verification_key (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	public_key  BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS last_applied_generation (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	generation  INTEGER NOT NULL,
	hash        TEXT NOT NULL
);
`

// Open opens (creating if necessary) the operational SQLite database at
// path, ensuring its schema exists. path's parent directory is created
// (mode 0700) if missing; use ":memory:" for an ephemeral, test-only store.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("identity: create dir %s: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("identity: open %s: %w", path, err)
	}

	// The relay is a single process talking to its own local file; one
	// connection avoids modernc.org/sqlite's known SQLITE_BUSY surface
	// under concurrent writers on the same *os.File handle.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: create schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// tableNames returns the names of every user table in the store (excluding
// sqlite's own internal sqlite_% tables) — used by
// TestOpenCreatesExactOperationalTableSet to pin the schema's table set.
func (s *Store) tableNames() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("identity: query sqlite_master: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("identity: scan table name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// RelayIdentity is the relay's own persisted enrollment identity: its
// permanent relay_id (REL-014), the certificate most recently issued to it,
// and the private key of that certificate's own keypair — retained even
// past the certificate's expiry (REL-142), so it remains available to prove
// possession of at a later expired-certificate re-enrollment.
type RelayIdentity struct {
	RelayID    string
	CertPEM    []byte
	PrivateKey ed25519.PrivateKey
}

// SetIdentity persists (replacing any previously persisted identity) the
// relay's own relay_id, issued certificate, and the private key it
// generated for enrollment.
func (s *Store) SetIdentity(relayID string, certPEM []byte, priv ed25519.PrivateKey) error {
	keyPEM, err := marshalPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("identity: SetIdentity: %w", err)
	}

	_, err = s.db.Exec(
		`INSERT INTO relay_identity (id, relay_id, cert_pem, private_key_pem) VALUES (1, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET relay_id = excluded.relay_id, cert_pem = excluded.cert_pem, private_key_pem = excluded.private_key_pem`,
		relayID, certPEM, keyPEM,
	)
	if err != nil {
		return fmt.Errorf("identity: SetIdentity: %w", err)
	}
	return nil
}

// Identity returns the relay's persisted identity, and whether one has been
// persisted yet (SetIdentity has never been called returns ok=false, not an
// error).
func (s *Store) Identity() (RelayIdentity, bool, error) {
	var relayID string
	var certPEM, keyPEM []byte

	err := s.db.QueryRow(`SELECT relay_id, cert_pem, private_key_pem FROM relay_identity WHERE id = 1`).
		Scan(&relayID, &certPEM, &keyPEM)
	if err == sql.ErrNoRows {
		return RelayIdentity{}, false, nil
	}
	if err != nil {
		return RelayIdentity{}, false, fmt.Errorf("identity: Identity: %w", err)
	}

	priv, err := unmarshalPrivateKey(keyPEM)
	if err != nil {
		return RelayIdentity{}, false, fmt.Errorf("identity: Identity: %w", err)
	}

	return RelayIdentity{RelayID: relayID, CertPEM: certPEM, PrivateKey: priv}, true, nil
}

// SetDesiredStateVerificationKey persists (replacing any previously
// persisted key) the feeder's desired-state signing public key learned at
// enrollment (REL-012) — the trust anchor the relay verifies every
// subsequent snapshot against (REL-071).
func (s *Store) SetDesiredStateVerificationKey(pub ed25519.PublicKey) error {
	_, err := s.db.Exec(
		`INSERT INTO desired_state_verification_key (id, public_key) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET public_key = excluded.public_key`,
		[]byte(pub),
	)
	if err != nil {
		return fmt.Errorf("identity: SetDesiredStateVerificationKey: %w", err)
	}
	return nil
}

// DesiredStateVerificationKey returns the persisted feeder verification
// key, and whether one has been persisted yet.
func (s *Store) DesiredStateVerificationKey() (ed25519.PublicKey, bool, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT public_key FROM desired_state_verification_key WHERE id = 1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("identity: DesiredStateVerificationKey: %w", err)
	}
	return ed25519.PublicKey(raw), true, nil
}

// SetLastAppliedGeneration persists (replacing any previously persisted
// value) the relay's last-applied desired-state generation number and
// section hash (REL-073).
func (s *Store) SetLastAppliedGeneration(generation int64, hash string) error {
	_, err := s.db.Exec(
		`INSERT INTO last_applied_generation (id, generation, hash) VALUES (1, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET generation = excluded.generation, hash = excluded.hash`,
		generation, hash,
	)
	if err != nil {
		return fmt.Errorf("identity: SetLastAppliedGeneration: %w", err)
	}
	return nil
}

// LastAppliedGeneration returns the persisted last-applied generation and
// hash, and whether one has been persisted yet.
func (s *Store) LastAppliedGeneration() (generation int64, hash string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT generation, hash FROM last_applied_generation WHERE id = 1`).Scan(&generation, &hash)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("identity: LastAppliedGeneration: %w", err)
	}
	return generation, hash, true, nil
}

// marshalPrivateKey PKCS8/PEM-encodes priv, matching the encoding
// internal/feeder/signing uses for its own signing key — a consistent
// on-disk convention for ed25519 private keys across this codebase.
func marshalPrivateKey(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func unmarshalPrivateKey(keyPEM []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("private key did not decode to a PRIVATE KEY PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key parsed as %T, want ed25519.PrivateKey", key)
	}
	return priv, nil
}
