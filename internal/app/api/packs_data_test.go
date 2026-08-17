package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// A fixture scope-node ULID (fixture-ULID convention; no secrets). The suffix
// letters order A<B so an external_id-scope test can use two distinct nodes.
const (
	rowScopeNodeA = "01J8Z2Q1A0000000000000000A"
	rowScopeNodeB = "01J8Z2Q1B0000000000000000B"
)

// dataPackManifest is the base pack manifest with a richer menu_items collection
// (mixed declared field types) plus a draft-publish `articles` collection, so the
// pack-data tests can exercise field-type validation AND both MAN-052 lifecycle
// branches over HTTP.
func dataPackManifest() map[string]any {
	m := packManifest()
	m["dataModel"] = map[string]any{"version": 1, "collections": []any{
		map[string]any{"name": "menu_items", "fields": []any{
			map[string]any{"name": "name", "type": "string", "role": "title", "searchable": true},
			map[string]any{"name": "price", "type": "number"},
			map[string]any{"name": "section", "type": "string"},
			map[string]any{"name": "available", "type": "boolean"},
		}},
		map[string]any{"name": "articles", "fields": []any{
			map[string]any{"name": "title", "type": "string", "role": "title", "lifecycle": "draft-publish"},
		}},
		// The singleton record the base manifest's settings-form page binds
		// (MAN-064), which also gives these tests a collection to exercise the
		// MAN-056 one-row bound against over HTTP.
		map[string]any{"name": "settings", "singleton": true, "fields": []any{
			map[string]any{"name": "board_name", "type": "string"},
		}},
	}}
	return m
}

// installDataPack installs the data pack (acme/menu-board) and fails on a non-201.
//
// It also seeds the two scope nodes the row fixtures are placed at: a pack row
// carries the same DAT-006 placement a resource row does, and the store resolves
// it, so a row placed at an id no node carries is refused.
func (e *testEnv) installDataPack(t *testing.T) {
	t.Helper()
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/extensions", packBundle(t, dataPackManifest()), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install data pack: status %d, body %s", resp.StatusCode, raw)
	}
}

const menuRowsPath = "/api/v1/extensions/acme/menu-board/data/menu_items"

// createRow POSTs a row body to the menu_items collection and returns the response
// + decoded body.
func (e *testEnv) createRow(t *testing.T, body map[string]any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, menuRowsPath, mustJSON(t, body), headers)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// TestPackRowCreateAndGet: a valid row creates (201, ETag "1", Location), carries
// the universal envelope (host-assigned entity_id, revision 1) beside its declared
// fields, and round-trips through GET.
func TestPackRowCreateAndGet(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)

	resp, row := e.createRow(t, map[string]any{
		"scope_node": rowScopeNodeA, "name": "Burger", "price": 9.5, "available": true, "section": "Mains",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%+v)", resp.StatusCode, row)
	}
	if et := resp.Header.Get("ETag"); et != `"1"` {
		t.Fatalf("ETag = %q, want \"1\"", et)
	}
	entityID, _ := row["entity_id"].(string)
	if len(entityID) != 26 {
		t.Fatalf("entity_id = %v, want a 26-char ULID", row["entity_id"])
	}
	if row["revision"] != float64(1) || row["lifecycle_state"] != "published" || row["scope_node"] != rowScopeNodeA {
		t.Fatalf("envelope = %+v", row)
	}
	if row["name"] != "Burger" || row["price"] != 9.5 || row["available"] != true {
		t.Fatalf("declared fields = %+v", row)
	}
	if loc := resp.Header.Get("Location"); loc != menuRowsPath+"/"+entityID {
		t.Fatalf("Location = %q, want %q", loc, menuRowsPath+"/"+entityID)
	}

	resp, raw := e.do(t, http.MethodGet, menuRowsPath+"/"+entityID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if et := resp.Header.Get("ETag"); et != `"1"` {
		t.Fatalf("get ETag = %q, want \"1\"", et)
	}
}

