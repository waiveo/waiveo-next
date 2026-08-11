package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/castbundle"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// castbundles.go moves ONE authored slidecast between boxes (parity row 1.9,
// the legacy stack's `.cast` bundles):
//
//	GET  /api/v1/casts/{cast_id}/export   the design + its images, as one file
//	POST /api/v1/casts/import             that file, as a new cast here
//
// The legacy system had this and it was used weekly: build a menu on the office
// box, put it on the shop's box. The workspace archive (parity row 7.5) does not
// replace it — that moves the WHOLE workspace and replaces the destination, which
// is the wrong instrument entirely for "here, have this design".
//
// # The import writes through the ORDINARY create path
//
// This is the load-bearing decision in the file. The import does not call
// store.Create itself: it assembles a CastCreate body and hands it to the SAME
// `resource.createExec` that serves `POST /api/v1/casts`. So an imported cast
// goes through the identical schema gate, the identical undeclared-member gate,
// the identical server-side id minting, the identical placement authorization,
// the identical layer validation, the identical asset-reference guard, the
// identical audit record and the identical 201 + ETag + Location.
//
// The alternative — a bespoke write path — is how an import becomes the one door
// into the store that skips a rule. Every rule this platform has about a cast
// exists because a cast that breaks it renders wrong on a television, and a
// bundle is UNTRUSTED INPUT: it arrived as a file over email. It must be held to
// a higher bar than the editor, never a lower one, and reusing the editor's own
// path is the only way to be sure it is held to the same one.
//
// # Why the bundle carries no identity and no placement
//
// See internal/castbundle's package doc. The short version: an id, a scope node,
// an external_id and a revision all describe the SOURCE deployment, and each is
// either meaningless or actively wrong here. The importer names the placement;
// the platform mints the identity.
//
// # Why the import is not signed, and what stands in for a signature
//
// Nothing about the file is trusted. Every asset's bytes are re-hashed and must
// match the reference they are carried under (castbundle.Read), the manifest is
// the sole list of what will be written, and the slides are re-validated by the
// platform's own authoring rules on the way in. A signature would tell the
// importer WHO made the file, which is not the question an import has to answer.

// castExportResourceType is the display noun a 404 on the export names.
const castBundleDisplayName = "cast"

// mountCastBundles registers both operations.
//
// `POST /casts/import` cannot be shadowed by the family's own `POST /casts`
// (different segment counts), and `GET /casts/{cast_id}/export` cannot be
// shadowed by `GET /casts/{id}` for the same reason. Registering them beside the
// family rather than inside `mount` keeps the generic resource machinery generic
// — a cast is the only kind with a portable single-row bundle, because it is the
// only kind an operator hands to another operator.
func (srv *server) mountCastBundles(rt *router) {
	rt.HandleFunc("GET "+apiPrefix+"/casts/{cast_id}/export", srv.exportCast)
	rt.HandleFunc("POST "+apiPrefix+"/casts/import", srv.importCast)
}

