package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/packsig"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// WithPackTrust wires the trust-anchor source the pack install pipeline
// verifies artifact signature envelopes against (marketplace/1 MKT-009b): the
// namespace-to-keys seam a host-provisioned anchor set fills today and the
// root-signed publisher-namespace delegation fills once the external trust
// root exists. Without this option the install surface still mounts but fails
// closed — every artifact is refused, signed or not.
func WithPackTrust(anchors packsig.TrustAnchors) Option {
	return func(s *server) { s.packTrust = anchors }
}

// WithRequiredPacks wires the deployment's required-pack roster (marketplace/1
// MKT-093a): which packs hold tier-granted "Required" status here and the floor
// version each may not be uninstalled or regressed below.
//
// It is host configuration, never discovery — nothing an index entry or an
// artifact claims puts a pack on it. The roster is handed to the STORE rather
// than kept here, because MKT-093b requires the floor to be enforced inside the
// install and uninstall transactions, where no caller can arrive beside it;
// this handler package only renders the refusal the store produces.
//
// Without this option no pack is required, which is MKT-093a's own default: an
// empty roster withholds a restriction, so failing closed would mean refusing to
// uninstall packs no deployment ever declared essential.
func WithRequiredPacks(r store.RequiredPacks) Option {
	return func(s *server) { s.store.SetRequiredPacks(r) }
}

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
func (srv *server) mountPacks(rt *router) {
	base := apiPrefix + "/packs"
	rt.HandleFunc("POST "+base, srv.installPack)
	rt.HandleFunc("GET "+base, srv.listPacks)
	// One literal segment, registered before the two-segment pack routes below.
	// Go's mux prefers the more specific pattern regardless of order, and
	// `{publisher}/{name}` needs TWO segments, so "catalog" can never be read as
	// a publisher — but the ordering states the intent for a reader.
	rt.HandleFunc("GET "+base+"/catalog", srv.browsePackCatalog)
	rt.HandleFunc("GET "+base+"/{publisher}/{name}", srv.getPack)
	rt.HandleFunc("DELETE "+base+"/{publisher}/{name}", srv.deletePack)
	rt.HandleFunc("GET "+base+"/{publisher}/{name}/pages/{path...}", srv.getPackPage)
	rt.HandleFunc("GET "+base+"/{publisher}/{name}/messages/{locale}", srv.getPackMessages)
	rt.HandleFunc("GET "+base+"/{publisher}/{name}/installs", srv.listPackInstalls)
	rt.HandleFunc("GET "+base+"/{publisher}/{name}/update", srv.getPackUpdateAvailability)
	rt.HandleFunc("PUT "+base+"/{publisher}/{name}/enabled", srv.setPackEnabled)
	rt.HandleFunc("POST "+base+"/{publisher}/{name}/update", srv.updatePack)

	// The pack-data surface: CRUD over a declared collection's universal-envelope
	// rows (MAN-051/052), with the full api/1 conventions. The literal `data`
	// segment is unambiguous against the sibling `pages`/`messages` routes.
	data := base + "/{publisher}/{name}/data/{collection}"
	rt.HandleFunc("GET "+data, srv.listPackRows)
	rt.HandleFunc("POST "+data, srv.createPackRow)
	rt.HandleFunc("GET "+data+"/{entity_id}", srv.getPackRow)
	rt.HandleFunc("PATCH "+data+"/{entity_id}", srv.patchPackRow)
	rt.HandleFunc("DELETE "+data+"/{entity_id}", srv.deletePackRow)
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
	// Enabled is MKT-097's withdrawal state. Always present rather than
	// `omitempty`: a false that vanished from the wire would be read as "this
	// build does not report enablement" by a client that has to tell a disabled
	// pack from an old server, and those need different UI.
	Enabled bool `json:"enabled"`
}

func packEnvelopeOf(p store.Pack) packEnvelope {
	return packEnvelope{
		ID: p.ID, Revision: p.Revision, Version: p.Version,
		DataModelVersion: p.DataModelVersion, CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt, Manifest: p.Manifest, Enabled: p.Enabled,
	}
}

// ---- install --------------------------------------------------------------

