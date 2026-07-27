package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The TOTP credential's persistence (SEC-003/004), split from store.go because
// it is the one credential kind whose secret is SEALED rather than hashed and
// the one whose use carries state (a consumed time step) beyond the credential
// row itself.
//
// The lifecycle is deliberately two-phase:
//
//	BeginTOTPEnrollment  → a `totp_pending` row: a sealed secret and nothing else.
//	ArmTOTPCredential    → a `totp` credential row (SEC-003) + its step floor.
//
// Nothing between those two calls can authenticate anything. That is the answer
// to "what proves the enrollment took": the credential relation gains a row only
// once the enrolling principal has returned a code computed from the secret,
// which is the only evidence available that the secret actually reached an
// authenticator rather than a QR code nobody scanned. An implementation that
// armed on the first call would lock a principal out of their own account every
// time an enrollment was abandoned halfway.

// ErrNoSecretSealer is returned by every path that would have to store or read a
// recoverable secret on a store constructed without a SecretSealer. It is a hard
// refusal rather than a fallback: the alternative to sealing is storing a TOTP
// shared secret in cleartext beside a password hash, which is not a degraded
// mode of this feature but the absence of it.
var ErrNoSecretSealer = errors.New("auth: this store holds no secret sealer, so a recoverable credential secret cannot be stored (SEC-040)")

// ErrNoPendingTOTP is returned when a confirmation arrives with no enrollment in
// flight for that principal.
var ErrNoPendingTOTP = errors.New("auth: no TOTP enrollment is in progress for this principal")

// ErrTOTPAlreadyEnrolled is returned when a principal that already holds an
// armed `totp` credential starts an ordinary enrollment. Clearing or re-enrolling
// an existing TOTP credential is not an ordinary self-service act — SEC-052
// routes it through an explicit owner-role path or the console binding — so this
// package refuses rather than quietly replacing a second factor an attacker
// holding a live session would love to replace.
var ErrTOTPAlreadyEnrolled = errors.New("auth: this principal already holds a TOTP credential")

// totpPendingAAD and totpCredentialAAD are the contexts a TOTP secret is sealed
// under in each of its two lives. They are DIFFERENT strings on purpose: a
// pending blob lifted straight into a credential row would fail to open, so the
// only way a secret becomes a credential's secret is through ArmTOTPCredential,
// which re-seals it under the credential's own id after opening it under the
// pending context.
func totpPendingAAD(principalID string) []byte { return []byte("totp-pending:" + principalID) }
func totpCredentialAAD(credentialID string) []byte {
	return []byte("totp-credential:" + credentialID)
}

// FindTOTPCredential returns the live `totp` credential belonging to
// principalID, or ErrCredentialNotFound. A revoked row is not returned.
//
// The credential's `identifier` column carries the principal's own id, which is
// what the schema's UNIQUE (kind, identifier) index turns into "at most one
// armed TOTP credential per principal". A TOTP credential has no login handle of
// its own — it is presented alongside the password credential's identifier, not
// instead of it — so the column is free to carry the constraint instead.
func (s *Store) FindTOTPCredential(ctx context.Context, principalID string) (CredentialRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var c CredentialRow
	err := s.db.QueryRowContext(ctx,
		`SELECT credential_id, principal_id, kind, identifier, secret, created_at, last_used_at, revoked_at
		 FROM credentials WHERE kind = ? AND principal_id = ? AND revoked_at IS NULL`,
		CredentialTOTP, principalID).
		Scan(&c.CredentialID, &c.PrincipalID, &c.Kind, &c.Identifier, &c.Secret, &c.CreatedAt, &c.LastUsedAt, &c.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialRow{}, ErrCredentialNotFound
	}
	if err != nil {
		return CredentialRow{}, fmt.Errorf("auth: find totp credential: %w", err)
	}
	return c, nil
}

// HasTOTPCredential reports whether principalID holds a live `totp` credential —
// the question the login path asks to decide whether a second factor is due.
func (s *Store) HasTOTPCredential(ctx context.Context, principalID string) (bool, error) {
	_, err := s.FindTOTPCredential(ctx, principalID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrCredentialNotFound):
		return false, nil
	default:
		return false, err
	}
}

