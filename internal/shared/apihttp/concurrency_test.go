package apihttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// concurrencyCase is the slice of an api-1 corpus case this package's
// optimistic-concurrency helpers drive: the If-Match/Trace-Id request headers,
// the current resource revision, and the `expected` status / write_executed /
// Problem body the case pins (API-022/API-023). The corpus JSON is the oracle
// (§10 differential) — the test reads its expectations from the frozen file,
// never from values re-typed here.
type concurrencyCase struct {
	CaseID string `json:"case_id"`
	Input  struct {
		Path                 string            `json:"path"`
		Headers              map[string]string `json:"headers"`
		CurrentResourceState struct {
			Revision int64 `json:"revision"`
		} `json:"current_resource_state"`
	} `json:"input"`
	Expected struct {
		Status        int            `json:"status"`
		ContentType   string         `json:"content_type"`
		WriteExecuted bool           `json:"write_executed"`
		Body          map[string]any `json:"body"`
	} `json:"expected"`
}

func loadConcurrencyCase(t *testing.T, name string) concurrencyCase {
	t.Helper()
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "api-1", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus case %s: %v", name, err)
	}
	var c concurrencyCase
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("decode corpus case %s: %v", name, err)
	}
	return c
}

// TestETagFromRevision asserts ETag derives a strong validator solely from the
// revision: the decimal revision quoted, interchangeable with the revision
// itself (API-020).
func TestETagFromRevision(t *testing.T) {
	cases := map[int64]string{
		0:  `"0"`,
		1:  `"1"`,
		3:  `"3"`,
		42: `"42"`,
	}
	for rev, want := range cases {
		if got := ETag(rev); got != want {
			t.Errorf("ETag(%d) = %q, want %q", rev, got, want)
		}
	}
}

// TestCheckIfMatchMissingCorpus drives API-022: a state-changing request that
// omits If-Match is rejected 428 / IF_MATCH_REQUIRED before any write — there
// is no unconditional-overwrite path. The emitted Problem body matches the
// corpus verbatim (pinned title/detail/code/trace_id).
func TestCheckIfMatchMissingCorpus(t *testing.T) {
	c := loadConcurrencyCase(t, "API-022-invalid-if-match-missing.json")

	ifMatch, present := c.Input.Headers["If-Match"]
	if present {
		t.Fatalf("corpus %s unexpectedly supplies an If-Match header", c.CaseID)
	}
	rev := c.Input.CurrentResourceState.Revision

	out := CheckIfMatch(ifMatch, present, rev)
	if out.OK {
		t.Fatal("CheckIfMatch with a missing If-Match returned OK, want a rejection (API-022)")
	}
	if out.CurrentRevision != nil {
		t.Errorf("missing-If-Match outcome carries CurrentRevision %v, want nil", *out.CurrentRevision)
	}

	writeExecuted, gotBody, gotStatus, gotCT := runConcurrencyOutcome(c, out)
	if writeExecuted != c.Expected.WriteExecuted {
		t.Errorf("write_executed = %v, want %v (no write on a rejected precondition, API-022)", writeExecuted, c.Expected.WriteExecuted)
	}
	if gotStatus != c.Expected.Status {
		t.Errorf("status = %d, want %d", gotStatus, c.Expected.Status)
	}
	if gotCT != c.Expected.ContentType {
		t.Errorf("Content-Type = %q, want %q", gotCT, c.Expected.ContentType)
	}
	assertBodyMatchesCorpus(t, gotBody, c.Expected.Body)
}

// TestCheckIfMatchConflictCorpus drives API-023: an If-Match that no longer
// equals the current ETag is rejected 412 / REVISION_CONFLICT before any
// write, and the Problem surfaces current_revision so the client can re-read.
// Quotes on the supplied validator are tolerated; the pinned detail is
// reproduced verbatim.
func TestCheckIfMatchConflictCorpus(t *testing.T) {
	c := loadConcurrencyCase(t, "API-023-invalid-if-match-conflict.json")

	ifMatch, present := c.Input.Headers["If-Match"]
	if !present {
		t.Fatalf("corpus %s is missing its If-Match header", c.CaseID)
	}
	rev := c.Input.CurrentResourceState.Revision

	out := CheckIfMatch(ifMatch, present, rev)
	if out.OK {
		t.Fatal("CheckIfMatch with a stale If-Match returned OK, want a REVISION_CONFLICT (API-023)")
	}
	if out.CurrentRevision == nil || *out.CurrentRevision != rev {
		t.Errorf("conflict outcome CurrentRevision = %v, want %d", out.CurrentRevision, rev)
	}

	writeExecuted, gotBody, gotStatus, gotCT := runConcurrencyOutcome(c, out)
	if writeExecuted != c.Expected.WriteExecuted {
		t.Errorf("write_executed = %v, want %v (no write on a rejected precondition, API-023)", writeExecuted, c.Expected.WriteExecuted)
	}
	if gotStatus != c.Expected.Status {
		t.Errorf("status = %d, want %d", gotStatus, c.Expected.Status)
	}
	if gotCT != c.Expected.ContentType {
		t.Errorf("Content-Type = %q, want %q", gotCT, c.Expected.ContentType)
	}
	assertBodyMatchesCorpus(t, gotBody, c.Expected.Body)
}

// TestCheckIfMatchOK asserts a supplied validator equal to the current ETag
// passes — and only then may a write proceed. Both the canonical quoted form
// and a bare-number form are accepted (API-020: ETag and revision are
// interchangeable).
func TestCheckIfMatchOK(t *testing.T) {
	for _, supplied := range []string{`"3"`, "3"} {
		out := CheckIfMatch(supplied, true, 3)
		if !out.OK {
			t.Errorf("CheckIfMatch(%q, true, 3) = not OK (status %d, code %q), want OK", supplied, out.Status, out.Code)
		}
		if out.CurrentRevision != nil {
			t.Errorf("OK outcome for %q carries CurrentRevision, want nil", supplied)
		}
	}
}

// runConcurrencyOutcome models the write path a handler follows: on a non-OK
// outcome it emits the Problem and NEVER performs the write; on OK it would.
// It returns whether the write ran plus the emitted response so the test can
// diff it against the corpus.
func runConcurrencyOutcome(c concurrencyCase, out ConcurrencyOutcome) (writeExecuted bool, body map[string]any, status int, contentType string) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, c.Input.Path, nil)
	traceID := c.Input.Headers["Trace-Id"]

	if out.OK {
		writeExecuted = true // a real handler performs the read-modify-write here
		w.WriteHeader(http.StatusOK)
		return writeExecuted, nil, http.StatusOK, ""
	}

	var extra map[string]any
	if out.CurrentRevision != nil {
		extra = map[string]any{"current_revision": *out.CurrentRevision}
	}
	WriteProblemExt(w, r, traceID, out.Status, out.Code, out.Title, out.Detail, extra)

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		return writeExecuted, nil, w.Code, w.Header().Get("Content-Type")
	}
	return writeExecuted, got, w.Code, w.Header().Get("Content-Type")
}

// assertBodyMatchesCorpus diffs every member the corpus `expected.body` pins
// against the emitted body. Extra members in the emission (e.g. instance,
// API-015) are permitted; every pinned member MUST be reproduced exactly.
func assertBodyMatchesCorpus(t *testing.T, got, want map[string]any) {
	t.Helper()
	if got == nil {
		t.Fatal("no Problem body emitted, want a conformant Problem document")
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("Problem body is missing member %q (corpus pins %v)", k, wv)
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			t.Errorf("Problem body[%q] = %#v, want %#v (corpus-pinned)", k, gv, wv)
		}
	}
}
