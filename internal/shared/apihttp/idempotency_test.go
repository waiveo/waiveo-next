package apihttp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// idempotencyCase is the slice of an api-1 Idempotency-Key corpus case this
// file drives: a sequence of POST requests (each with its principal, method,
// path, Idempotency-Key header, and body) plus the `expected` responses and
// resources_created count the case pins. The frozen corpus JSON is the oracle
// — expectations are read from the file, never re-typed here.
type idempotencyCase struct {
	CaseID string `json:"case_id"`
	Input  struct {
		Requests []struct {
			Method    string            `json:"method"`
			Path      string            `json:"path"`
			Headers   map[string]string `json:"headers"`
			Principal string            `json:"principal"`
			Body      map[string]any    `json:"body"`
		} `json:"requests"`
	} `json:"input"`
	Expected struct {
		Responses []struct {
			Status      int            `json:"status"`
			ContentType string         `json:"content_type"`
			Body        map[string]any `json:"body"`
			Replayed    bool           `json:"replayed"`
		} `json:"responses"`
		ResourcesCreated int `json:"resources_created"`
	} `json:"expected"`
}

func loadIdempotencyCase(t *testing.T, name string) idempotencyCase {
	t.Helper()
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "api-1", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus case %s: %v", name, err)
	}
	var c idempotencyCase
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("decode corpus case %s: %v", name, err)
	}
	return c
}

// fixedClock returns a func() int64 that always reports ms — the injectable
// clock NewIdempotencyStore takes, so no test ever reads the wall clock.
func fixedClock(ms int64) func() int64 { return func() int64 { return ms } }

const idempotencyBaseMs = int64(1_700_000_000_000)

// TestIdempotencyReplayCorpus drives API-050/051/052: two POSTs presenting the
// same Idempotency-Key scope and an identical body create exactly one resource
// — the first Begin is Fresh (executes, records the 201), the second is a
// Replay returning the first's stored response verbatim, and the create side
// effect runs exactly ONCE.
func TestIdempotencyReplayCorpus(t *testing.T) {
	c := loadIdempotencyCase(t, "API-052-valid-idempotency-replay.json")
	store := NewIdempotencyStore(fixedClock(idempotencyBaseMs), 0) // 0 → default 24h retention

	creates := 0
	// createResponse is the pinned 201 the create side effect produces; the
	// corpus's first and second (replayed) response bodies are byte-identical.
	createBody, err := json.Marshal(c.Expected.Responses[0].Body)
	if err != nil {
		t.Fatalf("marshal corpus create body: %v", err)
	}

	run := func(idx int) (BeginKind, StoredResponse) {
		req := c.Input.Requests[idx]
		scope := IdempotencyScope{Principal: req.Principal, Method: req.Method, Path: req.Path}
		key := req.Headers["Idempotency-Key"]
		bodyBytes, _ := json.Marshal(req.Body)
		hash := IdempotencyBodyHash(bodyBytes)

		outcome := store.Begin(scope, key, hash, idempotencyBaseMs)
		switch outcome.Kind {
		case BeginReplay:
			return outcome.Kind, outcome.Response
		case BeginFresh:
			creates++ // the CreateScopeNode side effect runs here, and only here
			resp := StoredResponse{Status: 201, Body: createBody, ContentType: "application/json"}
			store.Complete(scope, key, hash, resp, idempotencyBaseMs)
			return outcome.Kind, resp
		default:
			t.Fatalf("request %d: outcome kind %v, want Fresh or Replay", idx, outcome.Kind)
			return outcome.Kind, StoredResponse{}
		}
	}

	firstKind, first := run(0)
	secondKind, second := run(1)

	if firstKind != BeginFresh {
		t.Errorf("first request outcome = %v, want Fresh (API-050)", firstKind)
	}
	if secondKind != BeginReplay {
		t.Errorf("second request outcome = %v, want Replay (API-052)", secondKind)
	}
	if creates != c.Expected.ResourcesCreated {
		t.Errorf("resources created = %d, want %d (a replay must not re-execute the create, API-052)", creates, c.Expected.ResourcesCreated)
	}

	// The replayed response is byte-identical to the first (API-052 verbatim).
	if second.Status != first.Status {
		t.Errorf("replay status = %d, want %d (verbatim replay)", second.Status, first.Status)
	}
	if !bytes.Equal(second.Body, first.Body) {
		t.Errorf("replay body = %s, want %s (byte-identical replay, API-052)", second.Body, first.Body)
	}

	// And it matches the corpus's pinned second (replayed) response body.
	var got map[string]any
	if err := json.Unmarshal(second.Body, &got); err != nil {
		t.Fatalf("decode replayed body: %v", err)
	}
	assertBodyMatchesCorpus(t, got, c.Expected.Responses[1].Body)
	if !c.Expected.Responses[1].Replayed {
		t.Fatal("corpus API-052 responses[1] must be replayed:true (the marker this store enables)")
	}
}

