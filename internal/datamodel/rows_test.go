package datamodel

import (
	"encoding/json"
	"testing"
)

// topKeys marshals v and returns the set of its top-level JSON object keys.
func topKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	keys := map[string]bool{}
	for k := range m {
		keys[k] = true
	}
	return keys
}

func wantKeys(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	for _, k := range want {
		if !got[k] {
			t.Errorf("missing wire field %q (have %v)", k, got)
		}
	}
}

// Each of the six row kinds MUST marshal to its Wire-shape field names (DAT-005–008,
// DAT-040/050/060/070/080/090).
func TestRowsMarshalToWireFieldNames(t *testing.T) {
	pl := Playlist{
		ID: "01JPAYST15TMGDMFGS8KXM40X6", ScopeNode: "01JGRPNDE1PG2X7R5JQC42EJ5E", Name: "Lobby Rotation",
		Items:    []PlaylistItem{{Source: "asset", AssetRef: "sha256:abc"}, {Source: "playable", PackID: "acme-weather", ContentID: "forecast-strip"}},
		Revision: 2, CreatedAt: 1752537600000, UpdatedAt: 1752537600000,
	}
	wantKeys(t, topKeys(t, pl), "id", "scope_node", "name", "items", "revision", "created_at", "updated_at")
	// A playlist item carries source/asset_ref/pack_id/content_id (DAT-041).
	ik := topKeys(t, pl.Items[1])
	wantKeys(t, ik, "source", "pack_id", "content_id")

	pri := 0
	sch := Schedule{
		ID: "01JSCHED1S3AR0RGXJVZ9CJD33", ScopeNode: "01JGRPNDE1PG2X7R5JQC42EJ5E", Name: "Weekday Lobby",
		FallbackID: "01JFABACK169HJDNDGZG35VH20", Priority: &pri, Misfire: "catch_up_once",
		Revision: 1, CreatedAt: 1752537600000, UpdatedAt: 1752537600000,
	}
	wantKeys(t, topKeys(t, sch), "id", "scope_node", "name", "fallback_id", "priority", "misfire", "revision", "created_at", "updated_at")

	vw := ValidityWindow{
		ID: "01JVAWN14DG8P4FQJAWK0K68G7", ScheduleID: "01JSCHED1S3AR0RGXJVZ9CJD33", ScopeNode: "01JGRPNDE1PG2X7R5JQC42EJ5E",
		StartsAt: ptrI64(1752537600000), EndsAt: nil, Revision: 1, CreatedAt: 1752537600000, UpdatedAt: 1752537600000,
	}
	// starts_at and ends_at are always present (ends_at emitted as null, DAT-061).
	wantKeys(t, topKeys(t, vw), "id", "schedule_id", "scope_node", "starts_at", "ends_at", "revision", "created_at", "updated_at")

	dp := Daypart{
		ID: "01JDAYPART1M33YA35B44FS7F2", ScheduleID: "01JSCHED1S3AR0RGXJVZ9CJD33", ScopeNode: "01JGRPNDE1PG2X7R5JQC42EJ5E",
		Name: "Overnight", DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "22:00:00", EndTime: "06:00:00",
		DisplayPower: "off", PresetBatchID: "01JPRESET1N8GAWV07492Q9V82",
		Revision: 1, CreatedAt: 1752537600000, UpdatedAt: 1752537600000,
	}
	wantKeys(t, topKeys(t, dp), "id", "schedule_id", "scope_node", "days_of_week", "start_time", "end_time", "display_power", "preset_batch_id", "revision", "created_at", "updated_at")

	fb := Fallback{
		ID: "01JFABACK169HJDNDGZG35VH20", ScopeNode: "01JGRPNDE1PG2X7R5JQC42EJ5E", Name: "Default Lobby Content",
		DisplayPower: "on", PlaylistID: "01JPAYST15TMGDMFGS8KXM40X6", Revision: 1, CreatedAt: 1752537600000, UpdatedAt: 1752537600000,
	}
	wantKeys(t, topKeys(t, fb), "id", "scope_node", "name", "display_power", "playlist_id", "revision", "created_at", "updated_at")

	// A preset-batch row's identity field is preset_id, not id (DAT-005/090).
	pb := PresetBatch{
		PresetID: "01JPRESET1N8GAWV07492Q9V82", ScopeNode: "01JGRPNDE1PG2X7R5JQC42EJ5E", Name: "Power Down Lobby Devices",
		Commands: []PresetCommand{{EntityID: "01JENTTYAKQ2PDF6PT9FABT1BN", Command: "power", Params: json.RawMessage(`{"state":"off"}`)}},
		Revision: 4, CreatedAt: 1752537600000, UpdatedAt: 1752537600000,
	}
	k := topKeys(t, pb)
	wantKeys(t, k, "preset_id", "scope_node", "name", "commands", "revision", "created_at", "updated_at")
	if k["id"] {
		t.Errorf("preset batch must NOT carry an `id` field; its identity is preset_id (DAT-005/090)")
	}
}

