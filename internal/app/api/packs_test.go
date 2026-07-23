package api_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// ---- zip + manifest fixtures ----------------------------------------------

func packZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func packManifest() map[string]any {
	return map[string]any{
		"id":           "acme/menu-board",
		"version":      "1.0.0",
		"displayName":  "msg:pack.displayName",
		"compat":       map[string]any{"ctx": ">=1.0 <2.0", "renderer": []any{"list-detail", "settings-form"}},
		"capabilities": []any{},
		"egress":       []any{},
		"resources":    map[string]any{"memory": 64, "cpuWeight": 100, "storageQuota": 16, "maxScheduledTimers": 0},
		"dataModel": map[string]any{"version": 1, "collections": []any{
			map[string]any{"name": "menu_items", "fields": []any{
				map[string]any{"name": "name", "type": "string", "role": "title", "searchable": true},
			}},
		}},
		"retention": map[string]any{},
		"ui": map[string]any{"pages": []any{
			map[string]any{"path": "menu-items", "pageType": "list-detail", "titleMsg": "msg:page.menuItems.title"},
			map[string]any{"path": "settings", "pageType": "settings-form", "titleMsg": "msg:page.settings.title"},
		}},
		"actions": []any{},
	}
}

const (
	apiMenuDoc     = `{"pageType":"list-detail","list":{"source":"menu_items","display":{"type":"table"}}}`
	apiSettingsDoc = `{"pageType":"settings-form","source":"settings","sections":[],"actions":[]}`
	apiEnCatalog   = `{"page.menuItems.title":"Menu Items"}`
)

func packBundle(t *testing.T, m map[string]any) []byte {
	t.Helper()
	return packZip(t, map[string]string{
		"manifest.json":      string(mustJSON(t, m)),
		"ui/menu-items.json": apiMenuDoc,
		"ui/settings.json":   apiSettingsDoc,
		"messages/en.json":   apiEnCatalog,
	})
}

// installBase POSTs the base valid pack and asserts a 201, returning the decoded
// install response.
func (e *testEnv) installBase(t *testing.T) map[string]any {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", packBundle(t, packManifest()), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install status = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode install response: %v (%s)", err, raw)
	}
	return out
}

// errorsHasFieldCode asserts the Problem's errors[] extension carries a violation
// at field with code.
func errorsHasFieldCode(t *testing.T, p map[string]any, field, code string) {
	t.Helper()
	arr, ok := p["errors"].([]any)
	if !ok {
		t.Fatalf("problem has no errors[] extension: %+v", p)
	}
	for _, e := range arr {
		m, _ := e.(map[string]any)
		if m["field"] == field {
			if m["code"] != code {
				t.Fatalf("errors[] at %s has code %v, want %s", field, m["code"], code)
			}
			return
		}
	}
	t.Fatalf("no errors[] entry at field %q: %+v", field, arr)
}

// ---- tests ----------------------------------------------------------------

// TestPackInstall201: a valid pack installs — 201, Location, and a summary body.
func TestPackInstall201(t *testing.T) {
	e := newEnv(t)
	out := e.installBase(t)
	if out["id"] != "acme/menu-board" || out["version"] != "1.0.0" {
		t.Fatalf("install body = %+v; want acme/menu-board 1.0.0", out)
	}
	pages, _ := out["pages"].([]any)
	if len(pages) != 2 {
		t.Fatalf("install body pages = %v; want 2", out["pages"])
	}
	cols, _ := out["collections"].([]any)
	if len(cols) != 1 || cols[0] != "menu_items" {
		t.Fatalf("install body collections = %v; want [menu_items]", out["collections"])
	}
}

// TestPackReinstall200: reinstalling an existing pack at a non-regressing version
// returns 200 (updated in place), not 201.
func TestPackReinstall200(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	m := packManifest()
	m["version"] = "1.1.0"
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", packBundle(t, m), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reinstall status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
}

// TestPackInstallManifestRefused422: an unknown-capability manifest is refused
// 422 / MANIFEST_INVALID carrying the typed per-field error.
func TestPackInstallManifestRefused422(t *testing.T) {
	e := newEnv(t)
	m := packManifest()
	m["capabilities"] = []any{map[string]any{"capability": "world.domination", "scope": "*", "reason": "msg:x"}}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", packBundle(t, m), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "MANIFEST_INVALID")
	errorsHasFieldCode(t, p, "capabilities[0].capability", "UNKNOWN_CAPABILITY")
}

