package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	_ "modernc.org/sqlite"
)

// schema is the security-model/1 relational model: principals, their SEPARATE
// credential relation (SEC-001/003), the revocable session relation that carries
// both browser sessions and API keys (SEC-020), role bindings (SEC-010), and the
// generic purpose-scoped grant (SEC-030).
//
// Two shapes here are contract requirements, not conveniences:
//
//   - `credentials` is its own table with a principal_id foreign key, NEVER a
//     password_hash column on `principals`. SEC-001 forbids the latter in as
//     many words, and SEC-003's "a principal MAY hold more than one credential,
//     including more than one of the same kind" is only representable this way.
//
//   - `sessions` carries API keys too, keyed by token_kind. SEC-020 requires an
//     API-key credential be "revocable through the same mechanism" as a session;
//     one relation with one revoked_at column IS that mechanism, where two
//     parallel tables would be two mechanisms that drift.
//
// Every id column is a ULID (DAT-005a). token_hash and code_hash are UNIQUE so a
// lookup by presented secret is a single indexed probe and a hash collision is a
// constraint violation rather than an ambiguous match.
const schema = `
CREATE TABLE IF NOT EXISTS principals (
	principal_id TEXT PRIMARY KEY,
	kind         TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS credentials (
	credential_id TEXT PRIMARY KEY,
	principal_id  TEXT NOT NULL,
	kind          TEXT NOT NULL,
	identifier    TEXT NOT NULL DEFAULT '',
	secret        TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	last_used_at  INTEGER,
	revoked_at    INTEGER
);
CREATE INDEX IF NOT EXISTS credentials_by_principal ON credentials(principal_id);
CREATE UNIQUE INDEX IF NOT EXISTS credentials_identifier
	ON credentials(kind, identifier) WHERE identifier <> '';

CREATE TABLE IF NOT EXISTS sessions (
	session_id      TEXT PRIMARY KEY,
	principal_id    TEXT NOT NULL,
	credential_id   TEXT NOT NULL DEFAULT '',
	token_kind      TEXT NOT NULL,
	token_hash      TEXT NOT NULL UNIQUE,
	csrf_token      TEXT NOT NULL DEFAULT '',
	aal             TEXT NOT NULL,
	device_metadata TEXT NOT NULL DEFAULT '{}',
	created_at      INTEGER NOT NULL,
	revoked_at      INTEGER
);
CREATE INDEX IF NOT EXISTS sessions_by_principal ON sessions(principal_id);

CREATE TABLE IF NOT EXISTS role_bindings (
	binding_id   TEXT PRIMARY KEY,
	principal_id TEXT NOT NULL,
	scope_node   TEXT NOT NULL,
	role         TEXT NOT NULL,
	created_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS role_bindings_unique
	ON role_bindings(principal_id, scope_node);

CREATE TABLE IF NOT EXISTS grants (
	grant_id                 TEXT PRIMARY KEY,
	purpose                  TEXT NOT NULL,
	resulting_principal_kind TEXT NOT NULL,
	scope_node               TEXT NOT NULL DEFAULT '',
	labels                   TEXT NOT NULL DEFAULT '{}',
	role                     TEXT NOT NULL DEFAULT '',
	ttl_ms                   INTEGER NOT NULL,
	redemption_mode          TEXT NOT NULL,
	max_redemptions          INTEGER NOT NULL DEFAULT 1,
	redemption_count         INTEGER NOT NULL DEFAULT 0,
	consent_record           TEXT NOT NULL DEFAULT '',
	issued_via               TEXT NOT NULL,
	issued_at                INTEGER NOT NULL,
	code_hash                TEXT NOT NULL UNIQUE
);
`

// Store is the security-model/1 persistence layer. It owns its own SQLite file
// (see the package doc for why it is not the authoring store's), serializes
// writes under mu, and reads nothing from a wall clock or a package-level id
// generator — nowMs and newID are injected at Open, the same seam
// internal/app/store and the api layer already use.
type Store struct {
	db    *sql.DB
	mu    sync.RWMutex
	nowMs func() int64
	newID func() string

	// onRevoke (OnRevoke) runs after every session this store revokes, with
	// that session's id. It is the seam EVT-114's live-stream teardown hangs
	// off: the events binding registers a hook that closes every open stream
	// authenticated by the revoked session. It is invoked AFTER the write
	// transaction commits and OUTSIDE the write lock, so a hook that itself
	// touches the store cannot deadlock — the same discipline
	// internal/app/store.OnCommit follows.
	hookMu   sync.Mutex
	onRevoke func(sessionID string)
}

