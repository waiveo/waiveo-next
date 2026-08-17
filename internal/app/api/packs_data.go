package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// The pack-data surface — /api/v1/extensions/{publisher}/{name}/data/{collection} —
// makes a pack's manifest-declared collections (MAN-051) first-class api/1
// citizens: each row carries the universal entity envelope plus its declared
// fields, and every write is gated by packs.ValidateRowWrite (envelope invariants
// + the declared field types + the MAN-052 lifecycle rule) before it reaches the
// store. The conventions are the shared api/1 helpers, never re-derived: ETag /
// If-Match (concurrency), the keyset cursor (pagination), external_id uniqueness
// scoped per (pack, collection, scope_node), and Idempotency-Key on create.
//
// No pack code executes: a row is data, validated against the manifest's declared
// shape and persisted. An unknown pack or an undeclared collection is a 404
// Problem (resolved from the installed manifest, the single source of truth for
// which collections exist).

// packRowResourceType is the api/1 resource-type tag the pack-data list's keyset
// cursor and its external_id uniqueness are scoped by.
const packRowResourceType = "packrow"

// packRowCursorPrefix is the scope tag every pack-data cursor carries. The keyset
// position (entity_id) IS a ULID, but the cursor is additionally bound to its
// (pack, collection) so a cursor minted by ONE collection's list is refused under
// another (API-033/035) rather than paged from as an arbitrary keyset position —
// the position alone (a bare ULID) could not carry that binding.
const packRowCursorPrefix = packRowResourceType + "_"

// packRowCursorSep separates the pack id, collection, and entity_id inside a
// pack-data cursor's decoded payload. It is the ASCII unit separator (0x1f), which
// never appears in a pack id, a collection name, or a ULID, so the split is
// unambiguous. The payload is base64url-encoded, keeping the whole token opaque
// and within the api/1 opaque-cursor grammar (API-036).
const packRowCursorSep = "\x1f"

// encodePackRowCursorPos renders the keyset position after a row into the payload a
// next-page cursor carries: the (pack, collection, entity_id) triple, base64url
// (no padding) encoded so the token is opaque and URL-safe. apihttp.Page prefixes
// the packRowResourceType scope tag, yielding `packrow_<base64url(...)>`.
func encodePackRowCursorPos(packID, collection, entityID string) string {
	raw := packID + packRowCursorSep + collection + packRowCursorSep + entityID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodePackRowCursor recovers the keyset position (the last entity_id seen) a
// pack-data cursor names, scoped to THIS (pack, collection) list. A malformed
// token, one minted by a different pack or collection, or one whose position is
// not a valid ULID is refused 400 / CURSOR_INVALID (API-035) rather than silently
// paged from — the same refusal apihttp.DecodeCursor emits for a cross-scope
// cursor, adapted to the pack row's composite scope.
func decodePackRowCursor(cursor, packID, collection string) (string, *apihttp.PageParamError) {
	rest, ok := strings.CutPrefix(cursor, packRowCursorPrefix)
	if !ok {
		return "", packCursorInvalid(cursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(raw) == 0 {
		return "", packCursorInvalid(cursor)
	}
	parts := strings.SplitN(string(raw), packRowCursorSep, 3)
	if len(parts) != 3 || parts[0] != packID || parts[1] != collection || !ulid.Valid(parts[2]) {
		return "", packCursorInvalid(cursor)
	}
	return parts[2], nil
}

// resolvePackCollection loads the installed pack named in the request path and the
// manifest-declared collection named by {collection}. An unknown pack, or a
// collection the pack's manifest does not declare, is a 404 Problem (the installed
// manifest is the source of truth for which collections exist). It returns the pack
// id, the collection name, the collection declaration, and ok=false when it has
// already written the 404/500 — the caller returns immediately.
func (srv *server) resolvePackCollection(w http.ResponseWriter, r *http.Request) (packID, collection string, decl manifest.Collection, ok bool) {
	packID = packIDFromPath(r)
	collection = r.PathValue("collection")
	p, found, err := srv.store.GetPack(r.Context(), packID)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return "", "", manifest.Collection{}, false
	}
	if !found {
		srv.packNotFound(w, r)
		return "", "", manifest.Collection{}, false
	}
	var m manifest.PackManifest
	if err := json.Unmarshal(p.Manifest, &m); err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return "", "", manifest.Collection{}, false
	}
	decl, declared := packs.CollectionByName(&m, collection)
	if !declared {
		srv.packCollectionNotFound(w, r)
		return "", "", manifest.Collection{}, false
	}
	return packID, collection, decl, true
}

// ---- list -----------------------------------------------------------------

func (srv *server) listPackRows(w http.ResponseWriter, r *http.Request) {
	packID, collection, _, ok := srv.resolvePackCollection(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	cursor, limit, pperr := apihttp.ParsePageParams(q.Get("cursor"), q.Get("limit"))
	if pperr != nil {
		srv.packProblem(w, r, pperr.Status, pperr.Code, pperr.Title, pperr.Detail)
		return
	}
	var afterID string
	if cursor != "" {
		lastID, cerr := decodePackRowCursor(cursor, packID, collection)
		if cerr != nil {
			srv.packProblem(w, r, cerr.Status, cerr.Code, cerr.Title, cerr.Detail)
			return
		}
		afterID = lastID
	}

	rows, err := srv.store.ListPackRows(r.Context(), packID, collection)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}

	view, err := srv.scopeView(r)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}

	// ListPackRows is already entity_id-ascending; advance past the cursor position.
	// entity_id is a ULID, so a byte comparison is the keyset order. The
	// visible-set filter runs here, before apihttp.Page cuts the page, so a page
	// of `limit` rows is genuinely `limit` rows — a pack's rows carry the
	// universal envelope's own scope_node (MAN-052) and are scoped by it exactly
	// as a first-party resource is.
	window := make([]packRowWire, 0, len(rows))
	for _, row := range rows {
		if afterID != "" && row.EntityID <= afterID {
			continue
		}
		if !view.canRead(row.ScopeNode) {
			continue
		}
		window = append(window, packRowWire{row: row})
	}
	page := apihttp.Page(packRowResourceType, window, limit, func(pw packRowWire) string {
		return encodePackRowCursorPos(packID, collection, pw.row.EntityID)
	})
	writeJSONValue(w, http.StatusOK, page)
}