// TestIdempotencyReuseConflictCorpus drives API-051/053: a repeat presenting
// the same Idempotency-Key scope with a DIFFERENT body is a Conflict —
// 409/IDEMPOTENCY_KEY_REUSED — and MUST NOT execute the operation.
func TestIdempotencyReuseConflictCorpus(t *testing.T) {
	c := loadIdempotencyCase(t, "API-053-invalid-idempotency-key-reused-different-body.json")
	store := NewIdempotencyStore(fixedClock(idempotencyBaseMs), 0)

	creates := 0

	req0 := c.Input.Requests[0]
	scope0 := IdempotencyScope{Principal: req0.Principal, Method: req0.Method, Path: req0.Path}
	key := req0.Headers["Idempotency-Key"]
	body0, _ := json.Marshal(req0.Body)
	hash0 := IdempotencyBodyHash(body0)

	if out := store.Begin(scope0, key, hash0, idempotencyBaseMs); out.Kind != BeginFresh {
		t.Fatalf("first request outcome = %v, want Fresh", out.Kind)
	}
	creates++
	createBody, _ := json.Marshal(c.Expected.Responses[0].Body)
	store.Complete(scope0, key, hash0, StoredResponse{Status: 201, Body: createBody, ContentType: "application/json"}, idempotencyBaseMs)

	// Second POST: same key + scope, different body.
	req1 := c.Input.Requests[1]
	scope1 := IdempotencyScope{Principal: req1.Principal, Method: req1.Method, Path: req1.Path}
	body1, _ := json.Marshal(req1.Body)
	hash1 := IdempotencyBodyHash(body1)

	out := store.Begin(scope1, key, hash1, idempotencyBaseMs)
	if out.Kind != BeginConflict {
		t.Fatalf("reused key + different body outcome = %v, want Conflict (API-053)", out.Kind)
	}
	if creates != c.Expected.ResourcesCreated {
		t.Errorf("resources created = %d, want %d (a REUSED key must not execute, API-053)", creates, c.Expected.ResourcesCreated)
	}

	// The driver renders the Conflict as the corpus's pinned Problem; assert the
	// registry code + status + verbatim detail this store's constant produces.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, req1.Path, nil)
	detail := IdempotencyReuseDetail(key)
	WriteProblemExt(w, r, "", http.StatusConflict, CodeIdempotencyKeyReused, "Conflict", detail, nil)

	if w.Code != c.Expected.Responses[1].Status {
		t.Errorf("status = %d, want %d", w.Code, c.Expected.Responses[1].Status)
	}
	if ct := w.Header().Get("Content-Type"); ct != c.Expected.Responses[1].ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, c.Expected.Responses[1].ContentType)
	}
	var gotProblem map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &gotProblem); err != nil {
		t.Fatalf("decode Problem: %v", err)
	}
	assertBodyMatchesCorpus(t, gotProblem, c.Expected.Responses[1].Body)
}

// TestIdempotencyInProgress drives API-054: a repeat whose original request is
// still executing (begun, no Complete yet) with the SAME body is InProgress —
// 409/IDEMPOTENCY_KEY_IN_PROGRESS — never a concurrent execute or a block.
func TestIdempotencyInProgress(t *testing.T) {
	store := NewIdempotencyStore(fixedClock(idempotencyBaseMs), 0)
	scope := IdempotencyScope{Principal: "usr_01J8Z000000000000000000001", Method: http.MethodPost, Path: "/api/v1/scope-nodes"}
	const key = "b7e23ec2-9c4a-4a9b-8f2e-9d7c6b5a4321"
	hash := IdempotencyBodyHash([]byte(`{"kind":"site","name":"New Site"}`))

	if out := store.Begin(scope, key, hash, idempotencyBaseMs); out.Kind != BeginFresh {
		t.Fatalf("first Begin = %v, want Fresh", out.Kind)
	}
	// No Complete: the original is still in flight.
	if out := store.Begin(scope, key, hash, idempotencyBaseMs); out.Kind != BeginInProgress {
		t.Errorf("in-flight repeat, same body = %v, want InProgress (API-054)", out.Kind)
	}

	// A different body over the SAME in-flight key is a Conflict (REUSED),
	// not InProgress — the body-hash mismatch wins regardless of completion.
	otherHash := IdempotencyBodyHash([]byte(`{"kind":"site","name":"Other"}`))
	if out := store.Begin(scope, key, otherHash, idempotencyBaseMs); out.Kind != BeginConflict {
		t.Errorf("in-flight repeat, different body = %v, want Conflict (API-053)", out.Kind)
	}

	// The IN_PROGRESS registry code renders a conformant 409 Problem.
	if CodeIdempotencyKeyInProgress != "IDEMPOTENCY_KEY_IN_PROGRESS" {
		t.Errorf("in-progress code = %q, want IDEMPOTENCY_KEY_IN_PROGRESS (API-054)", CodeIdempotencyKeyInProgress)
	}
}

