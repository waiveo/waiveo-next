// Package api1 is the executable api/1 conformance driver: the §10
// differential oracle for the cross-cutting request conventions every
// management-door operation in api/openapi.yaml follows. It covers both
// halves of the contract's conventions:
//
//   - the synchronous conventions — optimistic concurrency (ETag/If-Match,
//     API-020-025), keyset pagination (opaque cursor + limit, API-030-036),
//     the label-selector grammar (API-040-046), and the client-assignable
//     external_id convention (API-100-104);
//   - the asynchronous conventions — Idempotency-Key replay/reuse/in-progress
//     (API-050-056) and the 202 + Job resource (API-110-117, API-120-123);
//   - the Problem error shape itself (API-010-016).
//
// Every frozen case is driven; none is pending.
//
// It replays every conformance/corpora/api-1 case against the LIVE,
// HTTP-mounted internal/app/api handler (api.New, over a real
// internal/app/store.Store and internal/shared/apihttp.IdempotencyStore) and
// diffs the actual HTTP behavior against each case's own declared `expected`
// block.
//
// This driver deliberately mounts the same *http.Handler production wires
// (api.New) rather than calling the convention helpers (apihttp/apiselector/
// apijob) directly: a prior version of this driver did the latter, and a
// 2026-07-26 audit found it certified the convention LIBRARIES, not the
// shipped /api/v1 surface — no conformance driver in the repo imported a
// single internal/app/ package. Driving the real handler is not cosmetic: it
// surfaced several genuine divergences between the frozen corpus and the
// shipped code that the old, library-level driver could not see (see the
// per-case doc comments below and pendingCaseIDs/knownDivergent).
//
// A corpus case is not named to a helper by hard-coded case-id knowledge:
// classifyShape inspects the case's own `input` block and dispatches to the
// matching family purely from its structure — the same shape distinctions a
// real router would use to decide which convention governs a given request.
package api1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"time"

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

const contract = "api/1"

// conformanceEntity is the fixture entity ULID every seeded automation's state
// trigger/device_command action names (the same value internal/app/api's own
// automations_test.go fixtures use) so a seeded automation body actually
// compiles (rules/1 RUL-282: the trigger subject and the command target are
// the same entity).
const conformanceEntity = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"

// Run loads the frozen api-1 corpus from disk and drives every convention
// case against the live, HTTP-mounted api.New handler.
func Run() report.Report {
	rep := report.Report{Driver: "api1", Target: "internal/app/api (api.New), internal/app/store"}

	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load api-1 corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against an explicit,
// caller-supplied case set. It is the seam the teeth-tests use: load the real
// corpus, corrupt one case's `expected` block in memory, and confirm the SAME
// comparison logic (never a re-implementation of it) reports FAIL against the
// corrupted expectation.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "api1", Target: "internal/app/api (api.New), internal/app/store"}
	driveCases(&rep, cases)
	return rep
}

// drivenCaseIDs are every api-1 corpus case this driver drives against the
// live HTTP handler — which, since POST /api/v1/workspace/export was mounted,
// is every case in the frozen corpus with none left pending.
var drivenCaseIDs = []string{
	"API-010", "API-013",
	"API-022", "API-023", "API-032", "API-035", "API-044", "API-045", "API-101", "API-102",
	"API-052", "API-053", "API-111", "API-121",
}

// pendingCaseIDs are api/1 cases frozen under conformance/corpora/api-1 that
// no mounted route exists to drive yet.
//
// It is EMPTY, and the map stays rather than being deleted: §10's "no silent
// caps" rule needs somewhere for the next undrivable case to be recorded WITH
// a reason, and a driver that has to grow the mechanism back before it can
// record one is a driver that will record none. Its last entry was API-121,
// pending solely because api.New's mux mounted no /api/v1/workspace/export
// route; that route now exists (internal/app/api/workspace.go) and the case is
// driven below.
var pendingCaseIDs = map[string]string{}

