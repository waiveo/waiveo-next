package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/platformlog"
)

// The actions plane over HTTP. The asymmetry is the thing under test: the
// producer route is addressed by pack, and the two consumer routes are not
// addressed at all — which queue they serve comes from the caller's principal.

// packSession redeems a tier grant the way a real pack does and returns its
// bearer token, so a test can call the consumer routes AS that pack.
func packSession(t *testing.T, e *testEnv, packID string) string {
	t.Helper()
	g, err := e.auth.Store.MintTierGrant(t.Context(), packID, rowScopeNodeA, auth.RoleOperator)
	if err != nil {
		t.Fatalf("MintTierGrant(%s): %v", packID, err)
	}
	sess, err := e.auth.Store.RedeemTierGrant(t.Context(), g.Code, nil)
	if err != nil {
		t.Fatalf("RedeemTierGrant(%s): %v", packID, err)
	}
	return sess.Token
}

// asPack performs a request authenticated as a pack — a bearer token, no cookie
// and no CSRF, which is what a pack process actually presents.
func asPack(t *testing.T, e *testEnv, token, method, path string, body []byte) (*http.Response, map[string]any) {
	t.Helper()
	// Built raw rather than through e.do: that helper attaches the env's own
	// operator session centrally, which would authenticate the call for reasons
	// having nothing to do with the pack token under test.
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		t.Fatalf("%s %s: %v", method, path, doErr)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// invokeAction queues work through the management route, as an operator would.
func invokeAction(t *testing.T, e *testEnv, packID, action string, params map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	// `params` is OMITTED when there are none, not sent as null. The declared
	// schema types it as an object, so an explicit null is refused — which is the
	// schema doing its job: a client meaning "no parameters" says so by leaving
	// the member out, and null would otherwise be silently read as {}.
	payload := map[string]any{}
	if params != nil {
		payload["params"] = params
	}
	body := mustJSON(t, payload)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs/"+packID+"/actions/"+action, body, nil)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// The whole round trip: an operator invokes, the pack leases, the pack answers.
func TestAnActionIsQueuedLeasedByThePackAndAnswered(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	token := packSession(t, e, "acme/menu-board")

	resp, queued := invokeAction(t, e, "acme/menu-board", "run-backup", map[string]any{"full": true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("invoke = %d, want 202 (%+v)", resp.StatusCode, queued)
	}
	if queued["state"] != "pending" || queued["invocation_id"] == "" {
		t.Fatalf("queued = %+v", queued)
	}

	lease, leased := asPack(t, e, token, http.MethodGet, "/api/v1/pack-invocations/pending", nil)
	if lease.StatusCode != http.StatusOK {
		t.Fatalf("lease = %d, want 200 (%+v)", lease.StatusCode, leased)
	}
	if leased["invocation_id"] != queued["invocation_id"] || leased["action"] != "run-backup" {
		t.Fatalf("leased = %+v, want the queued invocation", leased)
	}
	// The params the operator sent reached the pack — a queue that dropped them
	// would look identical at every other assertion.
	params, _ := leased["params"].(map[string]any)
	if params["full"] != true {
		t.Fatalf("params = %v, want the invoked params", leased["params"])
	}

	id, _ := leased["invocation_id"].(string)
	done, answered := asPack(t, e, token, http.MethodPost,
		"/api/v1/pack-invocations/"+id+"/result", mustJSON(t, map[string]any{"result": map[string]any{"archive": "a.zip"}}))
	if done.StatusCode != http.StatusOK {
		t.Fatalf("result = %d, want 200 (%+v)", done.StatusCode, answered)
	}
	if answered["state"] != "succeeded" {
		t.Fatalf("answered = %+v, want succeeded", answered)
	}
}

// An empty queue is 204, not an error. A pack polling an idle host is the common
// case, and an error would make "nothing to do" indistinguishable from a broken
// queue.
func TestAnEmptyQueueIs204(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	token := packSession(t, e, "acme/menu-board")

	resp, _ := asPack(t, e, token, http.MethodGet, "/api/v1/pack-invocations/pending", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty queue = %d, want 204", resp.StatusCode)
	}
}

// THE isolation property. A pack cannot reach another pack's queue, because
// there is no parameter through which it could ask — the route serves the
// caller's own principal and nothing else.
func TestAPackOnlyEverLeasesItsOwnWork(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	e.installSecondActionPack(t)

	// Work exists for menu-board only.
	if resp, body := invokeAction(t, e, "acme/menu-board", "run-backup", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("invoke = %d (%+v)", resp.StatusCode, body)
	}

	// The OTHER pack polls and must see nothing.
	other := packSession(t, e, "acme/other-pack")
	resp, body := asPack(t, e, other, http.MethodGet, "/api/v1/pack-invocations/pending", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a different pack leased %d (%+v) — queues are not isolated", resp.StatusCode, body)
	}
}

// A pack cannot answer another pack's invocation either. Reported as 404 rather
// than 403: the existence of another pack's invocation is not this caller's to
// learn, and a 403 would confirm the id is real.
func TestAPackCannotAnswerAnotherPacksInvocation(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	e.installSecondActionPack(t)

	owner := packSession(t, e, "acme/menu-board")
	invokeAction(t, e, "acme/menu-board", "run-backup", nil)
	_, leased := asPack(t, e, owner, http.MethodGet, "/api/v1/pack-invocations/pending", nil)
	id, _ := leased["invocation_id"].(string)

	thief := packSession(t, e, "acme/other-pack")
	resp, _ := asPack(t, e, thief, http.MethodPost, "/api/v1/pack-invocations/"+id+"/result",
		mustJSON(t, map[string]any{"result": map[string]any{"stolen": true}}))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another pack answered a foreign invocation with %d, want 404", resp.StatusCode)
	}
}

// A human session is refused from the pack-only routes with 403, not 404: the
// route exists and they reached it, and telling an operator it does not exist
// would send them looking for a typo.
func TestAHumanSessionIsRefusedFromThePackOnlyRoutes(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)

	resp, raw := e.do(t, http.MethodGet, "/api/v1/pack-invocations/pending", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator session on the lease route = %d, want 403 (%s)", resp.StatusCode, raw)
	}
}

// An action the pack never declared is refused. Queuing it would hand the pack
// work it never advertised, and the row would sit until its lease expired.
func TestAnUndeclaredActionIsRefused(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)

	resp, _ := invokeAction(t, e, "acme/menu-board", "not-a-declared-action", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("undeclared action = %d, want 404", resp.StatusCode)
	}
}

