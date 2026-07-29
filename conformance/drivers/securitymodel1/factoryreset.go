package securitymodel1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// driveFactoryReset drives SEC-121-valid-factory-reset-destroys-key-material
// against the LIVE, HTTP-mounted data-subject delete operation (POST
// /api/v1/workspace/delete, internal/app/api/workspace.go) — the route api/1
// API-122 routes to SEC-121's destruction path — run to completion on a real
// JobRunner.
//
// # Destruction is proved by INSPECTION, never by a delete call returning nil
//
// Every post-reset assertion below reads state back through a different door
// than the one the destruction wrote through:
//
//   - the data key is asked FOUR ways — DataKeyPresent, the sealer derivation
//     (which must now refuse with ErrDestroyed rather than hand back a working
//     AEAD over zeroed bytes), the private half (nil, not zeroed), and the
//     filesystem (the persisted key files must be GONE from the key directory);
//   - the workspace is asked through the store's own reader (the org node's
//     account_state and the survival of its child rows);
//   - "fresh enrollment required for every principal" is asked by presenting the
//     pre-reset session token to the live mux, which must now refuse it, and by
//     counting principal rows;
//   - "the first-boot claim window re-opened" is asked by RUNNING the real
//     first-boot bootstrap afterwards and requiring that it reports the box
//     unclaimed and mints a fresh setup grant.
//
// # What this case CANNOT prove here, stated rather than implied
//
// SEC-121 enumerates a box key (SEC-041) and the relay's device identity plus
// its enrollment certificate/keypair among the material a factory reset
// destroys. Neither exists in this tree: there is no box key at all (the
// wrapped-secret hierarchy stops at the workspace data key, see
// internal/app/workspacekey's own file doc), and a relay's enrollment material
// is the RELAY process's persisted state, not the app's. Their expected
// `false` values would therefore be satisfied vacuously — they were already
// absent BEFORE the reset — so this driver does not assert them and records a
// Note on the case instead. That is why the SEC-121 traceability row is not
// claimed as covered by this run.
func driveFactoryReset(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	topology, _ := inputString(c, "topology")
	if topology != "one-box-self-hosted" {
		k.fail(rep, "this driver drives the one-box self-hosted topology; the case declares %q", topology)
		return
	}
	if action, _ := inputString(c, "action"); action != "factory-reset" {
		k.fail(rep, "the case's action is %q, not factory-reset", action)
		return
	}

	h, err := newResetHarness()
	if err != nil {
		k.fail(rep, "build the factory-reset harness: %v", err)
		return
	}
	defer h.close()

	// --- the pre-reset state the case declares --------------------------------
	orgID, err := h.seedWorkspace(ctx)
	if err != nil {
		k.fail(rep, "seed the workspace: %v", err)
		return
	}
	if err := h.bindOwnerAt(ctx, orgID); err != nil {
		k.fail(rep, "bind the caller owner at the workspace root: %v", err)
		return
	}
	siteID, err := h.seedSite(ctx, orgID)
	if err != nil {
		k.fail(rep, "seed a site under the workspace: %v", err)
		return
	}

	// Each pre-reset claim the case makes is CHECKED, not assumed: a "destroyed"
	// assertion over material that was never there proves nothing.
	if want, _ := digInput(c, "pre_reset_state.workspace_present"); want == true {
		if _, found, err := h.store.Get(ctx, store.KindScopeNode, siteID); err != nil || !found {
			k.fail(rep, "the fixture workspace is not present before the reset (found=%v err=%v)", found, err)
			return
		}
	}
	if want, _ := digInput(c, "pre_reset_state.data_key_present"); want == true && !h.key.DataKeyPresent() {
		k.fail(rep, "the fixture holds no data key before the reset")
		return
	}
	if want, _ := digInput(c, "pre_reset_state.box_claim_state"); want == "claimed" {
		owners, err := h.auth.Store.CountOwnerBindings(ctx)
		if err != nil || owners == 0 {
			k.fail(rep, "the fixture box is not claimed before the reset (owner bindings=%d err=%v)", owners, err)
			return
		}
	}
	if boxKey, present := digInput(c, "pre_reset_state.box_key_present"); present && boxKey == true {
		k.note("pre_reset_state.box_key_present / post_reset_state.box_key_present NOT asserted: no box key (SEC-041) exists anywhere in this tree, so its post-reset absence would be vacuously true")
	}
	if relayID, present := digInput(c, "pre_reset_state.relay_device_identity_present"); present && relayID == true {
		k.note("post_reset_state.relay_device_identity_present / relay_enrollment_cert_present NOT asserted: a relay's enrollment material is the relay process's own persisted state (internal/relay/identity), which this app-side reset path does not reach")
	}

	// --- the reset itself, through the shipped route --------------------------
	resp := h.post("/api/v1/workspace/delete", map[string]any{"confirm_workspace_id": orgID})
	if resp.status != http.StatusAccepted {
		k.fail(rep, "the delete was not accepted: %d %s", resp.status, resp.raw)
		return
	}
	h.jobs.Start()
	h.jobs.Wait()

	// --- what survived ---------------------------------------------------------
	_, siteFound, err := h.store.Get(ctx, store.KindScopeNode, siteID)
	if err != nil {
		k.fail(rep, "read the site node after the reset: %v", err)
		return
	}
	_, accountState, workspaceErr := h.store.WorkspaceRoot(ctx)
	// "workspace_present" is read as the workspace's DATA being present: its
	// rows, and an org node that is not a `purged` tombstone. DAT-012 keeps the
	// org row itself as that tombstone on purpose, so its bare existence is not
	// the question.
	workspacePresent := siteFound || (workspaceErr == nil && accountState != "purged")

	// The data key, asked four ways.
	_, sealerErr := h.key.SecretSealer()
	keyFilesLeft := workspacekey.StrayKeyFiles(h.keyDir)
	dataKeyPresent := h.key.DataKeyPresent() || sealerErr == nil || h.key.Private() != nil || len(keyFilesLeft) > 0

	principals, err := h.auth.Store.CountPrincipals(ctx)
	if err != nil {
		k.fail(rep, "count principals after the reset: %v", err)
		return
	}
	owners, err := h.auth.Store.CountOwnerBindings(ctx)
	if err != nil {
		k.fail(rep, "count owner bindings after the reset: %v", err)
		return
	}
	// The pre-reset credential presented to the live mux: it must no longer
	// authenticate. This is the observable behind "MUST force fresh enrollment
	// on every principal" — a count of zero rows says the rows went, this says
	// nothing previously issued still works.
	stale := h.get("/api/v1/scope-nodes")
	freshEnrollmentRequired := principals == 0 && stale.status == http.StatusUnauthorized

	// The claim window, asked by RUNNING the real first-boot bootstrap.
	boot, err := auth.EnsureClaimWindow(ctx, h.auth.Store, h.stateDir, auth.RootScopeNode, nil)
	if err != nil {
		k.fail(rep, "re-run the first-boot bootstrap after the reset: %v", err)
		return
	}
	claimReopened := !boot.Claimed && boot.Code != ""

	k.boolAt("post_reset_state.workspace_present", workspacePresent)
	k.boolAt("post_reset_state.data_key_present", dataKeyPresent)
	k.stringAt("post_reset_state.box_claim_state", claimState(owners))
	k.boolAt("fresh_enrollment_required_for_every_principal", freshEnrollmentRequired)
	k.boolAt("first_boot_claim_window_reopened", claimReopened)
	k.finish(rep)
}