func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	for _, short := range drivenCaseIDs {
		driveByShape(rep, cases, short)
	}
	for _, short := range sortedKeys(pendingCaseIDs) {
		c, ok := corpus.ByID(cases, short)
		if !ok {
			rep.Fail(short, contract, "case not found in frozen corpus")
			continue
		}
		rep.Pending(c.CaseID, contract, pendingCaseIDs[short])
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// driveByShape looks up the named case, classifies its `input` block's shape,
// and dispatches to the matching helper family. A shape the driver does not
// recognize is a driver FAIL, not a silent skip.
func driveByShape(rep *report.Report, cases map[string]corpus.Case, short string) {
	c, ok := corpus.ByID(cases, short)
	if !ok {
		rep.Fail(short, contract, "case not found in frozen corpus")
		return
	}

	switch classifyShape(c.CaseID, c.Input) {
	case shapeProblemNotFound:
		driveProblemNotFound(rep, c)
	case shapeProblemValidation:
		driveProblemValidation(rep, c)
	case shapeConcurrency:
		driveConcurrency(rep, c)
	case shapePagination:
		drivePagination(rep, c)
	case shapeSelector:
		driveSelector(rep, c)
	case shapeExternalID:
		driveExternalID(rep, c)
	case shapeIdempotency:
		driveIdempotency(rep, c)
	case shapeJob:
		driveJob(rep, c)
	case shapeWorkspaceJob:
		driveWorkspaceJob(rep, c)
	default:
		rep.Fail(c.CaseID, contract, "input block did not match any known api/1 convention shape")
	}
}

// shape names the convention family a corpus case's `input` block structurally
// belongs to.
type shape int

const (
	shapeUnknown shape = iota
	shapeProblemNotFound
	shapeProblemValidation
	shapeConcurrency
	shapePagination
	shapeSelector
	shapeExternalID
	shapeIdempotency
	shapeJob
	shapeWorkspaceJob
)

// classifyShape inspects a case's own id and `input` block and reports which
// convention family it belongs to. API-010/013 are named explicitly (their
// input shape — a bare method+path+headers, or a bare method+path+body — is
// not otherwise distinguishable from a job-mutation shape without the case's
// own req_ids); every other case is classified purely from its input's
// structure, as a real router would dispatch on request shape.
func classifyShape(caseID string, input map[string]any) shape {
	switch caseID {
	case "API-010-valid-simple-problem":
		return shapeProblemNotFound
	case "API-013-valid-multi-field-validation-problem":
		return shapeProblemValidation
	}
	if _, ok := input["current_resource_state"]; ok {
		return shapeConcurrency
	}
	if reqs, ok := input["requests"].([]any); ok {
		if _, ok := input["collection_state"]; ok {
			return shapePagination
		}
		if requestsCarryIdempotencyKey(reqs) {
			return shapeIdempotency
		}
		return shapeUnknown
	}
	if req, ok := input["request"].(map[string]any); ok {
		if q, ok := req["query"].(map[string]any); ok {
			if _, ok := q["selector"]; ok {
				return shapeSelector
			}
		}
		return shapeUnknown
	}
	if body, ok := input["body"].(map[string]any); ok {
		if _, ok := body["external_id"]; ok {
			if _, ok := input["collection_state"]; ok {
				return shapeExternalID
			}
		}
	}
	// A case seeding the workspace's own org node is a data-subject operation
	// (API-120-123): its target is the workspace itself rather than a
	// selector-chosen set, which is exactly the structural difference
	// `workspace_state` records.
	if _, ok := input["workspace_state"]; ok {
		return shapeWorkspaceJob
	}
	if _, ok := input["method"].(string); ok {
		if _, ok := input["path"].(string); ok {
			return shapeJob
		}
	}
	return shapeUnknown
}

func requestsCarryIdempotencyKey(reqs []any) bool {
	if len(reqs) == 0 {
		return false
	}
	for _, r := range reqs {
		m, ok := r.(map[string]any)
		if !ok {
			return false
		}
		h, ok := m["headers"].(map[string]any)
		if !ok {
			return false
		}
		if _, ok := h["Idempotency-Key"]; !ok {
			return false
		}
	}
	return true
}

// --- the live harness -------------------------------------------------------

// harness mounts the SAME http.Handler production wires (api.New) over a real,
// in-memory internal/app/store.Store and a real apihttp.IdempotencyStore under
// an injected clock. Every case drives requests through h.do — the exact
// dispatch a real client's request takes, never a hand-built http.HandlerFunc.
type harness struct {
	store *store.Store
	h     http.Handler
	// auth is the seeded auth fixture every request is driven AS. api.New now
	// requires an authenticator and refuses an unresolvable principal (SEC-005),
	// so a driver that wants to exercise the api/1 conventions has to hold a
	// real credential — there is no bypass, which is exactly the property that
	// keeps SEC-005 a tested claim rather than an asserted one. api/1's own
	// Conformance notes already anticipate this: "cases that need a principal
	// treat one as a given, opaque input."
	auth *authtest.Fixture
	// archiveDir is the scratch workspace-archive destination the export
	// operation is wired with, removed on close.
	archiveDir string
}

// newHarness opens a fresh in-memory store and mounts api.New over it. nowMs
// is the fixed clock every Idempotency-Key / Job timestamp in this harness's
// lifetime is stamped with — never the wall clock — so a case's outcome is
// reproducible. newID is likewise the fixed id source every server-minted id
// (a create's server-assigned id, a run's run_id, a bulk-enable Job's id) is
// drawn from in this harness's lifetime — never a package-level generator —
// so a case's outcome is reproducible on that axis too.
func newHarness(nowMs int64, newID func() string) (*harness, error) {
	return newHarnessAs(nowMs, newID, "")
}

// newHarnessAs is newHarness with the driving principal's id pinned — for a case
// whose own frozen expectation names a principal-derived field (API-111's Job
// created_by), which is then driven FROM the fixture exactly as that case's
// clock and id source already are.
func newHarnessAs(nowMs int64, newID func() string, principalID string) (*harness, error) {
	return newHarnessAsRole(nowMs, newID, principalID, "")
}

// newHarnessAsRole is newHarnessAs with the driving principal's ROLE pinned too
// — for a case whose validity depends on the caller's authority rather than
// only on their identity (the data-subject operations are owner-only). An empty
// role leaves the fixture's own default (authtest binds `owner` at the
// workspace root), so every existing call site is unchanged.
func newHarnessAsRole(nowMs int64, newID func() string, principalID, role string) (*harness, error) {
	st, err := store.Open(":memory:")
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	clock := func() int64 { return nowMs }
	fixture, err := authtest.New(authtest.Config{NowMs: clock, PrincipalID: principalID, Role: auth.Role(role)})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("authtest.New: %w", err)
	}
	// The data-subject export writes an archive/1 container, so the handler
	// needs a destination and a workspace signing key to accept one at all
	// (api.WithWorkspaceArchive; without it the operation answers UNAVAILABLE
	// rather than 202). Both are scratch, per harness, and removed on close:
	// the corpus case asserts the ACCEPTANCE, and this harness's job runner is
	// never started, so nothing is ever written into the directory — wiring it
	// is what makes the acceptance reachable, not what makes it pass.
	archiveDir, err := os.MkdirTemp("", "api1-workspace-archive-")
	if err != nil {
		_ = st.Close()
		fixture.Close()
		return nil, fmt.Errorf("archive dir: %w", err)
	}
	wsKey, err := workspacekey.LoadOrCreate(archiveDir, func() string { return harnessSignerKeyID })
	if err != nil {
		_ = st.Close()
		fixture.Close()
		_ = os.RemoveAll(archiveDir)
		return nil, fmt.Errorf("workspacekey.LoadOrCreate: %w", err)
	}
	idem := apihttp.NewIdempotencyStore(clock, 0)
	// The Job runner is wired STOPPED and never started. Every corpus case this
	// driver runs is an assertion about a RESPONSE — for the async ones, about
	// the 202 that ACCEPTS work (API-111's "accepted, not-yet-complete work",
	// which is why its frozen expectation shows every target pending). Letting
	// execution run would change nothing the case reads, while writing to the
	// store on a background goroutine this harness is about to close: a source
	// of noise, never of signal.
	h := api.New(st, idem, clock, newID, origin.New(), "https://origin.example", fixture.Auth,
		api.WithJobRunner(api.NewJobRunner()),
		api.WithWorkspaceArchive(&api.WorkspaceArchive{Dir: archiveDir, Key: wsKey}))
	return &harness{store: st, h: h, auth: fixture, archiveDir: archiveDir}, nil
}

// harnessSignerKeyID is the fixed `signer_key_id` this harness's workspace
// signing key is minted under — a valid ULID (DAT-005a) from a pinned closure,
// never a package-level generator, exactly as every other id in this driver is.
const harnessSignerKeyID = "01J8Z9DRVWSPACEKEY00000001"

func (h *harness) close() {
	_ = h.store.Close()
	h.auth.Close()
	if h.archiveDir != "" {
		_ = os.RemoveAll(h.archiveDir)
	}
}