// Open opens (creating if necessary) the auth store at dsn and migrates the
// schema. Pass ":memory:" for an ephemeral, test-only store or a filesystem path
// for a real deployment; the parent directory is created 0700, since everything
// under it is credential material.
//
// nowMs and newID are the injected clock and id source. newID MUST mint valid
// ULIDs (DAT-005a) — every insert checks, so a misconfigured source fails at the
// first write rather than persisting an id no other component can parse.
func Open(dsn string, nowMs func() int64, newID func() string) (*Store, error) {
	if nowMs == nil || newID == nil {
		return nil, errors.New("auth: Open requires a clock and an id source")
	}
	var conn string
	switch dsn {
	case ":memory:":
		conn = "file::memory:?_txlock=immediate"
	default:
		if dir := filepath.Dir(dsn); dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("auth: create dir %s: %w", dir, err)
			}
		}
		conn = "file:" + dsn +
			"?_txlock=immediate" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=synchronous(FULL)" +
			"&_pragma=busy_timeout(5000)"
	}

	db, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("auth: open %s: %w", dsn, err)
	}
	// One connection: keeps a ":memory:" store a single shared database and
	// keeps the driver off SQLITE_BUSY under concurrent writers, exactly as
	// internal/app/store does. The RWMutex is the logical serialization.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("auth: migrate: %w", err)
	}
	// A real deployment's auth database is credential material at rest even
	// though every secret in it is hashed: the session token hashes are
	// bearer-equivalent to anyone who can also reach the box's memory, and the
	// principal roster alone is reconnaissance. 0600 matches the discipline
	// internal/feeder/signing and internal/feeder/enroll already apply to key
	// material.
	if dsn != ":memory:" {
		if err := os.Chmod(dsn, 0o600); err != nil && !os.IsNotExist(err) {
			_ = db.Close()
			return nil, fmt.Errorf("auth: chmod %s: %w", dsn, err)
		}
	}
	return &Store{db: db, nowMs: nowMs, newID: newID}, nil
}

// OpenDefault opens the auth store at dsn with the wall clock and ulid.New.
func OpenDefault(dsn string) (*Store, error) {
	return Open(dsn, func() int64 { return time.Now().UnixMilli() }, ulid.New)
}

// Close closes the database handle, flushing the WAL best-effort first.
func (s *Store) Close() error {
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

// Now returns the store's injected clock reading — exposed so a handler
// timestamps an audit event or a lockout decision from the SAME clock the store
// stamps rows with, rather than reaching for time.Now beside it.
func (s *Store) Now() int64 { return s.nowMs() }

// NewID returns a fresh id from the store's injected id source, for a caller
// that must mint an id of its own (an audit event's envelope id) on the same
// seam.
func (s *Store) NewID() string { return s.newID() }

// writeTx runs fn inside a serialized immediate transaction: on any error
// nothing is persisted.
func (s *Store) writeTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	return nil
}

// mintID returns a fresh ULID from the injected source, refusing anything that
// is not a valid ULID (DAT-005a) rather than persisting it.
func (s *Store) mintID(what string) (string, error) {
	id := s.newID()
	if !ulid.Valid(id) {
		return "", fmt.Errorf("auth: %s id %q is not a valid ULID (DAT-005a)", what, id)
	}
	return id, nil
}

// ---- principals -----------------------------------------------------------