// TOTPSecret opens an armed credential's sealed shared secret.
//
// The returned bytes are the live secret. Callers use them to recompute a code
// and then drop them; nothing here caches them, and nothing anywhere may log,
// wrap into an error string, or return them over the wire.
func (s *Store) TOTPSecret(cred CredentialRow) ([]byte, error) {
	if s.sealer == nil {
		return nil, ErrNoSecretSealer
	}
	secret, err := s.sealer.Open(cred.Secret, totpCredentialAAD(cred.CredentialID))
	if err != nil {
		// The sealed value is not echoed — it is credential material.
		return nil, fmt.Errorf("auth: this TOTP credential's secret did not open under the workspace's key")
	}
	return secret, nil
}

// BeginTOTPEnrollment mints a fresh shared secret for principalID, persists it
// SEALED as a pending (non-authenticating) enrollment, and returns the raw
// secret — the only moment it exists in plaintext on this side, so the caller
// can render it as an `otpauth://` URI or a typed key.
//
// allowReplace lets the caller arm over an existing credential. It is false for
// the ordinary self-service path (see ErrTOTPAlreadyEnrolled) and true for the
// one case SEC-022 requires to stay open: a principal on an `aal: recovery`
// session, which "MUST remain restricted ... until the target principal
// completes TOTP re-enrollment". Refusing that principal for already holding a
// TOTP credential would make the contract's own exit from the restricted tier
// unreachable.
func (s *Store) BeginTOTPEnrollment(ctx context.Context, principalID string, allowReplace bool) ([]byte, error) {
	if s.sealer == nil {
		return nil, ErrNoSecretSealer
	}
	p, err := s.GetPrincipal(ctx, principalID)
	if err != nil {
		return nil, err
	}
	if p.Kind == KindSystemConsole {
		return nil, errors.New("auth: system-console carries no credential row (SEC-002)")
	}
	if !allowReplace {
		enrolled, err := s.HasTOTPCredential(ctx, principalID)
		if err != nil {
			return nil, err
		}
		if enrolled {
			return nil, ErrTOTPAlreadyEnrolled
		}
	}
	secret, err := NewTOTPSecret()
	if err != nil {
		return nil, err
	}
	sealed, err := s.sealer.Seal(secret, totpPendingAAD(principalID))
	if err != nil {
		return nil, fmt.Errorf("auth: seal totp secret: %w", err)
	}
	now := s.nowMs()
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		// REPLACE, not INSERT: starting a second enrollment supersedes the first,
		// so an abandoned attempt cannot linger as a second secret that would also
		// arm the credential.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO totp_pending (principal_id, sealed_secret, created_at) VALUES (?, ?, ?)
			 ON CONFLICT(principal_id) DO UPDATE SET sealed_secret = excluded.sealed_secret, created_at = excluded.created_at`,
			principalID, sealed, now)
		return err
	}); err != nil {
		return nil, fmt.Errorf("auth: begin totp enrollment: %w", err)
	}
	return secret, nil
}

// PendingTOTPSecret opens the in-flight enrollment's secret for principalID, or
// ErrNoPendingTOTP.
func (s *Store) PendingTOTPSecret(ctx context.Context, principalID string) ([]byte, error) {
	if s.sealer == nil {
		return nil, ErrNoSecretSealer
	}
	s.mu.RLock()
	var sealed string
	err := s.db.QueryRowContext(ctx,
		`SELECT sealed_secret FROM totp_pending WHERE principal_id = ?`, principalID).Scan(&sealed)
	s.mu.RUnlock()
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoPendingTOTP
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read pending totp enrollment: %w", err)
	}
	secret, err := s.sealer.Open(sealed, totpPendingAAD(principalID))
	if err != nil {
		return nil, fmt.Errorf("auth: this pending TOTP secret did not open under the workspace's key")
	}
	return secret, nil
}

// ArmTOTPCredential promotes the pending enrollment into a real `totp`
// credential row (SEC-003) and seeds its step floor with usedStep — the step of
// the very code that proved the enrollment.
//
// Seeding with the CONFIRMING step rather than with zero is what stops the
// confirmation code being replayed at the login form thirty seconds later: the
// step that armed the credential is, from the credential's first instant, a step
// already spent.
//
// Everything happens in ONE transaction — discard the pending row, drop any
// prior TOTP credential and its step floor, insert the new credential, seed the
// floor — so a failure part-way cannot leave a principal with a credential whose
// floor is missing (replayable) or a floor whose credential is missing
// (unauthenticatable).
func (s *Store) ArmTOTPCredential(ctx context.Context, principalID string, secret []byte, usedStep int64) (CredentialRow, error) {
	if s.sealer == nil {
		return CredentialRow{}, ErrNoSecretSealer
	}
	credentialID, err := s.mintID("credential")
	if err != nil {
		return CredentialRow{}, err
	}
	// Re-sealed under the CREDENTIAL's context, never carried across from the
	// pending context — see totpPendingAAD.
	sealed, err := s.sealer.Seal(secret, totpCredentialAAD(credentialID))
	if err != nil {
		return CredentialRow{}, fmt.Errorf("auth: seal totp secret: %w", err)
	}
	now := s.nowMs()
	out := CredentialRow{
		CredentialID: credentialID, PrincipalID: principalID, Kind: CredentialTOTP,
		Identifier: principalID, Secret: sealed, CreatedAt: now,
	}
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var pending int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM totp_pending WHERE principal_id = ?`, principalID).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			return ErrNoPendingTOTP
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM totp_steps WHERE credential_id IN
			   (SELECT credential_id FROM credentials WHERE principal_id = ? AND kind = ?)`,
			principalID, CredentialTOTP); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM credentials WHERE principal_id = ? AND kind = ?`,
			principalID, CredentialTOTP); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO credentials (credential_id, principal_id, kind, identifier, secret, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			out.CredentialID, out.PrincipalID, out.Kind, out.Identifier, out.Secret, out.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO totp_steps (credential_id, last_step, updated_at) VALUES (?, ?, ?)`,
			out.CredentialID, usedStep, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM totp_pending WHERE principal_id = ?`, principalID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return CredentialRow{}, err
	}
	return out, nil
}

