package packs_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io/fs"
	"testing"
)

// zentry is one zip entry a test builds by hand: its name, body, and mode (0 =
// a regular 0644 file). A non-zero mode lets a test forge a symlink, a
// directory, or another irregular entry to exercise the reader's safety gates.
type zentry struct {
	name string
	body string
	mode fs.FileMode
}

// buildZip writes entries into an in-memory zip and returns its bytes. It sets
// each entry's mode verbatim (defaulting a zero-mode entry to a regular file) so
// a test can forge a symlink (fs.ModeSymlink), a directory (fs.ModeDir), or a
// traversal/absolute name and prove the reader refuses it.
func buildZip(t *testing.T, entries ...zentry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("zip create %q: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("zip write %q: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// filesZip builds a zip of plain regular files from a name→body map.
func filesZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	entries := make([]zentry, 0, len(files))
	for name, body := range files {
		entries = append(entries, zentry{name: name, body: body})
	}
	return buildZip(t, entries...)
}

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// The two minimal page documents the base pack bundles. They need only be valid
// JSON for install (the renderer validates the ui-schema/1 grammar at render
// time, a later task); they carry a plausible shape so the fixture reads true.
const (
	menuItemsDoc = `{"pageType":"list-detail","list":{"source":"menu_items","display":{"type":"table"}},` +
		`"detail":{"source":"menu_items","root":{"type":"text","value":"name"}}}`
	settingsDoc = `{"pageType":"settings-form","source":"settings",` +
		`"sections":[{"fields":[{"type":"text","value":"x"}]}],` +
		`"actions":[{"type":"button","on":{"press":{"verb":"submit"}}}]}`
	enCatalog = `{"page.menuItems.title":"Menu Items","page.settings.title":"Settings","pack.displayName":"Menu Board"}`
)

// baseManifest returns a fresh, fully-valid manifest map a test may mutate before
// packing. It declares two pages (list-detail + settings-form) and one
// menu_items collection — the same shape the example pack carries.
func baseManifest() map[string]any {
	return map[string]any{
		"id":          "acme/menu-board",
		"version":     "1.0.0",
		"displayName": "msg:pack.displayName",
		"compat": map[string]any{
			"ctx":      ">=1.0 <2.0",
			"renderer": []any{"list-detail", "settings-form"},
		},
		"capabilities": []any{},
		"egress":       []any{},
		"resources": map[string]any{
			"memory": 64, "cpuWeight": 100, "storageQuota": 16, "maxScheduledTimers": 0,
		},
		"dataModel": map[string]any{
			"version": 1,
			"collections": []any{
				map[string]any{
					"name": "menu_items",
					"fields": []any{
						map[string]any{"name": "name", "type": "string", "role": "title", "searchable": true},
						map[string]any{"name": "price", "type": "number"},
					},
				},
			},
		},
		"retention": map[string]any{},
		"ui": map[string]any{
			"pages": []any{
				map[string]any{"path": "menu-items", "pageType": "list-detail", "titleMsg": "msg:page.menuItems.title"},
				map[string]any{"path": "settings", "pageType": "settings-form", "titleMsg": "msg:page.settings.title"},
			},
		},
		"actions": []any{},
	}
}

// basePackFiles returns the full bundle file set for a given manifest.
func basePackFiles(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	return map[string]string{
		"manifest.json":      string(mustJSON(t, m)),
		"ui/menu-items.json": menuItemsDoc,
		"ui/settings.json":   settingsDoc,
		"messages/en.json":   enCatalog,
	}
}

// basePackZip packs the base bundle for manifest m into a zip.
func basePackZip(t *testing.T, m map[string]any) []byte {
	t.Helper()
	return filesZip(t, basePackFiles(t, m))
}