// Principal row (security-model/1 Wire shapes: Principal).
type PrincipalRow struct {
	PrincipalID string `json:"principal_id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// ErrPrincipalNotFound is returned when a principal id names no row.
var ErrPrincipalNotFound = errors.New("auth: principal not found")

// CreatePrincipal inserts a principal of the given kind and returns it. kind
// MUST be one of SEC-001's six values.
func (s *Store) CreatePrincipal(ctx context.Context, kind, displayName string) (PrincipalRow, error) {
	if !ValidPrincipalKind(kind) {
		return PrincipalRow{}, fmt.Errorf("auth: %q is not a principal kind (SEC-001)", kind)
	}
	var out PrincipalRow
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.CreatePrincipalTx(ctx, tx, kind, displayName)
		return err
	}); err != nil {
		return PrincipalRow{}, err
	}
	return out, nil
}

// CreatePrincipalTx is CreatePrincipal's transactional body, exported so a
// caller composing several writes into ONE atomic act — first-boot claim, which
// must create a principal, a credential and an owner binding inside the same
// transaction that consumes the setup grant (SEC-036/120) — can do so without a
// nested transaction. The store's write path serializes under a non-reentrant
// mutex, so calling the exported CreatePrincipal from inside a transaction would
// deadlock; these Tx variants are the supported way to compose.
func (s *Store) CreatePrincipalTx(ctx context.Context, tx *sql.Tx, kind, displayName string) (PrincipalRow, error) {
	if !ValidPrincipalKind(kind) {
		return PrincipalRow{}, fmt.Errorf("auth: %q is not a principal kind (SEC-001)", kind)
	}
	id, err := s.mintID("principal")
	if err != nil {
		return PrincipalRow{}, err
	}
	now := s.nowMs()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO principals (principal_id, kind, display_name, created_at) VALUES (?, ?, ?, ?)`,
		id, kind, displayName, now); err != nil {
		return PrincipalRow{}, fmt.Errorf("auth: create principal: %w", err)
	}
	return PrincipalRow{PrincipalID: id, Kind: kind, DisplayName: displayName, CreatedAt: now}, nil
}

// GetPrincipal returns the principal with id, or ErrPrincipalNotFound.
func (s *Store) GetPrincipal(ctx context.Context, id string) (PrincipalRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var p PrincipalRow
	err := s.db.QueryRowContext(ctx,
		`SELECT principal_id, kind, display_name, created_at FROM principals WHERE principal_id = ?`, id).
		Scan(&p.PrincipalID, &p.Kind, &p.DisplayName, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PrincipalRow{}, ErrPrincipalNotFound
	}
	if err != nil {
		return PrincipalRow{}, fmt.Errorf("auth: get principal: %w", err)
	}
	return p, nil
}

// ---- credentials ----------------------------------------------------------

// CredentialRow is one row of the credential relation (SEC-003). Secret is
// NEVER a raw credential value: for a password it is the Argon2id PHC string,
// for an API key the SHA-256 of the token. It is unexported from every wire
// shape this package returns — security-model/1's own Credential wire shape is
// metadata only.
type CredentialRow struct {
	CredentialID string
	PrincipalID  string
	Kind         string
	Identifier   string
	Secret       string
	CreatedAt    int64
	LastUsedAt   *int64
	RevokedAt    *int64
}

// ErrCredentialNotFound is returned when no credential matches a lookup.
var ErrCredentialNotFound = errors.New("auth: credential not found")

// PutPasswordCredential attaches a `password` credential to principalID,
// replacing any password credential that principal already holds. identifier is
// the login handle the credential is presented under and is unique across all
// password credentials.
//
// A `system-console` principal is refused: SEC-002 makes it the sole kind with
// no credential row, since admission over the console binding is itself its
// proof of identity. Enforcing that here — at the one place a credential is
// created — is what keeps it true regardless of which caller asks.
func (s *Store) PutPasswordCredential(ctx context.Context, principalID, identifier, password string) (CredentialRow, error) {
	if identifier == "" {
		return CredentialRow{}, errors.New("auth: a password credential needs an identifier")
	}
	p, err := s.GetPrincipal(ctx, principalID)
	if err != nil {
		return CredentialRow{}, err
	}
	if p.Kind == KindSystemConsole {
		return CredentialRow{}, errors.New("auth: system-console carries no credential row (SEC-002)")
	}
	var out CredentialRow
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.PutPasswordCredentialTx(ctx, tx, principalID, identifier, password)
		return err
	}); err != nil {
		return CredentialRow{}, err
	}
	return out, nil
}