// deterministicIDs mints deterministic, ascending, valid 26-char ULIDs — the
// injected id source (server.newID) this driver's harness supplies to every
// case that does not itself pin a server-minted id, so that id is reproducible
// rather than a fresh random ulid.New() on every run. Duplicated (not
// imported) from conformance/drivers/events1/driver.go's monotonicIDs: driver
// packages do not import each other, and the two generators mint into
// unrelated id spaces (api/1 resource/run/job ids vs. events/1 envelope ids),
// so sharing a literal prefix would invite confusion rather than reuse.
func deterministicIDs() func() string {
	const prefix = "01J8Z9DRVMNTAP1RESRCX000"
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	n := 0
	return func() string {
		hi := alphabet[(n/32)%32]
		lo := alphabet[n%32]
		n++
		return prefix + string([]byte{hi, lo})
	}
}

// result is the decoded outcome of one request driven through the live
// handler.
type result struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

// do drives one HTTP request through the live handler — no network, but the
// exact http.Handler a real listener would dispatch to.
func (h *harness) do(method, path string, body []byte, headers map[string]string) result {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// The fixture's real session cookie and CSRF token, attached centrally so no
	// individual case has to carry credentials that are not part of what it is
	// asserting.
	h.auth.Authorize(req)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return result{status: rec.Code, header: rec.Header(), body: decoded, raw: rec.Body.Bytes()}
}

// seedScopeNode writes a scope-node row directly into the store (fixture
// setup, not the operation under test — the same pattern internal/app/api's
// own e2e tests use: seed via the store, drive the case under test through the
// live handler). fields is marshaled as-is as the row body.
func (h *harness) seedScopeNode(fields map[string]any) error {
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = h.store.Create(context.Background(), store.KindScopeNode, b)
	return err
}

// seedAutomation writes a compile-clean edge-automation row directly into the
// store (fixture setup). Every seeded automation shares the same trigger/
// action shape (a state trigger rising to "on" firing a device_command on
// conformanceEntity) so it clears the rules/1 compile-gate every write path
// runs — the corpus's own collection_state fixtures for the automations kind
// carry only the api/1-relevant fields (id/scope_node/labels/name), never a
// full rule, since they were designed against the convention libraries
// directly; this driver supplies the rest of a valid rule so the SAME
// api/1-relevant fields can be seeded through the real, compile-gated store
// write path.
func (h *harness) seedAutomation(id, scopeNode string, labels map[string]string) error {
	m := map[string]any{
		"id":         id,
		"name":       "Conformance Fixture Automation",
		"scope_node": scopeNode,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "state", "entity_id": conformanceEntity, "to": []string{"on"}}},
		"conditions": []any{},
		"actions":    []any{map[string]any{"type": "device_command", "entity_id": conformanceEntity, "command": "launch"}},
	}
	if labels != nil {
		m["labels"] = labels
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = h.store.Create(context.Background(), store.KindAutomation, b)
	return err
}

// seedPlaylist writes a compile-clean playlist row directly into the store
// (fixture setup, same seed-via-store/drive-via-handler pattern as
// seedAutomation). It exists so a pagination-family case can seed a SECOND
// resource kind alongside automations (API-035's foreign-cursor case: a
// cursor minted by the automations list must be rejected by the playlists
// list) — store.Create validates a playlist's structural shape (DAT-041's
// item source/asset_ref pairing) but not that the asset_ref actually resolves
// in the content origin (that check is validatePlaylistAssets, an api/1 HTTP-
// layer guard on the create ROUTE, not on store.Create itself), so a fixture
// asset_ref never uploaded anywhere is fine here.
func (h *harness) seedPlaylist(id, scopeNode string) error {
	pl := datamodel.Playlist{
		ID:        id,
		ScopeNode: scopeNode,
		Name:      "Conformance Fixture Playlist",
		Items:     []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}},
	}
	b, err := json.Marshal(pl)
	if err != nil {
		return err
	}
	_, err = h.store.Create(context.Background(), store.KindPlaylist, b)
	return err
}

// --- api/1's own Problem error shape (API-010-016) --------------------------

// driveProblemNotFound drives API-010: GET on a scope-node id that does not
// exist. The prior version of this driver recorded this case PENDING,
// reasoning "a scope-node NOT_FOUND GET route ... does not exist yet" — a
// 2026-07-26 audit found that reasoning was itself wrong: the route
// (GET /api/v1/scope-nodes/{id}) has existed since it was built (api.go's
// generic resource GET, `rs.notFound`) and this driver was simply never wired
// to call it. Driven for real, the corpus's pinned `detail` — "No scope node
// exists with this identifier." — diverges from the live handler, which emits
// the resource-kind-agnostic generic detail every kind's 404 shares ("No
// resource exists at this identifier.", api.go's rs.notFound): a genuine,
// pre-existing corpus-vs-code mismatch this driver surfaces rather than
// silently reproducing.
func driveProblemNotFound(rep *report.Report, c corpus.Case) {
	var in struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	}
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs, deterministicIDs())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	res := h.do(in.Method, in.Path, nil, in.Headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	if want, ok := c.Expected["headers"].(map[string]any); ok {
		if wantTrace, ok := want["Trace-Id"].(string); ok {
			if got := res.header.Get("Trace-Id"); got != wantTrace {
				diffs = append(diffs, report.Diff{Field: "headers.Trace-Id", Expected: wantTrace, Actual: got})
			}
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "api/1 Problem (NOT_FOUND) diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// driveProblemValidation drives API-013: creating a scope node with an
// invalid kind and an empty name returns one VALIDATION_FAILED Problem
// carrying both field failures in its `errors` extension (api.go's
// writeValidationFailed renders a *datamodel.ScopeNode validator's per-field
// list, one entry per failing field). Per API-013a a body-validation
// VALIDATION_FAILED is 422; per DAT-001a the scope-node validator
// (internal/datamodel.BuildScopeTree) checks name ABOVE kind in its per-node
// loop, so a node failing both surfaces SCOPE_NODE_NAME_INVALID first and
// SCOPE_NODE_KIND_INVALID second — the corpus fixture is pinned to that same
// 422 status and errors order.
func driveProblemValidation(rep *report.Report, c corpus.Case) {
	var in struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    map[string]any    `json:"body"`
	}
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs, deterministicIDs())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	bodyBytes, err := json.Marshal(in.Body)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
		return
	}
	res := h.do(in.Method, in.Path, bodyBytes, in.Headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "api/1 Problem (VALIDATION_FAILED) diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- concurrency (API-020-025) -------------------------------------------

const fixedNowMs = int64(1_700_000_000_000)

type concurrencyInput struct {
	Path                 string            `json:"path"`
	Headers              map[string]string `json:"headers"`
	Body                 map[string]any    `json:"body"`
	CurrentResourceState struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	} `json:"current_resource_state"`
}

// driveConcurrency drives a conditional-write corpus case (API-022/023)
// against the live PATCH /api/v1/scope-nodes/{id} handler: a missing If-Match
// rejects 428/IF_MATCH_REQUIRED before any write (API-022); an If-Match that
// no longer matches the resource's current ETag rejects 412/REVISION_CONFLICT
// carrying current_revision (API-023). The target row is seeded, then bumped
// to the case's own current_resource_state.revision via two real writes
// (store.Update), so its live ETag genuinely is the corpus-pinned revision —
// never asserted by construction.
func driveConcurrency(rep *report.Report, c corpus.Case) {
	var in concurrencyInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs, deterministicIDs())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	id := in.CurrentResourceState.ID
	// A screen (not a site) needs no geo columns (DAT-031 only binds a site),
	// so seeding needs no fields beyond what the concurrency convention itself
	// cares about: identity and revision.
	if err := h.seedScopeNode(map[string]any{"id": id, "kind": "screen", "parent_id": "01J8Z0PLACEHOLDERPARENT01", "name": "Original Site"}); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed scope node: %v", err))
		return
	}
	cur, _, err := h.store.Get(context.Background(), store.KindScopeNode, id)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("get seeded scope node: %v", err))
		return
	}
	for cur.Revision < in.CurrentResourceState.Revision {
		cur, err = h.store.Update(context.Background(), store.KindScopeNode, id, cur.Revision, []byte(`{"name":"Original Site"}`))
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("bump seeded scope node to revision %d: %v", in.CurrentResourceState.Revision, err))
			return
		}
	}
	if cur.Revision != in.CurrentResourceState.Revision {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seeded scope node landed at revision %d, want the corpus's pinned %d", cur.Revision, in.CurrentResourceState.Revision))
		return
	}

	bodyBytes, err := json.Marshal(in.Body)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
		return
	}
	res := h.do("PATCH", in.Path, bodyBytes, in.Headers)

	afterRev, _, _ := h.store.Get(context.Background(), store.KindScopeNode, id)
	writeExecuted := afterRev.Revision != cur.Revision

	var diffs []report.Diff
	if want, ok := expectBool(c, "write_executed"); ok && writeExecuted != want {
		diffs = append(diffs, report.Diff{Field: "write_executed", Expected: want, Actual: writeExecuted})
	}
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "optimistic-concurrency outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- keyset pagination (API-030-036) --------------------------------------

