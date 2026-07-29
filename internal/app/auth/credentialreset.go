package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// credentialreset.go is the routine credential-reset flow security-model/1
// SEC-050-053 defines: an `admin` mints a `credential-reset` grant for a `user`
// principal, hands the target a one-time code (or URL), and the TARGET — never
// the admin — chooses the credential value when they redeem it.
//
// # The shape IS the requirement
//
// SEC-050 says the issuing admin "MUST NOT be shown, and MUST have no path to
// choose, the credential value the target user eventually sets." That is a
// statement about an API's SHAPE, not about a runtime check, and it is
// implemented as one here: CredentialResetOptions carries no credential-bearing
// field, CredentialResetHandoff returns none, and the only function anywhere in
// this flow that accepts a credential value is RedeemCredentialResetGrant, which
// is reached with the one-time code the target holds. There is deliberately no
// "admin sets the password directly" convenience path to be tempted by later —
// adding one would not be a policy regression, it would be a new function
// somebody has to write and defend.
//
// SEC-051 adds that no step may place secret material "in a process's
// command-line arguments, in a location a shell history file would capture, in
// `ps` output, or in a journald-logged line." Two properties here carry that:
// the grant's raw code exists in exactly one place (the value returned to the
// caller) and is persisted only as HashToken(code) — a `grants` row cannot yield
// a redeemable code however it is read — and this package's audit records name a
// grant by its `grant_id`, never by its code.
//
// SEC-053's default — redemption revokes every existing session and API-key
// credential of the target — runs INSIDE the redemption transaction, so a reset
// whose credential write failed cannot have evicted the target's sessions, and a
// reset that evicted them cannot fail to have set the new credential.

// CredentialResetOptions parameterizes issuance. It carries no credential value
// and no way to specify one; see the file doc for why that is load-bearing
// rather than an oversight (SEC-050).
type CredentialResetOptions struct {
	// KeepExistingSessions is SEC-053's explicit, per-issuance opt-out from the
	// revoke-everything default: "the issuing operator MAY explicitly opt out of
	// this default for a single issuance." It is a per-issuance field rather than
	// a deployment setting for exactly that reason — SEC-053 scopes the opt-out
	// to one issuance, and a configurable default would quietly turn the
	// post-takeover eviction property off for every reset.
	KeepExistingSessions bool
	// BaseURL, when set, is the origin the returned one-time URL is formed
	// against. Empty yields a handoff carrying only the code, which SEC-050
	// admits ("a one-time code or URL").
	BaseURL string
	// TTLMs overrides SEC-032's ~15-minute default for this purpose.
	TTLMs int64
}

// CredentialResetHandoff is what the ISSUING ADMIN receives. Every field is
// either an identifier or the one-time code itself; none is, or can become, the
// credential value the target eventually sets (SEC-050).
type CredentialResetHandoff struct {
	GrantID string `json:"grant_id"`
	// Code is the one-time code SEC-050 returns for the admin to hand over. It
	// identifies a grant; it "is not itself a rendered credential" (SEC-051).
	Code string `json:"code"`
	// URL is the same code in link form, or "" when no BaseURL was supplied.
	URL string `json:"url,omitempty"`
	// TargetPrincipalID names who the reset is for — an identifier, so an admin
	// can confirm they are resetting the account they meant to.
	TargetPrincipalID string `json:"target_principal_id"`
	ExpiresAtMs       int64  `json:"expires_at_ms"`
}

// CredentialResetRedemption is what redemption produced, for the caller that
// must record or report it.
type CredentialResetRedemption struct {
	GrantID           string
	TargetPrincipalID string
	// RevokedSessionIDs lists every session and API key SEC-053's default
	// revoked. It is empty when the issuance opted out, and empty (not nil-vs-
	// empty ambiguous) when the target simply held none.
	RevokedSessionIDs []string
	// SessionsRevoked distinguishes "the default ran and found nothing" from
	// "the issuance opted out", which the id list alone cannot.
	SessionsRevoked bool
}

