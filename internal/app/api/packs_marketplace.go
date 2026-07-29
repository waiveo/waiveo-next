package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// WithMarketplace wires the registry sources a pack install by marketplace
// reference resolves through (marketplace/1 MKT-060/060a). Without it, POST
// /api/v1/packs still installs a directly-supplied artifact; a marketplace
// reference is refused, because a deployment with no configured registry has
// nothing to resolve against and MUST NOT invent one.
//
// This option adds a way to LOCATE an artifact. It adds no trust: a resolved
// artifact is verified by the same packsig path, against the same anchors
// WithPackTrust supplies, as one uploaded by hand.
func WithMarketplace(m *packs.Market) Option {
	return func(s *server) { s.market = m }
}

// installRefBody is the JSON request body form of a marketplace reference
// (marketplace/1 Wire shapes, `MarketplaceRef`). `trust_channel` is required
// and never defaulted server-side — see packs.Ref.
type installRefBody struct {
	Source       string `json:"source"`
	PackID       string `json:"pack_id"`
	TrustChannel string `json:"trust_channel"`
	Version      string `json:"version"`
}

// installRefMediaType is the request content type that says "this body is a
// marketplace reference, not an artifact". Any other content type — including
// none — is read as artifact bytes, which is the pre-existing behaviour of this
// endpoint and stays the default.
const installRefMediaType = "application/json"

// wantsMarketplaceRef reports whether the request body is a marketplace
// reference rather than raw artifact bytes.
func wantsMarketplaceRef(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mt, installRefMediaType)
}

// installPackFromRef is the marketplace half of POST /api/v1/packs: the body
// names a pack in a configured registry, and the resolver fetches, verifies and
// installs it through the SAME pipeline a directly-uploaded artifact takes.
//
// It runs inside the same Idempotency-Key guard (the reference bytes are the
// replay-vs-reuse content hash) and answers with the same install summary, so a
// client cannot tell from the response shape which route installed the pack —
// only the install record can, which is the point (MKT-094a).
func (srv *server) installPackFromRef(w http.ResponseWriter, r *http.Request, body []byte) {
	var in installRefBody
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		srv.writeInstallError(w, r, &packs.ArtifactError{
			Code:    "MARKETPLACE_REF_INVALID",
			Message: "The request body is not a marketplace reference object {source?, pack_id, trust_channel, version?}.",
		})
		return
	}

	res, err := srv.installer.InstallRef(r.Context(), packs.Ref{
		Source:       in.Source,
		PackID:       in.PackID,
		TrustChannel: in.TrustChannel,
		Version:      in.Version,
	})
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

// ---- update check -----------------------------------------------------------

// packUpdateWire is one update check's outcome. `action` is the discriminant:
//
//   - `unchanged` — the pinned channel still points at the installed version.
//     Nothing was written, so `pages`/`collections`/`locales` are absent.
//   - `updated`   — the pointer named a different version and it installed.
//   - `reverted`  — the applied version had been yanked, and the most recent
//     still-resolvable version this install had previously applied was
//     reinstalled in its place (marketplace/1 MKT-093).
//
// `from_version`/`to_version` are equal exactly when `action` is `unchanged`.
type packUpdateWire struct {
	Action      string   `json:"action"`
	ID          string   `json:"id"`
	FromVersion string   `json:"from_version"`
	ToVersion   string   `json:"to_version"`
	Pages       []string `json:"pages,omitempty"`
	Collections []string `json:"collections,omitempty"`
	Locales     []string `json:"locales,omitempty"`
}

// updatePack runs one update check against an installed pack's own pinned
// trust channel (marketplace/1 MKT-090) and, if the applied version has been
// yanked, reverts it to its last known good version (MKT-093).
//
// Nothing about HOW the pack is re-resolved comes from the request: the source
// and trust channel are read off the install-record pin (MKT-094). That is what
// keeps a host from silently choosing a provenance tier (MKT-060a(b)) on an
// operator's behalf — and why a pack installed directly, with no channel pinned,
// is refused here rather than defaulted onto one (MKT-094a).
//
// It is a mutating POST outside plain resource creation, so it honors
// Idempotency-Key through the same srv.idempotent wrapper install and the
// automations `run` POST use. The body is empty and is the replay-vs-reuse
// content hash, so a retry with the same key replays the original outcome rather
// than running a second check.
func (srv *server) updatePack(w http.ResponseWriter, r *http.Request) {
	if !srv.packLifecycleWritable(w, r) {
		return
	}
	// The operation takes no body and is fully determined by its path, so the
	// Idempotency-Key replay-vs-reuse hash is over an EMPTY body rather than
	// over whatever a client happened to send: two update checks under one key
	// are the same operation, and there is no request content that could make
	// them differ. Whatever arrives is drained (bounded) rather than left
	// unread.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
	srv.idempotent(w, r, nil, func(w http.ResponseWriter) { srv.updatePackExec(w, r) })
}

