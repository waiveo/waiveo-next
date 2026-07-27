// Package api is the app-side api/1 HTTP surface for the authoring loop: the
// management door through which scope-nodes and the scheduling-core resources
// (schedules, dayparts, playlists) are authored over HTTP, persisted by
// internal/app/store, and thereby fed into the feeder's signed desired-state.
//
// Every handler routes through the already-built, corpus-driven api/1 convention
// helpers — it never re-derives one:
//
//   - optimistic concurrency (ETag/If-Match) via apihttp.ETag / CheckIfMatch;
//   - keyset pagination (opaque cursor + {items, cursor} envelope) via
//     apihttp.ParsePageParams / DecodeCursor / Page;
//   - the label-selector grammar via apiselector.Parse / Selector.Matches;
//   - the client-assignable external_id convention via apihttp.CheckExternalIDUnique;
//   - Idempotency-Key semantics on create via apihttp.IdempotencyStore;
//   - the RFC 9457 Problem shape (every error) + Trace-Id propagation via
//     apihttp.WriteProblem / WriteProblemExt / WithTraceID.
//
// Every request is authenticated and authorized before a handler sees it
// (internal/app/auth, security-model/1): the principal an Idempotency-Key is
// scoped by (API-051) and a Job's created_by is stamped from (API-112) is the
// REAL authenticated caller, resolved from a live session or API key. There is
// no fallback identity — SEC-005 requires a route that cannot resolve an
// authorization decision to refuse rather than default-permit, so a request that
// reaches a handler always carries a principal and a handler never has to invent
// one.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

// contentTypeJSON is the media type every success response body uses; error
// bodies use apihttp.ProblemContentType instead.
const contentTypeJSON = "application/json"

// apiPrefix is the version-scoped mount point every resource path hangs off.
const apiPrefix = "/api/v1"

// server holds the dependencies every resource handler shares: the store the
// resources live in, the idempotency store guarding create replays, and the
// injected clock (epoch ms) the idempotency Begin/Complete calls are timestamped
// with — so the api layer, like the idempotency store itself, reads no wall
// clock of its own. newID is likewise injected rather than read from a
// package-level generator: the api layer mints no id from a generator of its
// own, every server-minted id (a create's server-assigned id, a synchronous
// run's run_id, a bulk-enable Job's id) comes from this single seam. content
// is the shared content-addressed origin store the upload endpoint writes
// into (and the feeder serves GET /content/<hex> from over the SAME
// instance); contentBase is the feeder's own content-origin base URL the
// upload's returned url is built from (<base>/content/<hex>, snapshot.Build's
// form).
type server struct {
	store       *store.Store
	idem        *apihttp.IdempotencyStore
	nowMs       func() int64
	newID       func() string
	content     *origin.Store
	contentBase string
	// installer runs the manifest-gated declarative-pack install pipeline the
	// POST /api/v1/packs handler drives (internal/app/packs). It shares the same
	// store every other resource handler writes through.
	installer *packs.Installer
	// devices is the device plane's read model (adopted devices and the entities
	// they expose) the devices/entities list operations serve, and the resolver a
	// command's target entity is looked up in; dispatch carries that command down
	// the owning relay's persistent connection. Both are optional (WithDevicePlane)
	// — the routes mount either way, see devices.go.
	devices  *devices.Registry
	dispatch CommandDispatcher
}

// authExemptPaths are the api/1 operations that declare their own
// `security: []` override in api/openapi.yaml (API-090) and so are mounted
// AHEAD of the auth middleware rather than behind it. The list is deliberately
// short, explicit, and kept here beside the mount that honours it, so "which
// routes are reachable unauthenticated" is one readable set rather than a
// property emergent from mux pattern precedence.
//
// Both entries are credential-exchange operations under API-091: a login mints
// the session a caller does not yet hold, and the first-boot claim mints the
// very first principal on the box. Requiring a pre-existing session for either
// would be circular. Everything else under /api/v1 — including logout and the
// session read — is authenticated, which is what subjects logout to the same
// CSRF discipline as any other mutating browser route (SEC-024).
var authExemptPaths = map[string]bool{
	apiPrefix + "/auth/login": true,
	apiPrefix + "/auth/setup": true,
}

// authExempt reports whether r names one of the `security: []` operations.
func authExempt(r *http.Request) bool { return authExemptPaths[r.URL.Path] }

