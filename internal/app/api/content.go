package api

import (
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// uploadContent handles POST /api/v1/content: the content-addressed asset upload
// over the feeder's shared origin store (relay/1 REL-061).
//
// It reads the raw request body and stores the bytes under their OWN sha256
// content hash (server.content.Add), computing the asset_ref server-side — a
// client-supplied ref is never trusted. It responds 201 with the content-addressed
// {asset_ref, url}, where url is the single-sourced <base>/content/<hex> form
// snapshot.Build uses; a screen fetches those bytes directly from the content
// origin (the relay is never in this data path, REL-140 — the upload writes into
// the SAME origin.Store the feeder serves GET /content/<hex> from, so the asset is
// immediately servable). A zero-length body is rejected 400 / VALIDATION_FAILED —
// empty content cannot be stored.
//
// # Idempotency-Key
//
// This is a mutating POST, so it honors Idempotency-Key through the SAME
// srv.idempotent seam every other mutating route on this surface uses
// (API-050/052/053) — never a second mechanism.
//
// Content-addressing alone does not satisfy that requirement, which is why the
// route carrying it was the one route that did not. Re-posting IDENTICAL bytes
// does yield the same asset_ref, so the replay half looks free; the half that is
// not free is API-053, where the same key presented with DIFFERENT bytes must
// conflict. Under content-addressing that request quietly succeeds, mints a
// second, unrelated asset_ref, and hands the client back a ref for content it did
// not think it was uploading — exactly the retry-gone-wrong case the key exists to
// catch. The retained response also makes a replayed upload cost nothing on the
// content store.
func (srv *server) uploadContent(w http.ResponseWriter, r *http.Request) {
	// The one route whose body is asset bytes rather than a resource description,
	// so it carries its own, much larger ceiling (see maxContentUploadBytes).
	body, ok := readBodyLimit(w, r, maxContentUploadBytes)
	if !ok {
		return
	}
	// The body is the asset bytes, so it is also what the replay-vs-reuse content
	// hash is taken over (API-052) — the same bytes the ref is computed from.
	srv.idempotent(w, r, body, func(w http.ResponseWriter) { srv.uploadContentExec(w, r, body) })
}

// uploadContentExec performs the upload and writes its outcome. w is the response
// capture srv.idempotent owns, so the exact bytes are retainable for a keyed
// replay.
func (srv *server) uploadContentExec(w http.ResponseWriter, r *http.Request, body []byte) {
	if len(body) == 0 {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusBadRequest,
			"VALIDATION_FAILED", "Bad Request", "The content upload body must not be empty.", nil)
		return
	}

	assetRef, err := srv.content.Add(body)
	if err != nil {
		// The bytes could not be durably persisted to the content origin; the
		// upload is not honored rather than reported stored-but-volatile (which
		// would let a playlist reference content that vanishes on restart). A 5xx
		// leaves any presented Idempotency-Key retryable (srv.idempotent aborts
		// rather than completes it), which is what a transient store failure
		// wants.
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusInternalServerError,
			"INTERNAL", "Internal Server Error", "The content could not be stored.", nil)
		return
	}
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	writeJSONValue(w, http.StatusCreated, map[string]string{
		"asset_ref": assetRef,
		"url":       srv.contentBase + "/content/" + hexDigest,
	})
}