// ConsumeTOTPStep atomically claims step for credentialID, reporting ok=false
// when that step is not strictly above the credential's high-water mark.
//
// This ONE statement is the replay defense. A TOTP code is valid for its whole
// window plus the skew tolerance, so "verify the digits and let the request
// through" accepts the same code as many times as it is presented inside that
// window — which is exactly what an attacker who shoulder-surfed, phished, or
// intercepted one code needs. Claiming the step makes the code single-use: the
// first presentation advances the mark, every later presentation of the same
// digits finds `last_step >= step` and is refused.
//
// It is a conditional UPSERT rather than a read-then-write because two logins
// presenting the same code concurrently would otherwise both read the old mark
// and both conclude they were first. SQLite reports zero changed rows when the
// DO UPDATE's own WHERE fails, so exactly one of the two racers gets ok=true.
//
// The same statement is the credential's monotonic time floor (SEC-066 at
// credential grain): last_step never decreases, so a host clock moved backward
// cannot make an already-spent window spendable again.
func (s *Store) ConsumeTOTPStep(ctx context.Context, credentialID string, step int64) (bool, error) {
	now := s.nowMs()
	var claimed bool
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO totp_steps (credential_id, last_step, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(credential_id) DO UPDATE SET last_step = excluded.last_step, updated_at = excluded.updated_at
			 WHERE totp_steps.last_step < excluded.last_step`,
			credentialID, step, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		claimed = n > 0
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("auth: consume totp step: %w", err)
	}
	return claimed, nil
}

// TOTPLastStep returns the high-water step a credential has consumed, and
// whether it has consumed any. Exposed for tests and for a diagnostic surface;
// the login path never reads it, because reading and then writing is precisely
// the race ConsumeTOTPStep exists to avoid.
func (s *Store) TOTPLastStep(ctx context.Context, credentialID string) (int64, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var step int64
	err := s.db.QueryRowContext(ctx,
		`SELECT last_step FROM totp_steps WHERE credential_id = ?`, credentialID).Scan(&step)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("auth: read totp step: %w", err)
	}
	return step, true, nil
}
