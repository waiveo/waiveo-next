package auth

import (
	"context"
	"database/sql"
	"fmt"
)

// destroy.go is this package's half of the key-material destruction path
// `security-model.md` SEC-121 defines and `api/1` API-122 routes a workspace
// delete to.
//
// SEC-121 fixes the outcome in two clauses this file is written against: the
// destruction "MUST force fresh enrollment on every principal and MUST re-open
// the first-boot claim window (SEC-120)". Both fall out of removing every row in
// this package's five tables rather than from a separate flag anywhere:
//
//   - Fresh enrollment is forced because every credential, session and API key is
//     gone. There is no principal left holding anything to present, so no
//     previously-issued secret can authenticate against the deployment again —
//     which is the same guarantee SEC-020's revocation gives one session, applied
//     to every credential the workspace ever issued at once.
//   - The claim window re-opens because bootstrap.go decides "already claimed" by
//     counting `owner` role-bindings (CountOwnerBindings). Zero bindings is
//     structurally indistinguishable from a box that has never been claimed, so
//     the next boot mints a fresh `setup` grant and the first-boot claim flow is
//     live again (SEC-120) — no second mechanism, and nothing that could get out
//     of step with the mechanism that already decides it.
//
// SEC-011's "a deployment MUST always retain at least one `owner`-role principal
// in a claimed state" is NOT violated by this, and the contract says so in the
// same sentence: the last owner binding "MUST NOT be deletable through ordinary
// `api/1` mutation, only through factory reset ... which itself re-opens claim."
// DeleteRoleBinding enforces that floor for ordinary mutation (store.go); this
// path is the named exception, and it re-opens claim exactly as the exception
// requires.

// DestroyAllPrincipals removes every principal, credential, session, role
// binding and grant in ONE transaction — the auth-tier half of SEC-121's
// key-material destruction.
//
// It is deliberately all-or-nothing. A partial destruction that removed
// credentials but left role bindings, or removed principals but left live
// sessions, would leave a deployment that is neither claimable nor usable: the
// sessions would still authenticate against principals that no longer exist,
// and CountOwnerBindings would still report the box claimed. One transaction
// means the deployment is only ever observed in the state before or the state
// after.
//
// The order within the transaction is child-table-first, so a foreign-key
// enforcement added later cannot turn this into a partial delete.
func (s *Store) DestroyAllPrincipals(ctx context.Context) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"grants", "sessions", "credentials", "role_bindings", "principals"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
				return fmt.Errorf("auth: destroy %s: %w", table, err)
			}
		}
		return nil
	})
}
