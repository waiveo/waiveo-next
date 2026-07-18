package apihttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidTraceIDAccepted asserts a well-formed supplied Trace-Id (API-061:
// 20-36 chars, [A-Za-z0-9-]) is echoed back verbatim by WithTraceID.
func TestValidTraceIDAccepted(t *testing.T) {
	const supplied = "01J8Z3K4N5P6Q7R8S9T0V1W2X4" // 26 chars, a real ULID shape

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	r.Header.Set(TraceIDHeader, supplied)

	var gotFromContext string
	h := WithTraceID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotFromContext = TraceID(r)
	}))
	h.ServeHTTP(w, r)

	if got := w.Header().Get(TraceIDHeader); got != supplied {
		t.Errorf("response Trace-Id header = %q, want the supplied value %q echoed back", got, supplied)
	}
	if gotFromContext != supplied {
		t.Errorf("TraceID(r) inside the handler = %q, want %q", gotFromContext, supplied)
	}
}

// TestInvalidTraceIDReplacedNeverRejected asserts a too-short/malformed
// supplied Trace-Id is silently discarded and replaced with a fresh valid
// one (API-061) — the request still proceeds (200), it is never itself a
// request error.
func TestInvalidTraceIDReplacedNeverRejected(t *testing.T) {
	cases := []string{
		"",                          // absent
		"too-short",                 // < 20 chars
		"has a space here 01234567", // invalid charset
		"this-value-is-far-too-long-to-be-a-valid-trace-id-under-api-061-thirty-six-char-cap",
	}

	for _, supplied := range cases {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/whatever", nil)
		if supplied != "" {
			r.Header.Set(TraceIDHeader, supplied)
		}

		h := WithTraceID(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		h.ServeHTTP(w, r)

		got := w.Header().Get(TraceIDHeader)
		if got == "" {
			t.Errorf("supplied %q: response Trace-Id header is empty, want a freshly generated replacement", supplied)
			continue
		}
		if got == supplied {
			t.Errorf("supplied %q: response Trace-Id header echoed the invalid value back, want a fresh replacement", supplied)
		}
		if !validTraceID(got) {
			t.Errorf("supplied %q: freshly generated Trace-Id %q does not itself satisfy API-061", supplied, got)
		}
	}
}

// TestSuccessResponseCarriesTraceID asserts an ordinary success response
// passed through WithTraceID still carries the Trace-Id header (API-060: a
// MUST on every response, not just error ones).
func TestSuccessResponseCarriesTraceID(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/ok", nil)

	h := WithTraceID(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get(TraceIDHeader); !validTraceID(got) {
		t.Errorf("success response Trace-Id header = %q, want a valid trace id (API-060)", got)
	}
}

// TestWriteProblemShapeAndTraceIDAgreement asserts WriteProblem's body is a
// conformant Problem document (API-010/API-016) whose trace_id extension
// member equals the exact value passed in (API-062), and whose instance
// equals the request's own path (API-015).
func TestWriteProblemShapeAndTraceIDAgreement(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/enroll", nil)
	traceID := TraceID(r) // no WithTraceID in front here — falls back to a fresh id

	WriteProblem(w, r, traceID, http.StatusForbidden, "CLAIM_TOKEN_INVALID", "Claim Token Invalid")

	if got := w.Header().Get("Content-Type"); got != ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", got, ProblemContentType)
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}

	var body problem
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if body.Type != "about:blank" {
		t.Errorf("type = %q, want %q (API-016)", body.Type, "about:blank")
	}
	if body.Title == "" {
		t.Error("title is empty, want a short human string")
	}
	if body.Status != http.StatusForbidden {
		t.Errorf("body status = %d, want %d", body.Status, http.StatusForbidden)
	}
	if body.Code != "CLAIM_TOKEN_INVALID" {
		t.Errorf("code = %q, want %q", body.Code, "CLAIM_TOKEN_INVALID")
	}
	if body.TraceID != traceID {
		t.Errorf("trace_id = %q, want %q (API-062: must equal the header's own value)", body.TraceID, traceID)
	}
	if body.Instance != "/enroll" {
		t.Errorf("instance = %q, want the request's own path %q (API-015)", body.Instance, "/enroll")
	}
}