// PutPasswordCredentialTx is PutPasswordCredential's transactional body — see
// CreatePrincipalTx for why the Tx variants exist. It does NOT re-check the
// principal's kind: the exported wrapper does that, and the one other caller
// (first-boot claim) has just created the principal itself in the same
// transaction and knows its kind.
func (s *Store) PutPasswordCredentialTx(ctx context.Context, tx *sql.Tx, principalID, identifier, password string) (CredentialRow, error) {
	if identifier == "" {
		return CredentialRow{}, errors.New("auth: a password credential needs an identifier")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return CredentialRow{}, err
	}
	id, err := s.mintID("credential")
	if err != nil {
		return CredentialRow{}, err
	}
	now := s.nowMs()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM credentials WHERE principal_id = ? AND kind = ?`,
		principalID, CredentialPassword); err != nil {
		return CredentialRow{}, fmt.Errorf("auth: replace password credential: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credentials (credential_id, principal_id, kind, identifier, secret, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, principalID, CredentialPassword, identifier, hash, now); err != nil {
		return CredentialRow{}, fmt.Errorf("auth: put password credential: %w", err)
	}
	return CredentialRow{
		CredentialID: id, PrincipalID: principalID, Kind: CredentialPassword,
		Identifier: identifier, Secret: hash, CreatedAt: now,
	}, nil
}

// FindPasswordCredential returns the live `password` credential registered under
// identifier, or ErrCredentialNotFound. A revoked credential is not returned.
func (s *Store) FindPasswordCredential(ctx context.Context, identifier string) (CredentialRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var c CredentialRow
	err := s.db.QueryRowContext(ctx,
		`SELECT credential_id, principal_id, kind, identifier, secret, created_at, last_used_at, revoked_at
		 FROM credentials WHERE kind = ? AND identifier = ? AND revoked_at IS NULL`,
		CredentialPassword, identifier).
		Scan(&c.CredentialID, &c.PrincipalID, &c.Kind, &c.Identifier, &c.Secret, &c.CreatedAt, &c.LastUsedAt, &c.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialRow{}, ErrCredentialNotFound
	}
	if err != nil {
		return CredentialRow{}, fmt.Errorf("auth: find password credential: %w", err)
	}
	return c, nil
}

// TouchCredential records a credential's last_used_at (the field
// security-model/1's Credential wire shape carries).
func (s *Store) TouchCredential(ctx context.Context, credentialID string) error {
	now := s.nowMs()
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE credentials SET last_used_at = ? WHERE credential_id = ?`, now, credentialID)
		return err
	})
}

// ---- sessions and API keys ------------------------------------------------

// SessionRow is one row of the session relation — SEC-020's required field set
// `{session_id, principal_id, device_metadata, aal, created_at, revoked_at}`
// plus the columns that make an API key ride the same relation (token_kind,
// credential_id) and the double-submit CSRF token (SEC-024). The token itself is
// never stored: TokenHash is SHA-256 over it.
type SessionRow struct {
	SessionID      string
	PrincipalID    string
	CredentialID   string
	TokenKind      string
	TokenHash      string
	CSRFToken      string
	AAL            AAL
	DeviceMetadata map[string]string
	CreatedAt      int64
	RevokedAt      *int64
}

// ErrSessionNotFound is returned when a token hash names no live session. It
// covers "no such session" and "revoked" alike on purpose — a caller must not be
// able to distinguish a revoked token from a never-issued one.
var ErrSessionNotFound = errors.New("auth: session not found")

// Minted is a freshly issued session or API key: the row, plus the RAW token
// (and, for a browser session, the CSRF token) — the only moment either exists
// in plaintext on this side.
type Minted struct {
	Session   SessionRow
	Token     string
	CSRFToken string
}

