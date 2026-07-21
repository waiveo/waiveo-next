package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// playlistFixtureAsset is the content seedSchedulingScope uploads to the shared
// content origin so playlistFixture's asset_ref resolves — a playlist item's
// asset_ref MUST be present in the origin (you cannot schedule un-uploaded
// content). Fixture bytes only (no secrets).
var playlistFixtureAsset = []byte("waiveo-next scheduling-core fixture asset")

// playlistFixtureAssetRef is the server-computed content id of playlistFixtureAsset
// (the same value the upload endpoint returns for those bytes).
var playlistFixtureAssetRef = signhash.ContentID(playlistFixtureAsset)

// Fixture ULIDs for the scheduling-core CRUD tests (fixture-ULID convention; no
// secrets — Crockford-base32 only, so a minted keyset cursor over these ids
// round-trips through ulid.Valid, API-034). A dedicated site+screen pair,
// independent of scopenodes_test.go's own fixtures — every test opens a fresh
// :memory: store, so there is no collision.
const (
	schedSiteID       = "01J8Z0F0000000000000000000"
	schedScreenID     = "01J8Z0G0000000000000000000"
	playlistAID       = "01J8Z0H0000000000000000000"
	playlistBID       = "01J8Z0J0000000000000000000"
	missingPlaylistID = "01J8Z0K0000000000000000000"
	scheduleAID       = "01J8Z0M0000000000000000000"
	daypart1ID        = "01J8Z0N0000000000000000000"
	daypart2ID        = "01J8Z0P0000000000000000000"
	daypart3ID        = "01J8Z0Q0000000000000000000"
	daypartDangID     = "01J8Z0R0000000000000000000"
)

// seedSchedulingScope creates the site+screen scope-node pair every scheduling-
// core fixture below hangs off.
func seedSchedulingScope(t *testing.T, e *testEnv) {
	t.Helper()
	e.createNode(t, siteNode(schedSiteID))
	e.createNode(t, screenNode(schedScreenID, schedSiteID, ""))
	// A playlist item's asset_ref must resolve in the shared content origin, so
	// upload the fixture asset before any playlist referencing it is authored.
	e.uploadContent(t, playlistFixtureAsset)
}

func playlistFixture(id string, labels map[string]string) datamodel.Playlist {
	return datamodel.Playlist{
		ID:        id,
		ScopeNode: schedScreenID,
		Name:      "Demo Playlist " + id,
		Items:     []datamodel.PlaylistItem{{Source: "asset", AssetRef: playlistFixtureAssetRef}},
		Labels:    labels,
	}
}

func scheduleFixture(id string) datamodel.Schedule {
	return datamodel.Schedule{
		ID:        id,
		ScopeNode: schedScreenID,
		Name:      "Demo Schedule",
	}
}

func daypartFixture(id, scheduleID, playlistID, start, end string, days []int) datamodel.Daypart {
	return datamodel.Daypart{
		ID:           id,
		ScheduleID:   scheduleID,
		ScopeNode:    schedScreenID,
		DaysOfWeek:   days,
		StartTime:    start,
		EndTime:      end,
		DisplayPower: "on",
		PlaylistID:   playlistID,
	}
}

// createOK POSTs body to path and fails the test unless the response is 201,
// returning the created body.
func (e *testEnv) createOK(t *testing.T, path string, body []byte) []byte {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, path, body, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: status %d, body %s", path, resp.StatusCode, raw)
	}
	return raw
}

