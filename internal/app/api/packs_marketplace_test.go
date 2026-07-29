package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// This file drives the marketplace install over the REAL HTTP surface: nothing
// here calls the resolver or the installer directly. The bug class it exists
// against is a surface that accepts work it never performs — a route that takes
// a reference, answers 201, and installs nothing (or installs without the
// verification the artifact-upload route applies).

// mktNow is the resolution-time clock the fixture registry's holds are
// evaluated against.
const mktNow = int64(1_800_000_000_000)

// mktRegistry writes a local file:// registry into a temp dir and returns the
// api.Option wiring it, plus the artifact bytes it published.
type mktRegistry struct {
	t       *testing.T
	dir     string
	id      string
	entries []map[string]any
	ptrs    map[string]map[string]string
}

func newMktRegistry(t *testing.T) *mktRegistry {
	t.Helper()
	return &mktRegistry{t: t, dir: t.TempDir(), id: "fixture-registry", ptrs: map[string]map[string]string{}}
}

func (r *mktRegistry) publish(artifactID, version string, artifact []byte, extra map[string]any) {
	r.t.Helper()
	name := filepath.Base(artifactID) + "-" + version + ".zip"
	if err := os.WriteFile(filepath.Join(r.dir, name), artifact, 0o644); err != nil {
		r.t.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256(artifact)
	e := map[string]any{
		"artifact_id":  artifactID,
		"kind":         "pack",
		"version":      version,
		"status":       "active",
		"digest":       "sha256:" + hex.EncodeToString(sum[:]),
		"size":         len(artifact),
		"download_url": "file:///" + name,
	}
	for k, v := range extra {
		e[k] = v
	}
	r.entries = append(r.entries, e)
	if r.ptrs[artifactID] == nil {
		r.ptrs[artifactID] = map[string]string{}
	}
	r.ptrs[artifactID]["community"] = version
}

func (r *mktRegistry) option(reserved ...string) api.Option {
	r.t.Helper()
	pointers := []map[string]any{}
	for packID, channels := range r.ptrs {
		pointers = append(pointers, map[string]any{"pack_id": packID, "channels": channels})
	}
	doc := map[string]any{"signed": map[string]any{
		"role": "targets", "format_version": "1.0", "channel": "marketplace/stable",
		"version": 1, "artifacts": r.entries, "channel_pointers": pointers,
	}}
	raw, err := json.Marshal(doc)
	if err != nil {
		r.t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "index.json"), raw, 0o644); err != nil {
		r.t.Fatalf("write index: %v", err)
	}
	return api.WithMarketplace(packs.NewMarket(
		func() int64 { return mktNow },
		packs.Source{
			ID:                 r.id,
			Channel:            "marketplace/stable",
			IndexURL:           "file:///index.json",
			Fetcher:            packs.FileFetcher{Root: r.dir},
			ReservedNamespaces: reserved,
			StaleSource:        true,
		},
	))
}

// jsonHeaders marks a request body as a marketplace reference rather than
// artifact bytes.
var jsonHeaders = map[string]string{"Content-Type": "application/json"}

// installRecordWire mirrors what GET .../installs serves.
type installRecordWire struct {
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

type installRecordPage struct {
	Items  []installRecordWire `json:"items"`
	Cursor *string             `json:"cursor"`
}

func packInstallHistory(t *testing.T, e *testEnv, packID string) installRecordPage {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/"+packID+"/installs", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("install history status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var page installRecordPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode install history: %v (%s)", err, raw)
	}
	return page
}

// errorsExtra pulls the errors[] discriminant out of a Problem body.
func problemCodes(t *testing.T, raw []byte) []string {
	t.Helper()
	var p struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, raw)
	}
	out := make([]string, 0, len(p.Errors))
	for _, e := range p.Errors {
		out = append(out, e.Code)
	}
	return out
}

