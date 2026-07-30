package api_test

// webhookendpoints_test.go covers the registration surface's own behaviour: the
// conventions it inherits by being a resourceConfig family, and the three
// operations beyond CRUD it adds.
//
// The single invariant every case here circles is that a signing secret goes IN
// and never comes back out — not in a create, a read, a list, a rotation
// response, a refusal, or the delivery-state view.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/webhookdeliver"
	"github.com/maaxton/waiveo-next/internal/shared/secretseal"
)

// whSecret is a fixture signing secret at the surface's 32-character floor.
const whSecret = "whsec_case_0123456789abcdef01234"

// whSecondSecret is what a rotation installs over it.
const whSecondSecret = "whsec_case_fedcba98765432100abcd"

// whSealer is the REAL sealing construction over a fixed key. A stub here would
// let every "the secret was stored sealed" claim in this file hold against an
// implementation that stored it in the clear.
func whSealer(t *testing.T) *secretseal.Sealer {
	t.Helper()
	key := make([]byte, secretseal.KeySize)
	for i := range key {
		key[i] = byte(i*5 + 17)
	}
	s, err := secretseal.New(key)
	if err != nil {
		t.Fatalf("secretseal.New: %v", err)
	}
	return s
}

// newWebhookEnv is newEnv with the webhook secret sealer wired, which is what
// the rotate operation needs.
func newWebhookEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnvWithOptions(t, api.WithWebhookSecrets(webhookdeliver.NewSecrets(whSealer(t)), 0))
}

func (e *testEnv) createWebhookEndpoint(t *testing.T, scopeNode string, extra map[string]any) (string, []byte) {
	t.Helper()
	body := map[string]any{
		"name":       "Ops Endpoint",
		"scope_node": scopeNode,
		"url":        "https://hooks.example.invalid/waiveo",
	}
	for k, v := range extra {
		body[k] = v
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints", mustJSON(t, body), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create webhook endpoint: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw), raw
}

func (e *testEnv) rootOrg(t *testing.T) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes",
		mustJSON(t, map[string]any{"kind": "org", "name": "Root Org", "account_state": "active", "entitlements": map[string]any{}}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create org: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// TestSigningSecretNeverAppearsInAnyResponse is the file's central claim, driven
// across every response the surface produces for an endpoint that has one.
func TestSigningSecretNeverAppearsInAnyResponse(t *testing.T) {
	e := newWebhookEnv(t)
	org := e.rootOrg(t)
	id, created := e.createWebhookEndpoint(t, org, nil)

	rotResp, rotated := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/signing-secret",
		mustJSON(t, map[string]any{"secret": whSecret}), nil)
	if rotResp.StatusCode != http.StatusOK {
		t.Fatalf("install signing secret: %d %s", rotResp.StatusCode, rotated)
	}

	_, read := e.do(t, http.MethodGet, "/api/v1/webhook-endpoints/"+id, nil, nil)
	_, listed := e.do(t, http.MethodGet, "/api/v1/webhook-endpoints", nil, nil)
	_, delivery := e.do(t, http.MethodGet, "/api/v1/webhook-endpoints/"+id+"/delivery", nil, nil)
	_, enabled := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/enable", []byte(`{}`), nil)
	// A refusal is a response too, and the most tempting place to echo the
	// offending value back.
	_, refused := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/signing-secret",
		mustJSON(t, map[string]any{"secret": "too-short"}), nil)

	for name, body := range map[string][]byte{
		"create": created, "rotate": rotated, "get": read,
		"list": listed, "delivery": delivery, "enable": enabled, "refusal": refused,
	} {
		if strings.Contains(string(body), whSecret) {
			t.Errorf("the %s response carries the signing secret: %s", name, body)
		}
		if strings.Contains(string(body), "too-short") {
			t.Errorf("the %s response echoes a rejected secret back: %s", name, body)
		}
	}

	// And it IS stored — sealed, so the column is not the plaintext either.
	st, err := e.store.WebhookDeliveryStateFor(t.Context(), id)
	if err != nil {
		t.Fatalf("read delivery state: %v", err)
	}
	if st.SealedSecret == "" {
		t.Fatal("no secret was stored at all; the case above would then pass vacuously")
	}
	if strings.Contains(st.SealedSecret, whSecret) {
		t.Fatal("the stored column contains the plaintext secret")
	}
	opened, err := whSealer(t).Open(st.SealedSecret, []byte("webhook-secret:"+id))
	if err != nil {
		t.Fatalf("the stored secret did not open under the workspace key and this endpoint's context: %v", err)
	}
	if string(opened) != whSecret {
		t.Fatal("the stored secret did not round-trip to what was installed")
	}
}