// New builds the api/1 HTTP handler: a /api/v1-prefixed mux exposing the
// scope-nodes and scheduling-core (schedules/dayparts/playlists) CRUD
// operations over st, plus the content-addressed asset upload (POST
// /api/v1/content) over content, wrapped in apihttp.WithTraceID so every request
// resolves a Trace-Id once (echoed by the header and every Problem body). idem
// guards Idempotency-Key create replays; nowMs is the injected clock the
// idempotency calls are timestamped with; newID is the injected id source
// every server-minted id (a create's server-assigned id, a synchronous run's
// run_id, a bulk-enable Job's id) is drawn from — the same seam pattern as
// nowMs, so the api layer reads no wall clock AND mints no id of its own.
// content is the shared origin store the feeder also serves GET /content/<hex>
// from (one instance, so an upload is immediately servable); contentBase is
// the feeder's content-origin base URL the upload's returned url is built
// from. opts wire the optional collaborators — currently the device plane
// (WithDevicePlane), whose routes mount whether or not it is supplied.
//
// authn is REQUIRED, not an option, and that is the point: it is a positional
// parameter precisely so a caller cannot construct this handler without
// deciding who may reach it. Every route below hangs behind its middleware
// except the two credential-exchange operations in authExemptPaths, so an
// unauthenticated request to any resource family is refused before a handler
// runs (SEC-005).
func New(st *store.Store, idem *apihttp.IdempotencyStore, nowMs func() int64, newID func() string, content *origin.Store, contentBase string, authn *auth.Authenticator, opts ...Option) http.Handler {
	srv := &server{
		store: st, idem: idem, nowMs: nowMs, newID: newID, content: content, contentBase: contentBase,
		installer: packs.NewInstaller(st),
	}
	for _, opt := range opts {
		opt(srv)
	}
	mux := http.NewServeMux()
	srv.mount(mux, scopeNodesConfig())
	srv.mount(mux, schedulesConfig())
	srv.mount(mux, daypartsConfig())
	srv.mount(mux, playlistsConfig())
	srv.mount(mux, automationsConfig())
	srv.mountPacks(mux)
	// The automations family adds two operations beyond plain resource CRUD: a
	// synchronous per-automation run (openapi runAutomation) and a selector-targeted
	// fleet-mutating bulk enable/disable returning an api/1 Job (bulkEnableAutomations).
	// The literal `bulk-enable` segment is unambiguous — the generic mount registers
	// no POST on `automations/{id}` — and `{id}/run` has its own two-segment shape.
	mux.HandleFunc("POST "+apiPrefix+"/automations/{id}/run", srv.runAutomation)
	mux.HandleFunc("POST "+apiPrefix+"/automations/bulk-enable", srv.bulkEnableAutomations)
	mux.HandleFunc("POST "+apiPrefix+"/content", srv.uploadContent)
	// The read half of the async convention: a Job returned by 202 is polled
	// here until its state is terminal (API-112, openapi getJob). It is not a
	// resourceConfig mount — a Job carries no revision to condition a write on,
	// and has no write operations at all (jobs.go).
	mux.HandleFunc("GET "+apiPrefix+"/jobs/{job_id}", srv.getJob)
	// The device plane's two read families and its one mutating operation. They
	// are not resourceConfig mounts: a device is a read-only projection of the
	// relay's own discovery and adoption plane, with no revision to condition a
	// write on (devices.go).
	srv.mountDevicePlane(mux)

	// The session-management half of the auth family rides the AUTHENTICATED
	// mux: both operations act on the caller's own live session, so neither has
	// anything to do without one.
	authHandlers := auth.NewHandlers(authn, nil, auth.RootScopeNode)
	mux.HandleFunc("POST "+apiPrefix+"/auth/logout", authHandlers.Logout)
	mux.HandleFunc("GET "+apiPrefix+"/auth/session", authHandlers.Session)

	// The credential-exchange half (API-090/091) mounts on the ROOT mux, ahead
	// of the middleware. Go's ServeMux prefers the more specific method+path
	// pattern over the "/" catch-all, so these two reach their handlers directly
	// while every other path falls through to the authenticated mux.
	root := http.NewServeMux()
	root.HandleFunc("POST "+apiPrefix+"/auth/login", authHandlers.Login)
	root.HandleFunc("POST "+apiPrefix+"/auth/setup", authHandlers.Claim)
	root.Handle("/", authn.Middleware(auth.APICodes, authExempt)(mux))

	return apihttp.WithTraceID(root)
}

// resourceConfig parameterizes the generic resource handler for one resource
// kind: which store table it lives in, its URL path segment, the api/1
// resource-type tag its external_id uniqueness and cross-references are scoped
// by, the human noun a Problem's `detail` prose names it by (displayName), and
// the three per-kind field projections the conventions need — the selectable
// label set (labels + any reserved intrinsic), the scope-node placement a
// selector's scope_node term evaluates against, and the grouping external_id
// uniqueness is scoped by. A scope node's placement is itself and its
// external_id groups by parent; a scheduling-core row's placement and
// external_id grouping are both its scope_node (see scopenodes.go / a follow-up).
type resourceConfig struct {
	kind         store.Kind
	path         string
	resourceType string
	// displayName is the singular human noun this kind is named by in a Problem's
	// `detail` prose (a 404's "No <displayName> exists..." and an external_id
	// conflict's "...another <displayName> under this parent.") — resourceType
	// itself is a matching key (a family tag like "scope-nodes", not always a
	// grammatical noun on its own), so the two are kept separate rather than
	// reusing one for the other.
	displayName string
	selLabels   func(resourceFields) map[string]string
	placement   func(resourceFields) string
	extScope    func(resourceFields) string
	// writeScope is the scope node a WRITE of this kind is authorized at: the
	// node the row is placed UNDER, which is where SEC-010 authority over the
	// placement has to come from. For every kind whose own scope_node is its
	// placement the two coincide; for a scope node they do not — its placement
	// is ITSELF (a subtree selector must select it), but on a create that id
	// does not exist in the tree yet and on a patch it is the PARENT that
	// decides where the node hangs, so the write is authorized at parent_id.
	//
	// This is deliberately its own projection rather than a reuse of extScope,
	// which today returns the same node for all five kinds. The coincidence is
	// not accidental — API-101 groups external_id by the node a row is placed
	// under, which is the same node authority over that placement is anchored at
	// — but "which rows may collide with mine" and "may I write here at all" are
	// different questions, and a kind that answered them differently would
	// silently authorize against the wrong node if one field served both.
	writeScope func(resourceFields) string
	// validate, when non-nil, is a per-kind pre-write body validation run over the
	// EFFECTIVE request body — the create body, or a patch shallow-merged onto the
	// current row — BEFORE the store write; a non-empty result is rendered
	// 422 / VALIDATION_FAILED carrying the per-field errors as the api/1 `errors`
	// extension (API-013), and nothing is stored. Only the playlist kind sets it:
	// each item's asset_ref must resolve in the shared content origin (you cannot
	// schedule content that was never uploaded, DAT-041) — see scheduling.go.
	validate func(srv *server, body []byte) []datamodel.Error
}