// MintSession issues a new session (or API key, per tokenKind) for principalID
// at the given assurance level, recording device metadata (SEC-020) and, for a
// browser session, a fresh double-submit CSRF token (SEC-024).
//
// credentialID links an API key back to its own `api-key` credential row
// (SEC-003), so revoking the session and revoking the credential are one act
// rather than two that can disagree. It is empty for a password-authenticated
// browser session, whose credential is the password row that was verified, not a
// row the session itself owns.
func (s *Store) MintSession(ctx context.Context, principalID, tokenKind, credentialID string, aal AAL, device map[string]string) (Minted, error) {
	if tokenKind != TokenKindSession && tokenKind != TokenKindAPIKey {
		return Minted{}, fmt.Errorf("auth: unknown token kind %q", tokenKind)
	}
	if _, err := s.GetPrincipal(ctx, principalID); err != nil {
		return Minted{}, err
	}
	token, err := MintToken(tokenKind, aal)
	if err != nil {
		return Minted{}, err
	}
	var csrf string
	if tokenKind == TokenKindSession {
		// The CSRF token is a second independent secret, not derived from the
		// session token: it is deliberately readable by the page's own
		// JavaScript (non-HttpOnly cookie), so deriving it from the session
		// token would hand that same JavaScript a path back to the session
		// token it must never see.
		if csrf, err = MintGrantCode(); err != nil {
			return Minted{}, err
		}
	}
	id, err := s.mintID("session")
	if err != nil {
		return Minted{}, err
	}
	if device == nil {
		device = map[string]string{}
	}
	deviceJSON, err := json.Marshal(device)
	if err != nil {
		return Minted{}, fmt.Errorf("auth: encode device metadata: %w", err)
	}
	now := s.nowMs()
	row := SessionRow{
		SessionID: id, PrincipalID: principalID, CredentialID: credentialID,
		TokenKind: tokenKind, TokenHash: HashToken(token), CSRFToken: csrf,
		AAL: aal, DeviceMetadata: device, CreatedAt: now,
	}
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (session_id, principal_id, credential_id, token_kind, token_hash,
			                       csrf_token, aal, device_metadata, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.SessionID, row.PrincipalID, row.CredentialID, row.TokenKind, row.TokenHash,
			row.CSRFToken, string(row.AAL), string(deviceJSON), row.CreatedAt)
		return err
	}); err != nil {
		return Minted{}, fmt.Errorf("auth: mint session: %w", err)
	}
	return Minted{Session: row, Token: token, CSRFToken: csrf}, nil
}