// TestRotationReportsTheOverlapWindowAndOnlyAfterTheFirstInstall: the first
// install supersedes nothing, so it opens no overlap; a later one does and says
// when it closes.
func TestRotationReportsTheOverlapWindowAndOnlyAfterTheFirstInstall(t *testing.T) {
	e := newWebhookEnv(t)
	id, _ := e.createWebhookEndpoint(t, e.rootOrg(t), nil)

	_, first := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/signing-secret",
		mustJSON(t, map[string]any{"secret": whSecret}), nil)
	var f struct {
		RotatedAtMs int64  `json:"rotated_at_ms"`
		Expires     *int64 `json:"prior_secret_expires_at_ms"`
	}
	if err := json.Unmarshal(first, &f); err != nil {
		t.Fatalf("decode first rotation: %v (%s)", err, first)
	}
	if f.Expires != nil {
		t.Fatalf("the FIRST install published an overlap expiry (%d); it supersedes nothing", *f.Expires)
	}
	if f.RotatedAtMs == 0 {
		t.Fatal("the first install reported no rotation instant")
	}

	_, second := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/signing-secret",
		mustJSON(t, map[string]any{"secret": whSecondSecret}), nil)
	var s struct {
		RotatedAtMs int64  `json:"rotated_at_ms"`
		Expires     *int64 `json:"prior_secret_expires_at_ms"`
	}
	if err := json.Unmarshal(second, &s); err != nil {
		t.Fatalf("decode rotation: %v (%s)", err, second)
	}
	if s.Expires == nil {
		t.Fatal("a rotation over an existing secret published no overlap expiry; a receiver has no stated deadline to adopt by (EVT-158)")
	}
	if *s.Expires <= s.RotatedAtMs {
		t.Fatalf("overlap expiry %d is not after the rotation instant %d", *s.Expires, s.RotatedAtMs)
	}

	// Both blobs are present and open under their OWN slot contexts — the prior
	// one was re-sealed, not copied.
	st, err := e.store.WebhookDeliveryStateFor(t.Context(), id)
	if err != nil {
		t.Fatalf("read delivery state: %v", err)
	}
	sealer := whSealer(t)
	cur, err := sealer.Open(st.SealedSecret, []byte("webhook-secret:"+id))
	if err != nil || string(cur) != whSecondSecret {
		t.Fatalf("current secret did not open to the rotated value: %v", err)
	}
	prior, err := sealer.Open(st.SealedPriorSecret, []byte("webhook-prior-secret:"+id))
	if err != nil || string(prior) != whSecret {
		t.Fatalf("prior secret did not open to the superseded value under the PRIOR context: %v", err)
	}
	if _, err := sealer.Open(st.SealedPriorSecret, []byte("webhook-secret:"+id)); err == nil {
		t.Fatal("the prior blob also opens under the CURRENT context; the two slots are not distinguished, so a blob moved between them would open silently")
	}
}

// TestEnableClearsTheFailureRunAndKeepsTheCursor (EVT-154/155): re-enabling is
// an operator act that resets the failure run but never rewinds or skips the
// delivery cursor.
func TestEnableClearsTheFailureRunAndKeepsTheCursor(t *testing.T) {
	e := newWebhookEnv(t)
	id, _ := e.createWebhookEndpoint(t, e.rootOrg(t), nil)

	// The state a delivery loop would have left behind after auto-disabling it.
	if err := e.store.PutWebhookDeliveryProgress(t.Context(), store.WebhookDeliveryState{
		EndpointID: id, Status: "disabled", ConsecutiveFailures: 10,
		LastDeliveredID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", Attempt: 3,
		DeliveryID: "01J8Z3K4N5P6Q7R8S9T0V1W2YF", NextAttemptAtMs: 999_999,
	}); err != nil {
		t.Fatalf("seed delivery state: %v", err)
	}

	resp, raw := e.do(t, http.MethodGet, "/api/v1/webhook-endpoints/"+id+"/delivery", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read delivery state: %d %s", resp.StatusCode, raw)
	}
	var before struct {
		Status              string  `json:"status"`
		ConsecutiveFailures int     `json:"consecutive_failures"`
		LastDeliveredID     *string `json:"last_delivered_id"`
	}
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatalf("decode delivery state: %v", err)
	}
	if before.Status != "disabled" || before.ConsecutiveFailures != 10 {
		t.Fatalf("delivery state before enabling = %+v; want the disabled state that was seeded", before)
	}

	resp, raw = e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/enable", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable: %d %s", resp.StatusCode, raw)
	}
	var after struct {
		Status              string  `json:"status"`
		ConsecutiveFailures int     `json:"consecutive_failures"`
		LastDeliveredID     *string `json:"last_delivered_id"`
	}
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatalf("decode enable response: %v", err)
	}
	if after.Status != "active" || after.ConsecutiveFailures != 0 {
		t.Fatalf("after enabling = %+v; want active with a cleared failure run (EVT-154)", after)
	}
	if after.LastDeliveredID == nil || *after.LastDeliveredID != "01J8Z3K4N5P6Q7R8S9T0V1W2Y7" {
		t.Fatalf("last_delivered_id = %v; the cursor must survive a re-enable so delivery resumes where it stopped (EVT-155)", after.LastDeliveredID)
	}

	// Enabling an already-active endpoint is the same end state, not an error.
	resp, raw = e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/enable", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second enable: %d %s", resp.StatusCode, raw)
	}
}