// resource binds a resourceConfig to the shared server so the handler methods
// can be registered as http.HandlerFuncs.
type resource struct {
	srv *server
	cfg resourceConfig
}

// mount registers the five CRUD routes for cfg under /api/v1/<path>. Go 1.22+
// method+path patterns dispatch by method, and {id} captures the resource id.
func (srv *server) mount(mux *http.ServeMux, cfg resourceConfig) {
	rs := &resource{srv: srv, cfg: cfg}
	base := apiPrefix + "/" + cfg.path
	mux.HandleFunc("GET "+base, rs.list)
	mux.HandleFunc("POST "+base, rs.create)
	mux.HandleFunc("GET "+base+"/{id}", rs.get)
	mux.HandleFunc("PATCH "+base+"/{id}", rs.patch)
	mux.HandleFunc("DELETE "+base+"/{id}", rs.delete)
}

// resourceFields is the api/1 resource baseline the api layer reads out of a
// row's (or a request's) JSON body to drive the conventions. id/preset_id carry
// the two identity forms (a preset-batch keys on preset_id, the sole DAT-005
// exception); parent_id is a scope node's own placement parent; kind is a scope
// node's reserved intrinsic. It is a read-only projection — the store owns
// persistence and the authoritative baseline.
type resourceFields struct {
	ID         string
	PresetID   string
	ExternalID string
	ScopeNode  string
	ParentID   string
	Kind       string
	Labels     map[string]string
}

// parseFields projects a JSON body onto the resource baseline. A body that does
// not parse yields the zero fields (the store's own validation surfaces the real
// error on write).
func parseFields(body []byte) resourceFields {
	var raw struct {
		ID         string            `json:"id"`
		PresetID   string            `json:"preset_id"`
		ExternalID string            `json:"external_id"`
		ScopeNode  string            `json:"scope_node"`
		ParentID   *string           `json:"parent_id"`
		Kind       string            `json:"kind"`
		Labels     map[string]string `json:"labels"`
	}
	_ = json.Unmarshal(body, &raw)
	f := resourceFields{
		ID:         raw.ID,
		PresetID:   raw.PresetID,
		ExternalID: raw.ExternalID,
		ScopeNode:  raw.ScopeNode,
		Kind:       raw.Kind,
		Labels:     raw.Labels,
	}
	if raw.ParentID != nil {
		f.ParentID = *raw.ParentID
	}
	return f
}

// identity returns the id form a row of this kind keys on.
func (cfg resourceConfig) identity(f resourceFields) string {
	if cfg.kind == store.KindPresetBatch {
		return f.PresetID
	}
	return f.ID
}

// identityField returns the JSON key a row of this kind carries its id under.
func (cfg resourceConfig) identityField() string {
	if cfg.kind == store.KindPresetBatch {
		return "preset_id"
	}
	return "id"
}

// ---- create ---------------------------------------------------------------