// ---- create ---------------------------------------------------------------

func (srv *server) createPackRow(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	// A resource-creating POST → honors Idempotency-Key (API-050/052) through the
	// SAME srv.idempotent wrapper the generic create and pack install use.
	srv.idempotent(w, r, raw, func(w http.ResponseWriter) { srv.createPackRowExec(w, r, raw) })
}

func (srv *server) createPackRowExec(w http.ResponseWriter, r *http.Request, raw []byte) {
	packID, collection, decl, ok := srv.resolvePackCollection(w, r)
	if !ok {
		return
	}
	vr, verrs := packs.ValidateRowWrite(decl, raw)
	if len(verrs) > 0 {
		srv.writeRowValidation(w, r, verrs)
		return
	}
	// A pack row carries the universal envelope's own scope_node (MAN-052) and
	// is placed by it exactly as a first-party resource is, so its create is
	// authorized exactly as one: at the node the row will live under, and BEFORE
	// the external_id guard the store write carries — the guard scopes
	// uniqueness by (pack, collection, scope_node) and so must scan rows this
	// caller may not see, which is an existence oracle for anyone whose
	// placement was not refused first (api.go, scopeview.go).
	if !srv.packRowWritable(w, r, vr.ScopeNode) {
		return
	}
	created, err := srv.store.CreatePackRow(r.Context(), packID, collection, storeRowFrom(vr),
		srv.packRowExternalIDGuards(vr.ExternalID, vr.ScopeNode, "")...)
	if err != nil {
		srv.writePackRowStoreError(w, r, err)
		return
	}
	body, err := packRowJSON(created)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	w.Header().Set("ETag", apihttp.ETag(created.Revision))
	w.Header().Set("Location", apiPrefix+"/extensions/"+packID+"/data/"+collection+"/"+created.EntityID)
	writeJSON(w, http.StatusCreated, body)
}

// ---- get ------------------------------------------------------------------

func (srv *server) getPackRow(w http.ResponseWriter, r *http.Request) {
	packID, collection, _, ok := srv.resolvePackCollection(w, r)
	if !ok {
		return
	}
	row, found, err := srv.store.GetPackRow(r.Context(), packID, collection, r.PathValue("entity_id"))
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	if !found {
		srv.packRowNotFound(w, r)
		return
	}
	if !srv.packRowVisible(w, r, row) {
		return
	}
	body, err := packRowJSON(row)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	w.Header().Set("ETag", apihttp.ETag(row.Revision))
	writeJSON(w, http.StatusOK, body)
}