type paginationCollectionRow struct {
	ID string `json:"id"`
	// Kind selects which resource this row is seeded as: "automation" (the
	// zero value, so API-032's kind-less rows still seed as automations) or
	// "playlist" — a SECOND resource type a case like API-035 seeds so it can
	// prove a cursor minted by one resource's list is rejected by another's.
	Kind string `json:"kind"`
}

type paginationRequest struct {
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query"`
}

type paginationInput struct {
	CollectionState []paginationCollectionRow `json:"collection_state"`
	Requests        []paginationRequest       `json:"requests"`
}

type paginationExpected struct {
	Responses []struct {
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	} `json:"responses"`
	CombinedItemsCoverCollectionExactlyOnce bool `json:"combined_items_cover_collection_exactly_once"`
}

// responseMarkerPattern recognizes a chain marker in a request's query value
// (documented in conformance/corpora/README.md): "$responses[N].field" refers
// to a member of the Nth EARLIER response this same case already observed.
// It exists so a roundtrip case can pass back a REAL value the implementation
// returned (e.g. the cursor a list actually minted) instead of a corpus-
// authored literal — API-033 forbids a client from ever constructing a cursor,
// so the corpus itself must not hardcode one either.
var responseMarkerPattern = regexp.MustCompile(`^\$responses\[(\d+)\]\.([A-Za-z0-9_]+)$`)

