package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// packResourceType is the api/1 resource-type tag the packs list's keyset
// cursor is bound to, so a cursor minted by another resource's list is refused
// here rather than paged from as an arbitrary position (API-033/035).
const packResourceType = "pack"

// packCursorPrefix is the scope tag every packs-list cursor carries, so a cursor
// minted by another resource's list — a `<scope>_<ulid>` token or a bare ULID —
// is refused CURSOR_INVALID here rather than paged from as an arbitrary position.
const packCursorPrefix = packResourceType + "_"

// encodePackCursorID renders a pack id into the keyset position a next-page
// cursor carries. A pack id is <publisher>/<name> — lowercase and slash-bearing,
// so NOT the ULID the shared apihttp cursor helpers assume (they require the
// position to satisfy ulid.Valid, API-034, and the whole token to match the
// opaque-cursor grammar `^[A-Za-z0-9_-]+$`, API-036). base64url (no padding)
// encodes the id into exactly that grammar's alphabet, keeping the cursor opaque
// and URL-safe while still naming the exact id, so pagination works past page 1.
func encodePackCursorID(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

// decodePackCursor recovers the pack id a next-page cursor names, scoped to the
// packs list. A malformed token, or one minted by a different list (a wrong
// scope tag, a bare ULID, or non-base64url content), is refused 400 /
// CURSOR_INVALID rather than silently paged from as an arbitrary keyset position
// (API-035) — the same refusal the shared apihttp.DecodeCursor emits for a
// cross-scope cursor, adapted to the pack id's non-ULID shape.
func decodePackCursor(cursor string) (string, *apihttp.PageParamError) {
	rest, ok := strings.CutPrefix(cursor, packCursorPrefix)
	if !ok {
		return "", packCursorInvalid(cursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(raw) == 0 {
		return "", packCursorInvalid(cursor)
	}
	return string(raw), nil
}

// packCursorInvalid builds the 400 / CURSOR_INVALID Problem a rejected packs-list
// cursor yields — byte-identical to apihttp.DecodeCursor's own (closed code
// registry API-011, API-035).
func packCursorInvalid(cursor string) *apihttp.PageParamError {
	return &apihttp.PageParamError{
		Status: http.StatusBadRequest,
		Code:   "CURSOR_INVALID",
		Title:  "Bad Request",
		Detail: fmt.Sprintf("The pagination cursor %q is not valid.", cursor),
	}
}

// mountPacks registers the declarative-packs surface under /api/v1/packs. A pack
// id is <publisher>/<name> (two path segments, MAN-001), so the item routes
// capture {publisher} and {name} separately and rejoin them — the id never rides
// as a single opaque segment. A page path (UIS-001) MAY itself be nested, so the
// page route takes a trailing {path...} wildcard.
func (srv *server) mountPacks(mux *http.ServeMux) {
	base := apiPrefix + "/packs"
	mux.HandleFunc("POST "+base, srv.installPack)
	mux.HandleFunc("GET "+base, srv.listPacks)
	mux.HandleFunc("GET "+base+"/{publisher}/{name}", srv.getPack)
	mux.HandleFunc("DELETE "+base+"/{publisher}/{name}", srv.deletePack)
	mux.HandleFunc("GET "+base+"/{publisher}/{name}/pages/{path...}", srv.getPackPage)
	mux.HandleFunc("GET "+base+"/{publisher}/{name}/messages/{locale}", srv.getPackMessages)

	// The pack-data surface: CRUD over a declared collection's universal-envelope
	// rows (MAN-051/052), with the full api/1 conventions. The literal `data`
	// segment is unambiguous against the sibling `pages`/`messages` routes.
	data := base + "/{publisher}/{name}/data/{collection}"
	mux.HandleFunc("GET "+data, srv.listPackRows)
	mux.HandleFunc("POST "+data, srv.createPackRow)
	mux.HandleFunc("GET "+data+"/{entity_id}", srv.getPackRow)
	mux.HandleFunc("PATCH "+data+"/{entity_id}", srv.patchPackRow)
	mux.HandleFunc("DELETE "+data+"/{entity_id}", srv.deletePackRow)
}

// packIDFromPath rejoins the {publisher}/{name} path segments into the pack id.
func packIDFromPath(r *http.Request) string {
	return r.PathValue("publisher") + "/" + r.PathValue("name")
}

// packEnvelope is the JSON body a pack GET / list item returns: the identity and
// baseline plus the full manifest body. revision doubles as the ETag validator.
type packEnvelope struct {
	ID               string          `json:"id"`
	Revision         int64           `json:"revision"`
	Version          string          `json:"version"`
	DataModelVersion int             `json:"data_model_version"`
	CreatedAt        int64           `json:"created_at"`
	UpdatedAt        int64           `json:"updated_at"`
	Manifest         json.RawMessage `json:"manifest"`
}

func packEnvelopeOf(p store.Pack) packEnvelope {
	return packEnvelope{
		ID: p.ID, Revision: p.Revision, Version: p.Version,
		DataModelVersion: p.DataModelVersion, CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt, Manifest: p.Manifest,
	}
}

// ---- install --------------------------------------------------------------

// installPack installs a pack from a raw zip request body, gated by the real
// manifest engine (internal/app/packs). A fresh install is 201 with the pack
// identity + summary; a reinstall that updated an existing pack is 200. An
// unsafe/malformed artifact is a 422 whose errors[] extension carries the
// artifact's own stable code under the registry-valid top-level VALIDATION_FAILED;
// a manifest the engine refused is a 422 whose errors[] extension is the
// contract's typed per-field violations (API-013). NOTHING in the artifact is
// executed — a pack is data.
//
// It is a mutating, resource-creating POST, so it honors Idempotency-Key
// (API-050/052/072) through the SAME srv.idempotent wrapper the generic create()
// and the automations mcp:act POSTs use — never a second mechanism. A client's
// retry-on-timeout with the same key + the same artifact replays the original
// 201 verbatim rather than re-running the install (which would reinstall the pack
// in place, bumping its revision and returning 200); the same key with a
// different artifact is a 409 reuse conflict.
func (srv *server) installPack(w http.ResponseWriter, r *http.Request) {
	// Read the body under a hard cap (one beyond the artifact limit, so an
	// over-limit upload is detected and refused as oversize rather than buffered
	// whole). The pipeline's ReadBundle enforces the same limit authoritatively.
	// The artifact bytes are also the Idempotency-Key replay-vs-reuse content hash.
	limit := packs.DefaultLimits.MaxArtifactBytes
	artifact, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		srv.packProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "Bad Request",
			"The request body could not be read.")
		return
	}

	srv.idempotent(w, r, artifact, func(w http.ResponseWriter) { srv.installPackExec(w, r, artifact) })
}

// installPackExec is the install's actual work, run once per fresh (non-replayed)
// request under the Idempotency-Key guard in installPack. It writes into the
// response capture that guard owns, so a successful install's exact response
// bytes (201 + summary) are retained for a later retry's verbatim replay.
func (srv *server) installPackExec(w http.ResponseWriter, r *http.Request, artifact []byte) {
	res, err := srv.installer.Install(r.Context(), artifact)
	if err != nil {
		srv.writeInstallError(w, r, err)
		return
	}

	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Location", apiPrefix+"/packs/"+res.ID)
	writeJSONValue(w, status, res)
}

// writeInstallError maps an install failure onto its api/1 Problem. Both refusal
// kinds are a 422 under the SAME registry-valid top-level code VALIDATION_FAILED
// (API-011): api/1's error-code registry is closed, so a bespoke top-level code
// like MANIFEST_INVALID or a PACK_ARTIFACT_* value would fall outside it — the
// domain-specific code rides inside the errors[] extension instead, exactly as
// every sibling handler (datamodel/compile) already does. A *packs.ArtifactError
// (an unsafe/malformed/oversize artifact) carries its stable PACK_ARTIFACT_* code
// in a single-entry errors[] and its human message as the Problem detail. A
// *packs.ManifestError (the manifest engine's refusal) carries the contract's
// typed per-field manifest violations as errors[] (API-013). Anything else is a
// 500.
func (srv *server) writeInstallError(w http.ResponseWriter, r *http.Request, err error) {
	var aerr *packs.ArtifactError
	if errors.As(err, &aerr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Unprocessable Entity", aerr.Message, artifactErrorExtra(aerr))
		return
	}
	var merr *packs.ManifestError
	if errors.As(err, &merr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Unprocessable Entity",
			"The pack manifest failed validation.", manifestErrorsExtra(merr.Errors))
		return
	}
	srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
		"An unexpected server error occurred.")
}

