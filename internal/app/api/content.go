package api

import (
	"log"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// contentSigner is the minter for every content URL this surface hands back.
//
// It is taken from the ORIGIN STORE this surface writes to and the feeder
// serves from, so the key a URL is minted under is by construction the key that
// origin verifies under (origin.Store.Signer). Built any other way, this route
// can — and did — answer `201 {asset_ref, url}` with a url the very same
// process refuses `403`, because the origin enforced signatures while this
// route concatenated a bare address.
//
// contenturl.ServeTTL, not SnapshotTTL: these URLs are minted at RESPONSE time
// and fetched by whoever asked, within the life of one console session. The two
// constants hold the same value today, so naming the right one is not currently
// load-bearing — which is exactly why it is named deliberately rather than left
// to chance, since the day they diverge again this route must not silently
// acquire a build-time lifetime.
//
// A url minted here is DERIVED and short-lived, so it is never stored: an
// authoring surface persists the `asset_ref` and re-resolves the url from this
// listing when it needs to render. store.stripDerivedMembers enforces that on
// the write side — see its doc for what a persisted one did.
func (srv *server) contentSigner() contenturl.Signer {
	return srv.content.Signer(srv.contentBase, contenturl.ServeTTL)
}

// uploadContent handles POST /api/v1/content: the content-addressed asset upload
// over the feeder's shared origin store (relay/1 REL-061).
//
// It reads the raw request body and stores the bytes under their OWN sha256
// content hash (server.content.Add), computing the asset_ref server-side — a
// client-supplied ref is never trusted. It responds 201 with the content-addressed
// {asset_ref, url}, where url is MINTED by the origin's own signer
// (srv.contentSigner) rather than assembled here; a screen or a console fetches
// those bytes directly from the content origin (the relay is never in this data
// path, REL-140 — the upload writes into the SAME origin.Store the feeder serves
// GET /content/<hex> from, so the asset is immediately servable at the url
// returned). A zero-length body is rejected 400 / VALIDATION_FAILED — empty
// content cannot be stored.
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
	url, _ := srv.contentSigner().Mint(assetRef)

	writeJSONValue(w, http.StatusCreated, map[string]string{
		"asset_ref": assetRef,
		"url":       url,
	})
}

// contentEntry is one row of the content-origin listing: the same
// {asset_ref, url} an upload returns, plus the size and store time a media
// browser needs to show and sort the library. It is deliberately the upload
// response shape widened, not a new vocabulary — an authoring UI pastes an
// asset_ref from either surface interchangeably.
type contentEntry struct {
	AssetRef  string `json:"asset_ref"`
	URL       string `json:"url"`
	SizeBytes int64  `json:"size_bytes"`
	StoredAt  int64  `json:"stored_at"`
	// Referenced is whether any persisted asset-bearing row names this asset.
	//
	// It is the one fact that turns this listing from an inventory into an
	// answer: the retention sweep reclaims content that is UNREFERENCED and past
	// its windows, so without this an operator can see what exists and not what
	// is about to be destroyed — which was the whole of the complaint.
	//
	// A SNAPSHOT, not a promise. It is read under the store's read lock and is
	// true at the instant it was taken; a playlist authored a moment later
	// references an asset this response called unreferenced. That is acceptable
	// here precisely because this surface only REPORTS. The sweep answers the
	// same question under the WRITE lock, held across its deletions, because it
	// acts on the answer — and nothing may delete on the strength of this one.
	Referenced bool `json:"referenced"`
}

// assetReferenced decides the listing's `referenced` for one asset, and exists
// as a named function so the FAILURE POSTURE can be tested rather than inferred
// from an expression.
//
// The posture is deliberately asymmetric. When the reference set could not be
// read, every asset reports REFERENCED — the conservative direction — because
// the two errors are not equally bad:
//
//   - reporting in-use content as unreferenced invites an operator to treat a
//     live asset as disposable, which is the outcome this member exists to
//     prevent;
//   - reporting orphaned content as referenced merely withholds a cleanup
//     opportunity for one request.
//
// A boolean has no third state to say "unknown", so the value has to lean, and
// it leans away from the harm. The read failure is logged, so the reason a
// listing suddenly shows everything as in use is discoverable.
func assetReferenced(refErr error, digests map[string]bool, hexDigest string) bool {
	if refErr != nil {
		return true
	}
	return digests[hexDigest]
}

// listContent handles GET /api/v1/content: the content-origin's own listing.
//
// Upload was write-only — a client that did not keep the asset_ref an upload
// returned had no way to rediscover it, so an authoring surface could reference
// bytes only within the session that uploaded them. This is the read half: every
// asset the origin currently serves, so a media browser can show the library and
// an editor's image picker can offer it.
//
// It reads the SAME origin.Store the feeder serves GET /content/<hex> from, so
// the listing and the servable bytes cannot disagree — an entry here is fetchable
// now, and a swept asset is absent from both. Entries are digest-ordered
// (origin.Store.Entries), a stable order independent of upload time.
//
// Every row's url is minted by that same store's signer, once for the whole
// listing so a page of assets shares one deadline: an image picker that renders
// half its thumbnails before an unlucky second boundary must not have the other
// half expire a millisecond earlier than the first.
func (srv *server) listContent(w http.ResponseWriter, r *http.Request) {
	entries := srv.content.Entries()
	// The SAME projection the retention sweep reads, so the two cannot disagree
	// about what counts as a reference. A listing that called an asset
	// unreferenced while the sweep retained it — or the reverse — would be a
	// surface quietly contradicting the machinery it describes.
	//
	// A failure here is NOT fatal to the listing. The inventory is the answer to
	// "what exists", which is knowable without the reference set, and refusing
	// the whole response would take away the part that still works. The
	// reference column degrades instead: every asset reports `referenced: true`,
	// the conservative direction, because the alternative — an unreadable
	// reference set rendering as "nothing is referenced" — invites an operator to
	// treat content in use as due for reclamation.
	refs, refErr := srv.store.ContentReferenceSnapshot(r.Context())
	if refErr != nil {
		log.Printf("api: listing content could not read the reference set, so every asset "+
			"reports referenced: %v", refErr)
	}
	sign := srv.contentSigner()
	out := make([]contentEntry, 0, len(entries))
	for _, e := range entries {
		url, _ := sign.Mint(e.HexDigest)
		out = append(out, contentEntry{
			AssetRef:   "sha256:" + e.HexDigest,
			URL:        url,
			SizeBytes:  int64(e.SizeBytes),
			StoredAt:   e.StoredAtMs,
			Referenced: assetReferenced(refErr, refs.Digests, e.HexDigest),
		})
	}
	writeJSONValue(w, http.StatusOK, map[string]any{"content": out})
}