// LookupSession resolves a presented raw token to its live session row, or
// ErrSessionNotFound. It rejects a token whose self-carried AAL claim (SEC-021)
// disagrees with the stored row — which cannot normally happen, since the stored
// hash covers the AAL segment, and is checked anyway so a future change to the
// token format that broke that binding fails closed instead of silently
// promoting a recovery token to standard.
func (s *Store) LookupSession(ctx context.Context, token string) (SessionRow, error) {
	kind, tokenAAL, ok := ParseToken(token)
	if !ok {
		return SessionRow{}, ErrSessionNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		row        SessionRow
		aal        string
		deviceJSON string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id, principal_id, credential_id, token_kind, token_hash, csrf_token,
		        aal, device_metadata, created_at, revoked_at
		 FROM sessions WHERE token_hash = ? AND revoked_at IS NULL`, HashToken(token)).
		Scan(&row.SessionID, &row.PrincipalID, &row.CredentialID, &row.TokenKind, &row.TokenHash,
			&row.CSRFToken, &aal, &deviceJSON, &row.CreatedAt, &row.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, ErrSessionNotFound
	}
	if err != nil {
		return SessionRow{}, fmt.Errorf("auth: lookup session: %w", err)
	}
	row.AAL = AAL(aal)
	if row.TokenKind != kind || row.AAL != tokenAAL {
		return SessionRow{}, ErrSessionNotFound
	}
	if deviceJSON != "" {
		_ = json.Unmarshal([]byte(deviceJSON), &row.DeviceMetadata)
	}
	return row, nil
}

// RevokeSession marks a session revoked at the store's current clock reading,
// and — when the session owns an `api-key` credential row — revokes that row in
// the SAME transaction, so SEC-020's "revocable through the same mechanism" is
// one atomic act rather than two writes that can half-apply.
//
// Revoking an already-revoked session is a no-op, not an error: revocation is
// the one operation that must never fail for being redundant.
func (s *Store) RevokeSession(ctx context.Context, sessionID string) error {
	if err := s.revokeSessionTx(ctx, sessionID); err != nil {
		return err
	}
	s.hookMu.Lock()
	hook := s.onRevoke
	s.hookMu.Unlock()
	if hook != nil {
		hook(sessionID)
	}
	return nil
}

// OnRevoke registers fn to run after every committed session revocation, with
// the revoked session's id — the one seam an events/1 binding needs to tear down
// the streams that session authenticated (EVT-114). Passing nil clears the hook.
// Intended to be set once at startup, before the listener accepts requests;
// guarded so a concurrent revocation observes either the old or the new hook,
// never a torn value.
func (s *Store) OnRevoke(fn func(sessionID string)) {
	s.hookMu.Lock()
	s.onRevoke = fn
	s.hookMu.Unlock()
}

// revokeSessionTx is RevokeSession's transactional body, split out so the
// post-commit hook above runs outside the write lock.
func (s *Store) revokeSessionTx(ctx context.Context, sessionID string) error {
	now := s.nowMs()
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var credentialID string
		err := tx.QueryRowContext(ctx,
			`SELECT credential_id FROM sessions WHERE session_id = ?`, sessionID).Scan(&credentialID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET revoked_at = ? WHERE session_id = ? AND revoked_at IS NULL`,
			now, sessionID); err != nil {
			return err
		}
		if credentialID != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE credentials SET revoked_at = ? WHERE credential_id = ? AND revoked_at IS NULL`,
				now, credentialID); err != nil {
				return err
			}
		}
		return nil
	})
}

// RevokePrincipalSessions revokes every live session and API key belonging to
// principalID, returning the ids it revoked. This is the bulk form SEC-053's
// credential-reset default needs and the one a "sign out everywhere" control
// calls; the reset flow itself is a follow-up.
func (s *Store) RevokePrincipalSessions(ctx context.Context, principalID string) ([]string, error) {
	ids, err := s.ListSessionIDs(ctx, principalID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := s.RevokeSession(ctx, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// ListSessionIDs returns every LIVE session id belonging to principalID.
func (s *Store) ListSessionIDs(ctx context.Context, principalID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id FROM sessions WHERE principal_id = ? AND revoked_at IS NULL ORDER BY session_id ASC`,
		principalID)
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("auth: scan session: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MintAPIKey creates an `api-key` credential row (SEC-003) AND the session row
// that makes it revocable (SEC-020), linked to each other, and returns the raw
// key. label is carried as the credential's identifier so an operator can tell
// two keys apart in a list; it is namespaced with the principal id so two
// principals may both have a key labelled "cli".
func (s *Store) MintAPIKey(ctx context.Context, principalID, label string) (Minted, error) {
	p, err := s.GetPrincipal(ctx, principalID)
	if err != nil {
		return Minted{}, err
	}
	if p.Kind == KindSystemConsole {
		return Minted{}, errors.New("auth: system-console carries no credential row (SEC-002)")
	}
	credentialID, err := s.mintID("credential")
	if err != nil {
		return Minted{}, err
	}
	now := s.nowMs()
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO credentials (credential_id, principal_id, kind, identifier, secret, created_at)
			 VALUES (?, ?, ?, ?, '', ?)`,
			credentialID, principalID, CredentialAPIKey, principalID+":"+label, now)
		return err
	}); err != nil {
		return Minted{}, fmt.Errorf("auth: create api-key credential: %w", err)
	}
	minted, err := s.MintSession(ctx, principalID, TokenKindAPIKey, credentialID, AALStandard, map[string]string{"label": label})
	if err != nil {
		return Minted{}, err
	}
	// The credential row's own secret column carries the same token hash the
	// session row does, so the credential relation is self-describing (SEC-003:
	// a credential is "a provable means of acting as a principal") rather than
	// an empty shell that only points at a session.
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE credentials SET secret = ? WHERE credential_id = ?`,
			minted.Session.TokenHash, credentialID)
		return err
	}); err != nil {
		return Minted{}, fmt.Errorf("auth: link api-key credential: %w", err)
	}
	return minted, nil
}

// ---- role bindings --------------------------------------------------------

// ErrLastOwnerBinding is returned by DeleteRoleBinding when the binding named is
// the deployment's last remaining `owner` binding (SEC-011).
var ErrLastOwnerBinding = errors.New("auth: the last owner role-binding is not deletable through api/1 (SEC-011)")