func (rs *resource) create(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}

	// Idempotency-Key (optional, API-050): a keyed repeat replays or conflicts
	// before any write; an unkeyed request always executes.
	key := r.Header.Get("Idempotency-Key")
	// API-051: an Idempotency-Key is scoped to (authenticated principal, method,
	// path), so the SAME key value presented by a DIFFERENT principal is an
	// unrelated, fresh request and never a replay. The principal comes from the
	// request's resolved identity — the middleware guarantees one is present.
	scope := apihttp.IdempotencyScope{Principal: auth.PrincipalID(r), Method: r.Method, Path: r.URL.Path}
	hash := apihttp.IdempotencyBodyHash(raw)
	now := rs.srv.nowMs()
	if key != "" {
		switch out := rs.srv.idem.Begin(scope, key, hash, now); out.Kind {
		case apihttp.BeginReplay:
			replay(w, out.Response)
			return
		case apihttp.BeginConflict:
			rs.problem(w, r, http.StatusConflict, apihttp.CodeIdempotencyKeyReused, "Conflict", apihttp.IdempotencyReuseDetail(key))
			return
		case apihttp.BeginInProgress:
			rs.problem(w, r, http.StatusConflict, apihttp.CodeIdempotencyKeyInProgress, "Conflict", "A request with this Idempotency-Key is already in progress.")
			return
		}
	}

	// A fresh keyed request now holds an in-flight marker that MUST be resolved on
	// EVERY terminal path (API-052/054) — never only on success. The response is
	// composed into a capture so its exact bytes can be retained for replay, and a
	// definitive outcome (any status < 500, success OR a deterministic client
	// Problem) is Completed; a transient 5xx is Aborted so the key stays retryable.
	rc := &responseCapture{}
	rs.createExec(rc, r, raw)
	status, body, ct := rc.flush(w)

	if key != "" {
		if status < http.StatusInternalServerError {
			rs.srv.idem.Complete(scope, key, hash, apihttp.StoredResponse{
				Status:      status,
				Body:        body,
				ContentType: ct,
			}, now)
		} else {
			rs.srv.idem.Abort(scope, key, hash)
		}
	}
}

// createExec performs the create against the store and writes its outcome — a 201
// with ETag/Location, or an api/1 Problem — to w. A client-supplied id/preset_id
// is rejected upfront (rejectClientSuppliedID); external_id uniqueness
// (API-101/102) is enforced ATOMICALLY inside the store write (via a
// WriteGuard), closing the check-then-write race a pre-write snapshot in a
// separate critical section left open. w is the response capture create() owns,
// so the exact bytes are retainable for an Idempotency-Key replay.
func (rs *resource) createExec(w http.ResponseWriter, r *http.Request, raw []byte) {
	if rs.rejectClientSuppliedID(w, r, raw) {
		return
	}
	// The server always mints the id (openapi: id is not part of the create
	// body; a resource's id is server-assigned, api/1 API-100) — a
	// client-supplied one was already rejected above.
	body, id := rs.ensureID(raw)
	fields := parseFields(body)

	// SEC-005 authorizes "before executing", and on a create the placement is
	// the whole of what there is to authorize: the caller named the scope node
	// this row will live under, and a row written under a node the caller has no
	// authority at is invisible to its own author the moment it lands.
	//
	// The check sits AHEAD of every stage that consults stored state — the
	// per-kind body validation, and above all the external_id uniqueness guard
	// inside the store write. That ordering is the point rather than an
	// optimization: the uniqueness guard necessarily scans rows under the target
	// node whether the caller can see them or not (API-101 scopes uniqueness by
	// placement, not by principal), so reaching it with an unauthorized
	// placement is what turns a 400 EXTERNAL_ID_CONFLICT into an existence
	// oracle for a hidden row. Refusing the PLACEMENT first means the guard is
	// never reached by a caller who was not already entitled to enumerate what
	// it scans (scopeview.go).
	view, ok := rs.requestView(w, r)
	if !ok {
		return
	}
	if !view.canWrite(rs.cfg.writeScope(fields)) {
		rs.refuseWrite(w, r)
		return
	}

	if rs.writeValidationFailed(w, r, body) {
		return
	}

	res, err := rs.srv.store.Create(r.Context(), rs.cfg.kind, body,
		rs.externalIDGuards(fields.ExternalID, rs.cfg.extScope(fields), "")...)
	if err != nil {
		rs.writeStoreError(w, r, err)
		return
	}

	w.Header().Set("ETag", apihttp.ETag(res.Revision))
	w.Header().Set("Location", apiPrefix+"/"+rs.cfg.path+"/"+id)
	writeJSON(w, http.StatusCreated, res.Body)
}

// rejectClientSuppliedID writes a 422 / VALIDATION_FAILED Problem, naming the
// kind's own identity field (id, or a preset-batch's preset_id), when raw
// carries a non-empty client-supplied identity, and reports whether it wrote a
// response so the caller aborts before any store write. A resource's id is
// exclusively server-assigned (API-105): api/1's Definitions name id as the
// server-minted identity, external_id (API-100–104) is the sanctioned
// client-assigned identity slot, and every Create schema api/openapi.yaml
// declares already omits id (additionalProperties: false) — this closes the gap
// where the HTTP handlers did not yet enforce that schema at runtime. The
// per-field `errors[]` code this writes, ID_SERVER_ASSIGNED, is api/1's own
// (Error taxonomy) — id is an api/1-native field (API-013's "contract that owns
// the failing field's rule"), not a data-model/1 one. Applying the same check
// to a PATCH body (rs.patch) additionally prevents a client from overwriting a
// resource's already-assigned id after creation, which store.Update's
// merge-over-current-body would otherwise accept with no id-immutability check
// of its own.
func (rs *resource) rejectClientSuppliedID(w http.ResponseWriter, r *http.Request, raw []byte) bool {
	f := parseFields(raw)
	if rs.cfg.identity(f) == "" {
		return false
	}
	field := rs.cfg.identityField()
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
		"VALIDATION_FAILED", "Validation Failed",
		"One or more fields failed validation.",
		map[string]any{"errors": []map[string]string{{
			"field":   field,
			"code":    "ID_SERVER_ASSIGNED",
			"message": "a resource's " + field + " is assigned by the server and MUST NOT be supplied by the client; use external_id for a client-assigned identity (api/1 API-100).",
		}}})
	return true
}