// TestPackReinstallVersionRegression422: MAN-053 over HTTP — a reinstall with a
// lower dataModel.version is refused 422 with the DATAMODEL_VERSION_REGRESSION
// field error.
func TestPackReinstallVersionRegression422(t *testing.T) {
	e := newEnv(t)
	m := packManifest()
	m["dataModel"] = map[string]any{"version": 5, "collections": []any{
		map[string]any{"name": "menu_items", "fields": []any{map[string]any{"name": "name", "type": "string", "role": "title"}}},
	}}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", packBundle(t, m), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install v5 status = %d, want 201 (%s)", resp.StatusCode, raw)
	}
	// Now reinstall at the lower base dataModel.version 1.
	resp, raw = e.do(t, http.MethodPost, "/api/v1/packs", packBundle(t, packManifest()), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("regression status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "MANIFEST_INVALID")
	errorsHasFieldCode(t, p, "dataModel.version", "DATAMODEL_VERSION_REGRESSION")
}

// TestPackInstallMalformedArtifact422: non-zip bytes are a 422, never a panic/500.
func TestPackInstallMalformedArtifact422(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", []byte("not a zip at all"), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "PACK_ARTIFACT_INVALID")
}

// TestPackInstallZipSlip422: a ../ traversal entry is a 422.
func TestPackInstallZipSlip422(t *testing.T) {
	e := newEnv(t)
	z := packZip(t, map[string]string{
		"manifest.json":    string(mustJSON(t, packManifest())),
		"../escape.json":   "x",
		"messages/en.json": apiEnCatalog,
	})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/packs", z, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "PACK_ARTIFACT_UNSAFE_ENTRY")
}

// TestPackGet: after install, GET the pack by its two-segment id returns 200, an
// ETag, and the manifest body.
func TestPackGet(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if et := resp.Header.Get("ETag"); et != `"1"` {
		t.Fatalf("ETag = %q, want \"1\"", et)
	}
	var body struct {
		ID       string          `json:"id"`
		Revision int64           `json:"revision"`
		Manifest json.RawMessage `json:"manifest"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if body.ID != "acme/menu-board" || body.Revision != 1 || len(body.Manifest) == 0 {
		t.Fatalf("get body = %+v", body)
	}
}

// TestPackGetNotFound: an unknown pack is 404.
func TestPackGetNotFound(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/nobody/here", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
}

// TestPackList: the list endpoint returns installed packs in an {items, cursor}
// envelope.
func TestPackList(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var page struct {
		Items  []map[string]any `json:"items"`
		Cursor *string          `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode list: %v (%s)", err, raw)
	}
	if len(page.Items) != 1 || page.Items[0]["id"] != "acme/menu-board" {
		t.Fatalf("list items = %+v; want [acme/menu-board]", page.Items)
	}
	if page.Cursor != nil {
		t.Fatalf("cursor = %v, want null (single page)", *page.Cursor)
	}
}

// TestPackGetPageDoc: the page-document endpoint serves the stored ui-schema/1
// doc verbatim, keyed by its ui.pages[].path.
func TestPackGetPageDoc(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/pages/menu-items", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if string(raw) != apiMenuDoc {
		t.Fatalf("page doc = %s; want %s", raw, apiMenuDoc)
	}
}

// TestPackGetPageDocNotFound: an undeclared page path is 404.
func TestPackGetPageDocNotFound(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/pages/nope", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, raw)
	}
}

// TestPackGetMessages: the locale-catalog endpoint serves the stored catalog.
func TestPackGetMessages(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	resp, raw := e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/messages/en", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("messages status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if string(raw) != apiEnCatalog {
		t.Fatalf("catalog = %s; want %s", raw, apiEnCatalog)
	}
}

// TestPackDeleteRequiresIfMatch: DELETE without If-Match is 428 (no unconditional
// uninstall).
func TestPackDeleteRequiresIfMatch(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	resp, raw := e.do(t, http.MethodDelete, "/api/v1/packs/acme/menu-board", nil, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "IF_MATCH_REQUIRED")
}

// TestPackDeleteStaleIfMatch: a wrong If-Match is 412 / REVISION_CONFLICT.
func TestPackDeleteStaleIfMatch(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	resp, raw := e.do(t, http.MethodDelete, "/api/v1/packs/acme/menu-board", nil, map[string]string{"If-Match": `"99"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "REVISION_CONFLICT")
}

// TestPackDeleteUninstalls: a correct If-Match uninstalls the pack — 204, then the
// pack and its files are gone (404).
func TestPackDeleteUninstalls(t *testing.T) {
	e := newEnv(t)
	e.installBase(t)
	resp, raw := e.do(t, http.MethodDelete, "/api/v1/packs/acme/menu-board", nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (%s)", resp.StatusCode, raw)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/packs/acme/menu-board/pages/menu-items", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("page after delete = %d, want 404 (files must be gone)", resp.StatusCode)
	}
}
