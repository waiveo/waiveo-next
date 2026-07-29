package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// reads.go holds the small, read-only store accessors security-model/1's own
// flows need in order to be OBSERVABLE rather than merely implemented.
//
// They are gathered here rather than scattered because they share one purpose,
// and it is a contract-driven one: several security-model/1 requirements are
// statements about state a caller must be able to CHECK — that a system-console
// principal holds no credential row (SEC-002), that a factory reset left no
// principal behind (SEC-121), that a persisted grant row cannot yield a
// redeemable code (SEC-051). A property nothing can observe is a property
// nothing can be honest about, so each of these exists to answer a question the
// contract poses, not to round out a CRUD surface.

// ---- readers this flow and the console binding need ------------------------

// FindPrincipalPasswordCredential returns principalID's live `password`
// credential. It is the by-principal counterpart to FindPasswordCredential's
// by-identifier lookup: a reset names WHO, a login names WHAT HANDLE.
func (s *Store) FindPrincipalPasswordCredential(ctx context.Context, principalID string) (CredentialRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return scanCredential(s.db.QueryRowContext(ctx,
		`SELECT credential_id, principal_id, kind, identifier, secret, created_at, last_used_at, revoked_at
		 FROM credentials WHERE principal_id = ? AND kind = ? AND revoked_at IS NULL`,
		principalID, CredentialPassword))
}

func (s *Store) findPrincipalPasswordCredentialTx(ctx context.Context, tx *sql.Tx, principalID string) (CredentialRow, error) {
	return scanCredential(tx.QueryRowContext(ctx,
		`SELECT credential_id, principal_id, kind, identifier, secret, created_at, last_used_at, revoked_at
		 FROM credentials WHERE principal_id = ? AND kind = ? AND revoked_at IS NULL`,
		principalID, CredentialPassword))
}

func scanCredential(row *sql.Row) (CredentialRow, error) {
	var c CredentialRow
	err := row.Scan(&c.CredentialID, &c.PrincipalID, &c.Kind, &c.Identifier, &c.Secret,
		&c.CreatedAt, &c.LastUsedAt, &c.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialRow{}, ErrCredentialNotFound
	}
	if err != nil {
		return CredentialRow{}, fmt.Errorf("auth: read credential: %w", err)
	}
	return c, nil
}

// Credentials returns every credential row principalID holds, revoked ones
// included — the read side SEC-020's "revocable through the same mechanism"
// needs, since a caller proving an API key was revoked must be able to see the
// revoked row rather than merely its absence from a live-only listing.
func (s *Store) Credentials(ctx context.Context, principalID string) ([]CredentialRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT credential_id, principal_id, kind, identifier, secret, created_at, last_used_at, revoked_at
		 FROM credentials WHERE principal_id = ? ORDER BY credential_id ASC`, principalID)
	if err != nil {
		return nil, fmt.Errorf("auth: list credentials: %w", err)
	}
	defer rows.Close()
	out := []CredentialRow{}
	for rows.Next() {
		var c CredentialRow
		if err := rows.Scan(&c.CredentialID, &c.PrincipalID, &c.Kind, &c.Identifier, &c.Secret,
			&c.CreatedAt, &c.LastUsedAt, &c.RevokedAt); err != nil {
			return nil, fmt.Errorf("auth: scan credential: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CredentialCount returns how many live credential rows principalID holds. It is
// what makes SEC-002's "system-console MUST be the sole principal kind with no
// corresponding credential row" an OBSERVABLE property rather than an asserted
// one: a caller can ask, rather than infer it from a write being refused.
func (s *Store) CredentialCount(ctx context.Context, principalID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM credentials WHERE principal_id = ? AND revoked_at IS NULL`,
		principalID).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: count credentials: %w", err)
	}
	return n, nil
}

// Grant reads one grant row by id — the metadata half of SEC-030's record. The
// code is NOT among the columns it can return: only CodeHash is persisted, so no
// reader of this store, however privileged, can recover a redeemable code
// (SEC-051).
func (s *Store) Grant(ctx context.Context, grantID string) (GrantRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		g          GrantRow
		labelsJSON string
		role       string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT grant_id, purpose, resulting_principal_kind, scope_node, labels, role,
		        ttl_ms, redemption_mode, max_redemptions, redemption_count,
		        consent_record, issued_via, issued_at, code_hash
		 FROM grants WHERE grant_id = ?`, grantID).
		Scan(&g.GrantID, &g.Purpose, &g.ResultingPrincipalKind, &g.ScopeNode, &labelsJSON, &role,
			&g.TTLMs, &g.RedemptionMode, &g.MaxRedemptions, &g.RedemptionCount,
			&g.ConsentRecord, &g.IssuedVia, &g.IssuedAt, &g.CodeHash)
	if errors.Is(err, sql.ErrNoRows) {
		return GrantRow{}, ErrGrantNotFound
	}
	if err != nil {
		return GrantRow{}, fmt.Errorf("auth: read grant: %w", err)
	}
	g.Role = Role(role)
	if labelsJSON != "" {
		_ = json.Unmarshal([]byte(labelsJSON), &g.Labels)
	}
	return g, nil
}

// CountPrincipals returns how many principal rows exist — the observable behind
// SEC-120's "no new owner principal was created" and SEC-121's "force fresh
// enrollment on every principal".
func (s *Store) CountPrincipals(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM principals`).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: count principals: %w", err)
	}
	return n, nil
}