// ensureID returns the create body with a freshly minted identity injected
// under the kind's identity field, plus that id. A client-supplied id is
// rejected upstream (rejectClientSuppliedID) before this ever runs, so every
// body reaching here is minted fresh from the server's injected id source
// (srv.newID — never a package-level generator).
func (rs *resource) ensureID(raw []byte) (body []byte, id string) {
	id = rs.srv.newID()
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &m)
	idJSON, _ := json.Marshal(id)
	m[rs.cfg.identityField()] = idJSON
	out, err := json.Marshal(m)
	if err != nil {
		return raw, id
	}
	return out, id
}

// ---- get ------------------------------------------------------------------

func (rs *resource) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, found, err := rs.srv.store.Get(r.Context(), rs.cfg.kind, id)
	if err != nil {
		rs.internal(w, r, err)
		return
	}
	if !found {
		rs.notFound(w, r)
		return
	}
	if visible, err := rs.readable(r, res.Body); err != nil {
		rs.internal(w, r, err)
		return
	} else if !visible {
		rs.notFound(w, r)
		return
	}
	w.Header().Set("ETag", apihttp.ETag(res.Revision))
	writeJSON(w, http.StatusOK, res.Body)
}

// readable reports whether the request's principal may read a row of this kind
// with the given stored body — i.e. whether the row's scope-node placement is
// in the caller's visible set (scopeview.go). A false answer means the caller
// MUST be answered exactly as if the row did not exist (rs.notFound), never
// with a 403 that would confirm it does.
func (rs *resource) readable(r *http.Request, body []byte) (bool, error) {
	view, err := rs.srv.scopeView(r)
	if err != nil {
		return false, err
	}
	return view.canRead(rs.cfg.placement(parseFields(body))), nil
}

// requestView builds this request's scope view ONCE, writing the 500 Problem
// itself and reporting ok=false when the tree read fails, so a caller has a
// single thing to check.
//
// Every handler needing more than one answer out of it builds it through here
// rather than calling srv.scopeView per question: a patch that tested
// visibility against one read of the tree and placement authority against a
// later one could be told the row is addressable and the destination
// authorized by two different worlds, which is precisely the disagreement
// scopeview.go exists to make impossible.
func (rs *resource) requestView(w http.ResponseWriter, r *http.Request) (scopeView, bool) {
	view, err := rs.srv.scopeView(r)
	if err != nil {
		rs.internal(w, r, err)
		return scopeView{}, false
	}
	return view, true
}

// refuseWrite writes the 403 / FORBIDDEN Problem an api/1 write draws when the
// caller holds no write authority at the scope node the write acts on
// (SEC-005: an authorization that cannot be resolved refuses, never
// default-permits). See scopeview.go for why this refusal is 403 where the read
// side's is 404, and why that distinction discloses nothing.
func (rs *resource) refuseWrite(w http.ResponseWriter, r *http.Request) {
	rs.problem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
}

// unauthorizedWriteDetail is the ONE `detail` string every refusal of an
// unauthorized write carries, on every resource family and every mutating
// operation on this surface.
//
// It names no scope node, no resource kind and no id, and that is the whole of
// its design. A detail echoing the node back — or worded one way for "this node
// is not yours" and another for "this node does not exist" — would re-open in
// prose exactly the existence oracle the ordering of the check closes, and a
// Problem body is compared member for member by anyone looking for one.
const unauthorizedWriteDetail = "This principal holds no write authority at the scope node this request writes at."

// ---- list -----------------------------------------------------------------

