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
// immediately servable). Content-addressing makes the upload idempotent: re-posting
// identical bytes yields the same asset_ref. A zero-length body is rejected
// 400 / VALIDATION_FAILED — empty content cannot be stored.
func (srv *server) uploadContent(w http.ResponseWriter, r *http.Request) {
	// The one route whose body is asset bytes rather than a resource description,
	// so it carries its own, much larger ceiling (see maxContentUploadBytes).
	body, ok := readBodyLimit(w, r, maxContentUploadBytes)
	if !ok {
		return
	}
	if len(body) == 0 {
		apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusBadRequest,
			"VALIDATION_FAILED", "Bad Request", "The content upload body must not be empty.", nil)
		return
	}

	assetRef, err := srv.content.Add(body)
	if err != nil {
		// The bytes could not be durably persisted to the content origin; the
		// upload is not honored rather than reported stored-but-volatile (which
		// would let a playlist reference content that vanishes on restart).
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