// artifactErrorExtra renders a single artifact refusal as the api/1 errors[]
// extension (API-013): one {field, code, message} object carrying the artifact
// engine's stable code (PACK_ARTIFACT_*). The artifact has no request field to
// name — the uploaded zip as a whole is the offending member — so the field is
// the literal "artifact". This keeps the diagnostic discriminant in the response
// while the top-level code stays the registry-valid VALIDATION_FAILED (API-011),
// mirroring automations.go's single compile.CompileError → errors[] mapping.
func artifactErrorExtra(aerr *packs.ArtifactError) map[string]any {
	return map[string]any{"errors": []map[string]string{{
		"field":   "artifact",
		"code":    aerr.Code,
		"message": aerr.Message,
	}}}
}

// manifestErrorsExtra renders a manifest error list as the api/1 errors[]
// extension (API-013): one {field, code, message} object per violation, the
// codes being the manifest/1 taxonomy's own (UNKNOWN_CAPABILITY,
// RESOURCE_BELOW_FLOOR, DATAMODEL_VERSION_REGRESSION, …).
func manifestErrorsExtra(errs []manifest.Error) map[string]any {
	arr := make([]map[string]string, 0, len(errs))
	for _, e := range errs {
		arr = append(arr, map[string]string{"field": e.Field, "code": e.Code, "message": e.Message})
	}
	return map[string]any{"errors": arr}
}