// TestInstallByMarketplaceRefOverHTTP is the done bar driven end to end: POST a
// reference (never the bytes), get a real install, then read the install history
// back off a second, independent request.
func TestInstallByMarketplaceRefOverHTTP(t *testing.T) {
	reg := newMktRegistry(t)
	art := signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	e := newEnvWithOptions(t, reg.option())

	ref := mustJSON(t, map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community"})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", ref, jsonHeaders)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install-by-ref status = %d, want 201 (%s)", resp.StatusCode, raw)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/v1/packs/acme/menu-board" {
		t.Fatalf("Location = %q", loc)
	}

	// The pack genuinely landed — checked through a different route than the one
	// that claimed it did.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get pack status = %d, want 200 (%s)", resp.StatusCode, raw)
	}

	page := packInstallHistory(t, e, "acme/menu-board")
	if len(page.Items) != 1 {
		t.Fatalf("install history = %d records, want 1", len(page.Items))
	}
	rec := page.Items[0]
	if rec.ResolvedVersion != "1.0.0" || rec.Source != "fixture-registry" {
		t.Fatalf("record = %+v, want 1.0.0 from fixture-registry", rec)
	}
	if rec.TrustChannel == nil || *rec.TrustChannel != "community" {
		t.Fatalf("record trust_channel = %v, want community", rec.TrustChannel)
	}
	if rec.ArtifactDigest == nil || *rec.ArtifactDigest == "" {
		t.Fatal("a registry-mediated record must name the resolved entry's own digest (MKT-094a)")
	}
	if rec.ContentDigest == "" || rec.KeyID == "" {
		t.Fatalf("record provenance = %q / %q, want the verifying key and its signed content digest", rec.KeyID, rec.ContentDigest)
	}
	if !rec.StaleSource {
		t.Fatal("a file://-resolved install must be marked stale_source (MKT-063)")
	}
	if rec.InstalledAt == 0 || rec.ID == "" {
		t.Fatalf("record = %+v, want an id and an install time", rec)
	}
}

// TestDirectArtifactUploadAlsoRecordsOverHTTP: the artifact route is unchanged
// and now also answers the provenance question (MKT-094a's direct-path clause),
// with a null trust channel so nothing reads it as channel-tracked.
func TestDirectArtifactUploadAlsoRecordsOverHTTP(t *testing.T) {
	e := newEnv(t)
	art := signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0")
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", art, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install status = %d, want 201 (%s)", resp.StatusCode, raw)
	}

	page := packInstallHistory(t, e, "acme/menu-board")
	if len(page.Items) != 1 {
		t.Fatalf("install history = %d records, want 1", len(page.Items))
	}
	rec := page.Items[0]
	if rec.Source != "direct" {
		t.Fatalf("record source = %q, want direct", rec.Source)
	}
	if rec.TrustChannel != nil {
		t.Fatalf("record trust_channel = %v; a direct install pins none", *rec.TrustChannel)
	}
	if rec.ArtifactDigest != nil {
		t.Fatalf("record artifact_digest = %v; no index entry named this install", *rec.ArtifactDigest)
	}
	if rec.KeyID == "" || rec.ContentDigest == "" {
		t.Fatalf("record = %+v, want verified provenance on the direct path too", rec)
	}
}

// TestMarketplaceRefRefusalsSurfaceTheirCode: a refused resolution answers 422
// with the marketplace/1 code in errors[], the same shape every other pack
// refusal uses — and leaves nothing installed.
func TestMarketplaceRefRefusalsSurfaceTheirCode(t *testing.T) {
	reg := newMktRegistry(t)
	art := signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	e := newEnvWithOptions(t, reg.option())

	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"no trust channel", map[string]any{"pack_id": "acme/menu-board"}, "TRUST_CHANNEL_UNKNOWN"},
		{"unqualified id", map[string]any{"pack_id": "menu-board", "trust_channel": "community"}, "CROSS_PACK_REFERENCE_UNQUALIFIED"},
		{"unknown source", map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community", "source": "nope"}, "MARKETPLACE_REF_INVALID"},
		{"unknown pack", map[string]any{"pack_id": "acme/nothing", "trust_channel": "community"}, "MARKETPLACE_REF_UNRESOLVED"},
		{"bad pinned version", map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community", "version": "1.0"}, "MARKETPLACE_REF_INVALID"},
		// A client cannot dictate what the install record will say. The record's
		// provenance members come from the verification that admitted the
		// artifact and from nowhere else (MKT-094a), so a body trying to supply
		// them is refused outright rather than having them quietly ignored —
		// ignoring them would leave a caller believing they had been honoured.
		{"caller-supplied provenance", map[string]any{
			"pack_id": "acme/menu-board", "trust_channel": "community",
			"key_id": "ed25519:attacker", "content_digest": "sha256:00",
		}, "MARKETPLACE_REF_INVALID"},
		{"caller-supplied source sentinel", map[string]any{
			"pack_id": "acme/menu-board", "trust_channel": "community", "source": "direct",
		}, "MARKETPLACE_REF_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", mustJSON(t, tc.body), jsonHeaders)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", resp.StatusCode, raw)
			}
			codes := problemCodes(t, raw)
			if len(codes) != 1 || codes[0] != tc.code {
				t.Fatalf("errors[] codes = %v, want [%s] (%s)", codes, tc.code, raw)
			}
		})
	}

	// Nothing installed by any of the refusals.
	resp, _ := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a refused reference installed something: get pack = %d", resp.StatusCode)
	}
}