// TestPackRowCreateMissingScopeNode422: the required envelope field scope_node is
// enforced — a 422 mapping the failure onto the scope_node field.
func TestPackRowCreateMissingScopeNode422(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	resp, raw := e.do(t, http.MethodPost, menuRowsPath, mustJSON(t, map[string]any{"name": "Burger"}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	errorsHasFieldCode(t, p, "scope_node", "REQUIRED")
}

// TestPackRowCreateFieldTypeMismatch422: a declared field whose value's type is
// wrong is a 422 mapping the failure onto that declared field (API-013).
func TestPackRowCreateFieldTypeMismatch422(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	body := map[string]any{"scope_node": rowScopeNodeA, "name": "Burger", "price": "free"}
	resp, raw := e.do(t, http.MethodPost, menuRowsPath, mustJSON(t, body), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	errorsHasFieldCode(t, p, "price", "INVALID_TYPE")
}

// TestPackRowUnknownPack404: pack rows under a pack that is not installed → 404.
func TestPackRowUnknownPack404(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodGet, "/api/v1/extensions/nobody/here/data/menu_items", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
}

// TestPackRowUndeclaredCollection404: a collection the manifest does not declare → 404.
func TestPackRowUndeclaredCollection404(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	resp, raw := e.do(t, http.MethodGet, "/api/v1/extensions/acme/menu-board/data/nonexistent", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
	// And a create into an undeclared collection is likewise a 404.
	resp, _ = e.do(t, http.MethodPost, "/api/v1/extensions/acme/menu-board/data/nonexistent",
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA}), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create undeclared status = %d, want 404", resp.StatusCode)
	}
}

// TestPackRowGetNotFound: a good pack+collection but an unknown entity_id → 404.
func TestPackRowGetNotFound(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	resp, raw := e.do(t, http.MethodGet, menuRowsPath+"/01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, raw)
	}
}

// TestPackRowList: created rows appear in the collection list (the {items, cursor}
// envelope) with their declared fields.
func TestPackRowList(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Burger"}, nil)
	e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Fries"}, nil)

	resp, raw := e.do(t, http.MethodGet, menuRowsPath, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	page := decodePage(t, raw)
	if len(page.Items) != 2 {
		t.Fatalf("list items = %d, want 2 (%s)", len(page.Items), raw)
	}
	if page.Cursor != nil {
		t.Fatalf("cursor = %v, want null (single page)", *page.Cursor)
	}
}

// TestPackRowListPaginates: the cursor the list mints decodes on the next request
// and walks the collection one row per page, no repeat or skip. entity_id is a
// ULID, but the cursor is bound to (pack, collection).
func TestPackRowListPaginates(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "A"}, nil)
	e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "B"}, nil)

	resp, raw := e.do(t, http.MethodGet, menuRowsPath+"?limit=1", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page 1 status = %d (%s)", resp.StatusCode, raw)
	}
	p1 := decodePage(t, raw)
	if len(p1.Items) != 1 || p1.Cursor == nil {
		t.Fatalf("page 1 = %+v, cursor %v; want 1 item + a cursor", p1.Items, p1.Cursor)
	}
	first := p1.Items[0]["entity_id"]

	resp, raw = e.do(t, http.MethodGet, menuRowsPath+"?limit=1&cursor="+url.QueryEscape(*p1.Cursor), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page 2 status = %d — the server's own cursor was rejected (%s)", resp.StatusCode, raw)
	}
	p2 := decodePage(t, raw)
	if len(p2.Items) != 1 || p2.Cursor != nil {
		t.Fatalf("page 2 = %+v, cursor %v; want 1 item + null", p2.Items, p2.Cursor)
	}
	if p2.Items[0]["entity_id"] == first {
		t.Fatalf("page 2 repeated page 1's row %v", first)
	}
}

// TestPackRowListRejectsForeignCursor: a cursor not minted by THIS collection's
// list — a bare ULID, another resource's scoped token, or a cursor from a different
// collection — is refused 400 / CURSOR_INVALID (API-035).
func TestPackRowListRejectsForeignCursor(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "A"}, nil)

	// Mint a real cursor from the ARTICLES collection, then replay it on menu_items.
	_, _ = e.do(t, http.MethodPost, "/api/v1/extensions/acme/menu-board/data/articles",
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "title": "X"}), nil)
	_, araw := e.do(t, http.MethodGet, "/api/v1/extensions/acme/menu-board/data/articles?limit=1", nil, nil)
	articleCursor := ""
	if c := decodePage(t, araw).Cursor; c != nil {
		articleCursor = *c
	}

	foreign := []string{
		"01ARZ3NDEKTSV4RRFFQ69G5FAV",        // a bare ULID
		"device_01ARZ3NDEKTSV4RRFFQ69G5FAV", // another resource's scoped cursor
	}
	if articleCursor != "" {
		foreign = append(foreign, articleCursor) // a real cursor from a sibling collection
	}
	for _, c := range foreign {
		resp, raw := e.do(t, http.MethodGet, menuRowsPath+"?limit=1&cursor="+url.QueryEscape(c), nil, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("cursor %q status = %d, want 400 (%s)", c, resp.StatusCode, raw)
		}
		assertProblem(t, resp, raw, "CURSOR_INVALID")
	}
}