func (rs *resource) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	cursor, limit, pperr := apihttp.ParsePageParams(q.Get("cursor"), q.Get("limit"))
	if pperr != nil {
		rs.problem(w, r, pperr.Status, pperr.Code, pperr.Title, pperr.Detail)
		return
	}

	sel, serr := apiselector.Parse(q.Get("selector"))
	if serr != nil {
		rs.problem(w, r, serr.Status, serr.Code, serr.Title, serr.Detail)
		return
	}

	// The cursor is scoped to THIS resource type (rs.cfg.resourceType), so a
	// cursor minted by another resource's list — even a byte-identical id — is
	// rejected 400 / CURSOR_INVALID here rather than silently paged from as an
	// arbitrary keyset position in the wrong collection (API-033/035).
	var afterID string
	if cursor != "" {
		lastID, cerr := apihttp.DecodeCursor(rs.cfg.resourceType, cursor)
		if cerr != nil {
			rs.problem(w, r, cerr.Status, cerr.Code, cerr.Title, cerr.Detail)
			return
		}
		afterID = lastID
	}

	rows, err := rs.srv.store.List(r.Context(), rs.cfg.kind, store.ListFilter{})
	if err != nil {
		rs.internal(w, r, err)
		return
	}

	view, err := rs.srv.scopeView(r)
	if err != nil {
		rs.internal(w, r, err)
		return
	}

	// Keyset advance past the cursor position, then the visible-set filter, then
	// the selector. ULIDs sort lexicographically in id order, so a byte
	// comparison is the keyset order.
	//
	// BOTH filters run here, BEFORE apihttp.Page cuts the page — never after.
	// That ordering is what keeps a page of `limit` rows actually `limit` rows:
	// filtering a page after the keyset window was chosen would silently return
	// fewer than asked for whenever an out-of-reach or unselected row happened
	// to fall inside it, and would leak the count of the rows removed. Filtering
	// first means the window Page cuts from already contains only rows this
	// caller may see, so a short page means the collection is genuinely
	// exhausted (API-032/034).
	//
	// The visible-set test precedes the selector, which is what makes the
	// selector a pure NARROWING of the visible set (events/1 EVT-121's rule for
	// the same grammar): the two are ANDed, and an intersection can never widen.
	// A selector naming an out-of-reach node simply matches no visible row and
	// yields an empty page rather than an error (EVT-122).
	window := make([]json.RawMessage, 0, len(rows))
	for _, res := range rows {
		if afterID != "" && res.ID <= afterID {
			continue
		}
		f := parseFields(res.Body)
		if !view.canRead(rs.cfg.placement(f)) {
			continue
		}
		if !sel.Matches(rs.cfg.selLabels(f), rs.cfg.placement(f), view.inSubtree) {
			continue
		}
		window = append(window, res.Body)
	}

	// The next cursor is bound to this resource type (rs.cfg.resourceType) so it
	// names a keyset position ONLY for this resource's list and is refused under
	// any other (API-033); the encoded token stays opaque to the client.
	page := apihttp.Page(rs.cfg.resourceType, window, limit, func(b json.RawMessage) string {
		return parseFields(b).ID
	})
	writeJSONValue(w, http.StatusOK, page)
}

// ---- patch ----------------------------------------------------------------

func (rs *resource) patch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	current, found, err := rs.srv.store.Get(r.Context(), rs.cfg.kind, id)
	if err != nil {
		rs.internal(w, r, err)
		return
	}
	if !found {
		rs.notFound(w, r)
		return
	}
	// One view answers both halves of this request's authorization, from one
	// read of the tree: whether the row is addressable at all, and whether it
	// may be written where it is and where it is going.
	view, ok := rs.requestView(w, r)
	if !ok {
		return
	}
	// A row outside the caller's visible set is unaddressable, not merely
	// unwritable: the check sits AHEAD of the If-Match precondition so an
	// out-of-reach id draws the same 404 whatever ETag was (or was not)
	// presented. A 428 IF_MATCH_REQUIRED here would confirm the row exists just
	// as surely as a 403 would (scopeview.go).
	curFields := parseFields(current.Body)
	if !view.canRead(rs.cfg.placement(curFields)) {
		rs.notFound(w, r)
		return
	}
	// Addressable, but not necessarily writable: `viewer` clears the visible-set
	// floor and nothing above it (auth.CanWrite). This refusal is 403 rather
	// than 404 because the caller can demonstrably already READ this row — it
	// discloses nothing beyond the caller's own authority — and it too precedes
	// If-Match, since inviting a viewer to retry with an ETag would be an
	// invitation to an operation they can never complete.
	if !view.canWrite(rs.cfg.writeScope(curFields)) {
		rs.refuseWrite(w, r)
		return
	}

	ifMatch, present := r.Header["If-Match"]
	outcome := apihttp.CheckIfMatch(headerValue(ifMatch), present, current.Revision)
	if !outcome.OK {
		rs.concurrencyProblem(w, r, outcome)
		return
	}

	patchBody, ok := readBody(w, r)
	if !ok {
		return
	}
	if rs.rejectClientSuppliedID(w, r, patchBody) {
		return
	}

	merged := mergedBody(current.Body, patchBody)

	// A patch may MOVE the row. Authority over where it currently sits says
	// nothing about where it is going, so the destination is authorized on its
	// own — before the per-kind validation and before the external_id guard, for
	// the same reason the create path orders it there: the guard evaluates
	// uniqueness under the DESTINATION node (parseFields(merged) feeds
	// externalIDGuards below), so an unauthorized destination must be refused
	// before it can be used to probe one.
	//
	// A destination equal to the row's current one is not re-checked: it is not
	// a placement this request is deciding, and authority over it was just
	// established above. Everything else is checked, including the case the
	// visible-set filter cannot catch — moving a row OUT of the caller's reach,
	// where the row would vanish from its own author's view on success.
	eff := parseFields(merged)
	if dest := rs.cfg.writeScope(eff); dest != rs.cfg.writeScope(curFields) {
		if !view.canWrite(dest) {
			rs.refuseWrite(w, r)
			return
		}
	}

	// A per-kind pre-write validation (playlist asset_refs) runs over the EFFECTIVE
	// post-merge body, so a patch that introduces an un-uploaded asset_ref is
	// rejected exactly as a create would be — never stored.
	if rs.writeValidationFailed(w, r, merged) {
		return
	}

	// external_id uniqueness over the effective (post-merge) fields (API-101/102),
	// enforced atomically inside the store write by a WriteGuard — closing the
	// check-then-write race a pre-write snapshot in a separate critical section left
	// open. selfID excludes this row, so an unchanged external_id never self-collides.
	res, err := rs.srv.store.Update(r.Context(), rs.cfg.kind, id, current.Revision, patchBody,
		rs.externalIDGuards(eff.ExternalID, rs.cfg.extScope(eff), id)...)
	if err != nil {
		rs.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("ETag", apihttp.ETag(res.Revision))
	writeJSON(w, http.StatusOK, res.Body)
}

