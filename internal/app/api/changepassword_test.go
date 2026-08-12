package api_test

import (
	"context"
	"net/http"
	"testing"
)

// The fixture principal is seeded with a session but no PASSWORD credential —
// api tests drive it by token. These cases are about the password, so they put
// one on the principal first, through the same store call a claim would.
const envPassword = "the-current-password-1"

func withPassword(t *testing.T, e *testEnv) {
	t.Helper()
	if _, err := e.auth.Store.PutPasswordCredential(
		context.Background(), e.auth.PrincipalID, "operator@example.test", envPassword,
	); err != nil {
		t.Fatalf("seed password credential: %v", err)
	}
}

// Self-service password change (security-model SEC-054).
//
// The register's line was that an operator who KNOWS their password cannot
// rotate it — changing it needed an admin to mint a reset grant and a
// hand-rolled redeem POST. These pin the three rules that make the operation
// safe rather than merely present.

// The current password is required, and it is what separates this from an
// admin-issued reset: without it a stolen session is a permanent account
// takeover, since the thief could lock the owner out using only the session
// they already hold.
func TestChangingAPasswordRequiresTheCurrentOne(t *testing.T) {
	e := newEnv(t)
	withPassword(t, e)

	resp, raw := e.do(t, http.MethodPut, "/api/v1/auth/password",
		mustJSON(t, map[string]any{"current_password": "not-the-password", "new_password": "a-new-one"}), jsonHeaders)
	// A FIELD error, deliberately not a 401. The session that made this request
	// is valid and stays valid, so the failure belongs to the body — and a 401
	// would tell the console its session had ended, throwing the operator out of
	// the form over a typo.
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a wrong current password (%s)", resp.StatusCode, raw)
	}
	// The FIELD error names which member failed, so a form can mark it rather
	// than showing a page-level banner over one wrong box.
	if codes := problemCodes(t, raw); len(codes) == 0 || codes[0] != "incorrect" {
		t.Errorf("field codes = %v, want [incorrect] on current_password", codes)
	}

	// And the password did NOT change: the session that made the failed attempt
	// still works, and so does the original credential.
	if resp, _ := e.do(t, http.MethodGet, "/api/v1/auth/session", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the failed attempt disturbed the caller's session: %d", resp.StatusCode)
	}
}

// A body that omits the proof must be REFUSED, never treated as "no current
// password given, so skip the check".
func TestChangingAPasswordRefusesABodyWithNoProof(t *testing.T) {
	e := newEnv(t)
	withPassword(t, e)

	resp, raw := e.do(t, http.MethodPut, "/api/v1/auth/password",
		mustJSON(t, map[string]any{"new_password": "a-new-one"}), jsonHeaders)
	if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want a refusal for a body with no current_password (%s)", resp.StatusCode, raw)
	}
}

// The one that decides whether the operation is usable: the session that made
// the change SURVIVES it. Revoking it would sign the operator out mid-task and
// hand them a fresh login with a password they may have mistyped twice.
func TestChangingAPasswordKeepsTheSessionThatMadeTheChange(t *testing.T) {
	e := newEnv(t)
	withPassword(t, e)

	resp, raw := e.do(t, http.MethodPut, "/api/v1/auth/password",
		mustJSON(t, map[string]any{"current_password": envPassword, "new_password": "a-brand-new-password"}), jsonHeaders)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change status = %d, want 204 (%s)", resp.StatusCode, raw)
	}

	// Still signed in, on the same session, immediately afterwards.
	if resp, body := e.do(t, http.MethodGet, "/api/v1/auth/session", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the caller was signed out by their own password change: %d (%s)", resp.StatusCode, body)
	}

	// And the change really landed: the old password no longer authenticates.
	if resp, _ := e.do(t, http.MethodPut, "/api/v1/auth/password",
		mustJSON(t, map[string]any{"current_password": envPassword, "new_password": "another"}), jsonHeaders); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("the OLD password still works after a change: %d", resp.StatusCode)
	}
}
