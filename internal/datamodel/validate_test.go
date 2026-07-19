package datamodel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func corpusPath(name string) string {
	return filepath.Join("..", "..", "conformance", "corpora", "data-model-1", name)
}

// hasErr reports whether errs contains one with the given code (and, when field
// != "", that field).
func hasErr(errs []Error, code, field string) bool {
	for _, e := range errs {
		if e.Code == code && (field == "" || e.Field == field) {
			return true
		}
	}
	return false
}

// DAT-101: a schedule row submitted with a row-level pack-identifying field is
// rejected outright (SCHEDULER_ROW_PACK_OWNED). Wired to the named corpus case.
func TestDAT101PackOwnedSchedulerRejected(t *testing.T) {
	b, err := os.ReadFile(corpusPath("DAT-101-invalid-pack-owned-scheduler-rejected.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c struct {
		Input struct {
			Schedule json.RawMessage `json:"schedule"`
		} `json:"input"`
		Expected struct {
			Valid  bool `json:"valid"`
			Errors []struct {
				Field string `json:"field"`
				Code  string `json:"code"`
			} `json:"errors"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if c.Expected.Valid || len(c.Expected.Errors) == 0 {
		t.Fatalf("corpus expectation drifted: %+v", c.Expected)
	}
	want := c.Expected.Errors[0]

	_, errs := ValidateRows(RawRows{Schedules: []json.RawMessage{c.Input.Schedule}})
	if len(errs) == 0 {
		t.Fatalf("expected pack-owned schedule to be rejected, got no errors")
	}
	if !hasErr(errs, want.Code, want.Field) {
		t.Fatalf("expected error {code:%q field:%q}, got %+v", want.Code, want.Field, errs)
	}
}

// Every one of the six row kinds is rejected for a row-level pack field; a
// pack_id nested inside a playlist item is NOT a row-level field (DAT-041/101).
func TestPackOwnershipPerKindAndItemException(t *testing.T) {
	pack := json.RawMessage(`{"id":"01JX","scope_node":"01JN","name":"x","owner_pack":"acme"}`)
	for _, tc := range []struct {
		name string
		raw  RawRows
	}{
		{"schedule", RawRows{Schedules: []json.RawMessage{pack}}},
		{"daypart", RawRows{Dayparts: []json.RawMessage{pack}}},
		{"validity", RawRows{ValidityWindows: []json.RawMessage{pack}}},
		{"fallback", RawRows{Fallbacks: []json.RawMessage{pack}}},
		{"preset", RawRows{PresetBatches: []json.RawMessage{pack}}},
		{"playlist", RawRows{Playlists: []json.RawMessage{pack}}},
	} {
		_, errs := ValidateRows(tc.raw)
		if !hasErr(errs, "SCHEDULER_ROW_PACK_OWNED", "owner_pack") {
			t.Errorf("%s: expected SCHEDULER_ROW_PACK_OWNED, got %+v", tc.name, errs)
		}
	}
	// A playlist whose item carries pack_id (legitimate, DAT-041) is accepted.
	okPlaylist := json.RawMessage(`{"id":"01JPL","scope_node":"01JN","name":"p","items":[{"source":"playable","pack_id":"acme","content_id":"c"}],"revision":1,"created_at":1,"updated_at":1}`)
	_, errs := ValidateRows(RawRows{Playlists: []json.RawMessage{okPlaylist}})
	if hasErr(errs, "SCHEDULER_ROW_PACK_OWNED", "") {
		t.Errorf("a playlist item's nested pack_id must NOT trip pack-ownership (DAT-041), got %+v", errs)
	}
}

// DAT-007: a daypart or validity-window row whose scope_node differs from its
// owning schedule's scope_node is rejected.
func TestDAT007ScopeNodeMustMatchOwningSchedule(t *testing.T) {
	sch := json.RawMessage(`{"id":"01JS","scope_node":"01JN-A","name":"s","revision":1,"created_at":1,"updated_at":1}`)
	dp := json.RawMessage(`{"id":"01JD","schedule_id":"01JS","scope_node":"01JN-B","days_of_week":[1],"start_time":"06:00:00","end_time":"22:00:00","display_power":"on","revision":1,"created_at":1,"updated_at":1}`)
	vw := json.RawMessage(`{"id":"01JV","schedule_id":"01JS","scope_node":"01JN-B","starts_at":null,"ends_at":null,"revision":1,"created_at":1,"updated_at":1}`)

	_, errs := ValidateRows(RawRows{Schedules: []json.RawMessage{sch}, Dayparts: []json.RawMessage{dp}})
	if !hasErr(errs, "SCOPE_NODE_MISMATCH", "scope_node") {
		t.Errorf("daypart scope_node != schedule scope_node must be rejected (DAT-007), got %+v", errs)
	}
	_, errs = ValidateRows(RawRows{Schedules: []json.RawMessage{sch}, ValidityWindows: []json.RawMessage{vw}})
	if !hasErr(errs, "SCOPE_NODE_MISMATCH", "scope_node") {
		t.Errorf("validity-window scope_node != schedule scope_node must be rejected (DAT-007), got %+v", errs)
	}

	// Matching scope_node passes the DAT-007 check.
	dpOK := json.RawMessage(`{"id":"01JD","schedule_id":"01JS","scope_node":"01JN-A","days_of_week":[1],"start_time":"06:00:00","end_time":"22:00:00","display_power":"on","revision":1,"created_at":1,"updated_at":1}`)
	_, errs = ValidateRows(RawRows{Schedules: []json.RawMessage{sch}, Dayparts: []json.RawMessage{dpOK}})
	if hasErr(errs, "SCOPE_NODE_MISMATCH", "") {
		t.Errorf("matching scope_node must not be flagged, got %+v", errs)
	}
}

// Referential integrity: playlist/fallback/preset id references must resolve to
// an existing row (DAT-050/070/075/080).
func TestReferentialIntegrity(t *testing.T) {
	// schedule.fallback_id dangling
	sch := json.RawMessage(`{"id":"01JS","scope_node":"01JN","name":"s","fallback_id":"01JF-missing","revision":1,"created_at":1,"updated_at":1}`)
	_, errs := ValidateRows(RawRows{Schedules: []json.RawMessage{sch}})
	if !hasErr(errs, "REFERENCE_INVALID", "fallback_id") {
		t.Errorf("dangling fallback_id must be rejected, got %+v", errs)
	}
	// daypart.playlist_id + preset_batch_id dangling
	sch2 := json.RawMessage(`{"id":"01JS","scope_node":"01JN","name":"s","revision":1,"created_at":1,"updated_at":1}`)
	dp := json.RawMessage(`{"id":"01JD","schedule_id":"01JS","scope_node":"01JN","days_of_week":[1],"start_time":"06:00:00","end_time":"22:00:00","display_power":"on","playlist_id":"01JPL-missing","preset_batch_id":"01JPB-missing","revision":1,"created_at":1,"updated_at":1}`)
	_, errs = ValidateRows(RawRows{Schedules: []json.RawMessage{sch2}, Dayparts: []json.RawMessage{dp}})
	if !hasErr(errs, "REFERENCE_INVALID", "playlist_id") {
		t.Errorf("dangling playlist_id must be rejected, got %+v", errs)
	}
	if !hasErr(errs, "REFERENCE_INVALID", "preset_batch_id") {
		t.Errorf("dangling preset_batch_id must be rejected, got %+v", errs)
	}
	// daypart.schedule_id dangling
	dpNoSched := json.RawMessage(`{"id":"01JD","schedule_id":"01JS-missing","scope_node":"01JN","days_of_week":[1],"start_time":"06:00:00","end_time":"22:00:00","display_power":"on","revision":1,"created_at":1,"updated_at":1}`)
	_, errs = ValidateRows(RawRows{Dayparts: []json.RawMessage{dpNoSched}})
	if !hasErr(errs, "REFERENCE_INVALID", "schedule_id") {
		t.Errorf("dangling schedule_id must be rejected, got %+v", errs)
	}
}

// Closed vocabularies and simple row-shape rejections with taxonomy codes.
func TestVocabAndShapeRejections(t *testing.T) {
	// display_power outside {on,off,blank} (DAT-074)
	dp := json.RawMessage(`{"id":"01JD","schedule_id":"01JS","scope_node":"01JN","days_of_week":[1],"start_time":"06:00:00","end_time":"22:00:00","display_power":"dim","revision":1,"created_at":1,"updated_at":1}`)
	sch := json.RawMessage(`{"id":"01JS","scope_node":"01JN","name":"s","revision":1,"created_at":1,"updated_at":1}`)
	_, errs := ValidateRows(RawRows{Schedules: []json.RawMessage{sch}, Dayparts: []json.RawMessage{dp}})
	if !hasErr(errs, "DISPLAY_POWER_INVALID", "display_power") {
		t.Errorf("invalid display_power must be rejected (DAT-074), got %+v", errs)
	}
	// misfire outside {catch_up_once,skip,fire_each} (DAT-120)
	schM := json.RawMessage(`{"id":"01JS","scope_node":"01JN","name":"s","misfire":"maybe","revision":1,"created_at":1,"updated_at":1}`)
	_, errs = ValidateRows(RawRows{Schedules: []json.RawMessage{schM}})
	if !hasErr(errs, "MISFIRE_INVALID", "misfire") {
		t.Errorf("invalid misfire must be rejected (DAT-120), got %+v", errs)
	}
	// validity window with ends_at <= starts_at (DAT-061)
	vw := json.RawMessage(`{"id":"01JV","schedule_id":"01JS","scope_node":"01JN","starts_at":100,"ends_at":100,"revision":1,"created_at":1,"updated_at":1}`)
	_, errs = ValidateRows(RawRows{Schedules: []json.RawMessage{sch}, ValidityWindows: []json.RawMessage{vw}})
	if !hasErr(errs, "VALIDITY_WINDOW_RANGE_INVALID", "ends_at") {
		t.Errorf("ends_at <= starts_at must be rejected (DAT-061), got %+v", errs)
	}
	// preset batch with empty commands (DAT-091)
	pb := json.RawMessage(`{"preset_id":"01JPB","scope_node":"01JN","name":"p","commands":[],"revision":1,"created_at":1,"updated_at":1}`)
	_, errs = ValidateRows(RawRows{PresetBatches: []json.RawMessage{pb}})
	if !hasErr(errs, "PRESET_BATCH_COMMANDS_EMPTY", "commands") {
		t.Errorf("empty commands must be rejected (DAT-091), got %+v", errs)
	}
}

// A complete, consistent row set (the DAT-074 daypart pair plus the playlist and
// preset-batch rows they reference) validates clean.
func TestCompleteConsistentRowSetIsValid(t *testing.T) {
	sch := json.RawMessage(`{"id":"01JSCHED1S3AR0RGXJVZ9CJD33","scope_node":"01JGRPNDE1PG2X7R5JQC42EJ5E","name":"Weekday Lobby","revision":1,"created_at":1,"updated_at":1}`)
	dpOn := json.RawMessage(`{"id":"01JDAYPART2MV7RCG2V0CQV4NM","schedule_id":"01JSCHED1S3AR0RGXJVZ9CJD33","scope_node":"01JGRPNDE1PG2X7R5JQC42EJ5E","name":"Business Hours","days_of_week":[1,2,3,4,5],"start_time":"06:00:00","end_time":"22:00:00","display_power":"on","playlist_id":"01JPAYST15TMGDMFGS8KXM40X6","preset_batch_id":null,"misfire":null,"revision":1,"created_at":1,"updated_at":1}`)
	dpOff := json.RawMessage(`{"id":"01JDAYPART1M33YA35B44FS7F2","schedule_id":"01JSCHED1S3AR0RGXJVZ9CJD33","scope_node":"01JGRPNDE1PG2X7R5JQC42EJ5E","name":"Overnight","days_of_week":[0,1,2,3,4,5,6],"start_time":"22:00:00","end_time":"06:00:00","display_power":"off","playlist_id":null,"preset_batch_id":"01JPRESET1N8GAWV07492Q9V82","misfire":null,"revision":1,"created_at":1,"updated_at":1}`)
	pl := json.RawMessage(`{"id":"01JPAYST15TMGDMFGS8KXM40X6","scope_node":"01JGRPNDE1PG2X7R5JQC42EJ5E","name":"Lobby","items":[],"revision":1,"created_at":1,"updated_at":1}`)
	pb := json.RawMessage(`{"preset_id":"01JPRESET1N8GAWV07492Q9V82","scope_node":"01JGRPNDE1PG2X7R5JQC42EJ5E","name":"Power Down","commands":[{"entity_id":"01JE","command":"power","params":{"state":"off"}}],"revision":1,"created_at":1,"updated_at":1}`)

	rs, errs := ValidateRows(RawRows{
		Schedules: []json.RawMessage{sch}, Dayparts: []json.RawMessage{dpOn, dpOff},
		Playlists: []json.RawMessage{pl}, PresetBatches: []json.RawMessage{pb},
	})
	if len(errs) != 0 {
		t.Fatalf("expected clean validation, got %+v", errs)
	}
	if len(rs.Dayparts) != 2 || len(rs.Schedules) != 1 {
		t.Fatalf("row set not populated: %+v", rs)
	}
}