func (srv *server) updatePackExec(w http.ResponseWriter, r *http.Request) {
	packID := packIDFromPath(r)
	out, err := srv.installer.Update(r.Context(), packID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			srv.packNotFound(w, r)
			return
		}
		srv.writeInstallError(w, r, err)
		return
	}
	wire := packUpdateWire{
		Action:      string(out.Action),
		ID:          packID,
		FromVersion: out.FromVersion,
		ToVersion:   out.ToVersion,
	}
	if out.Action != packs.UpdateUnchanged {
		wire.Pages = out.Result.Pages
		wire.Collections = out.Result.Collections
		wire.Locales = out.Result.Locales
	}
	writeJSONValue(w, http.StatusOK, wire)
}

// ---- install history --------------------------------------------------------

// packInstallResourceType is the api/1 resource-type tag the install-history
// list's keyset cursor is bound to.
const packInstallResourceType = "packinstall"

// packInstallCursorPrefix / packInstallCursorSep mirror the pack-data list's
// composite cursor: the keyset position (record id) IS a ULID, but the cursor is
// additionally bound to the pack whose history it pages, so a cursor minted by
// one pack's history is refused under another's (API-033/035) rather than paged
// from as an arbitrary position.
const (
	packInstallCursorPrefix = packInstallResourceType + "_"
	packInstallCursorSep    = "\x1f"
)

func encodePackInstallCursorPos(packID, recordID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(packID + packInstallCursorSep + recordID))
}

func decodePackInstallCursor(cursor, packID string) (string, *apihttp.PageParamError) {
	rest, ok := strings.CutPrefix(cursor, packInstallCursorPrefix)
	if !ok {
		return "", packCursorInvalid(cursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(raw) == 0 {
		return "", packCursorInvalid(cursor)
	}
	parts := strings.SplitN(string(raw), packInstallCursorSep, 2)
	if len(parts) != 2 || parts[0] != packID || !ulid.Valid(parts[1]) {
		return "", packCursorInvalid(cursor)
	}
	return parts[1], nil
}

// packInstallWire is one install record as served (marketplace/1 MKT-094 +
// MKT-094a). `trust_channel` and `artifact_digest` are nullable because a direct,
// non-registry install genuinely has neither: no channel pointer stands behind it
// to auto-track (MKT-090) and no index entry named it. Serving them as `null`
// rather than "" keeps that distinction legible to a client instead of making an
// unchannelled install look like one pinned to the empty channel.
type packInstallWire struct {
	ID              string  `json:"id"`
	PackID          string  `json:"pack_id"`
	ResolvedVersion string  `json:"resolved_version"`
	TrustChannel    *string `json:"trust_channel"`
	Source          string  `json:"source"`
	StaleSource     bool    `json:"stale_source"`
	ContentDigest   string  `json:"content_digest"`
	KeyID           string  `json:"key_id"`
	ArtifactDigest  *string `json:"artifact_digest"`
	InstalledAt     int64   `json:"installed_at"`
}

func packInstallWireOf(rec store.PackInstallRecord) packInstallWire {
	w := packInstallWire{
		ID:              rec.RecordID,
		PackID:          rec.PackID,
		ResolvedVersion: rec.ResolvedVersion,
		Source:          rec.Source,
		StaleSource:     rec.StaleSource,
		ContentDigest:   rec.ContentDigest,
		KeyID:           rec.KeyID,
		InstalledAt:     rec.InstalledAt,
	}
	if rec.TrustChannel != "" {
		ch := rec.TrustChannel
		w.TrustChannel = &ch
	}
	if rec.ArtifactDigest != "" {
		d := rec.ArtifactDigest
		w.ArtifactDigest = &d
	}
	return w
}

// listPackInstalls serves a pack's install-record history, oldest first — the
// append-only record set MKT-094b requires, whose LAST entry is MKT-094's current
// pin. It is the surface that answers "which key vouched for what is running,
// and what ran before it": the signature envelope itself is dropped at install
// and never persisted (MKT-009a), so these rows are the only place that answer
// exists.
//
// A pack that is not installed is a 404, exactly as its pages and messages are:
// its records went with it on uninstall (MKT-094b). The per-(pack, channel)
// anti-rollback high-water mark deliberately did not, but that is verifier state
// rather than pack state and is not served here.
func (srv *server) listPackInstalls(w http.ResponseWriter, r *http.Request) {
	packID := packIDFromPath(r)

	if _, found, err := srv.store.GetPack(r.Context(), packID); err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	} else if !found {
		srv.packNotFound(w, r)
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
		lastID, cerr := decodePackInstallCursor(cursor, packID)
		if cerr != nil {
			srv.packProblem(w, r, cerr.Status, cerr.Code, cerr.Title, cerr.Detail)
			return
		}
		afterID = lastID
	}

	records, err := srv.store.ListPackInstalls(r.Context(), packID)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred.")
		return
	}

	window := make([]packInstallWire, 0, len(records))
	for _, rec := range records {
		if afterID != "" && rec.RecordID <= afterID {
			continue
		}
		window = append(window, packInstallWireOf(rec))
	}
	page := apihttp.Page(packInstallResourceType, window, limit, func(pw packInstallWire) string {
		return encodePackInstallCursorPos(packID, pw.ID)
	})
	writeJSONValue(w, http.StatusOK, page)
}