// cursorTokenGrammar is api/1 API-036's opaque-cursor grammar, recompiled here
// (rather than imported from apihttp, whose copy is unexported) so a chained
// cursor is checked against the frozen contract grammar, not the
// implementation's own regexp.
var cursorTokenGrammar = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// resolvePaginationQuery resolves every "$responses[N].field" marker in a
// request's query values against the bodies of the responses this case has
// already observed (prior), leaving any non-marker value untouched. When the
// referenced field is "cursor" it additionally enforces — a driver-side
// STRENGTHENING beyond a plain marker lookup — that the chained value is a
// non-null string satisfying the opaque-cursor grammar (API-032's "page 1
// returns a non-null opaque cursor" + API-036's grammar): a roundtrip case can
// only ever chain a real, spec-conformant continuation token, never a null,
// absent, or malformed one, so a case whose "page 1" never actually minted a
// cursor fails loudly here instead of silently sending the literal marker
// string (or an empty value) as the next request's cursor.
func resolvePaginationQuery(reqIdx int, q map[string]string, prior []map[string]any) (map[string]string, []report.Diff) {
	out := make(map[string]string, len(q))
	var diffs []report.Diff
	for k, raw := range q {
		m := responseMarkerPattern.FindStringSubmatch(raw)
		if m == nil {
			out[k] = raw
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		field := m[2]
		var val any
		if idx >= 0 && idx < len(prior) {
			val = prior[idx][field]
		}
		s, isString := val.(string)
		if !isString || (field == "cursor" && !cursorTokenGrammar.MatchString(s)) {
			wantDesc := "a non-null string"
			if field == "cursor" {
				wantDesc = "a non-null string matching ^[A-Za-z0-9_-]+$ (API-032 non-null cursor + API-036 grammar)"
			}
			diffs = append(diffs, report.Diff{
				Field:    fmt.Sprintf("requests[%d].query.%s (chained from responses[%d].%s)", reqIdx, k, idx, field),
				Expected: wantDesc,
				Actual:   val,
			})
			continue
		}
		out[k] = s
	}
	return out, diffs
}

// drivePagination drives an api/1 keyset-pagination corpus case against the
// live GET handler(s) for one or more resource lists: the corpus's
// collection_state rows are seeded as compile-clean automations and/or
// playlists (see seedAutomation/seedPlaylist, selected per row by `kind`),
// then each request is resolved (chaining any "$responses[N].field" marker
// against the response actually observed for an earlier request in the same
// case — see resolvePaginationQuery) and replayed through the live handler,
// diffed against that request's own pinned expected response.
//
// This drives BOTH the roundtrip case (API-032: page 2's cursor is chained
// from page 1's own real response, never hardcoded, since API-033 makes a
// cursor opaque to the client) and the foreign-cursor case (API-035: a cursor
// minted by one resource's list, chained into a DIFFERENT resource's list,
// must be rejected 400/CURSOR_INVALID) — the live api.go list handler scopes
// every resource's cursor uniformly by its own resourceType (automationsConfig,
// playlistsConfig, ... + apihttp.EncodeCursor/DecodeCursor), so both outcomes
// fall out of the same mechanism.
func drivePagination(rep *report.Report, c corpus.Case) {
	var in paginationInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var exp paginationExpected
	if err := decodeField(c.Expected, &exp); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs, deterministicIDs())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	const scopeNode = "01J8Z0PAGINATIONSCOPENODE1"
	for _, row := range in.CollectionState {
		var seedErr error
		switch row.Kind {
		case "", "automation":
			seedErr = h.seedAutomation(row.ID, scopeNode, nil)
		case "playlist":
			seedErr = h.seedPlaylist(row.ID, scopeNode)
		default:
			rep.Fail(c.CaseID, contract, fmt.Sprintf("collection_state row %s: unknown kind %q", row.ID, row.Kind))
			return
		}
		if seedErr != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed %s %s: %v", row.Kind, row.ID, seedErr))
			return
		}
	}

	if len(in.Requests) != len(exp.Responses) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus has %d requests but %d expected responses", len(in.Requests), len(exp.Responses)))
		return
	}

	var diffs []report.Diff
	seen := map[string]int{}
	var priorBodies []map[string]any
	for i, req := range in.Requests {
		query, qdiffs := resolvePaginationQuery(i, req.Query, priorBodies)
		diffs = append(diffs, qdiffs...)

		method := req.Method
		if method == "" {
			method = http.MethodGet
		}
		path := req.Path + "?" + encodeQuery(query)
		res := h.do(method, path, nil, nil)
		priorBodies = append(priorBodies, res.body)

		want := exp.Responses[i]
		if res.status != want.Status {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].status", i), Expected: want.Status, Actual: res.status})
		}
		for _, d := range listBodyDiffsRaw(fmt.Sprintf("responses[%d].body", i), want.Body, res.body) {
			diffs = append(diffs, d)
		}
		if items, ok := res.body["items"].([]any); ok {
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					if id, ok := m["id"].(string); ok {
						seen[id]++
					}
				}
			}
		}
	}

	if exp.CombinedItemsCoverCollectionExactlyOnce {
		if len(seen) != len(in.CollectionState) {
			diffs = append(diffs, report.Diff{Field: "combined_items_cover_collection_exactly_once", Expected: len(in.CollectionState), Actual: len(seen)})
		}
		for _, row := range in.CollectionState {
			if seen[row.ID] != 1 {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("row %s occurrence count", row.ID), Expected: 1, Actual: seen[row.ID]})
			}
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "keyset-pagination outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// encodeQuery builds a URL query string from a plain string map.
func encodeQuery(q map[string]string) string {
	v := url.Values{}
	for k, val := range q {
		v.Set(k, val)
	}
	return v.Encode()
}

// --- label-selector grammar (API-040-046) ---------------------------------

type selectorInput struct {
	CollectionState []struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		ParentID string `json:"parent_id"`
	} `json:"collection_state"`
	Headers map[string]string `json:"headers"`
	Request struct {
		Method string            `json:"method"`
		Path   string            `json:"path"`
		Query  map[string]string `json:"query"`
	} `json:"request"`
}

// siteGeo is synthetic (non-corpus) geo data this driver supplies for every
// seeded site-kind scope node — DAT-031 requires a site to declare non-null
// tz/lat/long, a rule orthogonal to what these selector/pagination/
// external_id cases assert (kind + placement + label matching), so seeding
// omits nothing the corpus itself pins; it only satisfies a live-store
// invariant the corpus's collection_state fixtures (written against the
// convention libraries directly) never had to satisfy.
func siteGeo(m map[string]any) {
	m["tz"] = "America/Chicago"
	m["lat"] = 41.8781
	m["long"] = -87.6298
}

// driveSelector drives a label-selector corpus case against the live
// GET /api/v1/scope-nodes handler. When the selector parses (API-044): seed
// the collection as real scope-node rows (site rows get synthetic geo, see
// siteGeo) and diff the selected + paginated ids against the pinned
// {items, cursor} envelope — the subtree containment comes from the REAL
// scope-node tree the handler reads (store.DesiredStateRows +
// datamodel.BuildScopeTree), not a driver-modeled parent map. When it fails to
// parse (API-045): diff the genuine response the live handler emits.
func driveSelector(rep *report.Report, c corpus.Case) {
	var in selectorInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs, deterministicIDs())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	for _, row := range in.CollectionState {
		m := map[string]any{"id": row.ID, "kind": row.Kind, "parent_id": row.ParentID, "name": "Node " + row.ID}
		if row.Kind == "site" {
			siteGeo(m)
		}
		if err := h.seedScopeNode(m); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed scope node %s: %v", row.ID, err))
			return
		}
	}

	method := in.Request.Method
	if method == "" {
		method = http.MethodGet
	}
	path := in.Request.Path + "?" + encodeQuery(in.Request.Query)
	headers := in.Headers
	res := h.do(method, path, nil, headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, listBodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "selector outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- client-assignable external_id (API-100-104) --------------------------

type externalIDInput struct {
	Path            string            `json:"path"`
	Headers         map[string]string `json:"headers"`
	Body            map[string]any    `json:"body"`
	CollectionState []struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		ParentID   string `json:"parent_id"`
		ExternalID string `json:"external_id"`
	} `json:"collection_state"`
}

// driveExternalID drives the external_id-conflict corpus cases (API-101/102)
// against the live POST /api/v1/scope-nodes handler: a create whose
// external_id already names another resource under the same parent scope node
// is rejected before the write executes — same-kind (API-102) or cross-kind
// (API-101), since the live handler scopes the check by
// resourceConfig.resourceType (api.go's refsFrom), the generic tag
// "scope-nodes" every scope-node kind shares, never the row's own `kind`.
func driveExternalID(rep *report.Report, c corpus.Case) {
	var in externalIDInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	h, err := newHarness(fixedNowMs, deterministicIDs())
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	for _, row := range in.CollectionState {
		m := map[string]any{"id": row.ID, "kind": row.Kind, "parent_id": row.ParentID, "external_id": row.ExternalID, "name": "Node " + row.ID}
		if err := h.seedScopeNode(m); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed scope node %s: %v", row.ID, err))
			return
		}
	}

	before, err := h.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("list before create: %v", err))
		return
	}

	bodyBytes, err := json.Marshal(in.Body)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
		return
	}
	res := h.do("POST", in.Path, bodyBytes, in.Headers)

	after, err := h.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("list after create: %v", err))
		return
	}
	writeExecuted := len(after) != len(before)

	var diffs []report.Diff
	if want, ok := expectBool(c, "write_executed"); ok && writeExecuted != want {
		diffs = append(diffs, report.Diff{Field: "write_executed", Expected: want, Actual: writeExecuted})
	}
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "external_id uniqueness outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- Idempotency-Key replay/reuse (API-050-056) ---------------------------