// TestSchedulingCoreCRUDHappyPath drives the openapi exemplar's authoring
// sequence — playlist, then a schedule, then two non-overlapping dayparts under
// it — through the SAME generic resource handler Task 2 proved over scope-nodes,
// now mounted for the three scheduling-core kinds this task adds.
func TestSchedulingCoreCRUDHappyPath(t *testing.T) {
	e := newEnv(t)
	seedSchedulingScope(t, e)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists", mustJSON(t, playlistFixture(playlistAID, nil)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create playlist status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("create playlist ETag = %q, want \"1\"", etag)
	}

	resp, raw = e.do(t, http.MethodPost, "/api/v1/schedules", mustJSON(t, scheduleFixture(scheduleAID)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create schedule status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("create schedule ETag = %q, want \"1\"", etag)
	}

	// Two non-overlapping dayparts (half-open [06:00,12:00) then [12:00,22:00),
	// same weekdays) referencing the schedule + playlist above.
	d1 := daypartFixture(daypart1ID, scheduleAID, playlistAID, "06:00:00", "12:00:00", []int{1, 2, 3, 4, 5})
	resp, raw = e.do(t, http.MethodPost, "/api/v1/dayparts", mustJSON(t, d1), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create daypart1 status = %d, body %s", resp.StatusCode, raw)
	}
	d2 := daypartFixture(daypart2ID, scheduleAID, playlistAID, "12:00:00", "22:00:00", []int{1, 2, 3, 4, 5})
	resp, raw = e.do(t, http.MethodPost, "/api/v1/dayparts", mustJSON(t, d2), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create daypart2 status = %d, body %s", resp.StatusCode, raw)
	}
}

// TestDaypartOverlapValidationFailed: a daypart whose declared coverage
// intersects a sibling's, within the same schedule, is rejected 422 /
// VALIDATION_FAILED carrying a DAYPART_OVERLAP field error (DAT-073) — the
// datamodel error list surfaced verbatim as the api/1 `errors` extension member
// (API-013), and nothing is stored.
func TestDaypartOverlapValidationFailed(t *testing.T) {
	e := newEnv(t)
	seedSchedulingScope(t, e)
	e.createOK(t, "/api/v1/playlists", mustJSON(t, playlistFixture(playlistAID, nil)))
	e.createOK(t, "/api/v1/schedules", mustJSON(t, scheduleFixture(scheduleAID)))

	d1 := daypartFixture(daypart1ID, scheduleAID, playlistAID, "06:00:00", "14:00:00", []int{1, 2, 3, 4, 5})
	e.createOK(t, "/api/v1/dayparts", mustJSON(t, d1))

	// d3 overlaps d1 on weekdays 1-5: [12:00,20:00) intersects [06:00,14:00).
	d3 := daypartFixture(daypart3ID, scheduleAID, playlistAID, "12:00:00", "20:00:00", []int{1, 2, 3, 4, 5})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/dayparts", mustJSON(t, d3), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("overlapping daypart create status = %d, want 422, body %s", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	errsAny, _ := p["errors"].([]any)
	if len(errsAny) == 0 {
		t.Fatalf("VALIDATION_FAILED carried no per-field errors array (body %s)", raw)
	}
	found := false
	for _, ei := range errsAny {
		em, _ := ei.(map[string]any)
		if em["code"] == "DAYPART_OVERLAP" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("VALIDATION_FAILED errors did not include DAYPART_OVERLAP (DAT-073): %v", errsAny)
	}

	// The overlapping daypart was not stored.
	resp, _ = e.do(t, http.MethodGet, "/api/v1/dayparts/"+daypart3ID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("overlapping daypart was stored despite VALIDATION_FAILED: get status %d", resp.StatusCode)
	}
}

// TestDaypartMissingPlaylistValidationFailed: a daypart whose playlist_id does
// not resolve to a present playlist row is rejected 422 / VALIDATION_FAILED
// (DAT-075 REFERENCE_INVALID on the playlist_id field), never stored as a
// dangling reference.
func TestDaypartMissingPlaylistValidationFailed(t *testing.T) {
	e := newEnv(t)
	seedSchedulingScope(t, e)
	e.createOK(t, "/api/v1/schedules", mustJSON(t, scheduleFixture(scheduleAID)))

	dangling := daypartFixture(daypartDangID, scheduleAID, missingPlaylistID, "06:00:00", "22:00:00", []int{0, 1, 2, 3, 4, 5, 6})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/dayparts", mustJSON(t, dangling), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("dangling-playlist daypart create status = %d, want 422, body %s", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	errsAny, _ := p["errors"].([]any)
	found := false
	for _, ei := range errsAny {
		em, _ := ei.(map[string]any)
		if em["field"] == "playlist_id" && em["code"] == "REFERENCE_INVALID" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("VALIDATION_FAILED errors did not include a playlist_id REFERENCE_INVALID error: %v", errsAny)
	}

	resp, _ = e.do(t, http.MethodGet, "/api/v1/dayparts/"+daypartDangID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dangling-reference daypart was stored despite VALIDATION_FAILED: get status %d", resp.StatusCode)
	}
}

