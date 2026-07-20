// Package api is the app-side api/1 HTTP surface for the authoring loop: the
// management door through which scope-nodes and (a follow-up task) the
// scheduling-core resources are authored over HTTP, persisted by internal/app/
// store, and thereby fed into the feeder's signed desired-state.
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
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
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
// clock of its own.
type server struct {
	store *store.Store
	idem  *apihttp.IdempotencyStore
	nowMs func() int64
}

// New builds the api/1 HTTP handler: a /api/v1-prefixed mux exposing the
// scope-nodes CRUD operations over st, wrapped in apihttp.WithTraceID so every
// request resolves a Trace-Id once (echoed by the header and every Problem body).
// idem guards Idempotency-Key create replays; nowMs is the injected clock the
// idempotency calls are timestamped with. The scheduling-core resources are
// mounted by a follow-up task onto this same mux.
func New(st *store.Store, idem *apihttp.IdempotencyStore, nowMs func() int64) http.Handler {
	srv := &server{store: st, idem: idem, nowMs: nowMs}
	mux := http.NewServeMux()
	srv.mount(mux, scopeNodesConfig())
	return apihttp.WithTraceID(mux)
}

// resourceConfig parameterizes the generic resource handler for one resource
// kind: which store table it lives in, its URL path segment, the api/1
// resource-type tag its external_id uniqueness and cross-references are scoped
// by, and the three per-kind field projections the conventions need — the
// selectable label set (labels + any reserved intrinsic), the scope-node
// placement a selector's scope_node term evaluates against, and the grouping
// external_id uniqueness is scoped by. A scope node's placement is itself and
// its external_id groups by parent; a scheduling-core row's placement and
// external_id grouping are both its scope_node (see scopenodes.go / a follow-up).
type resourceConfig struct {
	kind         store.Kind
	path         string
	resourceType string
	selLabels    func(resourceFields) map[string]string
	placement    func(resourceFields) string
	extScope     func(resourceFields) string
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

	// A server-assigned id when the client omitted one (openapi: id is not part
	// of the create body); a client-supplied id is honored as-is.
	body, id := rs.ensureID(raw)
	fields := parseFields(body)

	// external_id uniqueness (API-101/102) runs BEFORE the write.
	if fields.ExternalID != "" {
		refs, err := rs.externalRefs(r)
		if err != nil {
			rs.internal(w, r, err)
			return
		}
		if xerr := apihttp.CheckExternalIDUnique(refs, rs.cfg.resourceType, rs.cfg.extScope(fields), fields.ExternalID, ""); xerr != nil {
			rs.problem(w, r, xerr.Status, xerr.Code, xerr.Title, xerr.Detail)
			return
		}
	}

	res, err := rs.srv.store.Create(r.Context(), rs.cfg.kind, body)
	if err != nil {
		rs.writeStoreError(w, r, err)
		return
	}

	w.Header().Set("ETag", apihttp.ETag(res.Revision))
	w.Header().Set("Location", apiPrefix+"/"+rs.cfg.path+"/"+id)
	respBody := append([]byte(nil), res.Body...)
	writeJSON(w, http.StatusCreated, respBody)

	if key != "" {
		rs.srv.idem.Complete(scope, key, hash, apihttp.StoredResponse{
			Status:      http.StatusCreated,
			Body:        respBody,
			ContentType: contentTypeJSON,
		}, now)
	}
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

	var afterID string
	if cursor != "" {
		lastID, cerr := apihttp.DecodeCursor("", cursor)
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

	// The unscoped keyset: the next cursor is the last returned row's bare ULID.
	page := apihttp.Page("", window, limit, func(b json.RawMessage) string {
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

	// external_id uniqueness over the effective (post-merge) fields (API-101/102).
	eff := effectiveFields(current.Body, patchBody)
	if eff.ExternalID != "" {
		refs, err := rs.externalRefs(r)
		if err != nil {
			rs.internal(w, r, err)
			return
		}
		if xerr := apihttp.CheckExternalIDUnique(refs, rs.cfg.resourceType, rs.cfg.extScope(eff), eff.ExternalID, id); xerr != nil {
			rs.problem(w, r, xerr.Status, xerr.Code, xerr.Title, xerr.Detail)
			return
		}
	}

	res, err := rs.srv.store.Update(r.Context(), rs.cfg.kind, id, current.Revision, patchBody)
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

// externalRefs projects every stored row of the kind onto the ExternalRef shape
// CheckExternalIDUnique consumes, scoping external_id uniqueness by the kind's
// own grouping (a scope node by parent, a scheduling row by scope_node).
func (rs *resource) externalRefs(r *http.Request) ([]apihttp.ExternalRef, error) {
	rows, err := rs.srv.store.List(r.Context(), rs.cfg.kind, store.ListFilter{})
	if err != nil {
		return nil, err
	}
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
	return refs, nil
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

// effectiveFields shallow-merges a patch over the current body and projects the
// result onto the resource baseline — the fields a post-patch external_id check
// evaluates against.
func effectiveFields(current, patch []byte) resourceFields {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(current, &m)
	p := map[string]json.RawMessage{}
	_ = json.Unmarshal(patch, &p)
	for k, v := range p {
		m[k] = v
	}
	merged, err := json.Marshal(m)
	if err != nil {
		return parseFields(current)
	}
	return parseFields(merged)
}

// writeStoreError maps a store write error onto its api/1 Problem: a datamodel
// validation failure is 422 / VALIDATION_FAILED carrying the per-field errors
// (API-013); an optimistic-concurrency conflict is 412 / REVISION_CONFLICT with
// current_revision; a not-found is 404; anything else is 500 / INTERNAL.
func (rs *resource) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
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
	rs.problem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No resource exists at this identifier.")
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