type idempotencyDriverInput struct {
	// SeedScopeNodes are scope nodes created directly through the store (the
	// same seedScopeNode fixture-setup pattern driveSelector's collection_state
	// uses) BEFORE any request in Requests runs — so a request body's parent_id
	// can reference a REAL, already-existing scope node rather than leaning on
	// BuildScopeTree's subtree-boundary tolerance (DAT-002 requires an existing
	// parent; the tolerance is a read-side allowance for a relay/1 per-scope
	// snapshot, not a license for an authoritative create). Deliberately a
	// distinct field name from driveSelector/driveExternalID's own
	// "collection_state": classifyShape treats an input carrying BOTH
	// "requests" and "collection_state" as the pagination shape, so reusing
	// that name here would misroute every idempotency case.
	SeedScopeNodes []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"seed_scope_nodes"`
	Requests []struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    map[string]any    `json:"body"`
	} `json:"requests"`
}

type idempotencyDriverExpected struct {
	Responses []struct {
		Status      int            `json:"status"`
		ContentType string         `json:"content_type"`
		Body        map[string]any `json:"body"`
		Replayed    bool           `json:"replayed"`
	} `json:"responses"`
	ResourcesCreated int `json:"resources_created"`
}

// driveIdempotency drives an Idempotency-Key corpus case (API-052/053)
// against the live POST /api/v1/scope-nodes handler under a fixed injected
// clock: each request in the case is replayed in sequence and diffed against
// its own pinned expectation, plus the total create side effects.
//
// The frozen API-052/053 fixtures predate two datamodel rules this driver's
// harness enforces for real, since it is the live store: DAT-002 (parent_id
// MUST be null iff kind is org; every non-org node's parent_id MUST
// reference an EXISTING scope node) and DAT-031 (a site MUST declare
// tz/lat/long together). Both fixtures create a site under parent_id
// 01J8Z0A0000000000000000000, so seed_scope_nodes now creates that id as a
// real org-kind scope node before either request runs (h.seedScopeNode,
// below) — an authoritative create's parent_id MUST reference a node that
// genuinely exists; BuildScopeTree's subtree-boundary tolerance for a parent
// absent from the set (internal/datamodel/scopetree.go) is a read-side
// allowance for a relay/1 per-scope snapshot (REL-065), not a license to
// leave a create's own parent dangling. The site body itself still carries
// the same synthetic geo every other site-creating case in this driver uses
// (siteGeo's tz/lat/long values).
//
// The corpus pins each case's server-minted id (…W2ZA / …W2ZB). The id
// source, like driveJob's, is driven FROM the fixture: a closure returns the
// case's own pinned expected.responses[0].body.id on its first call — the
// only create either case ever executes, since API-052's second request
// replays and API-053's second request conflicts before any write — and
// falls through to the ordinary deterministicIDs() default on any call
// beyond that (there should be none, for either case, but a second call
// reusing the same id would be a silent id collision rather than a loud
// failure).
func driveIdempotency(rep *report.Report, c corpus.Case) {
	var in idempotencyDriverInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var exp idempotencyDriverExpected
	if err := decodeField(c.Expected, &exp); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected: %v", err))
		return
	}
	if len(in.Requests) != len(exp.Responses) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("corpus has %d requests but %d expected responses", len(in.Requests), len(exp.Responses)))
		return
	}

	def := deterministicIDs()
	newID := def
	if len(exp.Responses) > 0 {
		if pinned, ok := exp.Responses[0].Body["id"].(string); ok && pinned != "" {
			used := false
			newID = func() string {
				if used {
					return def()
				}
				used = true
				return pinned
			}
		}
	}

	h, err := newHarness(fixedNowMs, newID)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	for _, row := range in.SeedScopeNodes {
		if err := h.seedScopeNode(map[string]any{"id": row.ID, "kind": row.Kind, "name": row.Name}); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed scope node %s: %v", row.ID, err))
			return
		}
	}

	before, err := h.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("list before: %v", err))
		return
	}

	var diffs []report.Diff
	var rawResponses [][]byte
	for i, req := range in.Requests {
		bodyBytes, err := json.Marshal(req.Body)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal requests[%d].body: %v", i, err))
			return
		}
		res := h.do(req.Method, req.Path, bodyBytes, req.Headers)
		rawResponses = append(rawResponses, res.raw)

		want := exp.Responses[i]
		if res.status != want.Status {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].status", i), Expected: want.Status, Actual: res.status})
		}
		if want.ContentType != "" {
			if got := res.header.Get("Content-Type"); got != want.ContentType {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].content_type", i), Expected: want.ContentType, Actual: got})
			}
		}
		diffs = append(diffs, memberDiffs(fmt.Sprintf("responses[%d].body", i), want.Body, res.body)...)

		// replayed:true is the fixture's own claim that this response was NOT
		// freshly executed but returned verbatim from the Idempotency-Key
		// cache (api.go's replay(), fed the exact bytes an earlier createExec
		// captured) — so the assertion this earns is byte-for-byte equality
		// against that earlier response, strictly stronger than the
		// field-subset memberDiffs check above.
		if want.Replayed {
			if i == 0 {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].replayed", i), Expected: "a prior response in this case to replay", Actual: "responses[0] has no prior response"})
			} else if !bytes.Equal(res.raw, rawResponses[0]) {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("responses[%d].body", i), Expected: string(rawResponses[0]) + " (byte-identical to responses[0], replayed:true)", Actual: string(res.raw)})
			}
		}
	}

	after, err := h.store.List(context.Background(), store.KindScopeNode, store.ListFilter{})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("list after: %v", err))
		return
	}
	creates := len(after) - len(before)
	if creates != exp.ResourcesCreated {
		diffs = append(diffs, report.Diff{Field: "resources_created", Expected: exp.ResourcesCreated, Actual: creates})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "Idempotency-Key replay/reuse outcome diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- 202 + Job resource (API-110-117, API-120-123) ------------------------

type jobDriverInput struct {
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers"`
	Principal string            `json:"principal"`
	Body      struct {
		Selector string `json:"selector"`
		Enabled  bool   `json:"enabled"`
	} `json:"body"`
	CollectionState []struct {
		ID        string `json:"id"`
		ScopeNode string `json:"scope_node"`
		Labels    []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"labels"`
	} `json:"collection_state"`
}

// driveJob drives the 202 + Job corpus case (API-111 bulk-enable) against the
// live POST /api/v1/automations/bulk-enable handler. The selector's
// `scope_node subtree` term is resolved against a REAL, seeded scope-node
// tree (a site plus the screen the fixture automations are placed under) —
// the containment comes from the handler's own inSubtreeFn
// (store.DesiredStateRows + datamodel.BuildScopeTree.AncestorChain), not a
// driver-modeled membership set.
//
// Three asserted fields are driven through the seams api.New exposes for
// exactly that purpose: the harness clock is the case's pinned created_at and
// the harness id source is a closure returning the case's pinned Job id, both
// read from the case's own expectation (server-derived outputs whose
// determinism the Conformance notes sanction seaming). created_by is
// different: it is an INPUT, read from the case's own input.principal, and the
// expectation is then checked against it. Seeding it from expected.created_by
// instead would make that assertion pass by construction.
//
// created_by used to be the one field this driver could not reproduce, because
// the live handler stamped it from a fixed constant while auth was deferred.
// It now comes from the REAL authenticated caller, so the case declaring its
// own input.principal — the shape API-052/053 already use — closes it, exactly
// as contracts/api-1.md's Conformance notes describe: "cases that need a
// principal treat one as a given, opaque input."
func driveJob(rep *report.Report, c corpus.Case) {
	var in jobDriverInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var expBody struct {
		ID        string `json:"id"`
		CreatedBy string `json:"created_by"`
		CreatedAt string `json:"created_at"`
		Targets   []struct {
			TargetID string `json:"target_id"`
		} `json:"targets"`
	}
	if raw, ok := c.Expected["body"].(map[string]any); ok {
		if err := decodeField(raw, &expBody); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected body: %v", err))
			return
		}
	}

	createdAt, err := time.Parse(time.RFC3339, expBody.CreatedAt)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("parse created_at %q: %v", expBody.CreatedAt, err))
		return
	}

	// The id source, like the clock above, is driven FROM the fixture: a closure
	// returning the case's own pinned expected body id, so the live-minted Job
	// id now reproduces the corpus exactly (see the doc comment above) — the
	// remaining, genuinely-unclosable divergence is created_by alone.
	pinnedID := func() string { return expBody.ID }
	if in.Principal == "" {
		rep.Fail(c.CaseID, contract, "case declares no input.principal: a Job's created_by is the authenticated caller, "+
			"which this driver must be TOLD (contracts/api-1.md's Conformance notes: a case that needs a principal treats "+
			"one as a given, opaque input) — deriving it from the case's own expectation would let the assertion pass by construction")
		return
	}
	h, err := newHarnessAs(createdAt.UnixMilli(), pinnedID, in.Principal)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	// Seed a real scope-node subtree: a site plus one screen under it, so the
	// selector's `scope_node subtree` term resolves against the SAME tree the
	// live inSubtreeFn reads, not a driver-modeled membership set.
	const siteNode = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	siteBody := map[string]any{"id": siteNode, "kind": "site", "parent_id": "01J8Z0JOBPLACEHOLDERORG01", "name": "Bulk-Enable Site"}
	siteGeo(siteBody)
	if err := h.seedScopeNode(siteBody); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed site scope node: %v", err))
		return
	}
	screenNodes := map[string]bool{}
	for _, row := range in.CollectionState {
		screenNodes[row.ScopeNode] = true
	}
	for screenID := range screenNodes {
		if err := h.seedScopeNode(map[string]any{"id": screenID, "kind": "screen", "parent_id": siteNode, "name": "Screen " + screenID}); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed screen scope node %s: %v", screenID, err))
			return
		}
	}
	for _, row := range in.CollectionState {
		labels := make(map[string]string, len(row.Labels))
		for _, l := range row.Labels {
			labels[l.Key] = l.Value
		}
		if err := h.seedAutomation(row.ID, row.ScopeNode, labels); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("seed automation %s: %v", row.ID, err))
			return
		}
	}

	bodyBytes, err := json.Marshal(map[string]any{"selector": in.Body.Selector, "enabled": in.Body.Enabled})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
		return
	}
	res := h.do(in.Method, in.Path, bodyBytes, in.Headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	if want, ok := expectString(c, "headers.Trace-Id"); ok && want != "" {
		if got := res.header.Get("Trace-Id"); got != want {
			diffs = append(diffs, report.Diff{Field: "headers.Trace-Id", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "202 Job response diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- data-subject export / delete (API-120-124) -------------------------

type workspaceJobDriverInput struct {
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Headers        map[string]string `json:"headers"`
	Principal      string            `json:"principal"`
	PrincipalRole  string            `json:"principal_role"`
	WorkspaceState struct {
		ID           string `json:"id"`
		Kind         string `json:"kind"`
		Name         string `json:"name"`
		AccountState string `json:"account_state"`
	} `json:"workspace_state"`
	Body map[string]any `json:"body"`
}

// driveWorkspaceJob drives the data-subject export corpus case (API-121)
// against the live POST /api/v1/workspace/export handler.
//
// Everything the assertion turns on comes from the case's own `input`, never
// from its `expected` — the one exception being the two server-derived outputs
// api/1's Conformance notes sanction seaming (the Job's own id and created_at,
// which are the harness's pinned id source and pinned clock, exactly as
// driveJob seams them):
//
//   - `input.principal` is the authenticated caller the handler stamps
//     created_by from, so the expectation's created_by is CHECKED against an
//     input rather than seeded from itself.
//   - `input.principal_role` is the role that principal is bound at. It is an
//     input because the operation is owner-only (internal/app/api/workspace.go
//     argues why from SEC-010/011), so the role is part of what makes this a
//     VALID request — a case that let the fixture's default role stand would be
//     asserting a 202 without ever stating that an owner is what produces one.
//   - `input.workspace_state` is the deployment's own org-kind scope node,
//     seeded into a REAL store through the same write path the authoring
//     surface uses. The Job's single target is that node's id (API-123: "each
//     operation's target is the workspace itself, implicit in the request
//     path"), so the expectation's target_id is likewise checked against a
//     seeded input rather than fed from the expectation.
//   - `input.body` is the request body. archive/1 derives a container's
//     encryption key from an export passphrase supplied at export time
//     (ARC-010), so the export operation requires one; the case declares it as
//     the input it is.
func driveWorkspaceJob(rep *report.Report, c corpus.Case) {
	var in workspaceJobDriverInput
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}
	var expBody struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	}
	if raw, ok := c.Expected["body"].(map[string]any); ok {
		if err := decodeField(raw, &expBody); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected body: %v", err))
			return
		}
	}
	createdAt, err := time.Parse(time.RFC3339, expBody.CreatedAt)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("parse created_at %q: %v", expBody.CreatedAt, err))
		return
	}
	if in.Principal == "" {
		rep.Fail(c.CaseID, contract, "case declares no input.principal: a Job's created_by is the authenticated caller, "+
			"which this driver must be TOLD (contracts/api-1.md's Conformance notes: a case that needs a principal treats "+
			"one as a given, opaque input) — deriving it from the case's own expectation would let the assertion pass by construction")
		return
	}
	if in.WorkspaceState.ID == "" {
		rep.Fail(c.CaseID, contract, "case declares no input.workspace_state.id: the Job's single target IS the workspace "+
			"(API-123), so the target id must be seeded from an input, never read out of the expectation it is checked against")
		return
	}

	pinnedID := func() string { return expBody.ID }
	h, err := newHarnessAsRole(createdAt.UnixMilli(), pinnedID, in.Principal, in.PrincipalRole)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("newHarness: %v", err))
		return
	}
	defer h.close()

	// The workspace's own org node, seeded through the real store write path —
	// the same fixture-setup pattern every other case here uses, and the same
	// one internal/app/api's own e2e tests use.
	orgBody := map[string]any{
		"id":            in.WorkspaceState.ID,
		"kind":          in.WorkspaceState.Kind,
		"name":          in.WorkspaceState.Name,
		"parent_id":     nil,
		"account_state": in.WorkspaceState.AccountState,
	}
	if err := h.seedScopeNode(orgBody); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed workspace org scope node: %v", err))
		return
	}

	var bodyBytes []byte
	if in.Body != nil {
		bodyBytes, err = json.Marshal(in.Body)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal input.body: %v", err))
			return
		}
	}
	res := h.do(in.Method, in.Path, bodyBytes, in.Headers)

	var diffs []report.Diff
	if want, ok := expectInt(c, "status"); ok && res.status != want {
		diffs = append(diffs, report.Diff{Field: "status", Expected: want, Actual: res.status})
	}
	if want, ok := expectString(c, "content_type"); ok && want != "" {
		if got := res.header.Get("Content-Type"); got != want {
			diffs = append(diffs, report.Diff{Field: "content_type", Expected: want, Actual: got})
		}
	}
	if want, ok := expectString(c, "headers.Trace-Id"); ok && want != "" {
		if got := res.header.Get("Trace-Id"); got != want {
			diffs = append(diffs, report.Diff{Field: "headers.Trace-Id", Expected: want, Actual: got})
		}
	}
	diffs = append(diffs, bodyDiffs(c, res.body)...)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "202 Job response diverged from the corpus expectation", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract)
}

// --- shared helpers ---------------------------------------------------

func decodeField(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal corpus field: %w", err)
	}
	return json.Unmarshal(b, v)
}

func expectInt(c corpus.Case, key string) (int, bool) {
	v, ok := c.Expect(key)
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func expectBool(c corpus.Case, key string) (bool, bool) {
	v, ok := c.Expect(key)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func expectString(c corpus.Case, key string) (string, bool) {
	v, ok := c.Expect(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// bodyDiffs diffs every member the corpus `expected.body` pins against the
// emitted body. Extra members in the emission are permitted; every pinned
// member MUST be reproduced exactly (the corpus is the oracle).
func bodyDiffs(c corpus.Case, got map[string]any) []report.Diff {
	want, ok := c.Expected["body"].(map[string]any)
	if !ok {
		return nil
	}
	return memberDiffs("body", want, got)
}

// listBodyDiffs is bodyDiffs for a list-endpoint response whose corpus
// `expected.body.items` pins only the id-projection shape (`{id: ...}`) a
// convention-library-level driver would have produced, while the live
// resource-list handlers (api.go's generic `list`) return each item as the
// FULL stored row body (every column, not an id-only projection) — extra
// per-item members are exactly as permitted as extra top-level members
// (bodyDiffs's own rule), so `items` is compared by its ids only, in order;
// every other top-level member (cursor, status) is still compared exactly.
func listBodyDiffs(c corpus.Case, got map[string]any) []report.Diff {
	want, ok := c.Expected["body"].(map[string]any)
	if !ok {
		return nil
	}
	return listBodyDiffsRaw("body", want, got)
}

// listBodyDiffsRaw is listBodyDiffs over explicit want/got maps and an
// explicit field prefix, for a caller (like drivePagination) diffing a
// response nested under `responses[i].body` rather than a case's top-level
// `expected.body`.
func listBodyDiffsRaw(prefix string, want, got map[string]any) []report.Diff {
	if len(want) == 0 {
		return nil
	}
	if got == nil {
		return []report.Diff{{Field: prefix, Expected: want, Actual: nil}}
	}
	var diffs []report.Diff
	for k, wv := range want {
		if k == "items" {
			wantIDs := itemIDs(wv)
			gotIDs := itemIDs(got["items"])
			if !reflect.DeepEqual(wantIDs, gotIDs) {
				diffs = append(diffs, report.Diff{Field: prefix + ".items[].id", Expected: wantIDs, Actual: gotIDs})
			}
			continue
		}
		gv, present := got[k]
		if !present {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: "<absent>"})
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: gv})
		}
	}
	return diffs
}

// itemIDs extracts the ordered `id` projection from a decoded JSON `items`
// array (each element a map that carries at least an `id` string).
func itemIDs(v any) []string {
	items, _ := v.([]any)
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func memberDiffs(prefix string, want, got map[string]any) []report.Diff {
	if len(want) == 0 {
		return nil
	}
	if got == nil {
		return []report.Diff{{Field: prefix, Expected: want, Actual: nil}}
	}
	var diffs []report.Diff
	for k, wv := range want {
		gv, present := got[k]
		if !present {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: "<absent>"})
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			diffs = append(diffs, report.Diff{Field: prefix + "." + k, Expected: wv, Actual: gv})
		}
	}
	return diffs
}

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "api-1")
}

// LoadCorpus loads every frozen api-1 corpus case, keyed by case_id.
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