// packRowVisible reports whether the request's principal may read row, WRITING
// the refusal itself when it may not — the 404 an entity_id naming no row draws
// (packRowNotFound), byte for byte, so a row outside the visible set is
// indistinguishable from one that was never created (scopeview.go). It also
// writes the 500 on a failed tree read, so a caller has only one thing to check.
func (srv *server) packRowVisible(w http.ResponseWriter, r *http.Request, row store.PackRow) bool {
	view, err := srv.scopeView(r)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return false
	}
	if !view.canRead(row.ScopeNode) {
		srv.packRowNotFound(w, r)
		return false
	}
	return true
}

// packRowWritable reports whether the request's principal holds write authority
// at node, WRITING the 403 / FORBIDDEN refusal itself — byte for byte the one
// every other api/1 write emits (unauthorizedWriteDetail) — when it does not.
// It also writes the 500 on a failed tree read, so a caller has one thing to
// check.
//
// A pack's rows are scoped by their own scope_node (MAN-052) exactly as a
// first-party resource is, so there is deliberately no pack-specific
// authorization rule here: the same auth.CanWrite, over the same tree, with the
// same refusal.
func (srv *server) packRowWritable(w http.ResponseWriter, r *http.Request, node string) bool {
	view, err := srv.scopeView(r)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return false
	}
	if !view.canWrite(node) {
		srv.packProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return false
	}
	return true
}

// ---- patch ----------------------------------------------------------------

func (srv *server) patchPackRow(w http.ResponseWriter, r *http.Request) {
	packID, collection, decl, ok := srv.resolvePackCollection(w, r)
	if !ok {
		return
	}
	entityID := r.PathValue("entity_id")

	current, found, err := srv.store.GetPackRow(r.Context(), packID, collection, entityID)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	if !found {
		srv.packRowNotFound(w, r)
		return
	}
	// Ahead of the If-Match precondition, so an out-of-reach row draws the same
	// 404 whatever ETag was presented (api.go's patch applies the same ordering
	// for the same reason), and a caller who may read this row but not mutate it
	// is refused before being invited to supply an ETag they can do nothing with.
	if !srv.packRowVisible(w, r, current) {
		return
	}
	if !srv.packRowWritable(w, r, current.ScopeNode) {
		return
	}

	ifMatch, present := r.Header["If-Match"]
	outcome := apihttp.CheckIfMatch(headerValue(ifMatch), present, current.Revision)
	if !outcome.OK {
		var extra map[string]any
		if outcome.CurrentRevision != nil {
			extra = map[string]any{"current_revision": *outcome.CurrentRevision}
		}
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), outcome.Status, outcome.Code, outcome.Title, outcome.Detail, extra)
		return
	}

	patchBody, ok := readBody(w, r)
	if !ok {
		return
	}

	// Validate the EFFECTIVE post-merge row: the current row's full JSON
	// representation shallow-merged with the patch, so unchanged envelope/declared
	// values are inherited and one validation path covers create and patch alike. A
	// patch that nulls scope_node, breaks a field's type, or violates MAN-052 is
	// rejected exactly as a create would be — nothing is stored.
	curJSON, err := packRowJSON(current)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	merged := mergedBody(curJSON, patchBody)
	vr, verrs := packs.ValidateRowWrite(decl, merged)
	if len(verrs) > 0 {
		srv.writeRowValidation(w, r, verrs)
		return
	}

	// A patch may MOVE the row: MAN-052's scope_node is a writable envelope
	// field, so the DESTINATION is authorized on its own, before the external_id
	// guard that would otherwise evaluate uniqueness under a node this caller
	// was never entitled to write at. An unchanged destination is not
	// re-checked — it is not a placement this request decides, and authority
	// over it was established above.
	if vr.ScopeNode != current.ScopeNode && !srv.packRowWritable(w, r, vr.ScopeNode) {
		return
	}

	updated, err := srv.store.UpdatePackRow(r.Context(), packID, collection, entityID, current.Revision, storeRowFrom(vr),
		srv.packRowExternalIDGuards(vr.ExternalID, vr.ScopeNode, entityID)...)
	if err != nil {
		srv.writePackRowStoreError(w, r, err)
		return
	}
	body, err := packRowJSON(updated)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	w.Header().Set("ETag", apihttp.ETag(updated.Revision))
	writeJSON(w, http.StatusOK, body)
}

