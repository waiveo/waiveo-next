package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Authenticator's refusal ladder is a cascade: no-credential, unresolvable
// session, and no-role-binding each end the request, and the rungs below a given
// one produce the SAME status (401 or 403) for many inputs. A mutation sweep
// found the ladder's guards unheld tree-wide even though a dedicated 403 test
// exists — because that test asserts only status + machine code, and removing
// the no-binding guard still yields 403/FORBIDDEN via the role-floor guard one
// rung down. Status alone cannot tell two rungs apart.
//
// These cases pin each rung by its DISTINCTIVE detail text (and, for the title
// selector, the Problem title), which is the granularity at which the rungs
// actually differ. Each drives the real middleware-wrapped route in the harness.

// problemDetail reads the human-readable `detail` out of a Problem body — the
// field that distinguishes two refusals sharing one status and code.
func problemDetail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %q)", err, rec.Body.String())
	}
	s, _ := p["detail"].(string)
	return s
}

func problemTitle(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %q)", err, rec.Body.String())
	}
	s, _ := p["title"].(string)
	return s
}

func TestAuthLadderRefusesEachRungWithItsOwnReason(t *testing.T) {
	h := newHTTPHarness(t)

	t.Run("credential in query string is refused, not ignored (EVT-112)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, testProtectedPath+"?token=wvs_std_abc", nil)
		rec := h.do(req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if d := problemDetail(t, rec); !strings.Contains(d, "query-string parameter") {
			t.Fatalf("detail = %q, want the query-credential refusal — without it the credential is "+
				"merely ignored and has already reached the access log", d)
		}
	})

	t.Run("no credential presented", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
		rec := h.do(req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if d := problemDetail(t, rec); !strings.Contains(d, "No session cookie or Authorization") {
			t.Fatalf("detail = %q, want the no-credential reason (distinct from the bad-session one a "+
				"rung down)", d)
		}
	})

	t.Run("well-formed token naming no live session", func(t *testing.T) {
		// This pins the PROPERTY, not one rung. The unresolvable-session guard
		// and the vanished-principal guard one rung down emit the IDENTICAL
		// detail by design — an attacker must not be able to tell an unknown
		// token from a revoked principal — so the two are deliberately
		// indistinguishable, and neither can be (or should be) killed
		// individually. What must hold is that a credential naming no live
		// session is refused with exactly this reason, whichever rung refuses.
		//
		// A token of the exact minted shape that was never stored: it passes
		// ParseToken and reaches LookupSession, which finds nothing.
		tok, err := MintToken(TokenKindSession, AALStandard)
		if err != nil {
			t.Fatalf("MintToken: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tok})
		rec := h.do(req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if d := problemDetail(t, rec); !strings.Contains(d, "does not name a live session") {
			t.Fatalf("detail = %q, want the unresolvable-session reason", d)
		}
	})

	t.Run("authenticated but unbound principal is 403 by its own reason and title", func(t *testing.T) {
		ctx := context.Background()
		stranger, err := h.store.CreatePrincipal(ctx, KindUser, "stranger")
		if err != nil {
			t.Fatalf("CreatePrincipal: %v", err)
		}
		minted, err := h.store.MintSession(ctx, stranger.PrincipalID, TokenKindSession, "", AALStandard, nil)
		if err != nil {
			t.Fatalf("MintSession: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, testProtectedPath, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: minted.Token})
		rec := h.do(req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		// The distinctive reason: no binding, NOT insufficient role. The
		// role-floor guard one rung down would also 403 an empty role, so only
		// the detail separates the no-binding rung from it.
		if d := problemDetail(t, rec); !strings.Contains(d, "holds no role binding") {
			t.Fatalf("detail = %q, want the no-binding reason (distinct from a role-floor refusal)", d)
		}
		// The refuse() title selector: a 403 is titled Forbidden, not Unauthorized.
		if title := problemTitle(t, rec); title != "Forbidden" {
			t.Fatalf("title = %q, want Forbidden — the title must track the 403 status", title)
		}
	})
}