// ---- delete ---------------------------------------------------------------

func (rs *resource) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	current, found, err := rs.srv.store.Get(r.Context(), rs.cfg.kind, id)
	if err != nil {
		rs.internal(w, r, err)
		return
	}
	if !found {
		rs.notFound(w, r)
		return
	}
	// Same ordering as patch: unaddressable before unwritable before the
	// precondition, so an out-of-reach id is a 404 regardless of the presented
	// ETag and a reader who may not mutate is refused before being invited to
	// supply one. A delete moves the row nowhere, so the row's current placement
	// is the whole of what there is to authorize.
	view, ok := rs.requestView(w, r)
	if !ok {
		return
	}
	curFields := parseFields(current.Body)
	if !view.canRead(rs.cfg.placement(curFields)) {
		rs.notFound(w, r)
		return
	}
	if !view.canWrite(rs.cfg.writeScope(curFields)) {
		rs.refuseWrite(w, r)
		return
	}

	ifMatch, present := r.Header["If-Match"]
	outcome := apihttp.CheckIfMatch(headerValue(ifMatch), present, current.Revision)
	if !outcome.OK {
		rs.concurrencyProblem(w, r, outcome)
		return
	}

	if err := rs.srv.store.Delete(r.Context(), rs.cfg.kind, id, current.Revision); err != nil {
		rs.writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- shared helpers -------------------------------------------------------

// externalIDGuards returns the store WriteGuard(s) that enforce external_id
// uniqueness (API-101/102) INSIDE the write transaction — atomically with the
// write, closing the check-then-write race a pre-write snapshot leaves open. The
// guard reuses apihttp.CheckExternalIDUnique over the tx snapshot the store hands
// it (it never re-derives the rule) and returns the *apihttp.ExternalIDError
// verbatim for writeStoreError to render. selfID excuses the row being updated (so
// an unchanged external_id never self-collides); an empty externalID needs no
// guard, since it can never collide (API-100).
func (rs *resource) externalIDGuards(externalID, scopeNode, selfID string) []store.WriteGuard {
	if externalID == "" {
		return nil
	}
	return []store.WriteGuard{func(existing []store.Resource) error {
		refs := rs.refsFrom(existing)
		if xerr := apihttp.CheckExternalIDUnique(refs, rs.cfg.resourceType, rs.cfg.displayName, scopeNode, externalID, selfID); xerr != nil {
			return xerr
		}
		return nil
	}}
}

// refsFrom projects the store rows a WriteGuard is handed onto the ExternalRef
// shape CheckExternalIDUnique consumes. Every row is tagged with the kind's
// resourceType (the family api/1 scopes external_id uniqueness by — API-101
// scopes by resource type together with placement, not by the row's own
// store.Kind), and placed under the kind's own grouping (a scope node by
// parent, a scheduling row by scope_node).
func (rs *resource) refsFrom(rows []store.Resource) []apihttp.ExternalRef {
	refs := make([]apihttp.ExternalRef, 0, len(rows))
	for _, res := range rows {
		f := parseFields(res.Body)
		refs = append(refs, apihttp.ExternalRef{
			ID:           res.ID,
			ExternalID:   res.ExternalID,
			ResourceType: rs.cfg.resourceType,
			ScopeNode:    rs.cfg.extScope(f),
		})
	}
	return refs
}

// mergedBody shallow-merges a patch over the current body — the EFFECTIVE
// post-patch body a per-kind validation and the external_id check both evaluate
// against. A body that cannot be re-marshaled degrades to the current body (the
// store's own validation surfaces the real error on write).
func mergedBody(current, patch []byte) []byte {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(current, &m)
	p := map[string]json.RawMessage{}
	_ = json.Unmarshal(patch, &p)
	for k, v := range p {
		m[k] = v
	}
	merged, err := json.Marshal(m)
	if err != nil {
		return current
	}
	return merged
}

// writeValidationFailed runs the resource kind's per-kind pre-write validation (if
// any) over body and, on a non-empty result, writes the 422 / VALIDATION_FAILED
// Problem carrying the per-field errors as the api/1 `errors` extension (API-013),
// returning true so the caller aborts before any store write. It returns false
// (writing nothing) when the kind declares no validation or the body passes.
func (rs *resource) writeValidationFailed(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if rs.cfg.validate == nil {
		return false
	}
	verrs := rs.cfg.validate(rs.srv, body)
	if len(verrs) == 0 {
		return false
	}
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
		"VALIDATION_FAILED", "Validation Failed",
		"One or more fields failed validation.", validationExtra(verrs))
	return true
}