// The invocation carries the action's MAN-103 idempotency class, copied at
// enqueue. That value is what decides its fate on a lease expiry, so a queue
// that lost it would silently pick one of the two behaviours.
func TestTheInvocationCarriesTheDeclaredIdempotencyClass(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)

	_, safe := invokeAction(t, e, "acme/menu-board", "run-backup", nil)
	if safe["idempotency"] != "safe-to-retry" {
		t.Fatalf("run-backup idempotency = %v, want safe-to-retry", safe["idempotency"])
	}
	_, risky := invokeAction(t, e, "acme/menu-board", "charge-card", nil)
	if risky["idempotency"] != "not-idempotent" {
		t.Fatalf("charge-card idempotency = %v, want not-idempotent", risky["idempotency"])
	}
}

// actionPackManifest is the base fixture plus two declared actions whose
// idempotency classes differ (MAN-103) — the pair the expiry rule turns on.
func actionPackManifest(id string) map[string]any {
	m := packManifest()
	m["id"] = id
	// An action's capabilityScope must name a capability THIS manifest declares
	// (MAN-100) — the validator refuses "*" outright, which is the check working.
	m["capabilities"] = []any{
		map[string]any{"capability": "storage.write", "scope": "*", "reason": "msg:cap.storage"},
	}
	m["actions"] = []any{
		map[string]any{
			"name": "run-backup", "paramsSchema": map[string]any{},
			"capabilityScope": "storage.write", "auditClass": "write", "idempotencyClass": "safe-to-retry",
		},
		map[string]any{
			"name": "charge-card", "paramsSchema": map[string]any{},
			"capabilityScope": "storage.write", "auditClass": "write", "idempotencyClass": "not-idempotent",
		},
	}
	return m
}