// TestIdempotencyAbortReleasesInFlight covers Abort: a fresh Begin whose request
// then fails transiently (no retainable response) is released so the SAME key+body
// re-executes as Fresh rather than wedging InProgress forever. A completed entry,
// by contrast, is never dropped by Abort.
func TestIdempotencyAbortReleasesInFlight(t *testing.T) {
	store := NewIdempotencyStore(fixedClock(idempotencyBaseMs), 0)
	scope := IdempotencyScope{Principal: "usr_01J8Z000000000000000000001", Method: http.MethodPost, Path: "/api/v1/scope-nodes"}
	const key = "c8f34fd3-0d5b-4bac-9a3f-0e8d7c6b5432"
	hash := IdempotencyBodyHash([]byte(`{"kind":"site","name":"New Site"}`))

	if out := store.Begin(scope, key, hash, idempotencyBaseMs); out.Kind != BeginFresh {
		t.Fatalf("first Begin = %v, want Fresh", out.Kind)
	}
	// The original failed transiently: release the in-flight marker.
	store.Abort(scope, key, hash)
	if n := store.len(); n != 0 {
		t.Fatalf("Abort left %d entries, want 0 (in-flight marker released)", n)
	}
	// The retry re-executes as Fresh, not InProgress — the key is not wedged.
	if out := store.Begin(scope, key, hash, idempotencyBaseMs); out.Kind != BeginFresh {
		t.Fatalf("retry after Abort = %v, want Fresh", out.Kind)
	}

	// Abort never drops an already-completed entry (a retained response — success
	// OR a deterministic failure — is preserved for replay).
	store.Complete(scope, key, hash, StoredResponse{Status: 400, Body: []byte(`{"code":"EXTERNAL_ID_CONFLICT"}`), ContentType: ProblemContentType}, idempotencyBaseMs)
	store.Abort(scope, key, hash)
	if out := store.Begin(scope, key, hash, idempotencyBaseMs); out.Kind != BeginReplay {
		t.Fatalf("Begin after Complete+Abort = %v, want Replay (completed entry not dropped)", out.Kind)
	}

	// A hash mismatch or an unknown key is a no-op.
	store.Abort(scope, key, IdempotencyBodyHash([]byte(`{"other":true}`)))
	store.Abort(scope, "no-such-key", hash)
	store.Abort(scope, "", hash) // empty key: nothing was ever stored
}

// TestIdempotencyScopeIsolation drives API-051: the identical key value under a
// different principal, method, or path is a fresh request, never a replay.
func TestIdempotencyScopeIsolation(t *testing.T) {
	store := NewIdempotencyStore(fixedClock(idempotencyBaseMs), 0)
	const key = "3f1b1e2a-6c4d-4a9b-8f2e-9d7c6b5a4321"
	hash := IdempotencyBodyHash([]byte(`{"kind":"site","name":"New Site"}`))

	base := IdempotencyScope{Principal: "usr_A", Method: http.MethodPost, Path: "/api/v1/scope-nodes"}
	if out := store.Begin(base, key, hash, idempotencyBaseMs); out.Kind != BeginFresh {
		t.Fatalf("first Begin = %v, want Fresh", out.Kind)
	}
	store.Complete(base, key, hash, StoredResponse{Status: 201, Body: []byte(`{}`)}, idempotencyBaseMs)

	// The base scope now replays; each different-scope variant is Fresh.
	if out := store.Begin(base, key, hash, idempotencyBaseMs); out.Kind != BeginReplay {
		t.Fatalf("same scope + key + body = %v, want Replay (control)", out.Kind)
	}

	variants := map[string]IdempotencyScope{
		"different principal": {Principal: "usr_B", Method: http.MethodPost, Path: "/api/v1/scope-nodes"},
		"different method":    {Principal: "usr_A", Method: http.MethodPut, Path: "/api/v1/scope-nodes"},
		"different path":      {Principal: "usr_A", Method: http.MethodPost, Path: "/api/v1/automations"},
	}
	for name, scope := range variants {
		if out := store.Begin(scope, key, hash, idempotencyBaseMs); out.Kind != BeginFresh {
			t.Errorf("%s: outcome = %v, want Fresh (API-051 scope isolation)", name, out.Kind)
		}
	}
}