// ---- delete ---------------------------------------------------------------

func (srv *server) deletePackRow(w http.ResponseWriter, r *http.Request) {
	packID, collection, _, ok := srv.resolvePackCollection(w, r)
	if !ok {
		return
	}
	entityID := r.PathValue("entity_id")

	current, found, err := srv.store.GetPackRow(r.Context(), packID, collection, entityID)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	if !found {
		srv.packRowNotFound(w, r)
		return
	}
	// Same ordering as patchPackRow: unaddressable before unwritable, both ahead
	// of the precondition. A delete moves the row nowhere, so its current
	// placement is the whole of what there is to authorize.
	if !srv.packRowVisible(w, r, current) {
		return
	}
	if !srv.packRowWritable(w, r, current.ScopeNode) {
		return
	}

	ifMatch, present := r.Header["If-Match"]
	outcome := apihttp.CheckIfMatch(headerValue(ifMatch), present, current.Revision)
	if !outcome.OK {
		var extra map[string]any
		if outcome.CurrentRevision != nil {
			extra = map[string]any{"current_revision": *outcome.CurrentRevision}
		}
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), outcome.Status, outcome.Code, outcome.Title, outcome.Detail, extra)
		return
	}

	if err := srv.store.DeletePackRow(r.Context(), packID, collection, entityID, current.Revision); err != nil {
		srv.writePackRowStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers --------------------------------------------------------------

// storeRowFrom projects a validated row onto the store's caller-supplied PackRow
// fields (the store owns entity_id/revision/timestamps).
func storeRowFrom(vr packs.ValidatedRow) store.PackRow {
	return store.PackRow{
		LifecycleState: vr.LifecycleState,
		ScopeNode:      vr.ScopeNode,
		Labels:         vr.Labels,
		TemplateRef:    vr.TemplateRef,
		Params:         vr.Params,
		ExternalID:     vr.ExternalID,
		Body:           vr.Body,
	}
}

// packRowWire marshals a store.PackRow to its flat api/1 JSON representation: the
// universal envelope keys and the declared fields together in one object (MAN-051
// guarantees no declared field can collide with an envelope key, so the flatten is
// safe). It carries the row so a list's idOf can read the entity_id for the cursor.
type packRowWire struct {
	row store.PackRow
}

func (pw packRowWire) MarshalJSON() ([]byte, error) { return packRowJSON(pw.row) }

// packRowJSON renders a stored row as its flat api/1 body: the declared fields,
// then the universal envelope (entity_id/revision/lifecycle_state/scope_node/
// labels/template_ref/params) and the row's created/updated timestamps and
// external_id. template_ref and params serialize to JSON null when absent; labels
// is always an array; external_id is omitted when unset.
func packRowJSON(row store.PackRow) ([]byte, error) {
	m := map[string]json.RawMessage{}
	if len(row.Body) > 0 {
		if err := json.Unmarshal(row.Body, &m); err != nil {
			return nil, err
		}
	}
	m["entity_id"] = mustRawJSON(row.EntityID)
	m["revision"] = mustRawJSON(row.Revision)
	m["lifecycle_state"] = mustRawJSON(row.LifecycleState)
	m["scope_node"] = mustRawJSON(row.ScopeNode)
	labels := row.Labels
	if labels == nil {
		labels = []string{}
	}
	m["labels"] = mustRawJSON(labels)
	if row.TemplateRef == "" {
		m["template_ref"] = json.RawMessage("null")
	} else {
		m["template_ref"] = mustRawJSON(row.TemplateRef)
	}
	if len(row.Params) == 0 {
		m["params"] = json.RawMessage("null")
	} else {
		m["params"] = row.Params
	}
	if row.ExternalID != "" {
		m["external_id"] = mustRawJSON(row.ExternalID)
	}
	m["created_at"] = mustRawJSON(row.CreatedAt)
	m["updated_at"] = mustRawJSON(row.UpdatedAt)
	return json.Marshal(m)
}

// mustRawJSON marshals a value known to be JSON-encodable (a string, int, or
// []string) into a json.RawMessage.
func mustRawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// packRowExternalIDGuards returns the store guard that enforces external_id
// uniqueness per (pack, collection, scope_node) atomically inside the write
// (API-101/102). The store hands the guard the collection's existing rows (already
// pack+collection-scoped); CheckExternalIDUnique further narrows by scope_node, so
// the effective uniqueness scope is exactly (pack, collection, scope_node). selfID
// excuses the row being updated; an empty external_id needs no guard.
func (srv *server) packRowExternalIDGuards(externalID, scopeNode, selfID string) []store.PackRowGuard {
	if externalID == "" {
		return nil
	}
	return []store.PackRowGuard{func(existing []store.PackRow) error {
		refs := make([]apihttp.ExternalRef, 0, len(existing))
		for _, row := range existing {
			refs = append(refs, apihttp.ExternalRef{
				ID:           row.EntityID,
				ExternalID:   row.ExternalID,
				ResourceType: packRowResourceType,
				ScopeNode:    row.ScopeNode,
			})
		}
		if xerr := apihttp.CheckExternalIDUnique(refs, packRowResourceType, "pack row", scopeNode, externalID, selfID); xerr != nil {
			return xerr
		}
		return nil
	}}
}

// writeRowValidation renders a pack-data write's field errors as a 422 Problem
// under the registry-valid top-level VALIDATION_FAILED (API-011), the per-field
// codes riding inside the errors[] extension (API-013).
func (srv *server) writeRowValidation(w http.ResponseWriter, r *http.Request, errs []packs.RowError) {
	arr := make([]map[string]string, 0, len(errs))
	for _, e := range errs {
		arr = append(arr, map[string]string{"field": e.Field, "code": e.Code, "message": e.Message})
	}
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
		"VALIDATION_FAILED", "Validation Failed",
		"One or more fields failed validation.", map[string]any{"errors": arr})
}

