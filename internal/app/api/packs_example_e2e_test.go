package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	examplepacks "github.com/maaxton/waiveo-next/examples/packs"
)

// This is the declarative-packs living proof over the REAL api/1 surface: the
// in-repo example pack (examples/packs/menu-board) is installed through the actual
// POST /api/v1/extensions — gated by the real manifest engine — then exercised exactly
// as a third party's pack would be. It proves the whole promise of the wave end to
// end: install → the pack + its page documents + its collection all land → a row
// created over the pack-data api/1 surface persists and lists → the page document
// the console renders is served back byte-for-byte. NO pack code runs anywhere: a
// manifest, two page docs, a catalog, and a menu-item row are all data.
//
// The pack content installed here is identical to what `make example-pack`
// writes and what `make dev`'s pack smoke installs (one source of truth:
// examplepacks); only the signature envelope differs — this test signs with its
// fixture publisher key where the dev loop signs with the make-dev key.

const examplePackID = "waiveo/menu-board"

// exampleMenuRowsPath is the pack-data collection surface for the example pack's
// menu_items collection.
const exampleMenuRowsPath = "/api/v1/extensions/waiveo/menu-board/data/menu_items"

func TestExamplePackInstallsAndServesDataEndToEnd(t *testing.T) {
	e := newEnv(t)
	// The node the menu-item row below is placed at: a pack row carries the same
	// DAT-006 placement a resource row does, and the store resolves it.
	e.seedPlacementNodes(t, rowScopeNodeA)

	raw, err := examplepacks.MenuBoardZip()
	if err != nil {
		t.Fatalf("build example pack zip: %v", err)
	}
	// Sign the artifact exactly as the publisher tooling does (`make
	// example-pack` signs with the make-dev publisher key); the env's trust
	// anchors authorize the fixture signer for the waiveo namespace.
	art := signPack(t, raw, examplePackID, "1.0.0")

	// 1. Install the REAL example artifact over the REAL install endpoint.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/extensions", art, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install status = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	var res struct {
		ID          string   `json:"id"`
		Version     string   `json:"version"`
		Pages       []string `json:"pages"`
		Collections []string `json:"collections"`
		Locales     []string `json:"locales"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode install summary: %v (%s)", err, raw)
	}
	if res.ID != examplePackID || res.Version != "1.0.0" {
		t.Fatalf("install summary identity = %q %q, want waiveo/menu-board 1.0.0", res.ID, res.Version)
	}
	if len(res.Pages) != 2 {
		t.Fatalf("install summary pages = %v, want 2", res.Pages)
	}
	if len(res.Collections) != 2 || res.Collections[0] != "menu_items" || res.Collections[1] != "settings" {
		t.Fatalf("install summary collections = %v, want [menu_items settings]", res.Collections)
	}
	if len(res.Locales) != 1 || res.Locales[0] != "en" {
		t.Fatalf("install summary locales = %v, want [en]", res.Locales)
	}

	// 2. The pack landed: GET it (ETag "1", the manifest body present).
	resp, raw = e.do(t, http.MethodGet, "/api/v1/extensions/waiveo/menu-board", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get pack status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if et := resp.Header.Get("ETag"); et != `"1"` {
		t.Fatalf("pack ETag = %q, want \"1\"", et)
	}

	// 3. Its files landed: both page documents are served back byte-for-byte as they
	// ship on disk (the console renders exactly these bytes), and the en catalog too.
	for _, page := range []string{"menu-items", "settings"} {
		want, err := examplepacks.MenuBoardFile("ui/" + page + ".json")
		if err != nil {
			t.Fatalf("read on-disk page %q: %v", page, err)
		}
		resp, got := e.do(t, http.MethodGet, "/api/v1/extensions/waiveo/menu-board/pages/"+page, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get page %q status = %d, want 200 (%s)", page, resp.StatusCode, got)
		}
		if string(got) != string(want) {
			t.Fatalf("page %q served bytes differ from the on-disk document", page)
		}
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/extensions/waiveo/menu-board/messages/en", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get en catalog status = %d, want 200", resp.StatusCode)
	}

	// 4. Create a menu item over the pack-data api/1 surface — a full envelope citizen.
	item := map[string]any{
		"scope_node": rowScopeNodeA, "name": "Cortado", "section": "Coffee", "price": 4.5, "available": true,
	}
	resp, raw = e.do(t, http.MethodPost, exampleMenuRowsPath, mustJSON(t, item), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create menu item status = %d, want 201 (%s)", resp.StatusCode, raw)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode created row: %v (%s)", err, raw)
	}
	entityID, _ := row["entity_id"].(string)
	if len(entityID) != 26 {
		t.Fatalf("created row entity_id = %v, want a 26-char ULID", row["entity_id"])
	}
	if row["name"] != "Cortado" || row["section"] != "Coffee" || row["price"] != 4.5 || row["available"] != true {
		t.Fatalf("created row declared fields = %+v", row)
	}
	if row["lifecycle_state"] != "published" || row["scope_node"] != rowScopeNodeA {
		t.Fatalf("created row envelope = %+v", row)
	}

	// 5. The row landed in the collection list — the rows are real, queryable data.
	resp, raw = e.do(t, http.MethodGet, exampleMenuRowsPath, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list rows status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	page := decodePage(t, raw)
	if len(page.Items) != 1 || page.Items[0]["name"] != "Cortado" {
		t.Fatalf("collection list = %+v, want the one Cortado row", page.Items)
	}

	// 6. The lifecycle-participating collection (name: draft-publish) accepts a draft,
	// proving MAN-052 rides the example pack's own declaration end to end.
	resp, raw = e.do(t, http.MethodPost, exampleMenuRowsPath,
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Seasonal special", "lifecycle_state": "draft"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create draft item status = %d, want 201 (%s)", resp.StatusCode, raw)
	}
	var draft map[string]any
	_ = json.Unmarshal(raw, &draft)
	if draft["lifecycle_state"] != "draft" {
		t.Fatalf("draft item lifecycle_state = %v, want draft (the collection declares lifecycle: draft-publish)", draft["lifecycle_state"])
	}
}