// PutRoleBinding binds principalID to role at scopeNode, replacing whatever role
// that principal held at exactly that node (the unique index makes one binding
// per (principal, node) the only representable state).
func (s *Store) PutRoleBinding(ctx context.Context, principalID, scopeNode string, role Role) (Binding, error) {
	if !ValidRole(role) {
		return Binding{}, fmt.Errorf("auth: %q is not a role (SEC-010)", role)
	}
	if scopeNode == "" {
		return Binding{}, errors.New("auth: a role binding needs a scope node")
	}
	if _, err := s.GetPrincipal(ctx, principalID); err != nil {
		return Binding{}, err
	}
	var out Binding
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.PutRoleBindingTx(ctx, tx, principalID, scopeNode, role)
		return err
	}); err != nil {
		return Binding{}, err
	}
	return out, nil
}

// PutRoleBindingTx is PutRoleBinding's transactional body — see CreatePrincipalTx
// for why the Tx variants exist.
func (s *Store) PutRoleBindingTx(ctx context.Context, tx *sql.Tx, principalID, scopeNode string, role Role) (Binding, error) {
	if !ValidRole(role) {
		return Binding{}, fmt.Errorf("auth: %q is not a role (SEC-010)", role)
	}
	if scopeNode == "" {
		return Binding{}, errors.New("auth: a role binding needs a scope node")
	}
	id, err := s.mintID("role binding")
	if err != nil {
		return Binding{}, err
	}
	now := s.nowMs()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM role_bindings WHERE principal_id = ? AND scope_node = ?`,
		principalID, scopeNode); err != nil {
		return Binding{}, fmt.Errorf("auth: replace role binding: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO role_bindings (binding_id, principal_id, scope_node, role, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, principalID, scopeNode, string(role), now); err != nil {
		return Binding{}, fmt.Errorf("auth: put role binding: %w", err)
	}
	return Binding{BindingID: id, PrincipalID: principalID, ScopeNode: scopeNode, Role: role, CreatedAt: now}, nil
}

// DeleteRoleBinding removes a role binding, refusing with ErrLastOwnerBinding
// when it is the deployment's last `owner` binding.
//
// SEC-011: "A deployment MUST always retain at least one owner-role principal in
// a claimed state: the last remaining owner role-binding MUST NOT be deletable
// through ordinary api/1 mutation, only through factory reset." The check and
// the delete run in ONE transaction, so two concurrent deletions of the last two
// owner bindings cannot both observe "there are two, mine is safe" and leave the
// deployment ownerless — the same race SEC-036 closes for grant redemption,
// closed the same way.
func (s *Store) DeleteRoleBinding(ctx context.Context, bindingID string) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var role string
		err := tx.QueryRowContext(ctx,
			`SELECT role FROM role_bindings WHERE binding_id = ?`, bindingID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // deleting an absent binding is already the desired state
		}
		if err != nil {
			return err
		}
		if Role(role) == RoleOwner {
			var owners int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM role_bindings WHERE role = ?`, string(RoleOwner)).Scan(&owners); err != nil {
				return err
			}
			if owners <= 1 {
				return ErrLastOwnerBinding
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM role_bindings WHERE binding_id = ?`, bindingID)
		return err
	})
}

// RoleBindings returns every binding held by principalID.
func (s *Store) RoleBindings(ctx context.Context, principalID string) ([]Binding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT binding_id, principal_id, scope_node, role, created_at
		 FROM role_bindings WHERE principal_id = ? ORDER BY binding_id ASC`, principalID)
	if err != nil {
		return nil, fmt.Errorf("auth: list role bindings: %w", err)
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		var b Binding
		var role string
		if err := rows.Scan(&b.BindingID, &b.PrincipalID, &b.ScopeNode, &role, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("auth: scan role binding: %w", err)
		}
		b.Role = Role(role)
		out = append(out, b)
	}
	return out, rows.Err()
}

// CountOwnerBindings returns how many `owner` bindings exist across the whole
// deployment. Zero means the box is unclaimed — the condition that opens the
// first-boot claim window (SEC-120).
func (s *Store) CountOwnerBindings(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM role_bindings WHERE role = ?`, string(RoleOwner)).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: count owner bindings: %w", err)
	}
	return n, nil
}