// writeStoreError maps a store write error onto its api/1 Problem: a datamodel
// validation failure is 422 / VALIDATION_FAILED carrying the per-field errors
// (API-013); an optimistic-concurrency conflict is 412 / REVISION_CONFLICT with
// current_revision; a not-found is 404; anything else is 500 / INTERNAL.
func (rs *resource) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	// An external_id-uniqueness rejection a WriteGuard raised inside the store write
	// (API-101/102): 400 / EXTERNAL_ID_CONFLICT, surfaced atomically with the write
	// it prevented.
	var xerr *apihttp.ExternalIDError
	if errors.As(err, &xerr) {
		rs.problem(w, r, xerr.Status, xerr.Code, xerr.Title, xerr.Detail)
		return
	}
	// A rules/1 compile failure the store's compile-gate raised on an automation
	// write (compile.Compile, never re-run here): the authored rule is rejected
	// 422 / VALIDATION_FAILED carrying the compiler's message as the Problem detail
	// and its offending member as the api/1 `errors` extension (API-013) — a
	// non-compiling rule is never stored nor carried to the relay.
	var cerr *compile.CompileError
	if errors.As(err, &cerr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Validation Failed", cerr.Message, compileErrorExtra(cerr))
		return
	}
	var verr *store.ValidationError
	if errors.As(err, &verr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Validation Failed",
			"One or more fields failed validation.", validationExtra(verr.Errors))
		return
	}
	var rme *store.RevisionMismatchError
	if errors.As(err, &rme) {
		cur := rme.Current
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusPreconditionFailed,
			"REVISION_CONFLICT", "Precondition Failed",
			"The resource was modified concurrently.", map[string]any{"current_revision": cur})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		rs.notFound(w, r)
		return
	}
	rs.internal(w, r, err)
}

// validationExtra renders a datamodel error list as the api/1 `errors` extension
// member (API-013): one {field, code, message} object per failure.
func validationExtra(errs []datamodel.Error) map[string]any {
	arr := make([]map[string]string, 0, len(errs))
	for _, e := range errs {
		arr = append(arr, map[string]string{"field": e.Field, "code": e.Code, "message": e.Message})
	}
	return map[string]any{"errors": arr}
}

// concurrencyProblem writes the Problem a failed If-Match precondition yields,
// carrying current_revision for a REVISION_CONFLICT (API-023).
func (rs *resource) concurrencyProblem(w http.ResponseWriter, r *http.Request, o apihttp.ConcurrencyOutcome) {
	var extra map[string]any
	if o.CurrentRevision != nil {
		extra = map[string]any{"current_revision": *o.CurrentRevision}
	}
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), o.Status, o.Code, o.Title, o.Detail, extra)
}

func (rs *resource) problem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), status, code, title, detail, nil)
}

func (rs *resource) notFound(w http.ResponseWriter, r *http.Request) {
	rs.problem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No "+rs.cfg.displayName+" exists with this identifier.")
}

// internal is the fallback for an unexpected server-side failure (API error
// taxonomy INTERNAL); the detail is deliberately generic.
func (rs *resource) internal(w http.ResponseWriter, r *http.Request, _ error) {
	rs.problem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
}

// readBody reads the full request body, writing a 400 Problem and returning
// ok=false on a read failure.
func readBody(w http.ResponseWriter, r *http.Request) (body []byte, ok bool) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusBadRequest,
			"VALIDATION_FAILED", "Bad Request", "The request body could not be read.", nil)
		return nil, false
	}
	return b, true
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSONValue(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// responseCapture buffers a handler's composed response (status, handler-set
// headers, body) so the exact bytes can be BOTH flushed to the real ResponseWriter
// AND retained verbatim for an Idempotency-Key replay (API-052). It captures only
// what the handler writes; the Trace-Id response header WithTraceID already set on
// the real writer is untouched and still emitted on flush.
type responseCapture struct {
	hdr    http.Header
	status int
	body   bytes.Buffer
}

func (c *responseCapture) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}

func (c *responseCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *responseCapture) Write(b []byte) (int, error) { return c.body.Write(b) }

// flush copies the captured headers and body onto w, then returns the status, a
// COPY of the body bytes, and the Content-Type — the fields an Idempotency-Key
// StoredResponse retains (the copy so the retained record is immune to later buffer
// reuse). A handler that never wrote a status is treated as 200.
func (c *responseCapture) flush(w http.ResponseWriter) (status int, body []byte, contentType string) {
	for k, vs := range c.hdr {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status = c.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	b := c.body.Bytes()
	_, _ = w.Write(b)
	return status, append([]byte(nil), b...), c.hdr.Get("Content-Type")
}

// replay writes a retained idempotent response verbatim (status + content-type +
// body). Per the StoredResponse shape, per-response headers such as ETag/Location
// are not part of the retained record and are not reproduced on replay.
func replay(w http.ResponseWriter, resp apihttp.StoredResponse) {
	ct := resp.ContentType
	if ct == "" {
		ct = contentTypeJSON
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// headerValue returns the first value of a possibly-multi-valued header slice.
func headerValue(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}