// TestIdempotencyRetentionExpiry drives API-052/055: a completed entry replays
// through the full 24h window (retained AT LEAST that long) and then, once the
// window has elapsed, the same key executes as a fresh request.
func TestIdempotencyRetentionExpiry(t *testing.T) {
	const retentionMs = int64(24 * 60 * 60 * 1000)
	store := NewIdempotencyStore(fixedClock(idempotencyBaseMs), retentionMs)

	scope := IdempotencyScope{Principal: "usr_A", Method: http.MethodPost, Path: "/api/v1/scope-nodes"}
	const key = "9c2a4b6e-1234-4a9b-8f2e-9d7c6b5a4321"
	hash := IdempotencyBodyHash([]byte(`{"kind":"site","name":"New Site"}`))

	if out := store.Begin(scope, key, hash, idempotencyBaseMs); out.Kind != BeginFresh {
		t.Fatalf("first Begin = %v, want Fresh", out.Kind)
	}
	store.Complete(scope, key, hash, StoredResponse{Status: 201, Body: []byte(`{}`)}, idempotencyBaseMs)

	// At exactly the 24h boundary the entry is still retained (API-052 "at
	// least 24 hours from first use") → a matching repeat still replays.
	if out := store.Begin(scope, key, hash, idempotencyBaseMs+retentionMs); out.Kind != BeginReplay {
		t.Errorf("at exactly retentionMs = %v, want Replay (retained at least 24h, API-052)", out.Kind)
	}

	// One ms past the window the entry is treated as absent → Fresh (API-055).
	if out := store.Begin(scope, key, hash, idempotencyBaseMs+retentionMs+1); out.Kind != BeginFresh {
		t.Errorf("past retentionMs = %v, want Fresh (window elapsed, API-055)", out.Kind)
	}
}

// TestIdempotencyEmptyKeyAlwaysFresh drives API-050: the header is optional; an
// empty key always yields Fresh and stores nothing, so it can never replay,
// conflict, or block a later request.
func TestIdempotencyEmptyKeyAlwaysFresh(t *testing.T) {
	store := NewIdempotencyStore(fixedClock(idempotencyBaseMs), 0)
	scope := IdempotencyScope{Principal: "usr_A", Method: http.MethodPost, Path: "/api/v1/scope-nodes"}
	hash := IdempotencyBodyHash([]byte(`{"kind":"site","name":"New Site"}`))

	for i := 0; i < 3; i++ {
		if out := store.Begin(scope, "", hash, idempotencyBaseMs); out.Kind != BeginFresh {
			t.Errorf("empty-key Begin #%d = %v, want Fresh (optional header, API-050)", i, out.Kind)
		}
		// Complete with an empty key must be a no-op (no storage).
		store.Complete(scope, "", hash, StoredResponse{Status: 201, Body: []byte(`{}`)}, idempotencyBaseMs)
	}
	if n := store.len(); n != 0 {
		t.Errorf("store retained %d entries for empty-key requests, want 0 (no storage, API-050)", n)
	}
}

// TestIdempotencyBodyHashSHA256Hex asserts the body-hash helper is SHA-256 hex
// of the raw bytes — the content hash API-052 keys replay-vs-reuse on — and is
// stable and body-sensitive.
func TestIdempotencyBodyHashSHA256Hex(t *testing.T) {
	// Known SHA-256 of the empty input.
	if got := IdempotencyBodyHash(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("IdempotencyBodyHash(nil) = %q, want the SHA-256 hex of the empty input", got)
	}
	a := IdempotencyBodyHash([]byte(`{"kind":"site"}`))
	if len(a) != 64 {
		t.Errorf("hash length = %d, want 64 (SHA-256 hex)", len(a))
	}
	if a != IdempotencyBodyHash([]byte(`{"kind":"site"}`)) {
		t.Error("hash is not stable for identical bytes")
	}
	if a == IdempotencyBodyHash([]byte(`{"kind":"screen"}`)) {
		t.Error("hash collided across different bodies")
	}
}
