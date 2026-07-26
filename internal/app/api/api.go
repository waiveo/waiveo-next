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
// Auth is DEFERRED for this dev-lab POC: the surface is unauthenticated (api/1
// treats the principal as an opaque given; the sessions/API-keys/roles model is
// a separate security-model contract). The Idempotency-Key scope's principal is
// therefore a single fixed POC principal (pocPrincipal) — documented here, not
// silently omitted.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// pocPrincipal is the fixed dev-lab POC principal the Idempotency-Key scope is
// keyed by while auth is deferred (see the package doc). It is an opaque,
// fixture ULID — never a credential — and is the single principal every request
// in this unauthenticated increment is attributed to.
const pocPrincipal = "01J8Z0DEV00PRNC0PA00000000"

// contentTypeJSON is the media type every success response body uses; error
// bodies use apihttp.ProblemContentType instead.
const contentTypeJSON = "application/json"

// apiPrefix is the version-scoped mount point every resource path hangs off.
const apiPrefix = "/api/v1"

// server holds the dependencies every resource handler shares: the store the
// resources live in, the idempotency store guarding create replays, and the
// injected clock (epoch ms) the idempotency Begin/Complete calls are timestamped
// with — so the api layer, like the idempotency store itself, reads no wall
// clock of its own. content is the shared content-addressed origin store the
// upload endpoint writes into (and the feeder serves GET /content/<hex> from over
// the SAME instance); contentBase is the feeder's own content-origin base URL the
// upload's returned url is built from (<base>/content/<hex>, snapshot.Build's form).
type server struct {
	store       *store.Store
	idem        *apihttp.IdempotencyStore
	nowMs       func() int64
	content     *origin.Store
	contentBase string
	// installer runs the manifest-gated declarative-pack install pipeline the
	// POST /api/v1/packs handler drives (internal/app/packs). It shares the same
	// store every other resource handler writes through.
	installer *packs.Installer
}

// New builds the api/1 HTTP handler: a /api/v1-prefixed mux exposing the
// scope-nodes and scheduling-core (schedules/dayparts/playlists) CRUD
// operations over st, plus the content-addressed asset upload (POST
// /api/v1/content) over content, wrapped in apihttp.WithTraceID so every request
// resolves a Trace-Id once (echoed by the header and every Problem body). idem
// guards Idempotency-Key create replays; nowMs is the injected clock the
// idempotency calls are timestamped with. content is the shared origin store the
// feeder also serves GET /content/<hex> from (one instance, so an upload is
// immediately servable); contentBase is the feeder's content-origin base URL the
// upload's returned url is built from.
func New(st *store.Store, idem *apihttp.IdempotencyStore, nowMs func() int64, content *origin.Store, contentBase string) http.Handler {
	srv := &server{
		store: st, idem: idem, nowMs: nowMs, content: content, contentBase: contentBase,
		installer: packs.NewInstaller(st),
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
	return apihttp.WithTraceID(mux)
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
	scope := apihttp.IdempotencyScope{Principal: pocPrincipal, Method: r.Method, Path: r.URL.Path}
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
// with ETag/Location, or an api/1 Problem — to w. external_id uniqueness
// (API-101/102) and client-supplied-id collision are enforced ATOMICALLY inside the
// store write (via a WriteGuard and the store's own id check), closing the
// check-then-write race a pre-write snapshot in a separate critical section left
// open. w is the response capture create() owns, so the exact bytes are retainable
// for an Idempotency-Key replay.
func (rs *resource) createExec(w http.ResponseWriter, r *http.Request, raw []byte) {
	// A server-assigned id when the client omitted one (openapi: id is not part of
	// the create body); a client-supplied id is honored as-is.
	body, id := rs.ensureID(raw)
	fields := parseFields(body)

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

// ensureID returns the create body guaranteed to carry an identity, plus that
// id. A client-supplied id is kept; otherwise a fresh ULID is minted and
// injected under the kind's identity field.
func (rs *resource) ensureID(raw []byte) (body []byte, id string) {
	f := parseFields(raw)
	if got := rs.cfg.identity(f); got != "" {
		return raw, got
	}
	id = ulid.New()
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
	w.Header().Set("ETag", apihttp.ETag(res.Revision))
	writeJSON(w, http.StatusOK, res.Body)
}

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
	// arbitrary keyset position in the wrong collection (API-033/035). The
	// unscoped bare-ULID form is the corpus exception pinned to the automations
	// list, not a default for every kind mounted through this handler.
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

	inSubtree, err := rs.srv.inSubtreeFn(r)
	if err != nil {
		rs.internal(w, r, err)
		return
	}

	// Keyset advance past the cursor position, then selector filter. ULIDs sort
	// lexicographically in id order, so a byte comparison is the keyset order.
	window := make([]json.RawMessage, 0, len(rows))
	for _, res := range rows {
		if afterID != "" && res.ID <= afterID {
			continue
		}
		f := parseFields(res.Body)
		if !sel.Matches(rs.cfg.selLabels(f), rs.cfg.placement(f), inSubtree) {
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

	// A per-kind pre-write validation (playlist asset_refs) runs over the EFFECTIVE
	// post-merge body, so a patch that introduces an un-uploaded asset_ref is
	// rejected exactly as a create would be — never stored.
	merged := mergedBody(current.Body, patchBody)
	if rs.writeValidationFailed(w, r, merged) {
		return
	}

	// external_id uniqueness over the effective (post-merge) fields (API-101/102),
	// enforced atomically inside the store write by a WriteGuard — closing the
	// check-then-write race a pre-write snapshot in a separate critical section left
	// open. selfID excludes this row, so an unchanged external_id never self-collides.
	eff := parseFields(merged)
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

// inSubtreeFn builds the scope-subtree predicate a selector's `scope_node
// subtree` term consults: whether node lies strictly below ancestor in the
// current scope-node tree. The tree is read fresh per list (POC).
func (srv *server) inSubtreeFn(r *http.Request) (func(ancestor, node string) bool, error) {
	nodes, _, _, _, err := srv.store.DesiredStateRows(r.Context())
	if err != nil {
		return nil, err
	}
	tree, _ := datamodel.BuildScopeTree(nodes)
	return func(ancestor, node string) bool {
		if ancestor == node {
			return false
		}
		for _, id := range tree.AncestorChain(node) {
			if id == ancestor {
				return true
			}
		}
		return false
	}, nil
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
		"VALIDATION_FAILED", "Unprocessable Entity",
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
			"VALIDATION_FAILED", "Unprocessable Entity", cerr.Message, compileErrorExtra(cerr))
		return
	}
	var verr *store.ValidationError
	if errors.As(err, &verr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Unprocessable Entity",
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