// TestSchedulingListGetPatchDeleteConventions exercises list/selector/pagination/
// get/patch/delete for a scheduling-core kind (playlists) — the same api/1
// conventions Task 2 proved over scope-nodes, now generalized.
func TestSchedulingListGetPatchDeleteConventions(t *testing.T) {
	e := newEnv(t)
	seedSchedulingScope(t, e)

	e.createOK(t, "/api/v1/playlists", mustJSON(t, playlistFixture(playlistAID, map[string]string{"region": "east"})))
	e.createOK(t, "/api/v1/playlists", mustJSON(t, playlistFixture(playlistBID, map[string]string{"region": "west"})))

	// selector: only the east-labeled playlist.
	q := url.Values{"selector": {"region=east"}}
	resp, raw := e.do(t, http.MethodGet, "/api/v1/playlists?"+q.Encode(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list selector status = %d, body %s", resp.StatusCode, raw)
	}
	var page struct {
		Items  []json.RawMessage `json:"items"`
		Cursor *string           `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode page: %v (body %s)", err, raw)
	}
	if len(page.Items) != 1 || decodeID(t, page.Items[0]) != playlistAID {
		t.Fatalf("selector region=east returned %d items (want 1, playlistA): %s", len(page.Items), raw)
	}

	// pagination: limit=1 over both playlists covers each exactly once.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/playlists?limit=1", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list p1 status = %d, body %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode p1: %v", err)
	}
	if len(page.Items) != 1 || page.Cursor == nil {
		t.Fatalf("list p1 = %+v, want 1 item + a continuation cursor", page)
	}
	resp, raw = e.do(t, http.MethodGet, "/api/v1/playlists?limit=1&cursor="+url.QueryEscape(*page.Cursor), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list p2 status = %d, body %s", resp.StatusCode, raw)
	}
	var page2 struct {
		Items  []json.RawMessage `json:"items"`
		Cursor *string           `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page2); err != nil {
		t.Fatalf("decode p2: %v", err)
	}
	if len(page2.Items) != 1 || page2.Cursor != nil {
		t.Fatalf("list p2 = %+v, want 1 item + a null cursor (last page)", page2)
	}

	// get: ETag on a fetched playlist.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/playlists/"+playlistAID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("get ETag = %q, want \"1\"", etag)
	}

	// patch: requires If-Match; stale If-Match is 412; correct If-Match applies +
	// bumps the ETag.
	patch := mustJSON(t, map[string]string{"name": "Renamed Playlist"})
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/playlists/"+playlistAID, patch, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("patch-no-ifmatch status = %d, want 428", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "IF_MATCH_REQUIRED")

	resp, raw = e.do(t, http.MethodPatch, "/api/v1/playlists/"+playlistAID, patch, map[string]string{"If-Match": `"9"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("patch-stale status = %d, want 412", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "REVISION_CONFLICT")

	resp, raw = e.do(t, http.MethodPatch, "/api/v1/playlists/"+playlistAID, patch, map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch-ok status = %d, body %s", resp.StatusCode, raw)
	}
	if etag := resp.Header.Get("ETag"); etag != `"2"` {
		t.Fatalf("patch-ok ETag = %q, want \"2\"", etag)
	}

	// delete: requires If-Match; correct If-Match removes it.
	resp, raw = e.do(t, http.MethodDelete, "/api/v1/playlists/"+playlistAID, nil, nil)
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("delete-no-ifmatch status = %d, want 428", resp.StatusCode)
	}
	assertProblem(t, resp, raw, "IF_MATCH_REQUIRED")

	resp, _ = e.do(t, http.MethodDelete, "/api/v1/playlists/"+playlistAID, nil, map[string]string{"If-Match": `"2"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodGet, "/api/v1/playlists/"+playlistAID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404", resp.StatusCode)
	}
}
