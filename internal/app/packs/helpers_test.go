package packs_test

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"io/fs"
	"testing"

	"github.com/maaxton/waiveo-next/internal/packsig"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
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

// basePackZip packs the base bundle for manifest m into a zip — UNSIGNED. Most
// install tests sign it via a testSigner; the unsigned form is itself a fixture
// (the PACK_UNSIGNED refusal path).
func basePackZip(t *testing.T, m map[string]any) []byte {
	t.Helper()
	return filesZip(t, basePackFiles(t, m))
}

// ---- signing fixtures (marketplace/1 MKT-009a/b) ---------------------------

// testSigner is the fixture publisher identity install tests sign artifacts
// with: a fresh ed25519 keypair and its derived key id.
type testSigner struct {
	keyID string
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	pub, priv := signhash.GenerateKey()
	return &testSigner{keyID: packsig.KeyIDFor(pub), pub: pub, priv: priv}
}

// anchorsFor returns a trust-anchor set authorizing this signer for exactly the
// given publisher namespaces — namespace binding stays real in every test.
func (s *testSigner) anchorsFor(namespaces ...string) packsig.StaticAnchors {
	anchors := packsig.StaticAnchors{}
	for _, ns := range namespaces {
		anchors[ns] = []packsig.TrustedKey{{KeyID: s.keyID, PublicKey: s.pub}}
	}
	return anchors
}

// sign wraps an artifact zip with this signer's envelope for (id, version).
func (s *testSigner) sign(t *testing.T, artifact []byte, id, version string) []byte {
	t.Helper()
	signed, err := packsig.Sign(artifact, id, version, s.keyID, s.priv)
	if err != nil {
		t.Fatalf("sign artifact: %v", err)
	}
	return signed
}

// signedPackZip builds the base bundle for manifest m and signs it as the
// identity the manifest itself declares.
func signedPackZip(t *testing.T, s *testSigner, m map[string]any) []byte {
	t.Helper()
	id, _ := m["id"].(string)
	version, _ := m["version"].(string)
	return s.sign(t, basePackZip(t, m), id, version)
}

// signedFilesZip signs an arbitrary file-map bundle as (id, version).
func signedFilesZip(t *testing.T, s *testSigner, files map[string]string, id, version string) []byte {
	t.Helper()
	return s.sign(t, filesZip(t, files), id, version)
}

// fixtureNamespaces are the publisher namespaces the manifest fixtures in this
// package use; granting them all to the fixture signer keeps every
// manifest-engine test exercising ITS refusal rather than a namespace refusal.
var fixtureNamespaces = []string{"acme", "Acme", "BAD", "waiveo"}

// tamperEntry rewrites one entry's bytes inside a (signed) zip, keeping every
// other entry — including the signature envelope — intact: the
// altered-after-signing fixture.
func tamperEntry(t *testing.T, artifact []byte, name, body string) []byte {
	return rewriteZip(t, artifact, map[string]*string{name: &body})
}

// stripEntry removes one entry from a zip — the signature-stripping fixture.
func stripEntry(t *testing.T, artifact []byte, name string) []byte {
	return rewriteZip(t, artifact, map[string]*string{name: nil})
}

// rewriteZip copies a zip, applying per-entry edits: a non-nil value replaces
// (or, for a name the zip does not carry, ADDS) the entry's bytes; a nil value
// drops the entry.
func rewriteZip(t *testing.T, artifact []byte, edits map[string]*string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(artifact), int64(len(artifact)))
	if err != nil {
		t.Fatalf("rewrite: read zip: %v", err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	seen := map[string]bool{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		seen[f.Name] = true
		body, edited := edits[f.Name]
		if edited && body == nil {
			continue // dropped
		}
		var data []byte
		if edited {
			data = []byte(*body)
		} else {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("rewrite: open %q: %v", f.Name, err)
			}
			data, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatalf("rewrite: read %q: %v", f.Name, err)
			}
		}
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatalf("rewrite: create %q: %v", f.Name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("rewrite: write %q: %v", f.Name, err)
		}
	}
	for name, body := range edits {
		if seen[name] || body == nil {
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("rewrite: add %q: %v", name, err)
		}
		if _, err := w.Write([]byte(*body)); err != nil {
			t.Fatalf("rewrite: write added %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("rewrite: close: %v", err)
	}
	return buf.Bytes()
}