// Credential-reset failures.
var (
	// ErrResetTargetNotUser is SEC-012's subject restriction: a credential-reset
	// grant is issued "for any `user` principal". A `screen`, `relay` or
	// `pack-service` principal authenticates by a mechanism this flow does not
	// reset, and minting a grant that could never be redeemed usefully would be a
	// live code with no legitimate redemption.
	ErrResetTargetNotUser = errors.New("auth: a credential-reset grant targets a `user` principal (SEC-012)")
	// ErrResetNotAdmin is SEC-012's issuer restriction.
	ErrResetNotAdmin = errors.New("auth: issuing a credential-reset grant requires at least the `admin` role (SEC-012)")
	// ErrResetIdentifierUnknown is returned when the target holds no password
	// credential to reset. This flow resets an EXISTING login handle rather than
	// inventing one, since the identifier is what the target types.
	ErrResetIdentifierUnknown = errors.New("auth: the target principal holds no password credential to reset")
)

// resetLabelTarget and resetLabelRevoke are the grant labels (SEC-030's
// `labels`) this flow carries its own two facts in. They ride ON the grant
// rather than in a side table because SEC-053's opt-out is a property of the
// ISSUANCE and must be honored by whichever process serves the redemption —
// possibly a different process, after a restart.
const (
	resetLabelTarget = "target_principal_id"
	resetLabelRevoke = "revoke_sessions"
)

// IssueCredentialResetGrant is SEC-050's issuance: it mints a
// `credential-reset`-purpose grant for targetPrincipalID and returns the
// one-time handoff. issuingPrincipalID must hold at least `admin` somewhere in
// the tree (SEC-012).
//
// It does not touch the target's credentials at all. That is the point: after
// this call the target's existing password still works and their TOTP
// enrollment is untouched (SEC-052) — nothing has changed except that a live,
// short-lived, single-use code now exists.
func (s *Store) IssueCredentialResetGrant(ctx context.Context, issuingPrincipalID, targetPrincipalID string, opt CredentialResetOptions) (CredentialResetHandoff, error) {
	if err := s.requireAdmin(ctx, issuingPrincipalID); err != nil {
		return CredentialResetHandoff{}, err
	}
	target, err := s.GetPrincipal(ctx, targetPrincipalID)
	if err != nil {
		return CredentialResetHandoff{}, err
	}
	if target.Kind != KindUser {
		return CredentialResetHandoff{}, ErrResetTargetNotUser
	}
	if _, err := s.FindPrincipalPasswordCredential(ctx, targetPrincipalID); err != nil {
		return CredentialResetHandoff{}, ErrResetIdentifierUnknown
	}
	ttl := opt.TTLMs
	if ttl <= 0 {
		ttl = DefaultResetGrantTTLMs
	}
	minted, err := s.MintGrant(ctx, MintGrantOptions{
		Purpose:                PurposeCredentialReset,
		ResultingPrincipalKind: KindUser,
		TTLMs:                  ttl,
		RedemptionMode:         RedemptionOneTime, // SEC-031
		IssuedVia:              IssuedViaAPI,      // SEC-030; the console path passes IssuedViaConsole
		Labels: map[string]string{
			resetLabelTarget: targetPrincipalID,
			resetLabelRevoke: boolLabel(!opt.KeepExistingSessions),
		},
	})
	if err != nil {
		return CredentialResetHandoff{}, err
	}
	out := CredentialResetHandoff{
		GrantID:           minted.Grant.GrantID,
		Code:              minted.Code,
		TargetPrincipalID: targetPrincipalID,
		ExpiresAtMs:       minted.Grant.ExpiresAt(),
	}
	if opt.BaseURL != "" {
		// The code rides as a query parameter of a URL the admin hands over, not
		// as a path segment, so a proxy access log that records paths but not
		// query strings does not capture it — the same class of exposure SEC-051
		// bounds for argv and journald.
		out.URL = strings.TrimRight(opt.BaseURL, "/") + "/auth/reset?code=" + url.QueryEscape(minted.Code)
	}
	return out, nil
}