// claimState renders the box's claim state the way the corpus spells it.
func claimState(ownerBindings int) string {
	if ownerBindings > 0 {
		return "claimed"
	}
	return "unclaimed"
}

// resetHarness mounts the SAME http.Handler production wires (api.New) over a
// real store, a real workspace signing key in its OWN directory (SEC-047's
// separation, and what lets this driver check the filesystem for leftovers), and
// a real, explicitly-owned JobRunner so the accepted delete can be run to
// completion and its effects inspected.
type resetHarness struct {
	store      *store.Store
	auth       *authtest.Fixture
	key        *workspacekey.Key
	keyDir     string
	archiveDir string
	stateDir   string
	jobs       *api.JobRunner
	handler    http.Handler
}

func newResetHarness() (*resetHarness, error) {
	root, err := os.MkdirTemp("", "secmodel1-factoryreset-")
	if err != nil {
		return nil, err
	}
	keyDir := filepath.Join(root, "keys")
	archiveDir := filepath.Join(root, "archives")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{keyDir, archiveDir, stateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(":memory:")
	if err != nil {
		return nil, err
	}
	fixedNow := int64(1752537600000)
	clock := func() int64 { return fixedNow }
	fixture, err := authtest.New(authtest.Config{NowMs: clock})
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	key, err := workspacekey.LoadOrCreate(keyDir, deterministicIDs())
	if err != nil {
		_ = st.Close()
		fixture.Close()
		return nil, err
	}
	jobs := api.NewJobRunner()
	handler := api.New(st, apihttp.NewIdempotencyStore(clock, 0), clock, deterministicIDs(),
		origin.New(), "https://origin.example", fixture.Auth,
		api.WithJobRunner(jobs),
		api.WithWorkspaceArchive(&api.WorkspaceArchive{Dir: archiveDir, Key: key}))
	return &resetHarness{
		store: st, auth: fixture, key: key,
		keyDir: keyDir, archiveDir: archiveDir, stateDir: stateDir,
		jobs: jobs, handler: handler,
	}, nil
}

func (h *resetHarness) close() {
	h.jobs.Shutdown(context.Background())
	_ = h.store.Close()
	h.auth.Close()
	_ = os.RemoveAll(filepath.Dir(h.keyDir))
}

// seedWorkspace creates the deployment's org node — the row that IS the
// workspace's identity (DAT-010/012/014) — through the live mux.
func (h *resetHarness) seedWorkspace(ctx context.Context) (string, error) {
	return h.createNode(datamodel.ScopeNode{Kind: "org", Name: "Conformance Org", AccountState: "active"})
}

func (h *resetHarness) seedSite(ctx context.Context, orgID string) (string, error) {
	return h.createNode(datamodel.ScopeNode{
		Kind: "site", ParentID: &orgID, Name: "Conformance Site",
		TZ: strPtr("America/Chicago"), Lat: f64Ptr(41.8781), Long: f64Ptr(-87.6298),
	})
}

func (h *resetHarness) createNode(node datamodel.ScopeNode) (string, error) {
	body, err := json.Marshal(node)
	if err != nil {
		return "", err
	}
	res := h.post("/api/v1/scope-nodes", json.RawMessage(body))
	if res.status != http.StatusCreated {
		return "", errorString("create scope node: status " + http.StatusText(res.status) + ": " + string(res.raw))
	}
	id, _ := res.body["id"].(string)
	if id == "" {
		return "", errorString("create scope node: response carried no id: " + string(res.raw))
	}
	return id, nil
}

// bindOwnerAt binds the driving principal `owner` AT the workspace's own org
// node. The delete operation is owner-at-the-workspace-root only, and the
// authtest fixture's default binding is at the tree-wide sentinel — which
// resolves for reads but is not the node this operation authorizes against.
func (h *resetHarness) bindOwnerAt(ctx context.Context, orgID string) error {
	_, err := h.auth.Store.PutRoleBinding(ctx, h.auth.PrincipalID, orgID, auth.RoleOwner)
	return err
}

func (h *resetHarness) post(path string, body any) httpResult {
	var payload []byte
	switch v := body.(type) {
	case json.RawMessage:
		payload = v
	default:
		payload, _ = json.Marshal(v)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	h.auth.Authorize(req)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return httpResult{status: rec.Code, body: decoded, raw: rec.Body.Bytes()}
}

func (h *resetHarness) get(path string) httpResult {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.auth.Authorize(req)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return httpResult{status: rec.Code, body: decoded, raw: rec.Body.Bytes()}
}

func strPtr(s string) *string   { return &s }
func f64Ptr(f float64) *float64 { return &f }