// The Daypart/PresetBatch wire shapes from contracts/data-model-1.md round-trip
// through the structs without dropping populated fields.
func TestPresetBatchOutcomeRoundTrips(t *testing.T) {
	wire := []byte(`{
	  "preset_id": "01JPRESET1N8GAWV07492Q9V82",
	  "scope_node": "01JGRPNDE1PG2X7R5JQC42EJ5E",
	  "name": "Power Down Lobby Devices",
	  "commands": [
	    { "entity_id": "01JENTTYAKQ2PDF6PT9FABT1BN", "command": "power", "params": { "state": "off" } }
	  ],
	  "last_outcome": {
	    "outcome": "partial",
	    "results": [
	      { "target": "01JENTTYAKQ2PDF6PT9FABT1BN", "command": "power", "ok": true },
	      { "target": "01JENTTYBTFHA6R2YECXPKEE1C", "command": "power", "ok": false, "error": "unreachable" }
	    ],
	    "evaluated_at": 1752537600000
	  },
	  "revision": 4, "created_at": 1752537600000, "updated_at": 1752537600000
	}`)
	var pb PresetBatch
	if err := json.Unmarshal(wire, &pb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pb.PresetID != "01JPRESET1N8GAWV07492Q9V82" || len(pb.Commands) != 1 {
		t.Fatalf("preset batch decoded wrong: %+v", pb)
	}
	if pb.LastOutcome == nil || pb.LastOutcome.Outcome != "partial" || pb.LastOutcome.EvaluatedAt != 1752537600000 {
		t.Fatalf("last_outcome decoded wrong: %+v", pb.LastOutcome)
	}
	if len(pb.LastOutcome.Results) != 2 || pb.LastOutcome.Results[1].OK != false || pb.LastOutcome.Results[1].Error != "unreachable" {
		t.Fatalf("last_outcome.results decoded wrong: %+v", pb.LastOutcome.Results)
	}
}

// priority defaults to 0 when absent (DAT-050); daypart effective misfire falls
// daypart -> schedule -> catch_up_once (DAT-076/121).
func TestEffectivePriorityAndMisfireDefaults(t *testing.T) {
	if (Schedule{}).EffectivePriority() != 0 {
		t.Errorf("absent priority must default to 0 (DAT-050)")
	}
	p := 7
	if (Schedule{Priority: &p}).EffectivePriority() != 7 {
		t.Errorf("declared priority must be honored (DAT-050)")
	}
	if got := (Daypart{}).EffectiveMisfire(Schedule{}); got != "catch_up_once" {
		t.Errorf("neither declares misfire -> catch_up_once, got %q (DAT-076)", got)
	}
	if got := (Daypart{}).EffectiveMisfire(Schedule{Misfire: "skip"}); got != "skip" {
		t.Errorf("daypart absent -> schedule misfire, got %q (DAT-076)", got)
	}
	if got := (Daypart{Misfire: "fire_each"}).EffectiveMisfire(Schedule{Misfire: "skip"}); got != "fire_each" {
		t.Errorf("daypart own misfire wins, got %q (DAT-076)", got)
	}
}

func ptrI64(v int64) *int64 { return &v }