// TestPackRowPatchIfMatch: PATCH honors optimistic concurrency — 428 without
// If-Match, 412 on a wrong one, 200 (revision bumped) on a correct one.
func TestPackRowPatchIfMatch(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	_, row := e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Burger", "price": 9.5}, nil)
	id, _ := row["entity_id"].(string)
	path := menuRowsPath + "/" + id
	patch := mustJSON(t, map[string]any{"price": 10.5})

	resp, raw := e.do(t, http.MethodPatch, path, patch, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("no If-Match status = %d, want 428 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "IF_MATCH_REQUIRED")

	resp, raw = e.do(t, http.MethodPatch, path, patch, map[string]string{"If-Match": `"99"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match status = %d, want 412 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "REVISION_CONFLICT")

	resp, raw = e.do(t, http.MethodPatch, path, patch, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good If-Match status = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	if et := resp.Header.Get("ETag"); et != `"2"` {
		t.Fatalf("patched ETag = %q, want \"2\"", et)
	}
	var patched map[string]any
	_ = json.Unmarshal(raw, &patched)
	if patched["price"] != 10.5 || patched["name"] != "Burger" {
		t.Fatalf("patched row = %+v; want price 10.5, name preserved", patched)
	}
}

// TestPackRowPatchValidates: a patch that breaks a declared field's type is a 422
// (the effective post-merge row is validated) and nothing changes.
func TestPackRowPatchValidates(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	_, row := e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Burger"}, nil)
	id, _ := row["entity_id"].(string)
	resp, raw := e.do(t, http.MethodPatch, menuRowsPath+"/"+id,
		mustJSON(t, map[string]any{"price": "free"}), map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	errorsHasFieldCode(t, p, "price", "INVALID_TYPE")
}

// TestPackRowDeleteIfMatch: DELETE honors If-Match — 428 without, 204 with the
// right revision, then the row is gone.
func TestPackRowDeleteIfMatch(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	_, row := e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Burger"}, nil)
	id, _ := row["entity_id"].(string)
	path := menuRowsPath + "/" + id

	resp, raw := e.do(t, http.MethodDelete, path, nil, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("no If-Match delete = %d, want 428 (%s)", resp.StatusCode, raw)
	}
	resp, raw = e.do(t, http.MethodDelete, path, nil, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (%s)", resp.StatusCode, raw)
	}
	resp, _ = e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", resp.StatusCode)
	}
}

// TestPackRowExternalIDUnique: external_id is unique per (pack, collection,
// scope_node) — a duplicate under the same scope_node is a 400 EXTERNAL_ID_CONFLICT,
// but the SAME external_id under a DIFFERENT scope_node is accepted (API-101/102).
func TestPackRowExternalIDUnique(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)

	resp, _ := e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Burger", "external_id": "sku-1"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d, want 201", resp.StatusCode)
	}
	// Same external_id, same scope_node → conflict.
	resp, raw := e.do(t, http.MethodPost, menuRowsPath,
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Fries", "external_id": "sku-1"}), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dup external_id status = %d, want 400 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "EXTERNAL_ID_CONFLICT")

	// Same external_id under a DIFFERENT scope_node → accepted.
	resp, _ = e.createRow(t, map[string]any{"scope_node": rowScopeNodeB, "name": "Fries", "external_id": "sku-1"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("same external_id other scope = %d, want 201", resp.StatusCode)
	}
}

// TestPackRowCreateIdempotency: a keyed create replays the retained 201 verbatim
// (one row created), and the same key with a different body is a 409 reuse conflict.
func TestPackRowCreateIdempotency(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)
	keyed := map[string]string{"Idempotency-Key": "pack-row-key-1"}
	body := mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Burger"})

	resp, raw1 := e.do(t, http.MethodPost, menuRowsPath, body, keyed)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201 (%s)", resp.StatusCode, raw1)
	}
	resp, raw2 := e.do(t, http.MethodPost, menuRowsPath, body, keyed)
	if resp.StatusCode != http.StatusCreated || string(raw1) != string(raw2) {
		t.Fatalf("replay status = %d, body-equal = %v; want a verbatim 201 replay", resp.StatusCode, string(raw1) == string(raw2))
	}
	// Exactly one row created.
	_, lraw := e.do(t, http.MethodGet, menuRowsPath, nil, nil)
	if n := len(decodePage(t, lraw).Items); n != 1 {
		t.Fatalf("rows after keyed retry = %d, want 1 (the create re-ran)", n)
	}
	// Same key, different body → 409.
	resp, raw := e.do(t, http.MethodPost, menuRowsPath,
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Fries"}), keyed)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("same-key different-body = %d, want 409 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "IDEMPOTENCY_KEY_REUSED")
}

// TestPackRowLifecycleNotAllowed422: MAN-052 over HTTP — a non-participating
// collection (menu_items) rejects a non-published lifecycle_state; the participating
// `articles` collection accepts a draft.
func TestPackRowLifecycleNotAllowed422(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)

	resp, raw := e.do(t, http.MethodPost, menuRowsPath,
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "name": "Burger", "lifecycle_state": "draft"}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	errorsHasFieldCode(t, p, "lifecycle_state", "LIFECYCLE_NOT_ALLOWED")

	// The participating collection accepts a draft row.
	resp, raw = e.do(t, http.MethodPost, "/api/v1/extensions/acme/menu-board/data/articles",
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "title": "Draft One", "lifecycle_state": "draft"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("draft article status = %d, want 201 (%s)", resp.StatusCode, raw)
	}
	var article map[string]any
	_ = json.Unmarshal(raw, &article)
	if article["lifecycle_state"] != "draft" {
		t.Fatalf("article lifecycle_state = %v, want draft", article["lifecycle_state"])
	}
}

// ── MAN-056: a singleton collection holds exactly one row ───────────────────

const settingsRowsPath = "/api/v1/extensions/acme/menu-board/data/settings"

// TestSingletonCollectionAcceptsOneRowThenRefusesTheNext: the first create into
// a collection declared `singleton: true` lands; the second is refused 409 with
// SINGLETON_COLLECTION_OCCUPIED rather than quietly becoming a second record.
//
// Over HTTP rather than against the store directly, because the bound has to
// hold for EVERY writer — the whole reason it lives in the store instead of in
// the console that happens to edit a settings-form. This is the path any pack,
// script, or integration takes.
func TestSingletonCollectionAcceptsOneRowThenRefusesTheNext(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)

	first, body := e.do(t, http.MethodPost, settingsRowsPath,
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "board_name": "The Corner Cafe"}), nil)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201 (%s)", first.StatusCode, body)
	}

	second, raw := e.do(t, http.MethodPost, settingsRowsPath,
		mustJSON(t, map[string]any{"scope_node": rowScopeNodeA, "board_name": "Second Board"}), nil)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409 (%s)", second.StatusCode, raw)
	}
	var problem map[string]any
	_ = json.Unmarshal(raw, &problem)
	if problem["code"] != "SINGLETON_COLLECTION_OCCUPIED" {
		t.Fatalf("second create code = %v, want SINGLETON_COLLECTION_OCCUPIED (%s)", problem["code"], raw)
	}

	// And the refusal did not disturb the row that was already there: a bound
	// that rejected the write but left a partial one behind would be worse than
	// no bound at all.
	list, listRaw := e.do(t, http.MethodGet, settingsRowsPath, nil, nil)
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", list.StatusCode)
	}
	page := decodePage(t, listRaw)
	if len(page.Items) != 1 || page.Items[0]["board_name"] != "The Corner Cafe" {
		t.Fatalf("settings rows = %+v, want exactly the first row", page.Items)
	}
}

// TestAnOrdinaryCollectionStillTakesManyRows: the bound is the singleton
// annotation, not a new rule for every pack collection. Without this the
// implementation could enforce one row everywhere and the test above would
// still pass.
func TestAnOrdinaryCollectionStillTakesManyRows(t *testing.T) {
	e := newEnv(t)
	e.installDataPack(t)

	for i, name := range []string{"Burger", "Fries"} {
		resp, raw := e.createRow(t, map[string]any{"scope_node": rowScopeNodeA, "name": name}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d (%s) status = %d, want 201 (%s)", i, name, resp.StatusCode, raw)
		}
	}
}
