package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The claim route has one 500 that does not mean "your request did nothing".
//
// Everything the claim performs — the owner principal, its password credential,
// its role binding — commits with the grant's consumption in a single
// transaction. A failure after that is the session mint alone, so the workspace
// IS claimed and the credentials DO work. The generic detail is misleading at
// the most expensive moment, because the setup code is one-time and now spent.

func TestTheClaimedButNoSessionProblemSaysWhatIsTrueAndWhatToDo(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/claim", nil)
	writeClaimedButNoSession(rr, req, "01J8ZTRACE0000000000000000")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — the request did not do all it promised", rr.Code)
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, rr.Body.String())
	}

	// It must say the claim SUCCEEDED, or an operator reads a 500 as "nothing
	// happened" and retries a code that is already spent.
	if !strings.Contains(problem.Detail, "was claimed") {
		t.Errorf("the detail does not say the workspace was claimed: %q", problem.Detail)
	}
	// It must say not to retry, because the retry's refusal ("already claimed")
	// is the thing that makes the box look broken.
	if !strings.Contains(problem.Detail, "Do not retry") {
		t.Errorf("the detail does not warn against retrying a spent code: %q", problem.Detail)
	}
	// And it must name the remedy that actually works.
	if !strings.Contains(problem.Detail, "Sign in") {
		t.Errorf("the detail does not name the remedy: %q", problem.Detail)
	}
	// The generic sentence must be gone — its presence is the defect.
	if strings.Contains(problem.Detail, "An unexpected server error occurred") {
		t.Errorf("the generic detail survived: %q", problem.Detail)
	}
	if problem.Code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL (the taxonomy entry is unchanged)", problem.Code)
	}
}
