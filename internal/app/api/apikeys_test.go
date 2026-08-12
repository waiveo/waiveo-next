package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// doBearer issues a request carrying ONLY a bearer token — no session cookie,
// no CSRF header. The shared `do` attaches the env's session centrally, which
// is right for every other test and wrong for this one: the whole question here
// is whether the KEY authenticates, and a request that also carries a session
// would answer yes whatever the key did.
func doBearer(t *testing.T, e *testEnv, method, path, token string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

// API keys over the real HTTP surface (security-model SEC-003a–e, SEC-020).
//
// The register's line for this family is "production automation has no
// credential path" — the transport authenticated a bearer token, and nothing an
// operator could do produced one. These are the three operations that close it,
// and each test below pins the rule that makes the operation safe rather than
// merely present.

type apiKeyListResponse struct {
	Items []struct {
		ID         string `json:"id"`
		Label      string `json:"label"`
		CreatedAt  int64  `json:"created_at"`
		LastUsedAt int64  `json:"last_used_at"`
	} `json:"items"`
}

func TestMintApiKeyReturnsThePlaintextExactlyOnce(t *testing.T) {
	e := newEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/auth/api-keys",
		mustJSON(t, map[string]any{"label": "ci-runner"}), jsonHeaders)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint status = %d, want 201 (%s)", resp.StatusCode, raw)
	}
	var minted struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Key   string `json:"key"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil {
		t.Fatalf("decode mint: %v (%s)", err, raw)
	}
	if minted.Key == "" {
		t.Fatal("the mint returned no plaintext — it is the only time it is ever returned (SEC-003e)")
	}
	if minted.ID == "" || minted.Label != "ci-runner" {
		t.Fatalf("mint = %+v, want an id and the label back", minted)
	}

	// SEC-003e's actual bar: the listing carries the label and the timestamps,
	// and NEITHER the secret NOR any prefix of it. A prefix is a shortcut for an
	// attacker holding a partial capture and buys a legitimate operator nothing
	// a label does not.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/auth/api-keys", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), minted.Key) {
		t.Fatal("the listing returned the key's plaintext (SEC-003e)")
	}
	// Any run of the secret long enough to be a useful prefix must be absent too.
	if len(minted.Key) >= 8 && strings.Contains(string(raw), minted.Key[:8]) {
		t.Fatal("the listing returned a PREFIX of the key (SEC-003e forbids a prefix, not just the whole secret)")
	}
	// And the FIELD SET is closed, which is the assertion that actually holds.
	// Searching the body for the plaintext only catches a leak of the plaintext,
	// and the credential row stores a token HASH — a listing that leaked THAT
	// would pass every check above while shipping credential material off the
	// box. Measured: leaking `c.secret` into the wire survived the two greps and
	// fails here. Any new member has to be added deliberately, in this list.
	var shape struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("decode list shape: %v (%s)", err, raw)
	}
	allowed := map[string]bool{"id": true, "label": true, "created_at": true, "last_used_at": true, "expires_at": true}
	for _, item := range shape.Items {
		for field := range item {
			if !allowed[field] {
				t.Errorf("an api-key listing carries an undeclared field %q — SEC-003e's listing is label, created_at and last_used_at, and nothing that could be credential material", field)
			}
		}
	}
	var list apiKeyListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, raw)
	}
	// Counted BY LABEL: the test environment authenticates with an api-key of
	// its own, so the listing legitimately carries more than the one just
	// minted. Asserting a total would be asserting a fixture detail.
	mine := 0
	for _, it := range list.Items {
		if it.Label != "ci-runner" {
			continue
		}
		mine++
		if it.CreatedAt == 0 {
			t.Error("the listing carries no created_at, which SEC-003e requires")
		}
	}
	if mine != 1 {
		t.Fatalf("keys labelled ci-runner = %d, want 1; list = %+v", mine, list.Items)
	}
}

// A minted key actually authenticates — otherwise the credential path is a
// listing of things that do not work.
func TestAMintedApiKeyAuthenticates(t *testing.T) {
	e := newEnv(t)

	_, raw := e.do(t, http.MethodPost, "/api/v1/auth/api-keys",
		mustJSON(t, map[string]any{"label": "ci-runner"}), jsonHeaders)
	var minted struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil {
		t.Fatalf("decode mint: %v (%s)", err, raw)
	}

	// Presented as a bearer, with no session cookie.
	resp, body := doBearer(t, e, http.MethodGet, "/api/v1/auth/api-keys", minted.Key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer-authenticated request = %d, want 200 (%s)", resp.StatusCode, body)
	}

	// SEC-020: revoked through the same mechanism a session is, and refused
	// from the next request onward.
	if resp, body := e.do(t, http.MethodDelete, "/api/v1/auth/api-keys/"+minted.ID, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204 (%s)", resp.StatusCode, body)
	}
	if resp, _ := doBearer(t, e, http.MethodGet, "/api/v1/auth/api-keys", minted.Key); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a REVOKED key still authenticated: %d", resp.StatusCode)
	}
}

// A retry that re-minted would leave a second live credential whose plaintext
// the operator never saw — and SEC-003e means they can never see it again.
func TestMintingIsIdempotentUnderOneKey(t *testing.T) {
	e := newEnv(t)
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "01JZZZZZZZZZZZZZZZZZZZZZZZ"}
	body := mustJSON(t, map[string]any{"label": "ci-runner"})

	resp1, raw1 := e.do(t, http.MethodPost, "/api/v1/auth/api-keys", body, headers)
	resp2, raw2 := e.do(t, http.MethodPost, "/api/v1/auth/api-keys", body, headers)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first mint = %d (%s)", resp1.StatusCode, raw1)
	}
	if resp2.StatusCode != http.StatusCreated || string(raw1) != string(raw2) {
		t.Fatalf("the retry did not replay the original outcome: %d\n%s\n%s", resp2.StatusCode, raw1, raw2)
	}

	_, listRaw := e.do(t, http.MethodGet, "/api/v1/auth/api-keys", nil, nil)
	var list apiKeyListResponse
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	minted := 0
	for _, it := range list.Items {
		if it.Label == "ci-runner" {
			minted++
		}
	}
	if minted != 1 {
		t.Fatalf("keys labelled ci-runner after a RETRIED mint = %d, want 1 — a second key nobody saw the plaintext of is one nobody can knowingly revoke", minted)
	}
}

func TestRevokingAKeyThatIsNotYoursIs404(t *testing.T) {
	e := newEnv(t)
	// A well-formed id that is not among the caller's keys. Same answer whether
	// it belongs to someone else or never existed, so this cannot be used to
	// probe which ids are real.
	if resp, raw := e.do(t, http.MethodDelete, "/api/v1/auth/api-keys/01JZZZZZZZZZZZZZZZZZZZZZZZ", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, raw)
	}
}