// installPack installs a pack, from one of two request bodies distinguished by
// content type: raw artifact bytes (the default, any content type but JSON), or
// a marketplace reference (`application/json`, marketplace/1 MKT-060a) the
// configured registry sources are resolved against. Both routes converge on the
// same pipeline — same signature envelope verification against the same trust
// anchors, same manifest engine, same atomic store write, same install record —
// so resolving a pack can never accept an artifact a direct upload would refuse.
//
// The rest of this comment describes the raw-artifact body, which is unchanged.
//
// It is gated by the real manifest engine (internal/app/packs). A fresh install is 201 with the pack
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
	if !srv.packLifecycleWritable(w, r) {
		return
	}
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

	// The body bytes are the Idempotency-Key replay-vs-reuse content hash on
	// EITHER route, so a retried marketplace reference replays its original
	// response exactly as a retried artifact upload does.
	if wantsMarketplaceRef(r) {
		// Deliberately NOT bounded by the declared member set here, unlike the other
		// action POSTs. This route already refuses an undeclared member — with a
		// better answer: a caller-supplied `key_id` or `content_digest` is an attempt
		// to assert provenance the resolution must establish itself, and
		// installPackFromRef refuses it as MARKETPLACE_REF_INVALID, a published code
		// that says which mistake was made. The generic member check would fire
		// first and replace that with "not a member this operation declares",
		// which is true and less useful.
		//
		// The member check exists to catch members NOTHING else refuses. Where a
		// published per-field code already covers them, adding it is a regression in
		// the answer rather than an improvement in the enforcement.
		srv.idempotent(w, r, artifact, func(w http.ResponseWriter) { srv.installPackFromRef(w, r, artifact) })
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
			"VALIDATION_FAILED", "Validation Failed", aerr.Message, artifactErrorExtra(aerr))
		return
	}
	var merr *packs.ManifestError
	if errors.As(err, &merr) {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
			"VALIDATION_FAILED", "Validation Failed",
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
	if !srv.packLifecycleWritable(w, r) {
		return
	}
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
		// MKT-093b(i): a required pack cannot be uninstalled. The decision was
		// made inside the removal transaction — this only renders it, through
		// the same 422 / VALIDATION_FAILED + errors[] discriminant every other
		// pack refusal uses (the api/1 top-level code registry is closed,
		// API-011, so REQUIRED_PACK_FLOOR rides in errors[] rather than as the
		// top-level code).
		var ferr *store.RequiredPackFloorError
		if errors.As(err, &ferr) {
			apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
				"VALIDATION_FAILED", "Validation Failed",
				fmt.Sprintf("%s is a required pack on this deployment (floor version %s) and cannot be uninstalled.", ferr.PackID, ferr.Floor),
				map[string]any{"errors": []map[string]string{{
					"field":   "pack",
					"code":    "REQUIRED_PACK_FLOOR",
					"message": ferr.Error(),
				}}})
			return
		}
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
// getPackPage serves one installed page document — unless the pack is DISABLED
// (marketplace/1 MKT-097), in which case its surfaces are withdrawn and this is
// a 404.
//
// The refusal lives here rather than only in the nav, and that distinction is
// the whole of whether disabling means anything: a nav that omits a pack while
// its route still answers has hidden a destination, not withdrawn it, and the
// URL an operator already has in a tab keeps working. A pack turned off because
// it is misbehaving must actually stop being served.
//
// 404, not 403: a disabled pack HAS no page surface, which is what the status
// says. A 403 would report a permission this caller might obtain, and there is
// no permission that makes a withdrawn page appear.
//
// Locale catalogs are deliberately NOT withheld — see servePackFile.
func (srv *server) getPackPage(w http.ResponseWriter, r *http.Request) {
	pack, found, err := srv.store.GetPack(r.Context(), packIDFromPath(r))
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}
	if !found || !pack.Enabled {
		srv.packNotFound(w, r)
		return
	}
	srv.servePackFile(w, r, store.PackFilePage, r.PathValue("path"))
}

// getPackMessages serves one installed locale catalog verbatim, keyed by locale.
//
// NOT withheld from a disabled pack (MKT-097), deliberately. A catalog is how
// anything renders the pack's own NAME, and the console still has to name a
// disabled pack — in the installed list, in the control that re-enables it.
// Withholding it would make a pack an operator has turned off render as its raw
// `msg:` key, which is a worse console for no security gain: a locale catalog
// is display strings the box already served.
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

// packLifecycleWritable gates the pack LIFECYCLE routes — install, update,
// uninstall — and writes its own refusal, so a caller has one thing to check.
//
// Every other mutating surface on this api authorizes against the scope node the
// row it touches sits at. A pack has no such node: it is workspace-global, its
// declared collections and pages are reachable from every scope, and the
// capabilities its manifest requests (MAN-021 — device.command, egress.http,
// storage.write) are granted workspace-wide the moment it installs. So the
// question "may this caller write HERE" has no per-node answer for a pack, and
// answering it at some leaf would be authorizing a workspace-wide privilege
// grant against a screen.
//
// The grain is therefore ADMIN AT THE WORKSPACE ORG NODE:
//
//   - not `operator`, which canWrite's floor would give: an operator runs the
//     place day to day, and installing a pack is not an operation on the
//     workspace's content but an addition to what the platform can do — a pack
//     that requests egress.http is asking for a capability an operator is not
//     otherwise able to grant themselves;
//   - not `owner`, which the workspace DESTRUCTION path requires
//     (authorizeWorkspaceOwner): removing the workspace is irreversible, and
//     reserving owner for it keeps that refusal meaningful. Managing packs is
//     administration, not destruction.
//
// security-model/1 defers the complete permission matrix (the draft-note beneath
// SEC-012), so this is a HOST decision inside a deliberately open space rather
// than a requirement being implemented. It is recorded here because the next
// reader will otherwise have to infer it from a comparison.
//
// A root-bound principal passes without an org node existing: RootScopeNode is
// the implicit outermost ancestor of every chain (auth.Resolve), which is what
// the first-boot owner holds before any org node is authored.
func (srv *server) packLifecycleWritable(w http.ResponseWriter, r *http.Request) bool {
	view, err := srv.scopeView(r)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return false
	}
	node, _, err := srv.store.WorkspaceRoot(r.Context())
	if err != nil {
		// No org node authored yet: authorize against the root sentinel itself,
		// which is where a first-boot owner's binding sits. Any other store error
		// is a refusal, never a pass — an authorization question that cannot be
		// resolved refuses (SEC-005).
		if err != store.ErrNoWorkspace {
			srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
				"An unexpected server error occurred.")
			return false
		}
		node = auth.RootScopeNode
	}
	if role, bound := view.roleAt(node); !bound || !role.AtLeast(auth.RoleAdmin) {
		srv.packProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return false
	}
	return true
}