// TestEndpointURLValidation refuses at the door the URLs whose contents would be
// disclosed everywhere the URL is served.
func TestEndpointURLValidation(t *testing.T) {
	e := newWebhookEnv(t)
	org := e.rootOrg(t)

	for name, url := range map[string]string{
		"no scheme":    "hooks.example.invalid/waiveo",
		"wrong scheme": "ftp://hooks.example.invalid/waiveo",
		"userinfo":     "https://user:hunter2@hooks.example.invalid/waiveo",
		"query string": "https://hooks.example.invalid/waiveo?token=s3cr3t",
		"no host":      "https:///waiveo",
		"empty":/* */ "",
	} {
		t.Run(name, func(t *testing.T) {
			resp, raw := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints", mustJSON(t, map[string]any{
				"name": "Bad", "scope_node": org, "url": url,
			}), nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 for a %s URL (body %s)", resp.StatusCode, name, raw)
			}
			assertProblem(t, resp, raw, "VALIDATION_FAILED")
			if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "s3cr3t") {
				t.Fatalf("the refusal echoes the credential it refused: %s", raw)
			}
		})
	}
}

// TestSchemasIsMaterializedOnAMinimalCreate: `schemas` is declared required on
// the response, so a create that never mentioned it must still come back with
// the empty list that says "no schema restriction" rather than with nothing.
func TestSchemasIsMaterializedOnAMinimalCreate(t *testing.T) {
	e := newWebhookEnv(t)
	_, raw := e.createWebhookEndpoint(t, e.rootOrg(t), nil)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if string(got["schemas"]) != "[]" {
		t.Fatalf("schemas = %s on a create that never named it; want []", got["schemas"])
	}
	if string(got["labels"]) != "{}" {
		t.Fatalf("labels = %s; want {}", got["labels"])
	}
}

// TestRotateRefusesWithoutASealer: a deployment with no workspace sealer answers
// UNAVAILABLE rather than storing the secret unsealed. The routes still mount —
// the surface tells the truth about what it cannot do instead of disappearing.
func TestRotateRefusesWithoutASealer(t *testing.T) {
	e := newEnv(t) // deliberately WITHOUT WithWebhookSecrets
	id, _ := e.createWebhookEndpoint(t, e.rootOrg(t), nil)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints/"+id+"/signing-secret",
		mustJSON(t, map[string]any{"secret": whSecret}), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "UNAVAILABLE")

	if _, err := e.store.WebhookDeliveryStateFor(t.Context(), id); err == nil {
		t.Fatal("a refused rotation still wrote delivery state; nothing may be stored when the secret could not be sealed")
	}
}

// TestClientSuppliedIDIsRejected and TestUnknownEndpointIsNotFound pin that the
// family inherits the surface's conventions rather than re-deciding them.
func TestClientSuppliedIDIsRejected(t *testing.T) {
	e := newWebhookEnv(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/webhook-endpoints", mustJSON(t, map[string]any{
		"id": "01J8Z3K4N5P6Q7R8S9T0V1W2YB", "name": "Mine",
		"scope_node": e.rootOrg(t), "url": "https://hooks.example.invalid/waiveo",
	}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a client-supplied id (body %s)", resp.StatusCode, raw)
	}
	// API-105 names the code at the TOP LEVEL of the Problem, which is where a
	// client switches on it (API-016) — not inside an errors[] entry.
	assertProblem(t, resp, raw, "ID_SERVER_ASSIGNED")
}

func TestUnknownEndpointIsNotFoundOnEveryOperation(t *testing.T) {
	e := newWebhookEnv(t)
	e.rootOrg(t)
	const missing = "01J8Z3K4N5P6Q7R8S9T0V1W2ZZ"

	for _, c := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/api/v1/webhook-endpoints/" + missing, nil},
		{http.MethodGet, "/api/v1/webhook-endpoints/" + missing + "/delivery", nil},
		{http.MethodPost, "/api/v1/webhook-endpoints/" + missing + "/enable", []byte(`{}`)},
		{http.MethodPost, "/api/v1/webhook-endpoints/" + missing + "/signing-secret", mustJSON(t, map[string]any{"secret": whSecret})},
	} {
		resp, raw := e.do(t, c.method, c.path, c.body, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404 (body %s)", c.method, c.path, resp.StatusCode, raw)
		}
		assertProblem(t, resp, raw, "NOT_FOUND")
	}
}