// TestInstallHistoryIsRemovedWithThePack: uninstall is destructive of
// pack-owned state (MKT-094b), and the history route 404s for a pack that is
// not installed exactly as its pages and messages do.
func TestInstallHistoryIsRemovedWithThePack(t *testing.T) {
	e := newEnv(t)
	art := signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0")
	if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", art, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("install status = %d (%s)", resp.StatusCode, raw)
	}
	if len(packInstallHistory(t, e, "acme/menu-board").Items) != 1 {
		t.Fatal("expected one install record before uninstall")
	}

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/packs/acme/menu-board",
		nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("uninstall status = %d, want 204 (%s)", resp.StatusCode, raw)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/installs", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("install history for an uninstalled pack = %d, want 404", resp.StatusCode)
	}
}

// TestInstallHistoryCursorIsScopedToItsPack: a cursor minted by one pack's
// history is refused under another's rather than paged from as an arbitrary
// keyset position (API-033/035).
func TestInstallHistoryCursorIsScopedToItsPack(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"acme/menu-board", "acme/other-board"} {
		m := packManifest()
		m["id"] = id
		art := signPack(t, packBundle(t, m), id, "1.0.0")
		if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", art, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("install %s = %d (%s)", id, resp.StatusCode, raw)
		}
		// A second install so a page of limit=1 yields a next cursor.
		if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", art, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("reinstall %s = %d (%s)", id, resp.StatusCode, raw)
		}
	}

	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/installs?limit=1", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history page status = %d (%s)", resp.StatusCode, raw)
	}
	var page installRecordPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Cursor == nil {
		t.Fatal("a limit=1 page over two records must carry a next cursor")
	}

	// The same cursor under a different pack's history is refused.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/packs/acme/other-board/installs?cursor="+*page.Cursor, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-pack cursor status = %d, want 400 (%s)", resp.StatusCode, raw)
	}

	// Under its own pack it pages correctly.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/installs?cursor="+*page.Cursor, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-pack cursor status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var next installRecordPage
	if err := json.Unmarshal(raw, &next); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(next.Items) != 1 || next.Items[0].ID <= page.Items[0].ID {
		t.Fatalf("second page = %+v, want the record strictly after the first", next.Items)
	}
}

// TestMarketplaceRefIsIdempotent: a retried reference under the same
// Idempotency-Key replays the original 201 rather than reinstalling (which
// would answer 200 and append a second record).
func TestMarketplaceRefIsIdempotent(t *testing.T) {
	reg := newMktRegistry(t)
	art := signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	e := newEnvWithOptions(t, reg.option())

	ref := mustJSON(t, map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community"})
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "mkt-ref-retry-1"}

	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", ref, headers)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first install status = %d (%s)", resp.StatusCode, raw)
	}
	resp, raw = e.do(t, http.MethodPost, "/api/v1/packs", ref, headers)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry status = %d, want the original 201 replayed (%s)", resp.StatusCode, raw)
	}
	if got := packInstallHistory(t, e, "acme/menu-board"); len(got.Items) != 1 {
		t.Fatalf("install records after a replayed retry = %d, want 1", len(got.Items))
	}
}

// TestMarketplaceRefWithoutAConfiguredRegistry: the artifact route still works;
// the reference route refuses rather than inventing a registry.
func TestMarketplaceRefWithoutAConfiguredRegistry(t *testing.T) {
	e := newEnv(t) // no WithMarketplace
	ref := mustJSON(t, map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community"})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", ref, jsonHeaders)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	if codes := problemCodes(t, raw); len(codes) != 1 || codes[0] != "MARKETPLACE_REF_UNRESOLVED" {
		t.Fatalf("errors[] codes = %v, want [MARKETPLACE_REF_UNRESOLVED]", codes)
	}
}
