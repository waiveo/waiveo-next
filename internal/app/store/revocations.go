package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Revocation is the authoring half of two security controls whose enforcement
// halves already exist and could not be invoked (`api/1` API-140).
//
// A screen's revocation rides the signed snapshot's
// `revocation_and_site.revoked` (`relay/1` REL-066), which a relay consults on
// every channel-token presentation (`player/1` PLY-072) — including while
// disconnected from its app peer, which is the whole point of carrying it in
// desired state rather than asking at use time.
//
// # Why revocations are their own table rather than a column on the screen row
//
// Because API-141 makes revocation an act distinct from deletion, and a column
// on the row would tie the two together: the store hard-deletes, so a revoked
// screen that was then deleted would take its revocation with it. A relay would
// stop refusing a token it should still refuse, on the strength of an operator
// tidying up a row.
//
// The separate table also says what the column could not: a revocation is a
// record of a decision, with its own instant and actor, and it outlives the row
// it concerns on purpose.

// revocationsDDL is the revoked-identity table.
//
// `subject_kind` distinguishes a screen from a relay certificate, so one table
// records both acts the joint revocation operation performs — they share an
// authorization floor, an audit shape and a confirmation step (API-140), and
// splitting the record would be the drift that decision exists to prevent.
const revocationsDDL = `
CREATE TABLE IF NOT EXISTS revocations (
	subject_kind TEXT NOT NULL,
	subject_id   TEXT NOT NULL,
	revoked_at   INTEGER NOT NULL,
	actor        TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (subject_kind, subject_id)
);
`

// Revocation subject kinds.
const (
	RevocationSubjectScreen = "screen"
	RevocationSubjectRelay  = "relay"
)

// RevokeSubject records a revocation. It is idempotent: revoking an
// already-revoked subject leaves the original `revoked_at` and actor in place.
//
// The first record is the one that matters. API-142 makes revocation terminal,
// so a second revocation of the same subject is not a new decision — and
// overwriting the instant would move the audit trail's own answer to "when did
// this stop being trusted", which is the question a later investigation asks.
func (s *Store) RevokeSubject(ctx context.Context, kind, subjectID, actor string) error {
	if kind != RevocationSubjectScreen && kind != RevocationSubjectRelay {
		return fmt.Errorf("store: revoke: unknown subject kind %q", kind)
	}
	if subjectID == "" {
		return fmt.Errorf("store: revoke: empty subject id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO revocations (subject_kind, subject_id, revoked_at, actor)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (subject_kind, subject_id) DO NOTHING`,
		kind, subjectID, s.nowMs(), actor)
	if err != nil {
		return fmt.Errorf("store: revoke %s %s: %w", kind, subjectID, err)
	}
	return nil
}

// IsRevoked reports whether a subject has been revoked.
func (s *Store) IsRevoked(ctx context.Context, kind, subjectID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM revocations WHERE subject_kind = ? AND subject_id = ?`, kind, subjectID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: is-revoked %s %s: %w", kind, subjectID, err)
	}
	return true, nil
}

// revokedSubjectsLocked reads every revoked subject id of one kind, in id order.
//
// Unexported and lock-free by contract: DesiredState calls it inside its own
// single read section, because a revocation read outside that section could be
// bound to a different generation than the rest of the snapshot — the exact
// stale-generation-with-fresher-content fault that method's own doc explains at
// length. A revocation is the one section where being one generation behind
// means a relay keeps honouring a credential an operator has just withdrawn.
func (s *Store) revokedSubjectsLocked(ctx context.Context, kind string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT subject_id FROM revocations WHERE subject_kind = ? ORDER BY subject_id ASC`, kind)
	if err != nil {
		return nil, fmt.Errorf("store: read revoked %ss: %w", kind, err)
	}
	defer rows.Close()
	// Non-nil and empty rather than nil: the snapshot section marshals it as
	// `[]`, never `null` (relay/1 REL-060).
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan revoked %s: %w", kind, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RevokedScreens returns every revoked screen id, in id order.
func (s *Store) RevokedScreens(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revokedSubjectsLocked(ctx, RevocationSubjectScreen)
}

// RevokedAt reports when a subject was revoked.
//
// It exists so the "first record wins" property RevokeSubject documents is
// OBSERVABLE: a property no consumer can read is one that will not survive a
// refactor, because nothing fails when it stops being true.
func (s *Store) RevokedAt(ctx context.Context, kind, subjectID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT revoked_at FROM revocations WHERE subject_kind = ? AND subject_id = ?`, kind, subjectID).Scan(&at)
	if err != nil {
		return 0, fmt.Errorf("store: revoked-at %s %s: %w", kind, subjectID, err)
	}
	return at, nil
}