// ---- list -----------------------------------------------------------------

func (srv *server) listPacks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	cursor, limit, pperr := apihttp.ParsePageParams(q.Get("cursor"), q.Get("limit"))
	if pperr != nil {
		srv.packProblem(w, r, pperr.Status, pperr.Code, pperr.Title, pperr.Detail)
		return
	}
	var afterID string
	if cursor != "" {
		lastID, cerr := decodePackCursor(cursor)
		if cerr != nil {
			srv.packProblem(w, r, cerr.Status, cerr.Code, cerr.Title, cerr.Detail)
			return
		}
		afterID = lastID
	}

	all, err := srv.store.ListPacks(r.Context())
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}

	// ListPacks is already id-ascending; advance past the cursor position. Pack
	// ids are not ULIDs but sort lexicographically the same, so a byte comparison
	// is the keyset order.
	window := make([]packEnvelope, 0, len(all))
	for _, p := range all {
		if afterID != "" && p.ID <= afterID {
			continue
		}
		window = append(window, packEnvelopeOf(p))
	}
	// The next cursor is bound to this resource type AND base64url-encodes the pack
	// id (which is not a ULID), so apihttp.Page mints a `pack_<base64url(id)>` token
	// decodePackCursor round-trips — never a raw slash-bearing id the grammar rejects.
	page := apihttp.Page(packResourceType, window, limit, func(e packEnvelope) string { return encodePackCursorID(e.ID) })
	writeJSONValue(w, http.StatusOK, page)
}

// ---- get ------------------------------------------------------------------

func (srv *server) getPack(w http.ResponseWriter, r *http.Request) {
	id := packIDFromPath(r)
	p, found, err := srv.store.GetPack(r.Context(), id)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	if !found {
		srv.packNotFound(w, r)
		return
	}
	w.Header().Set("ETag", apihttp.ETag(p.Revision))
	writeJSONValue(w, http.StatusOK, packEnvelopeOf(p))
}

// ---- delete (uninstall) ---------------------------------------------------

func (srv *server) deletePack(w http.ResponseWriter, r *http.Request) {
	id := packIDFromPath(r)
	p, found, err := srv.store.GetPack(r.Context(), id)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	if !found {
		srv.packNotFound(w, r)
		return
	}

	ifMatch, present := r.Header["If-Match"]
	outcome := apihttp.CheckIfMatch(headerValue(ifMatch), present, p.Revision)
	if !outcome.OK {
		var extra map[string]any
		if outcome.CurrentRevision != nil {
			extra = map[string]any{"current_revision": *outcome.CurrentRevision}
		}
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), outcome.Status, outcome.Code, outcome.Title, outcome.Detail, extra)
		return
	}

	if err := srv.store.UninstallPack(r.Context(), id, p.Revision); err != nil {
		var rme *store.RevisionMismatchError
		if errors.As(err, &rme) {
			apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusPreconditionFailed,
				"REVISION_CONFLICT", "Precondition Failed",
				"The pack was modified concurrently.", map[string]any{"current_revision": rme.Current})
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			srv.packNotFound(w, r)
			return
		}
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- page document + locale catalog ---------------------------------------

// getPackPage serves one installed page document (ui-schema/1) verbatim, keyed
// by its ui.pages[].path. The document is stored data; it is returned as-is for
// the console to render (or, on an invalid doc, to fall back to the standard
// error EmptyState — the renderer's concern, not this endpoint's).
func (srv *server) getPackPage(w http.ResponseWriter, r *http.Request) {
	srv.servePackFile(w, r, store.PackFilePage, r.PathValue("path"))
}

// getPackMessages serves one installed locale catalog verbatim, keyed by locale.
func (srv *server) getPackMessages(w http.ResponseWriter, r *http.Request) {
	srv.servePackFile(w, r, store.PackFileLocale, r.PathValue("locale"))
}

func (srv *server) servePackFile(w http.ResponseWriter, r *http.Request, kind, name string) {
	id := packIDFromPath(r)
	body, found, err := srv.store.GetPackFile(r.Context(), id, kind, name)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	if !found {
		srv.packNotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// ---- helpers --------------------------------------------------------------

func (srv *server) packProblem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), status, code, title, detail, nil)
}

func (srv *server) packNotFound(w http.ResponseWriter, r *http.Request) {
	srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No pack exists at this identifier.")
}