func (e *testEnv) installActionPack(t *testing.T) {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", packBundle(t, actionPackManifest("acme/menu-board")), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install action pack: %d %s", resp.StatusCode, raw)
	}
}

// installSecondActionPack installs a DIFFERENT pack, so queue isolation is
// tested against a real second identity rather than against an absence.
func (e *testEnv) installSecondActionPack(t *testing.T) {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", packBundle(t, actionPackManifest("acme/other-pack")), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install second action pack: %d %s", resp.StatusCode, raw)
	}
}

// A pack's log line is attributed to the CALLING pack, and shows up in the
// platform log an operator already reads — filterable by source like any other
// component.
func TestAPackLogLineIsAttributedToTheCallingPack(t *testing.T) {
	// A real buffer, because the assertion is that the line is READABLE
	// afterwards. Without one the route takes its degrade path and drops the
	// line, which is correct behaviour and proves nothing about attribution.
	buf := platformlog.New(64, func() int64 { return 1_752_537_600_000 })
	e := newEnvWithOptions(t, api.WithPlatformLog(buf))
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	token := packSession(t, e, "acme/menu-board")

	resp, _ := asPack(t, e, token, http.MethodPost, "/api/v1/pack-logs",
		mustJSON(t, map[string]any{"level": "warn", "message": "the archive directory is nearly full"}))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("append = %d, want 204", resp.StatusCode)
	}

	// Read it back the way an operator would: through the ordinary log route,
	// filtered to this pack.
	got, raw := e.do(t, http.MethodGet, "/api/v1/platform-logs?source=pack:acme/menu-board", nil, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("read logs = %d (%s)", got.StatusCode, raw)
	}
	// Decoded rather than substring-matched. The body always contains the string
	// "warn" in its level_counts histogram, so `bytes.Contains(raw, "warn")`
	// passes even when the level was thrown away — a mutation proved exactly that
	// about the first version of this assertion.
	var page struct {
		Items []struct {
			Level   string `json:"level"`
			Source  string `json:"source"`
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode logs: %v (%s)", err, raw)
	}
	if len(page.Items) != 1 {
		t.Fatalf("logs for this pack = %d items, want exactly the one it wrote (%s)", len(page.Items), raw)
	}
	entry := page.Items[0]
	if entry.Source != "pack:acme/menu-board" {
		t.Fatalf("source = %q, want the calling pack", entry.Source)
	}
	if entry.Level != "warn" {
		t.Fatalf("level = %q, want the reported warn — an error must not be able to hide as info", entry.Level)
	}
	if entry.Message != "the archive directory is nearly full" {
		t.Fatalf("message = %q", entry.Message)
	}
}

// A pack cannot attribute a line to anyone else. There is no source parameter,
// and the schema refuses one — so a pack cannot make a line look like the
// platform's, or like another extension's.
func TestAPackCannotForgeAnotherSource(t *testing.T) {
	buf := platformlog.New(64, func() int64 { return 1_752_537_600_000 })
	e := newEnvWithOptions(t, api.WithPlatformLog(buf))
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	token := packSession(t, e, "acme/menu-board")

	// An undeclared member is refused outright (API-013a), which is the first
	// line of defence; the second is that nothing reads one even if it arrived.
	resp, _ := asPack(t, e, token, http.MethodPost, "/api/v1/pack-logs",
		mustJSON(t, map[string]any{"message": "impostor", "source": "waiveo-feeder"}))
	if resp.StatusCode == http.StatusNoContent {
		t.Fatal("a body carrying its own source was accepted")
	}

	// And a line whose TEXT looks like another component's prefix is still
	// attributed to the pack — the source comes from the principal, not the words.
	if r, _ := asPack(t, e, token, http.MethodPost, "/api/v1/pack-logs",
		mustJSON(t, map[string]any{"message": "waiveo-relay: pretending to be the relay"})); r.StatusCode != http.StatusNoContent {
		t.Fatalf("append = %d, want 204", r.StatusCode)
	}
	_, raw := e.do(t, http.MethodGet, "/api/v1/platform-logs?source=waiveo-relay", nil, nil)
	if bytes.Contains(raw, []byte("pretending to be the relay")) {
		t.Fatal("a pack's line was attributed to waiveo-relay by its own text")
	}
}

