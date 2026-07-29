package auth

// devscriptkey.go is the store side of the repo's dev-script credential: the one
// function that mints (or re-confirms) the api-key a developer's local probes
// present to a running feeder. Its only caller is scripts/devkey; the read side,
// the on-disk discipline, and the argument for the role and scope it grants live
// in scripts/devcred's package doc, which is the single place that story is told.
//
// It exists here, beside the store, rather than in the script for one reason: it
// needs to find the dev principal again on a re-run, and principals are not
// indexed by display name on any exported read. A tool that could not find its
// own principal would create a fresh one every time it ran, leaving a growing
// roster of admin-bound identities behind on a box whose whole point is to be
// re-run constantly.
//
// It grants nothing a developer with write access to this SQLite file could not
// already grant themselves by writing the rows — the same reasoning SEC-076
// applies to the console binding's verbs. What it adds is a shape: one principal
// with one binding, and a key that is an ordinary revocable session row rather
// than a special case the revocation path does not know about.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const (
	// DevScriptPrincipalName is the display name the dev-script principal is
	// created under, and the key this function finds it again by.
	//
	// Its KIND is KindUser. SEC-001's closed set offers no generic automation
	// kind: `pack-service` is a pack's own identity and `ingest-token` a
	// telemetry writer, so labelling a developer's probe either would misreport
	// what it is everywhere the audit trail names a kind. `user` is the honest
	// answer — these are the developer's own scripted hands — and the display
	// name is what tells it apart from a person.
	DevScriptPrincipalName = "dev-script"

	// DevScriptKeyLabel is the PREFIX of the api-key credential's label, so an
	// operator listing credentials can see at a glance which rows are the dev
	// script's.
	//
	// It is a prefix and not the whole label because the label is not free-form:
	// MintAPIKey stores `<principal-id>:<label>` as the credential's identifier,
	// and a UNIQUE index covers (kind, identifier) with no exception for revoked
	// rows. A fixed label would therefore mint exactly once per principal and
	// then fail forever with a constraint violation — including on the very path
	// this tool exists to serve, re-provisioning after the key file is lost. Each
	// mint appends a fresh id, so the identifier is unique per mint and the
	// revoked rows stay as the honest record of what was issued.
	DevScriptKeyLabel = "dev-script"

	// DevScriptRole and DevScriptScopeNode are the single role binding the
	// dev-script principal holds. See scripts/devcred's package doc for why it
	// is exactly this much authority and not more.
	DevScriptRole      = RoleAdmin
	DevScriptScopeNode = RootScopeNode
)

// DevScriptKey is the outcome of EnsureDevScriptKey.
//
// Token is a live bearer credential. NOTHING may render it: not a log line, not
// an error, not a status message. Every other field is safe to print and is
// there so a provisioning tool can report what it did without reporting the
// secret it did it with.
type DevScriptKey struct {
	Token       string
	PrincipalID string
	SessionID   string
	Role        Role
	ScopeNode   string
	// Label is the credential label this key was minted under (empty when a
	// live key was reused rather than minted), so a tool can name the row an
	// operator would revoke.
	Label string
	// Reused reports that `presented` still named a live api-key session for
	// this principal, so no new key was minted and the file on disk is still
	// current.
	Reused bool
}

// EnsureDevScriptKey brings the dev-script principal, its role binding, and its
// api-key into the state scripts/devcred expects, and returns the key to present.
//
// presented is the token already on disk, or "" when there is none. It is an
// INPUT rather than something this function reads, because the file's location
// and its permission discipline belong to scripts/devcred and this package has
// no business growing a second opinion about either.
//
// The function is idempotent in the way that matters for a target run on every
// `make dev-up`: if presented still names a live api-key session belonging to
// this principal, it is returned unchanged. Otherwise every live session the
// principal holds is revoked BEFORE a fresh key is minted, so a stale copy of
// the old key — in a shell that exported it, in a backup of the file — stops
// authenticating the moment a new one exists, rather than accumulating into a
// set of live admin credentials nobody is tracking.
func (s *Store) EnsureDevScriptKey(ctx context.Context, presented string) (DevScriptKey, error) {
	principalID, err := s.ensureDevScriptPrincipal(ctx)
	if err != nil {
		return DevScriptKey{}, err
	}
	// Re-asserted on every call rather than only at creation: the binding is
	// what the key's authority actually IS, and a run that found the principal
	// but skipped the binding would hand back a key that authenticates and is
	// authorized for nothing (SEC-005's 403).
	if _, err := s.PutRoleBinding(ctx, principalID, DevScriptScopeNode, DevScriptRole); err != nil {
		return DevScriptKey{}, fmt.Errorf("auth: bind dev-script principal: %w", err)
	}

	if presented != "" {
		session, err := s.LookupSession(ctx, presented)
		if err == nil && session.PrincipalID == principalID && session.TokenKind == TokenKindAPIKey {
			return DevScriptKey{
				Token:       presented,
				PrincipalID: principalID,
				SessionID:   session.SessionID,
				Role:        DevScriptRole,
				ScopeNode:   DevScriptScopeNode,
				Reused:      true,
			}, nil
		}
	}

	if _, err := s.RevokePrincipalSessions(ctx, principalID); err != nil {
		return DevScriptKey{}, fmt.Errorf("auth: revoke prior dev-script keys: %w", err)
	}
	label := DevScriptKeyLabel + "-" + s.NewID()
	minted, err := s.MintAPIKey(ctx, principalID, label)
	if err != nil {
		return DevScriptKey{}, fmt.Errorf("auth: mint dev-script key: %w", err)
	}
	return DevScriptKey{
		Token:       minted.Token,
		PrincipalID: principalID,
		SessionID:   minted.Session.SessionID,
		Role:        DevScriptRole,
		ScopeNode:   DevScriptScopeNode,
		Label:       label,
	}, nil
}

// ensureDevScriptPrincipal returns the dev-script principal's id, creating the
// principal on first use. The lowest id wins if a previous tool run somehow left
// two, so repeated runs converge on one identity rather than alternating.
func (s *Store) ensureDevScriptPrincipal(ctx context.Context) (string, error) {
	id, err := s.findDevScriptPrincipal(ctx)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	p, err := s.CreatePrincipal(ctx, KindUser, DevScriptPrincipalName)
	if err != nil {
		return "", fmt.Errorf("auth: create dev-script principal: %w", err)
	}
	return p.PrincipalID, nil
}

// findDevScriptPrincipal is the read that no exported method offers: a lookup by
// (kind, display_name). It returns sql.ErrNoRows unwrapped so the caller can
// branch on absence without treating a real database failure as "not there yet".
func (s *Store) findDevScriptPrincipal(ctx context.Context) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT principal_id FROM principals
		 WHERE kind = ? AND display_name = ?
		 ORDER BY principal_id ASC LIMIT 1`,
		KindUser, DevScriptPrincipalName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("auth: find dev-script principal: %w", err)
	}
	return id, nil
}