// writePackRowStoreError maps a pack-row store write error onto its api/1 Problem:
// an external_id-uniqueness rejection is 400 / EXTERNAL_ID_CONFLICT, an
// optimistic-concurrency conflict is 412 / REVISION_CONFLICT with current_revision,
// a missing row is 404, anything else 500.
func (srv *server) writePackRowStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var xerr *apihttp.ExternalIDError
	if errors.As(err, &xerr) {
		srv.packProblem(w, r, xerr.Status, xerr.Code, xerr.Title, xerr.Detail)
		return
	}
	// MAN-056: a create into an occupied singleton collection. 409 rather than
	// 422 — the body is well-formed and would be accepted against an empty
	// collection, so this is a conflict with existing state, not a validation
	// failure the caller can fix by editing fields. The Detail names the remedy
	// because there always is one: the row the caller wants already exists.
	var serr *store.SingletonOccupiedError
	if errors.As(err, &serr) {
		srv.packProblem(w, r, http.StatusConflict, "SINGLETON_COLLECTION_OCCUPIED", "Conflict",
			"This collection holds a single record and already has one. Update the existing record instead of creating another.")
		return
	}
	// A store-side data-model refusal — a pack row placed at a scope node that
	// does not exist (DAT-006) is the one that reaches here today. It renders as
	// the SAME 422 VALIDATION_FAILED with an API-013 `errors[]` extension a
	// first-party resource write's refusal does: a pack row carries the universal
	// envelope's own scope_node and is placed by it exactly as a resource is, so a
	// caller must be able to branch on the identical shape. Without this arm the
	// store's refusal fell through to INTERNAL, which tells a client with a fixable
	// request that the server is broken.
	var verr *store.ValidationError
	if errors.As(err, &verr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Validation Failed",
			"One or more fields failed validation.", validationExtra(verr.Errors))
		return
	}
	var rme *store.RevisionMismatchError
	if errors.As(err, &rme) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusPreconditionFailed,
			"REVISION_CONFLICT", "Precondition Failed",
			"The row was modified concurrently.", map[string]any{"current_revision": rme.Current})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		srv.packRowNotFound(w, r)
		return
	}
	srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
		"An unexpected server error occurred.")
}

// packCollectionNotFound is the 404 for a collection the pack's manifest does not
// declare (MAN-051).
func (srv *server) packCollectionNotFound(w http.ResponseWriter, r *http.Request) {
	srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
		"No collection with this name is declared by this extension.")
}

// packRowNotFound is the 404 for an entity_id that names no row in the collection.
func (srv *server) packRowNotFound(w http.ResponseWriter, r *http.Request) {
	srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
		"No row exists at this identifier.")
}
