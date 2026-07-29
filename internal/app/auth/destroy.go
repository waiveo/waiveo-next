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
// this package's tables rather than from a separate flag anywhere:
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

// DestroyLocalAuthState removes EVERYTHING this package persists for a
// workspace — every principal, credential, session, role binding and grant, and
// the persisted clock floor beside them — as the auth-tier half of SEC-121's
// key-material destruction.
//
// The row destruction is deliberately all-or-nothing. A partial destruction that
// removed credentials but left role bindings, or removed principals but left
// live sessions, would leave a deployment that is neither claimable nor usable:
// the sessions would still authenticate against principals that no longer exist,
// and CountOwnerBindings would still report the box claimed. One transaction
// means the deployment is only ever observed in the state before or the state
// after. The order within the transaction is child-table-first, so a foreign-key
// enforcement added later cannot turn this into a partial delete.
//
// # Why the clock floor goes too, and why it goes FIRST
//
// clockfloor.go states that the floor "rides beside the auth database because
// they share a lifecycle: a factory reset that destroys the credential store has
// no business leaving a clock floor behind". That sentence was a claim no code
// made true — the reset destroyed the rows and left clock-floor.json on disk —
// and this is where it becomes true.
//
// It matters operationally, not just for tidiness. The floor is a one-way
// ratchet whose only sanctioned way down is the console binding's reset verb
// (SEC-075), and that binding has no transport in this tree. A box handed on
// with a future-dated floor would clamp its NEXT owner's clock to the previous
// owner's last verified reading, with no reachable way to lower it — a reset
// that leaves the box unadministrable in a new way is not a reset. It is also
// the app-side analog of what SEC-124 requires outright of a relay-only
// appliance, whose factory reset "MUST destroy its device identity, its
// certificate/keypair, its operational state ... and its persisted clock floor".
//
// The floor goes FIRST because of the same ordering rule the api-tier path
// follows (internal/app/api/workspacerun.go): everything before the credential
// destruction must be retryable by an operator who still holds a live session.
// Removing the floor is idempotent and costs no administrability; destroying the
// credentials is the step there is no coming back from, so it stays last.
//
// A Store opened without WithClockFloor has no floor to destroy and skips the
// step — never silently, since a deployment that wired one gets it reset and a
// test that did not never had one on disk to begin with.
func (s *Store) DestroyLocalAuthState(ctx context.Context) error {
	if s.floor != nil {
		if err := s.floor.Reset(); err != nil {
			return fmt.Errorf("auth: destroy the persisted clock floor: %w", err)
		}
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		// totp_pending and totp_steps ride the same delete. A pending enrollment
		// left behind would let a code minted before the reset arm a credential
		// after it; a step floor left behind would attach to a credential id that
		// no longer exists and, worse, would survive as a stale high-water mark if
		// that id were ever minted again by a deterministic id source.
		for _, table := range []string{"grants", "sessions", "totp_steps", "totp_pending", "credentials", "role_bindings", "principals"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
				return fmt.Errorf("auth: destroy %s: %w", table, err)
			}
		}
		return nil
	})
}
