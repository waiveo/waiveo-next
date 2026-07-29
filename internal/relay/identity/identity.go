// Package identity manages the relay's persistent operational SQLite
// store: its own enrollment identity (relay_id, issued certificate, and
// the private key it generated), the feeder's `desired_state_verification_key`
// learned at enrollment (relay/1 REL-012, REL-071 — the enrollment-anchored
// trust anchor, `#28`), and its last-applied desired-state generation
// (REL-073: persisted beside the verification key, so both survive a power
// cycle and remain usable for offline verification without contacting the
// app peer) — including that generation's own `revocation_and_site.revoked`
// set, which REL-123 requires the relay enforce from its last-synced copy
// "regardless of connectivity", a restart included.
//
// This is deliberately a narrow, operational store — relay/1 REL-142 scopes
// a relay's durable local state to exactly this identity/trust/progress
// data, its persisted clock floor (REL-130/132, below), the enrollment-anchored
// app-peer trust pin (REL-011 `trust_pin`, the REL-137 key pin, below), and a
// bounded telemetry buffer (the telemetry_queue + telemetry_loss_marker tables
// plus the telemetry_seq_high_water cursor, REL-090/091, see telemetrystore.go),
// and nothing else. In particular, `#52`'s
// gateway posture means the relay MUST NOT cache asset/media bytes anywhere:
// the schema this package creates has no table capable of holding them (the
// telemetry_queue holds only small event bodies, never content), and
// TestOpenCreatesExactOperationalTableSet pins the table set to guard against
// one ever being added by accident.
//
// This store also holds the player/1-facing credential state PLY-091 and
// PLY-105 each name as belonging to "relay/1's own operational storage,
// mirroring the persistence relay/1 REL-142 already requires of it" — a
// minted channel token (hashed, never raw; see playersession.go's HashToken)
// and a one-time pairing grant's own redemption marker (relay/1 REL-121).
// REL-142's own text scopes only relay/1's OWN durable state; player-facing
// credential issuance is explicitly a distinct contract (player/1) this one
// only supplies inputs to, and player/1 itself is what extends the SAME
// durable tier to this state — see playersession.go's own package doc.
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