// RedeemCredentialResetGrant is SEC-050's redemption half and SEC-053's
// revocation default, as ONE atomic act.
//
// newPassword is supplied by the TARGET, here, at redemption — the only point in
// this flow where a credential value is accepted at all (SEC-050).
//
// Everything below runs inside RedeemGrant's single transaction (SEC-036's
// atomic check-and-consume), so the three possible half-states are all
// unreachable: a consumed code that set no credential, a set credential whose
// code stayed live, and a credential set without the session eviction SEC-053
// makes the default.
func (s *Store) RedeemCredentialResetGrant(ctx context.Context, code, newPassword string) (CredentialResetRedemption, error) {
	if newPassword == "" {
		return CredentialResetRedemption{}, errors.New("auth: a credential-reset redemption needs a new credential value")
	}
	var out CredentialResetRedemption
	grant, err := s.RedeemGrant(ctx, code, PurposeCredentialReset, func(tx *sql.Tx, g GrantRow) error {
		targetID := g.Labels[resetLabelTarget]
		if targetID == "" {
			return errors.New("auth: this credential-reset grant names no target principal")
		}
		cred, err := s.findPrincipalPasswordCredentialTx(ctx, tx, targetID)
		if err != nil {
			return err
		}
		if _, err := s.PutPasswordCredentialTx(ctx, tx, targetID, cred.Identifier, newPassword); err != nil {
			return err
		}
		out.TargetPrincipalID = targetID
		// SEC-053: revoke every existing session and API-key credential by
		// default. The revocation is INSIDE the transaction so it cannot half-
		// apply; the live-stream teardown hooks fire after commit, below.
		if g.Labels[resetLabelRevoke] != "false" {
			ids, err := s.revokePrincipalSessionsTx(ctx, tx, targetID)
			if err != nil {
				return err
			}
			out.RevokedSessionIDs, out.SessionsRevoked = ids, true
		}
		// SEC-052 is satisfied by what is ABSENT here: this consume function does
		// not touch totp credentials, totp_pending or totp_steps. "A
		// credential-reset-purpose grant MUST NOT itself authorize a change to
		// the target principal's TOTP enrollment."
		return nil
	})
	if err != nil {
		return CredentialResetRedemption{}, err
	}
	out.GrantID = grant.GrantID
	if out.RevokedSessionIDs == nil {
		out.RevokedSessionIDs = []string{}
	}
	// EVT-114's live-stream teardown, fired after the transaction committed and
	// outside the write lock — the same discipline RevokeSession follows.
	s.hookMu.Lock()
	hook := s.onRevoke
	s.hookMu.Unlock()
	if hook != nil {
		for _, id := range out.RevokedSessionIDs {
			hook(id)
		}
	}
	return out, nil
}

// requireAdmin enforces SEC-012's issuer restriction: `admin` (or above)
// somewhere in the tree. It reads the principal's complete binding set and takes
// their highest role, since SEC-010 lets a principal hold different roles at
// different nodes and an admin at any node may reset a user's credential.
func (s *Store) requireAdmin(ctx context.Context, principalID string) error {
	if principalID == "" {
		return ErrResetNotAdmin
	}
	bindings, err := s.RoleBindings(ctx, principalID)
	if err != nil {
		return err
	}
	role, ok := Effective(bindings)
	if !ok || !role.AtLeast(RoleAdmin) {
		return ErrResetNotAdmin
	}
	return nil
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// revokePrincipalSessionsTx is RevokePrincipalSessions' transactional body: it
// marks every live session and API key of principalID revoked, and revokes each
// linked `api-key` credential row in the same statement pair, returning the ids
// it revoked so the caller can fire the post-commit teardown hooks.
//
// It exists because SEC-053's revocation must ride inside the redemption's own
// transaction, and the store's write path serializes under a non-reentrant
// mutex — calling the exported RevokePrincipalSessions from inside a consume
// function would deadlock, exactly as CreatePrincipalTx exists to avoid.
func (s *Store) revokePrincipalSessionsTx(ctx context.Context, tx *sql.Tx, principalID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT session_id, credential_id FROM sessions
		 WHERE principal_id = ? AND revoked_at IS NULL ORDER BY session_id ASC`, principalID)
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions to revoke: %w", err)
	}
	var (
		ids           []string
		credentialIDs []string
	)
	for rows.Next() {
		var sessionID, credentialID string
		if err := rows.Scan(&sessionID, &credentialID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("auth: scan session to revoke: %w", err)
		}
		ids = append(ids, sessionID)
		if credentialID != "" {
			credentialIDs = append(credentialIDs, credentialID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("auth: list sessions to revoke: %w", err)
	}
	_ = rows.Close()

	now := s.nowMs()
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE principal_id = ? AND revoked_at IS NULL`,
		now, principalID); err != nil {
		return nil, fmt.Errorf("auth: revoke sessions: %w", err)
	}
	for _, credentialID := range credentialIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE credentials SET revoked_at = ? WHERE credential_id = ? AND revoked_at IS NULL`,
			now, credentialID); err != nil {
			return nil, fmt.Errorf("auth: revoke api-key credential: %w", err)
		}
	}
	return ids, nil
}
