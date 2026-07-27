package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// SetupGrantFile is the filename the first-boot setup code is persisted under,
// inside the auth state directory.
const SetupGrantFile = "setup-code.txt"

// UnresolvedActorPrincipal is the reserved actor id an `audit.event` names when
// the acting identity did not resolve to a real principal — a login attempt for
// an identifier that exists nowhere, most importantly.
//
// EVT-080 requires `actor_principal` to be a ULID and EVT-083 requires a
// `result: failure` record to carry every field a success does, so such a record
// cannot simply omit the actor; and a failed login is squarely on EVT-081's
// mandatory-emission list, so it cannot simply not be emitted either. The nil
// ULID (26 zeroes — a valid ULID encoding timestamp 0 with zero randomness) is
// the honest value: it is a real, parseable id that can never collide with a
// minted one, and it reads unambiguously in an audit trail as "no principal".
const UnresolvedActorPrincipal = "00000000000000000000000000"

// Bootstrap is the first-run claim window (SEC-120). Called at startup, it
// reports whether the deployment is claimed and — when it is not — mints the
// one-time `setup`-purpose grant whose code is the ONLY way to claim it.
//
// SEC-120: "The installer MUST auto-generate a one-time setup-purpose grant at
// install time and present it printed, as a QR code, or on-screen; the setup
// endpoint MUST be claimable only by redeeming this grant. An installed-but-
// unclaimed box MUST NOT be first-come-first-served to whoever reaches its setup
// endpoint first on a shared network."
//
// This is that generation step for a self-hosted app process standing in for the
// installer. The code is written to disk 0600 (so a later boot re-presents the
// SAME code rather than silently minting a second live claim path) and returned
// so the caller can print it, which is this deployment's realization of "on
// screen".
//
// Re-running Bootstrap on an already-claimed deployment mints nothing and
// removes any stale code file: once an owner exists the claim window is shut,
// and leaving a live setup code on disk after that would be a second, unexpired
// route to ownership.
type Bootstrap struct {
	// Claimed reports whether the deployment already has an owner binding.
	Claimed bool
	// Code is the freshly minted (or previously persisted) setup code, empty
	// when Claimed.
	Code string
	// CodePath is where Code was persisted, empty when Claimed.
	CodePath string
}

// EnsureClaimWindow evaluates the claim state of store and, if unclaimed,
// ensures exactly one live setup grant exists with its code persisted under dir.
//
// The persisted code and the stored grant are kept in lock-step deliberately: on
// every unclaimed boot the previous grant is invalidated and a fresh one minted,
// then written. That means a code read off a screen or a printout is only ever
// the current one — an operator holding an old printout is refused rather than
// silently granted, and a box that has been power-cycled has not accumulated N
// live claim codes.
func EnsureClaimWindow(ctx context.Context, store *Store, dir, scopeNode string, auditor *Auditor) (Bootstrap, error) {
	owners, err := store.CountOwnerBindings(ctx)
	if err != nil {
		return Bootstrap{}, err
	}
	path := filepath.Join(dir, SetupGrantFile)
	if owners > 0 {
		// Claimed: shut the window and remove any stale code.
		if err := store.InvalidateGrants(ctx, PurposeSetup); err != nil {
			return Bootstrap{}, err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return Bootstrap{}, fmt.Errorf("auth: remove stale setup code: %w", err)
		}
		return Bootstrap{Claimed: true}, nil
	}

	if err := store.InvalidateGrants(ctx, PurposeSetup); err != nil {
		return Bootstrap{}, err
	}
	if scopeNode == "" {
		scopeNode = RootScopeNode
	}
	minted, err := store.MintGrant(ctx, MintGrantOptions{
		Purpose:                PurposeSetup,
		ResultingPrincipalKind: KindUser,
		ScopeNode:              scopeNode,
		Role:                   RoleOwner,
		TTLMs:                  DefaultSetupGrantTTLMs,
		RedemptionMode:         RedemptionOneTime,
		IssuedVia:              IssuedViaConsole,
	})
	if err != nil {
		return Bootstrap{}, err
	}
	if err := writeSecret(path, minted.Code); err != nil {
		return Bootstrap{}, err
	}
	// SEC-034: every grant creation emits an audit.event carrying the grant's
	// purpose and issued_via.
	auditor.Emit(Record{
		Actor: UnresolvedActorPrincipal, Action: ActionGrantCreated,
		Target: "grant:" + minted.Grant.GrantID, Result: "success",
		Purpose: minted.Grant.Purpose, IssuedVia: minted.Grant.IssuedVia,
	})
	return Bootstrap{Claimed: false, Code: minted.Code, CodePath: path}, nil
}

// writeSecret persists a secret to path with mode 0600, atomically: the bytes go
// to a temporary file in the SAME directory (so the rename cannot cross a
// filesystem boundary), are fsynced, and only then renamed into place.
//
// The tmp-and-rename discipline is what internal/feeder/signing and
// internal/feeder/enroll/persist already apply to key material, and it matters
// for the same reason here: a crash mid-write must leave EITHER the old code or
// the new one readable, never a truncated file that authenticates nothing and
// leaves an operator locked out of an unclaimed box with no way to read the code
// the process is enforcing.
//
// The temporary file is created 0600 from the outset rather than chmod'ed after,
// so the secret is never momentarily world-readable.
func writeSecret(path, secret string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".setup-code-*")
	if err != nil {
		return fmt.Errorf("auth: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup of a temp file the rename below never claimed.
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: chmod temp file: %w", err)
	}
	if _, err := tmp.WriteString(secret + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: write setup code: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: sync setup code: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: close setup code: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("auth: install setup code: %w", err)
	}
	return nil
}