// schema creates the five singleton operational tables REL-142/REL-130/REL-011
// scope a relay's durable local state to (the bounded telemetry queue is created
// separately by telemetrySchema, see telemetrystore.go). Each is a singleton
// (a single row keyed by the fixed id=1), since a relay holds exactly one
// identity, one verification key, one last-applied generation, one clock floor,
// and one app-peer trust pin at a time.
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
	id              INTEGER PRIMARY KEY CHECK (id = 1),
	generation      INTEGER NOT NULL,
	hash            TEXT NOT NULL,
	screen_programs BLOB NOT NULL DEFAULT '[]',
	revoked         BLOB NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS clock_floor (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	floor_ms    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS app_peer_trust_pin (
	id    INTEGER PRIMARY KEY CHECK (id = 1),
	spki  BLOB NOT NULL
);
`

// Open opens (creating if necessary) the operational SQLite database at
// path, ensuring its schema exists. path's parent directory is created
// (mode 0700) if missing; use ":memory:" for an ephemeral, test-only store.
//
// A file-backed store is opened under crash-safe write discipline (§10):
// journal_mode=WAL, synchronous=FULL, and busy_timeout=5000ms are applied to
// every connection (via the DSN, so a pooled reconnect re-applies them), and
// each is verified after Open. WAL + FULL fsync every commit is what lets a
// committed operational record — a last-applied {generation, hash} (REL-055)
// or a durable-class telemetry entry (REL-093) — survive an abrupt process
// kill (power-pull) without a torn/partial write ever surfacing as valid
// state on reopen. A ":memory:" store carries no such durability and skips
// the file-only journal_mode assertion.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("identity: create dir %s: %w", dir, err)
			}
		}
		// modernc.org/sqlite runs each `_pragma` on every new connection, so
		// the write-discipline PRAGMAs hold even if database/sql retires and
		// re-dials the pooled connection under SetMaxOpenConns(1).
		dsn = "file:" + path +
			"?_pragma=journal_mode(WAL)" +
			"&_pragma=synchronous(FULL)" +
			"&_pragma=busy_timeout(5000)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("identity: open %s: %w", path, err)
	}

	// The relay is a single process talking to its own local file; one
	// connection avoids modernc.org/sqlite's known SQLITE_BUSY surface
	// under concurrent writers on the same *os.File handle.
	db.SetMaxOpenConns(1)

	if path != ":memory:" {
		if err := verifyWriteDiscipline(db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("identity: %w", err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: create schema: %w", err)
	}
	if _, err := db.Exec(telemetrySchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: create telemetry schema: %w", err)
	}
	if _, err := db.Exec(playerSessionSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: create player-session schema: %w", err)
	}

	// Column migrations run AFTER every CREATE TABLE IF NOT EXISTS above:
	// each one exists precisely because that statement is a no-op against a
	// table an earlier build already created, so each must inspect the table
	// as it now stands.
	if err := migrateTelemetrySchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: migrate telemetry schema: %w", err)
	}
	if err := migrateLastAppliedSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: migrate last-applied schema: %w", err)
	}

	return &Store{db: db}, nil
}

// verifyWriteDiscipline confirms the §10 crash-safety PRAGMAs actually took
// on the connection — a silently-ignored journal_mode or synchronous setting
// would leave the store one power-pull away from a torn write, so Open fails
// loudly rather than run undurably.
func verifyWriteDiscipline(db *sql.DB) error {
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		return fmt.Errorf("read journal_mode: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("journal_mode = %q, want wal", journal)
	}

	var synchronous int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return fmt.Errorf("read synchronous: %w", err)
	}
	if synchronous != 2 { // 2 == FULL
		return fmt.Errorf("synchronous = %d, want 2 (FULL)", synchronous)
	}

	var busyTimeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		return fmt.Errorf("read busy_timeout: %w", err)
	}
	if busyTimeout != 5000 {
		return fmt.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}
	return nil
}

// migrateLastAppliedSchema brings a last_applied_generation created by an
// earlier build up to the current column set, exactly as migrateTelemetrySchema
// does for the telemetry queue and for the same reason: `CREATE TABLE IF NOT
// EXISTS` is a no-op against an existing table, so a relay that has been
// running since before `revoked` existed would keep a four-column row and fail
// every ApplyGeneration — no generation applying at all, which is a harder
// outage than the one the column exists to fix.
//
// The added column defaults to the REL-060 empty placeholder, so a relay
// upgraded mid-life comes up revoking nothing until its next pull restates the
// set. That is the correct default and not a silent un-revocation: a relay on
// the old build was not enforcing a persisted revocation at all (it had nowhere
// to persist one), so there is no prior durable statement for the migration to
// lose.
func migrateLastAppliedSchema(db *sql.DB) error {
	has, err := hasColumn(db, "last_applied_generation", "revoked")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE last_applied_generation ADD COLUMN revoked BLOB NOT NULL DEFAULT '[]'`); err != nil {
		return fmt.Errorf("add last_applied_generation.revoked: %w", err)
	}
	return nil
}

// hasColumn reports whether table already carries column. It is a PRAGMA read
// rather than a blind `ALTER TABLE ... ADD COLUMN` whose "duplicate column"
// error is swallowed: swallowing an error class to make a statement idempotent
// also swallows the unrelated failures that share it.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	// PRAGMA table_info takes no bound parameter, and both call sites pass a
	// package-constant table name — never anything a caller or a wire message
	// could reach.
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate %s columns: %w", table, err)
	}
	return found, nil
}

// Checkpoint flushes the WAL back into the main database file and truncates
// the -wal sidecar (PRAGMA wal_checkpoint(TRUNCATE)), bounding the WAL's
// on-disk growth over the appliance's life (§10). It is safe to call at any
// time; Close calls it on a clean shutdown.
func (s *Store) Checkpoint() error {
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("identity: checkpoint: %w", err)
	}
	return nil
}

// Close checkpoints the WAL (best-effort, so a clean shutdown leaves no
// unbounded sidecar) and closes the underlying database handle. Committed
// state is already durable via WAL before Close is ever reached (§10) — the
// checkpoint only bounds the WAL, it is not what makes writes durable.
func (s *Store) Close() error {
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
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

// ApplyGeneration persists the relay's last-applied desired-state generation
// number, section hash, the applied screen_programs array AND that generation's
// `revocation_and_site.revoked` set as ONE atomic row write (REL-055/056): the
// generation swap completes entirely against either the prior or the new
// generation, never a torn cross-generation mix where last-applied reports the
// new {generation, hash} while the served screen_programs — or the revocation
// set enforced against every credential decision — are still the prior
// generation's.
//
// This is the single apply-unit primitive the desired-state apply path
// (internal/relay/desiredstate.VerifyAndApply) MUST use instead of a separate
// SetLastAppliedGeneration + SetServedScreenPrograms pair: those are two
// distinct, un-transacted row writes, so a process killed between their two
// commits (a power-pull) surfaces exactly the torn state REL-056 forbids — the
// relay believing itself caught up on the new generation while still serving
// the old generation's screen_programs, silently never applying an emergency
// preempt/blank the new generation carried. Because all four fields ride a
// single INSERT … ON CONFLICT statement against the singleton row, SQLite's
// own statement-level atomicity (under the store's WAL + synchronous=FULL write
// discipline) guarantees they commit together or not at all — there is no
// intermediate row state where generation, screen_programs and revoked
// disagree.
//
// # Why `revoked` rides this same row
//
// REL-123 requires a SYNCED revocation be enforced "regardless of
// connectivity", and REL-073's rule for the trust anchor — persisted beside
// `{generation, hash}` so both "survive a power cycle and remain usable for
// offline verification without contacting the app peer" — is the same rule
// applied to the same class of state. A revocation held only in the running
// process's memory is enforced only until the next restart, so a relay that
// reboots during an app-peer outage would come up serving its PERSISTED
// programs to its PERSISTED channel tokens with an EMPTY revocation set —
// precisely the window REL-123 exists to close. And it belongs in THIS write
// rather than a write of its own for the reason the other three do: a row where
// the generation says N while the revocation set is still N-1's is the same
// torn cross-generation mix, one credential decision wide.
//
// An empty or nil screenProgramsJSON or revokedJSON is stored as the REL-060
// empty placeholder (`[]`), matching SetServedScreenPrograms.
func (s *Store) ApplyGeneration(generation int64, hash string, screenProgramsJSON, revokedJSON []byte) error {
	if len(screenProgramsJSON) == 0 {
		screenProgramsJSON = []byte("[]")
	}
	// "null" as well as empty: a nil []string marshals to `null`, and while
	// that decodes back to a nil slice that every consumer already reads as
	// "this generation revokes nothing", storing the REL-060 placeholder keeps
	// the persisted row saying what the section says.
	if len(revokedJSON) == 0 || string(revokedJSON) == "null" {
		revokedJSON = []byte("[]")
	}
	_, err := s.db.Exec(
		`INSERT INTO last_applied_generation (id, generation, hash, screen_programs, revoked) VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET generation = excluded.generation, hash = excluded.hash, screen_programs = excluded.screen_programs, revoked = excluded.revoked`,
		generation, hash, screenProgramsJSON, revokedJSON,
	)
	if err != nil {
		return fmt.Errorf("identity: ApplyGeneration: %w", err)
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

// SetServedScreenPrograms persists screenProgramsJSON — the applied
// snapshot's `screen_programs` array (relay/1 REL-061), a JSON array of
// `{screen_id, program_revision, priority, display, content}` entries — as a
// facet of the last-applied generation record (REL-055), so a relay serves
// its last-applied screen program purely from durable local storage across a
// restart or an extended disconnection, without first contacting its app peer
// (Offline continuity, REL-055/061). These are the signed content POINTERS a
// screen resolves against its own origin (`{asset_ref, url, expires_at}`),
// never asset bytes (REL-140) — nothing here caches media, keeping the store
// within REL-142's operational-footprint scope.
//
// This is a facet of the singleton last-applied row, so a last-applied
// generation MUST already be persisted (SetLastAppliedGeneration) when this is
// called; in the desired-state apply path (internal/relay/desiredstate.VerifyAndApply)
// that ordering always holds. An empty or nil screenProgramsJSON is stored as
// the REL-060 empty placeholder (`[]`).
func (s *Store) SetServedScreenPrograms(screenProgramsJSON []byte) error {
	if len(screenProgramsJSON) == 0 {
		screenProgramsJSON = []byte("[]")
	}
	if _, err := s.db.Exec(
		`UPDATE last_applied_generation SET screen_programs = ? WHERE id = 1`,
		screenProgramsJSON,
	); err != nil {
		return fmt.Errorf("identity: SetServedScreenPrograms: %w", err)
	}
	return nil
}

// LastAppliedScreenPrograms returns the persisted last-applied
// `screen_programs` array as its raw JSON bytes (REL-061). A store that has
// never applied a generation — or one whose applied generation carried no
// screen_programs — returns the REL-060 empty placeholder (`[]`), never a nil
// or an error, so the offline serve path
// (internal/relay/desiredstate.ServedProgram) decodes cleanly to an empty
// program set rather than failing.
func (s *Store) LastAppliedScreenPrograms() ([]byte, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT screen_programs FROM last_applied_generation WHERE id = 1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return []byte("[]"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("identity: LastAppliedScreenPrograms: %w", err)
	}
	if len(raw) == 0 {
		return []byte("[]"), nil
	}
	return raw, nil
}

// LastAppliedRevokedScreens returns the persisted last-applied
// `revocation_and_site.revoked` set as its raw JSON bytes (REL-066) — the read
// half of the durable revocation state ApplyGeneration commits, and the exact
// counterpart of LastAppliedScreenPrograms. A store that has never applied a
// generation — or one whose applied generation revoked nothing — returns the
// REL-060 empty placeholder (`[]`), never a nil or an error, so the offline
// boot path (internal/relay/desiredstate.ServedRevocation) decodes cleanly to
// an empty set rather than failing.
//
// An empty result is the app peer positively stating that nothing is revoked,
// not an absence of information — REL-066 puts the key on every snapshot. What
// a caller MUST NOT do is treat "never applied a generation" as an empty
// revocation set it may act on: the relay in that state has synced nothing at
// all, and REL-123's own carve-out is that "a revocation the relay has not yet
// pulled is not yet enforceable". cmd/waiveo-relay only reaches this read on a
// boot that HAS a persisted last-applied generation (its offline-serve
// fallback is fatal without one), so the two cases never have to be told apart
// here.
func (s *Store) LastAppliedRevokedScreens() ([]byte, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT revoked FROM last_applied_generation WHERE id = 1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return []byte("[]"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("identity: LastAppliedRevokedScreens: %w", err)
	}
	if len(raw) == 0 {
		return []byte("[]"), nil
	}
	return raw, nil
}

// SetClockFloor persists ms as the relay's clock floor (REL-130) — the
// latest time value it has ever verified independently or previously
// observed as current — but ONLY if ms is strictly greater than whatever
// floor is currently persisted (or no floor has ever been persisted yet).
// This enforces REL-130/132's "advance only, never regress" rule at the
// store itself: an equal or lower value leaves the persisted floor
// untouched and reports advanced=false. The caller remains responsible for
// REL-132's independent-verification requirement (the store persists
// whatever value it's given, refusing only a non-advancing one).
func (s *Store) SetClockFloor(ms int64) (advanced bool, err error) {
	res, err := s.db.Exec(
		`INSERT INTO clock_floor (id, floor_ms) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET floor_ms = excluded.floor_ms WHERE excluded.floor_ms > clock_floor.floor_ms`,
		ms,
	)
	if err != nil {
		return false, fmt.Errorf("identity: SetClockFloor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("identity: SetClockFloor: %w", err)
	}
	return n > 0, nil
}

// ClockFloor returns the persisted clock floor in milliseconds (REL-130),
// and whether one has been persisted yet (SetClockFloor has never advanced
// the floor returns ok=false, not an error).
func (s *Store) ClockFloor() (ms int64, ok bool, err error) {
	err = s.db.QueryRow(`SELECT floor_ms FROM clock_floor WHERE id = 1`).Scan(&ms)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("identity: ClockFloor: %w", err)
	}
	return ms, true, nil
}

// SetAppPeerTrustPin persists (replacing any previously persisted value) the
// enrollment-anchored app-peer leaf SubjectPublicKeyInfo the relay pins its app
// peer's server certificate against (relay/1 REL-011 `trust_pin`, the REL-137
// key pin). In a co-located (loopback) deployment REL-011 lets this pin be
// learned at enrollment rather than provisioned out-of-band; a re-anchored key
// at re-enrollment (REL-017) replaces it. The pin is a public key's
// SubjectPublicKeyInfo, never a secret.
func (s *Store) SetAppPeerTrustPin(spki []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO app_peer_trust_pin (id, spki) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET spki = excluded.spki`,
		spki,
	)
	if err != nil {
		return fmt.Errorf("identity: SetAppPeerTrustPin: %w", err)
	}
	return nil
}

// AppPeerTrustPin returns the persisted app-peer trust pin (REL-011) and whether
// one has been learned yet (never persisted returns ok=false, not an error). The
// connect-time peer-cert validation (internal/relay/clocktrust, REL-136/137)
// compares the app peer's presented leaf SPKI against this pin, key-for-key.
func (s *Store) AppPeerTrustPin() (spki []byte, ok bool, err error) {
	err = s.db.QueryRow(`SELECT spki FROM app_peer_trust_pin WHERE id = 1`).Scan(&spki)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("identity: AppPeerTrustPin: %w", err)
	}
	return spki, true, nil
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