// A human session cannot post pack logs: the route attributes to the caller's
// pack, and an operator has none.
func TestAHumanSessionCannotAppendAPackLog(t *testing.T) {
	buf := platformlog.New(64, func() int64 { return 1_752_537_600_000 })
	e := newEnvWithOptions(t, api.WithPlatformLog(buf))
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)

	resp, _ := e.do(t, http.MethodPost, "/api/v1/pack-logs", mustJSON(t, map[string]any{"message": "hello"}), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator append = %d, want 403", resp.StatusCode)
	}
	// And nothing was WRITTEN. The status alone is not enough: the refusal helper
	// writes 403 before returning, so a handler that ignored its verdict and
	// carried on would still answer 403 while appending a line attributed to an
	// empty pack. A mutation proved that gap in the first version of this test.
	if snap := buf.Read(platformlog.Filter{}); snap.Retained != 0 {
		t.Fatalf("a refused caller still wrote %d line(s) into the platform log", snap.Retained)
	}
}

// An extension's own report appears on the health page beside the first-party
// services — an operator asking whether a box is healthy should not have to know
// which parts of it are extensions.
func TestAPackHealthReportAppearsOnTheHealthPage(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	token := packSession(t, e, "acme/menu-board")

	resp, _ := asPack(t, e, token, http.MethodPost, "/api/v1/pack-health",
		mustJSON(t, map[string]any{"status": "degraded", "detail": "the archive credentials expire in two days"}))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("report = %d, want 204", resp.StatusCode)
	}

	got, raw := e.do(t, http.MethodGet, "/api/v1/system-health", nil, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("system-health = %d (%s)", got.StatusCode, raw)
	}
	var health struct {
		Services []struct {
			Name, Status, Detail string
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	var found bool
	for _, svc := range health.Services {
		if svc.Name != "pack:acme/menu-board" {
			continue
		}
		found = true
		if svc.Status != "degraded" {
			t.Fatalf("status = %q, want the reported degraded", svc.Status)
		}
		if svc.Detail != "the archive credentials expire in two days" {
			t.Fatalf("detail = %q, want what the pack said", svc.Detail)
		}
	}
	if !found {
		t.Fatalf("the pack's health line is not on the page: %+v", health.Services)
	}
}

// A status outside the closed set is refused. It has to be: these values drive
// the page's overall grade through a rank lookup, and an UNRANKED status counts
// as the best one — so an invented status would let an extension report its own
// failure in a way that improves the summary.
func TestAnInventedHealthStatusIsRefused(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	token := packSession(t, e, "acme/menu-board")

	resp, _ := asPack(t, e, token, http.MethodPost, "/api/v1/pack-health",
		mustJSON(t, map[string]any{"status": "mostly-fine", "detail": "eh"}))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invented status = %d, want 422", resp.StatusCode)
	}
	// And it did not land: a refused report must not reach the page.
	_, raw := e.do(t, http.MethodGet, "/api/v1/system-health", nil, nil)
	if bytes.Contains(raw, []byte("mostly-fine")) {
		t.Fatalf("a refused status reached the health page: %s", raw)
	}
}

// A detail is required even for `ok` — the difference between a check that ran
// and one that was skipped, which is what every other line on this page promises.
func TestAHealthReportWithoutADetailIsRefused(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)
	token := packSession(t, e, "acme/menu-board")

	resp, _ := asPack(t, e, token, http.MethodPost, "/api/v1/pack-health",
		mustJSON(t, map[string]any{"status": "ok"}))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("detail-less report = %d, want 422", resp.StatusCode)
	}
}

// An explicit null `params` is refused rather than silently read as "none". The
// declared schema types it as an object; a caller with no parameters omits the
// member. Pinned because the hand-written check this replaced accepted null, so
// the tightening is deliberate rather than incidental.
func TestAnExplicitNullParamsIsRefused(t *testing.T) {
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	e.installActionPack(t)

	resp, _ := e.do(t, http.MethodPost, "/api/v1/packs/acme/menu-board/actions/run-backup",
		mustJSON(t, map[string]any{"params": nil}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("params:null = %d, want 422", resp.StatusCode)
	}
}
