package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// packResourceType is the api/1 resource-type tag the packs list's keyset
// cursor is bound to, so a cursor minted by another resource's list is refused
// here rather than paged from as an arbitrary position (API-033/035).
const packResourceType = "pack"

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
// unsafe/malformed artifact is a 422 carrying the artifact's own code; a
// manifest the engine refused is a 422 whose errors[] extension is the
// contract's typed per-field violations (API-013). NOTHING in the artifact is
// executed — a pack is data.
func (srv *server) installPack(w http.ResponseWriter, r *http.Request) {
	// Read the body under a hard cap (one beyond the artifact limit, so an
	// over-limit upload is detected and refused as oversize rather than buffered
	// whole). The pipeline's ReadBundle enforces the same limit authoritatively.
	limit := packs.DefaultLimits.MaxArtifactBytes
	artifact, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		srv.packProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "Bad Request",
			"The request body could not be read.")
		return
	}

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

// writeInstallError maps an install failure onto its api/1 Problem. A
// *packs.ArtifactError (an unsafe/malformed/oversize artifact) is a 422 carrying
// its own stable code and message, no errors[]. A *packs.ManifestError (the
// manifest engine's refusal) is a 422 whose errors[] extension is the contract's
// typed per-field manifest violations. Anything else is a 500.
func (srv *server) writeInstallError(w http.ResponseWriter, r *http.Request, err error) {
	var aerr *packs.ArtifactError
	if errors.As(err, &aerr) {
		srv.packProblem(w, r, http.StatusUnprocessableEntity, aerr.Code, "Unprocessable Entity", aerr.Message)
		return
	}
	var merr *packs.ManifestError
	if errors.As(err, &merr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"MANIFEST_INVALID", "Unprocessable Entity",
			"The pack manifest failed validation.", manifestErrorsExtra(merr.Errors))
		return
	}
	srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
		"An unexpected server error occurred.")
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
		lastID, cerr := apihttp.DecodeCursor(packResourceType, cursor)
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
	page := apihttp.Page(packResourceType, window, limit, func(e packEnvelope) string { return e.ID })
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
