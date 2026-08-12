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
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/datamodel"
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

// reindex rewrites index.json in place, so a test can publish a new version (and
// move the channel pointer with it) AFTER the server was built — the Source
// keeps pointing at the same directory and the resolver re-reads the document on
// every resolution, which is what makes an update check observable over HTTP.
func (r *mktRegistry) reindex(t *testing.T) {
	t.Helper()
	r.writeIndex()
}

func (r *mktRegistry) writeIndex() {
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

// ---- update check + required-pack floor, over the real HTTP surface ---------

// packUpdateResponse mirrors what POST .../update serves.
type packUpdateResponse struct {
	Action      string `json:"action"`
	ID          string `json:"id"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
}

// TestUpdatePackOverHTTP: the update check driven end to end. Nothing about the
// re-resolution is in the request — the trust channel and registry source come
// off the install-record pin (marketplace/1 MKT-094/MKT-090) — so this also
// proves the pin is load-bearing rather than decorative.
func TestUpdatePackOverHTTP(t *testing.T) {
	reg := newMktRegistry(t)
	reg.publish("acme/menu-board", "1.0.0",
		signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0"), nil)
	e := newEnvWithOptions(t, reg.option())

	ref := mustJSON(t, map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community"})
	if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", ref, jsonHeaders); resp.StatusCode != http.StatusCreated {
		t.Fatalf("install status = %d, want 201 (%s)", resp.StatusCode, raw)
	}

	// A no-op check first: the pointer has not moved, so nothing is written and
	// the history stays at one record.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs/acme/menu-board/update", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var out packUpdateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode update: %v (%s)", err, raw)
	}
	if out.Action != "unchanged" || out.ToVersion != "1.0.0" {
		t.Fatalf("update = %+v, want unchanged at 1.0.0", out)
	}
	if page := packInstallHistory(t, e, "acme/menu-board"); len(page.Items) != 1 {
		t.Fatalf("a no-op update check appended a record: %d", len(page.Items))
	}

	// Publish 2.0.0 and move the pointer; the next check applies it in place.
	m := packManifest()
	m["version"] = "2.0.0"
	reg.publish("acme/menu-board", "2.0.0",
		signPack(t, packBundle(t, m), "acme/menu-board", "2.0.0"), nil)
	reg.reindex(t)

	resp, raw = e.do(t, http.MethodPost, "/api/v1/packs/acme/menu-board/update", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode update: %v (%s)", err, raw)
	}
	if out.Action != "updated" || out.FromVersion != "1.0.0" || out.ToVersion != "2.0.0" {
		t.Fatalf("update = %+v, want updated 1.0.0 -> 2.0.0", out)
	}

	// Checked through a different route than the one that claimed it: the pack
	// really is at 2.0.0 and the history really did grow.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get pack status = %d (%s)", resp.StatusCode, raw)
	}
	var got struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Version != "2.0.0" {
		t.Fatalf("installed version = %q (err=%v), want 2.0.0", got.Version, err)
	}
	page := packInstallHistory(t, e, "acme/menu-board")
	if len(page.Items) != 2 || page.Items[1].ResolvedVersion != "2.0.0" {
		t.Fatalf("install history = %d record(s), newest %+v", len(page.Items), page.Items[len(page.Items)-1])
	}
}

// TestUpdatePackOfAnUninstalledPackIs404: there is nothing to re-resolve, and
// no reference is invented for it.
func TestUpdatePackOfAnUninstalledPackIs404(t *testing.T) {
	reg := newMktRegistry(t)
	e := newEnvWithOptions(t, reg.option())
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs/acme/menu-board/update", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update of an uninstalled pack status = %d, want 404 (%s)", resp.StatusCode, raw)
	}
}

// TestUpdatePackRefusesADirectInstallOverHTTP: MKT-094a — a pack installed from
// raw artifact bytes pins no trust channel, is not channel auto-tracked, and the
// host does not default one for it.
func TestUpdatePackRefusesADirectInstallOverHTTP(t *testing.T) {
	reg := newMktRegistry(t)
	e := newEnvWithOptions(t, reg.option())

	art := signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0")
	if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", art, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("direct install status = %d, want 201 (%s)", resp.StatusCode, raw)
	}

	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs/acme/menu-board/update", nil, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("update of a direct install status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	if codes := problemCodes(t, raw); len(codes) != 1 || codes[0] != "TRUST_CHANNEL_UNKNOWN" {
		t.Fatalf("problem codes = %v, want [TRUST_CHANNEL_UNKNOWN]", codes)
	}
}

// TestUninstallOfARequiredPackIsRefusedOverHTTP: MKT-093b(i). The decision is
// made inside the removal transaction; this checks the surface renders it as the
// same 422 / VALIDATION_FAILED + errors[] discriminant every other pack refusal
// uses, and that the pack is genuinely still there afterwards.
func TestUninstallOfARequiredPackIsRefusedOverHTTP(t *testing.T) {
	reg := newMktRegistry(t)
	reg.publish("acme/menu-board", "1.0.0",
		signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0"), nil)
	roster, err := packs.NewRoster(map[string]string{"acme/menu-board": "1.0.0"})
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	e := newEnvWithOptions(t, reg.option(), api.WithRequiredPacks(roster))

	ref := mustJSON(t, map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community"})
	if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", ref, jsonHeaders); resp.StatusCode != http.StatusCreated {
		t.Fatalf("install status = %d, want 201 (%s)", resp.StatusCode, raw)
	}

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/packs/acme/menu-board", nil,
		map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("uninstall of a required pack status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	if codes := problemCodes(t, raw); len(codes) != 1 || codes[0] != "REQUIRED_PACK_FLOOR" {
		t.Fatalf("problem codes = %v, want [REQUIRED_PACK_FLOOR]", codes)
	}
	if resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the refused uninstall removed the pack: get status = %d (%s)", resp.StatusCode, raw)
	}
	if page := packInstallHistory(t, e, "acme/menu-board"); len(page.Items) != 1 {
		t.Fatalf("the refused uninstall removed the install records: %d left", len(page.Items))
	}
}

// TestPackLifecycleRefusesBelowAdminAtTheOrgNode is the authorization claim for
// the pack lifecycle, and the roles chosen are what make it mean something.
//
// `operator` clears auth.CanWrite's floor, which is what every OTHER mutating
// route on this surface authorizes against — so before this gate existed, an
// operator (indeed any authenticated principal at all) could install a pack, and
// a pack install grants the capabilities its manifest requests, workspace-wide.
// That is privilege acquisition, not an ordinary write.
//
// The site-scoped ADMIN is the sharper half: an admin binding at a leaf clears
// the role floor but not at the workspace org node, so this pins that the gate
// is asking at the right NODE rather than merely asking for the right role.
func TestPackLifecycleRefusesBelowAdminAtTheOrgNode(t *testing.T) {
	e := newEnv(t)
	orgID := e.createNode(t, orgNode("Fixture Org"))
	tz, lat, long := "America/Chicago", 41.8781, -87.6298
	siteID := e.createNode(t, datamodel.ScopeNode{
		Kind: "site", Name: "Fixture Site", ParentID: &orgID,
		TZ: &tz, Lat: &lat, Long: &long,
	})

	cases := []struct {
		name string
		cfg  authtest.Config
	}{
		{"operator at the org node", authtest.Config{Role: auth.RoleOperator, ScopeNode: orgID}},
		{"viewer at the org node", authtest.Config{Role: auth.RoleViewer, ScopeNode: orgID}},
		{"admin at a SITE, not the org", authtest.Config{Role: auth.RoleAdmin, ScopeNode: siteID}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			who, err := e.auth.AddPrincipal(tc.cfg)
			if err != nil {
				t.Fatalf("AddPrincipal: %v", err)
			}
			for _, rt := range []struct{ method, path string }{
				{http.MethodPost, "/api/v1/packs"},
				{http.MethodDelete, "/api/v1/packs/acme/menu-board"},
				{http.MethodPost, "/api/v1/packs/acme/menu-board/update"},
			} {
				resp, raw := e.doAsPrincipal(t, who, rt.method, rt.path, nil)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("%s %s as %s: status = %d, want 403; body %s", rt.method, rt.path, tc.name, resp.StatusCode, raw)
				}
				assertProblem(t, resp, raw, "FORBIDDEN")
			}
		})
	}

	// Positive control: an admin AT THE ORG NODE is admitted past the gate. It
	// fails downstream on the empty body rather than on authorization, which is
	// what proves the refusals above are the gate and not a route that refuses
	// everyone.
	admin, err := e.auth.AddPrincipal(authtest.Config{Role: auth.RoleAdmin, ScopeNode: orgID})
	if err != nil {
		t.Fatalf("AddPrincipal(admin at org): %v", err)
	}
	resp, raw := e.doAsPrincipal(t, admin, http.MethodPost, "/api/v1/packs", []byte("not-a-zip"))
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("an admin at the org node was refused by the lifecycle gate; body %s", raw)
	}
}

// ---- the roster a deployment actually authors ------------------------------

// requiredPacksRosterOnDisk writes a required-pack roster document (MKT-093a)
// the way a deployment provisions one — 0600, in a 0700 directory — and resolves
// it with packs.LoadRoster, which is the exact call the shipped feeder makes.
//
// Going through the FILE rather than through packs.NewRoster is the point. A
// test that hands the api a roster built in memory proves the store enforces
// what it is given; it proves nothing about whether anything a deployment can
// author ever becomes that value. Before the loader existed, every floor test in
// this repo was of the first kind, and the floor enforced nothing on a real box.
func requiredPacksRosterOnDisk(t *testing.T, body string) packs.Roster {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "required-packs.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write roster: %v", err)
	}
	r, err := packs.LoadRoster(path)
	if err != nil {
		t.Fatalf("packs.LoadRoster(%s): %v", path, err)
	}
	return r
}

// TestARosterAuthoredOnDiskRefusesTheUninstallOverHTTP is MKT-093b(i) driven
// from the artifact an operator writes: a roster document on disk, resolved by
// the loader, wired through the option the feeder passes, refusing a real
// DELETE. Every link the shipped binary uses is in this chain except main's own
// call sites, which cmd/waiveo-feeder's tests pin.
func TestARosterAuthoredOnDiskRefusesTheUninstallOverHTTP(t *testing.T) {
	roster := requiredPacksRosterOnDisk(t,
		`{"format":"required-packs/1","required":[{"pack_id":"acme/menu-board","floor_version":"1.0.0"}]}`)

	reg := newMktRegistry(t)
	reg.publish("acme/menu-board", "1.0.0",
		signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0"), nil)
	e := newEnvWithOptions(t, reg.option(), api.WithRequiredPacks(roster))

	ref := mustJSON(t, map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community"})
	if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", ref, jsonHeaders); resp.StatusCode != http.StatusCreated {
		t.Fatalf("install status = %d, want 201 (%s)", resp.StatusCode, raw)
	}

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/packs/acme/menu-board", nil,
		map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("uninstall of a roster-declared pack status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	if codes := problemCodes(t, raw); len(codes) != 1 || codes[0] != "REQUIRED_PACK_FLOOR" {
		t.Fatalf("problem codes = %v, want [REQUIRED_PACK_FLOOR]", codes)
	}
	if resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the refused uninstall removed the pack: get status = %d (%s)", resp.StatusCode, raw)
	}
}

// TestARosterAuthoredOnDiskRefusesABelowFloorInstallOverHTTP is the other half
// of MKT-093b from the same authored document: an install that would leave a
// required pack below its declared floor is refused, on the resolution path, at
// the same 422 discriminant.
func TestARosterAuthoredOnDiskRefusesABelowFloorInstallOverHTTP(t *testing.T) {
	roster := requiredPacksRosterOnDisk(t,
		`{"format":"required-packs/1","required":[{"pack_id":"acme/menu-board","floor_version":"2.0.0"}]}`)

	reg := newMktRegistry(t)
	reg.publish("acme/menu-board", "1.0.0",
		signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0"), nil)
	e := newEnvWithOptions(t, reg.option(), api.WithRequiredPacks(roster))

	ref := mustJSON(t, map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community"})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", ref, jsonHeaders)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("below-floor install status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	if codes := problemCodes(t, raw); len(codes) != 1 || codes[0] != "REQUIRED_PACK_FLOOR" {
		t.Fatalf("problem codes = %v, want [REQUIRED_PACK_FLOOR]", codes)
	}
	if resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the refused install landed the pack anyway: get status = %d (%s)", resp.StatusCode, raw)
	}
}

// packUpdateAvailabilityResponse mirrors what GET .../update serves (MKT-095).
type packUpdateAvailabilityResponse struct {
	Action       string `json:"action"`
	ID           string `json:"id"`
	FromVersion  string `json:"from_version"`
	ToVersion    string `json:"to_version"`
	TrustChannel string `json:"trust_channel"`
	Source       string `json:"source"`
}

// TestPackUpdateAvailabilityOverHTTP: the report, driven end to end.
//
// The unit tests in internal/app/packs already prove the report does not
// mutate. What only this level can prove is that the GET is REACHABLE — a
// handler registered under the wrong method or path is invisible to a package
// test and perfectly visible to an operator — and that GET and POST on the one
// path stay distinct: the report must not be served by the mutating handler,
// and the check must still run when asked.
func TestPackUpdateAvailabilityOverHTTP(t *testing.T) {
	reg := newMktRegistry(t)
	reg.publish("acme/menu-board", "1.0.0",
		signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0"), nil)
	e := newEnvWithOptions(t, reg.option())

	ref := mustJSON(t, map[string]any{"pack_id": "acme/menu-board", "trust_channel": "community"})
	if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", ref, jsonHeaders); resp.StatusCode != http.StatusCreated {
		t.Fatalf("install status = %d, want 201 (%s)", resp.StatusCode, raw)
	}

	// Nothing waiting yet.
	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/update", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("availability status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var out packUpdateAvailabilityResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode availability: %v (%s)", err, raw)
	}
	if out.Action != "unchanged" || out.ToVersion != "1.0.0" {
		t.Fatalf("availability = %+v, want unchanged at 1.0.0", out)
	}
	// The pin the answer came through — the half a client needs to act on it.
	if out.TrustChannel != "community" {
		t.Errorf("trust_channel = %q, want community", out.TrustChannel)
	}

	// Move the pointer. The report must now name 2.0.0 and STILL install nothing.
	m := packManifest()
	m["version"] = "2.0.0"
	reg.publish("acme/menu-board", "2.0.0", signPack(t, packBundle(t, m), "acme/menu-board", "2.0.0"), nil)
	// Publishing a version is not the same as moving the channel pointer, and
	// the report reads the POINTER (MKT-090) — without this the honest answer
	// is still "unchanged", which is what this test first asserted against.
	reg.reindex(t)

	_, raw = e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/update", nil, nil)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode availability: %v (%s)", err, raw)
	}
	if out.Action != "updated" || out.FromVersion != "1.0.0" || out.ToVersion != "2.0.0" {
		t.Fatalf("availability = %+v, want updated 1.0.0 -> 2.0.0", out)
	}
	if page := packInstallHistory(t, e, "acme/menu-board"); len(page.Items) != 1 {
		t.Fatalf("a REPORT appended an install record (%d) — MKT-094b makes a record evidence an install was applied", len(page.Items))
	}

	// And the POST on the same path still acts, so the two have not been merged.
	if resp, raw := e.do(t, http.MethodPost, "/api/v1/packs/acme/menu-board/update", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if page := packInstallHistory(t, e, "acme/menu-board"); len(page.Items) != 2 {
		t.Fatalf("install records = %d after the POST, want 2 — the check must still apply", len(page.Items))
	}
}

// TestBrowsePackCatalogOverHTTP: the discovery half, end to end.
//
// The package tests already cover what is withheld and why. What only this
// level proves is that the route is REACHABLE and that its literal `catalog`
// segment is not swallowed by the two-segment `{publisher}/{name}` pattern
// registered beside it — a collision no package test can see and every
// operator would.
func TestBrowsePackCatalogOverHTTP(t *testing.T) {
	reg := newMktRegistry(t)
	reg.publish("acme/menu-board", "1.0.0",
		signPack(t, packBundle(t, packManifest()), "acme/menu-board", "1.0.0"), nil)
	e := newEnvWithOptions(t, reg.option())

	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/catalog", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var out struct {
		Sources []struct {
			Source      string `json:"source"`
			Unavailable string `json:"unavailable"`
			Entries     []struct {
				ID      string `json:"id"`
				Version string `json:"version"`
				Source  string `json:"source"`
			} `json:"entries"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode catalog: %v (%s)", err, raw)
	}
	if len(out.Sources) != 1 {
		t.Fatalf("sources = %d, want 1 (%s)", len(out.Sources), raw)
	}
	if out.Sources[0].Unavailable != "" {
		t.Fatalf("source unavailable: %s", out.Sources[0].Unavailable)
	}
	if len(out.Sources[0].Entries) != 1 || out.Sources[0].Entries[0].ID != "acme/menu-board" {
		t.Fatalf("entries = %+v, want the published pack", out.Sources[0].Entries)
	}
	// Grouped by source and attributed, never merged into one flat catalog.
	if out.Sources[0].Entries[0].Source == "" {
		t.Error("entry names no source (MKT-061: order is not a trust decision)")
	}

	// The sibling two-segment route still resolves a real pack, so `catalog`
	// took nothing from it.
	if resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get of an uninstalled pack = %d, want 404 (%s)", resp.StatusCode, raw)
	}
}