// exportCast handles GET /api/v1/casts/{cast_id}/export.
func (srv *server) exportCast(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cast_id")
	res, found, err := srv.store.Get(r.Context(), store.KindCast, id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
			"No "+castBundleDisplayName+" exists with this identifier.")
		return
	}
	// Read authorization at the row's own placement — the same visible-set test
	// `GET /casts/{id}` applies, and answered the same way (404, never 403): a
	// distinct refusal would tell a caller a cast they may not see exists.
	view, err := srv.scopeView(r)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !view.canRead(parseFields(res.Body).ScopeNode) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
			"No "+castBundleDisplayName+" exists with this identifier.")
		return
	}

	var row datamodel.Cast
	if err := json.Unmarshal(res.Body, &row); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// The images. Read through store.RowAssetReferences — the ONE projection that
	// knows where a cast's references live — rather than by walking the layers
	// here, so this can never disagree with the retention sweep, the authoring
	// guard, or the workspace export about what a cast references.
	refs, err := store.RowAssetReferences(store.KindCast, res.Body)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	// Resolve every asset BEFORE a byte of response is written, so that the only
	// thing left after the header is bytes on a socket.
	//
	// Holding the map is free: origin.Store.Serve returns the resident slice it
	// already holds, so this is slice headers, not copies.
	assets := map[string][]byte{}
	for _, ref := range refs {
		if srv.content == nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
				"This deployment has no content origin, so a cast's images cannot be exported.")
			return
		}
		body := srv.content.Serve(strings.TrimPrefix(ref.Ref, "sha256:"))
		if body == nil {
			// The row references bytes the origin does not hold. Refusing beats
			// writing a bundle with a hole in it: the hole would only surface on
			// the destination box, as a slide that renders blank, long after
			// anyone could connect it to this export.
			writeProblem(w, r, http.StatusConflict, "CONFLICT", "Conflict",
				"This cast references an image this box no longer holds ("+ref.Ref+"), so it cannot be exported completely.")
			return
		}
		assets[ref.Ref] = body
	}

	// EVERY refusal, in one call, while a Problem document is still possible.
	//
	// castbundle.NewPlan is the whole refusal set — asset count, per-asset size,
	// asset total, manifest size, overhead reserve, and any refusal added later.
	// This route does not restate any of them, and that is the finding it
	// closes: the previous version hand-copied ONE of five into a pre-flight
	// here and then called the streaming writer after committing the 200, so a
	// cast referencing 513 images (nothing caps slides or layers) answered
	// `200 application/zip` with a zero-byte body and a `.cast` filename. The
	// operator saved a file the destination then called "not a cast bundle",
	// which pointed them at the wrong box for a fault that was on this one.
	//
	// A refusal added to NewPlan tomorrow reaches this response for free; there
	// is nothing here to keep in step. See castbundle's own two-phase note.
	plan, err := castbundle.NewPlan(castbundle.Manifest{
		ExportedAtMs: srv.nowMs(),
		SourceCastID: row.ID,
		Cast: castbundle.CastPayload{
			Name:              row.Name,
			Slides:            row.Slides,
			DefaultDurationMS: row.DefaultDurationMS,
			Template:          row.Template,
			Labels:            row.Labels,
		},
	}, assets)
	if err != nil {
		var refusal *castbundle.Refusal
		if !errors.As(err, &refusal) {
			// Not a refusal at all — the manifest would not encode. That is this
			// box malfunctioning, not the operator's design being wrong, and the
			// two must not read the same.
			writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
			return
		}
		writeProblem(w, r, http.StatusConflict, "CONFLICT", "Conflict", exportRefusal(refusal))
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+castbundle.FileName(row.Name)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Straight to the socket. Stream cannot refuse anything (its own doc, and
	// the syntax-tree fence in castbundle's tests), so every failure mode that
	// could still occur from here is the connection itself, and there is nothing
	// to report it to.
	if err := plan.Stream(w); err != nil {
		log.Printf("api: cast export %s failed mid-stream: %v", row.ID, err)
	}
}

// exportRefusal turns a castbundle refusal into what the operator reads:
// castbundle's own already-worded sentence, plus the ADVICE this layer is the
// one that knows.
//
// It deliberately does NOT enumerate the refusals. A switch here would be the
// hand-copied subset all over again — a refusal added to NewPlan would arrive
// with no advice, or fall into a default describing the wrong problem — so the
// only thing branched on is which of the two ADVICE shapes applies, and that is
// a sentinel test, not a restatement of the refusal set.
func exportRefusal(refusal *castbundle.Refusal) string {
	if errors.Is(refusal, castbundle.ErrIncomplete) {
		// Reachable only if the origin stopped holding bytes between the resolve
		// loop above and here. Kept because "an image is gone" and "this design
		// is too big" send an operator to different places, and NewPlan is the
		// backstop for the race that loop cannot close.
		return refusal.Detail + " A cast cannot be exported without every image it draws."
	}
	return refusal.Detail +
		" A bundle like that could not be imported anywhere, so it is refused here rather than " +
		"produced and rejected on arrival. Move this design with a workspace archive instead."
}

// importCast handles POST /api/v1/casts/import: the bundle's bytes as the
// request body, the destination placement as the `scope_node` query parameter.
//
// The placement is a QUERY parameter rather than a member of the body because
// the body is a binary file — the same shape `POST /content` takes, for the same
// reason. It is required: a cast has to live somewhere, and guessing (the
// workspace root, the caller's first binding) would put an operator's design at
// a node they did not choose and, half the time, cannot see.
func (srv *server) importCast(w http.ResponseWriter, r *http.Request) {
	// castbundle.MaxBundleBytes, not maxContentUploadBytes. A bundle is not an
	// upload of one asset: it is a design plus every image it draws, and capping
	// it at the single-asset ceiling is what made a bundle THIS BOX had just
	// produced permanently un-importable — two video layers was enough. The
	// reader enforces the same number, and TestExportAndImportAgreeOnOneBundleLimit
	// drives a bundle-sized body through this route to prove it.
	body, ok := readBodyLimit(w, r, castbundle.MaxBundleBytes)
	if !ok {
		return
	}
	// Keyed on the BUNDLE BYTES, so a retry-on-timeout replays the original 201
	// rather than importing a second copy of the same design (API-050/052/053) —
	// and the same key with a different bundle conflicts, which is exactly the
	// retry-gone-wrong this guards.
	srv.idempotent(w, r, body, func(w http.ResponseWriter) { srv.importCastExec(w, r, body) })
}

func (srv *server) importCastExec(w http.ResponseWriter, r *http.Request, body []byte) {
	if len(body) == 0 {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusBadRequest,
			"VALIDATION_FAILED", "Bad Request", "The request body must carry a cast bundle.", nil)
		return
	}
	scopeNode := strings.TrimSpace(r.URL.Query().Get("scope_node"))
	if scopeNode == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"`scope_node` is required: an imported cast must be placed at a node in this deployment's own tree.")
		return
	}

	bundle, err := castbundle.Read(body)
	if err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", bundleRefusal(err))
		return
	}

	// The images go into the content origin BEFORE the row is written, because
	// the create path's own asset guard refuses a cast naming an asset the origin
	// does not hold — correctly, since such a cast renders a blank. castbundle.Read
	// has already re-hashed every one of these, and origin.Add re-derives the ref
	// from the bytes again, so a bundle cannot make this box store content under a
	// reference that is not its own hash.
	//
	// An import that then FAILS validation leaves these bytes behind, unreferenced.
	// That is deliberate and it is the safe direction: the content origin is
	// append-only and content-addressed, an unreferenced asset is reclaimed by the
	// retention sweep on its own schedule, and the alternative — deleting them on
	// the failure path — would delete bytes an unrelated cast might have started
	// referencing in between.
	for ref, content := range bundle.Assets {
		if srv.content == nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
				"This deployment has no content origin, so a cast's images cannot be imported.")
			return
		}
		got, addErr := srv.content.Add(content)
		if addErr != nil {
			writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
				"An image in this bundle could not be stored.")
			return
		}
		if got != ref {
			// Unreachable through castbundle.Read, which verified the same hash.
			// Checked anyway: the cost of being wrong is a slide pointing at bytes
			// that are not the bytes it names.
			writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
				"An image in this bundle does not match the reference it is carried under.")
			return
		}
	}

	// The create body — the SAME shape POST /casts takes, deliberately assembled
	// as JSON and handed to the same handler rather than written directly. A
	// `name` query parameter renames on the way in, which is what an operator
	// importing a second copy of a design onto the same box needs.
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = bundle.Manifest.Cast.Name
	}
	create := map[string]any{
		"scope_node": scopeNode,
		"name":       name,
		"slides":     bundle.Manifest.Cast.Slides,
	}
	if bundle.Manifest.Cast.DefaultDurationMS != 0 {
		create["default_duration_ms"] = bundle.Manifest.Cast.DefaultDurationMS
	}
	if bundle.Manifest.Cast.Template {
		create["template"] = true
	}
	if len(bundle.Manifest.Cast.Labels) > 0 {
		create["labels"] = bundle.Manifest.Cast.Labels
	}
	raw, err := json.Marshal(create)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// The same ceiling `POST /casts` applies to a create body, applied to the one
	// this route assembles in process.
	//
	// It is not defence against a hostile caller — readBodyLimit already bounded
	// the bundle. It is what keeps castbundle.MaxManifestBytes' justification
	// TRUE. That limit is argued as safe on the grounds that "nothing this box
	// authors can approach it, because a cast's create body is capped at
	// maxJSONBodyBytes", and this path bypassed that cap: a bundle carrying a 4
	// MiB manifest imported cleanly, producing a cast whose slides alone were 4
	// MiB, whose re-export would then need a manifest of 4 MiB PLUS the asset
	// entries — past MaxManifestBytes, refused. An operator would have had a cast
	// this box imported and cannot export, which is the export/import
	// disagreement that whole limit block exists to make impossible.
	//
	// 422 rather than 400: the request was well-formed and the bundle was
	// readable. What is wrong is the design inside it, which is what the create
	// path would say about the same slides.
	if int64(len(raw)) > maxJSONBodyBytes {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			fmt.Sprintf("This bundle's cast describes %d bytes of slides, past the %d-byte limit this deployment accepts for a cast. "+
				"It was produced by a box with a larger limit; nothing was imported.", len(raw), maxJSONBodyBytes))
		return
	}

	// Through the ordinary create path — see this file's header for why that is
	// the whole point rather than a convenience.
	rs := &resource{srv: srv, cfg: castsConfig()}
	rs.createExec(w, r, raw)
}

// bundleRefusal turns a castbundle error into the sentence an operator reads.
//
// The distinctions are kept because they send somebody to different places: "you
// picked the wrong file" and "this file is damaged" and "this file has been
// altered" are three different next actions, and collapsing them into "invalid
// bundle" makes an operator retry the thing that will not work.
func bundleRefusal(err error) string {
	switch {
	case errors.Is(err, castbundle.ErrWrongFormat):
		return "This bundle was written by a newer version of Waiveo than this box runs; upgrade the box, or export it again from a box on this version."
	case errors.Is(err, castbundle.ErrAssetMismatch):
		return "An image in this bundle does not match the reference it is carried under; the file has been altered or corrupted in transit. Nothing was imported."
	case errors.Is(err, castbundle.ErrDamaged):
		return "This cast bundle is incomplete — part of it is missing. Export it again from the box it came from. Nothing was imported."
	case errors.Is(err, castbundle.ErrTooLarge):
		return "This bundle is larger than this box will accept."
	default:
		return "That file is not a cast bundle. A cast bundle is the file a Waiveo box produces from a cast's Export."
	}
}
