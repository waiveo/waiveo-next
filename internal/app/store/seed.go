package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// Demo scope-node + scheduling-core identities the make-dev seed inserts. They
// mirror the former hardcoded feeder demo (the shape internal/feeder/snapshot's
// demoschedule.go carries) so a fresh make dev resolves the same two-daypart
// program — now from authored, editable store rows rather than a constant.
//
// seedSiteScopeNodeID is deliberately the SAME id cmd/waiveo-feeder's
// firstPhotonSite (relay/1 REL-036) binds a relay to via the hello handshake, so
// the seeded site node and the relay's adopted site_binding never name two
// different sites. Every id is a fixture ULID (no secrets); the site geo is
// documentation-range placement data.
const (
	seedOrgAncestorScopeNodeID = "01J8Z0DEMOORGANCESTORBOUND"
	seedSiteScopeNodeID        = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	seedScreenScopeNodeID      = "01J8Z4DEMOSCREENFIRSTPHOTN"
	seedScheduleID             = "01J8Z5DEMOSCHEDULETWODAYPT"
	seedPlaylistID             = "01J8Z6DEMOPLAYLISTCONTENT1"
	seedContentDaypartID       = "01J8Z7DEMODAYPARTCONTENT01"
	seedOvernightDaypartID     = "01J8Z7DEMODAYPARTOVERNIGHT"
	seedPresetBatchID          = "01J8Z8DEMOPRESETBATCHFIRE1"
	seedRuleEntityID           = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"

	// seedDemoAutomationID is the SAME rule id the pre-store feeder hardcoded as
	// demoRuleID (internal/feeder/snapshot's former demoEdgeRuleJSON constant),
	// so a fresh make dev still loads the identical edge rule — now an authored,
	// editable store row rather than a wire-level constant (REL-062).
	seedDemoAutomationID = "01J8Z3K4N5P6Q7R8S9T0V1AT01"

	seedSiteTZ   = "America/Chicago"
	seedSiteLat  = 41.8781
	seedSiteLong = -87.6298

	// The two dayparts' half-open coverage (DAT-071–073): content 06:00–22:00 and
	// a blank overnight 22:00–06:00 (wrapping), abutting with no gap and no
	// overlap — a valid every-day partition.
	seedContentStartTime   = "06:00:00"
	seedContentEndTime     = "22:00:00"
	seedOvernightStartTime = "22:00:00"
	seedOvernightEndTime   = "06:00:00"
)

// seedAllWeek is every weekday (DAT-071 0=Sunday..6=Saturday), so both dayparts
// hold every day.
var seedAllWeek = []int{0, 1, 2, 3, 4, 5, 6}

// seedDemoAutomationJSON is the demo edge rule as an authored automation body: a
// state trigger on seedRuleEntityID rising to "on" firing a device_command
// launch on that same entity (RUL-002 edge, the same shape a fixture-registry
// media-player resolves). It is byte-for-byte the rule the pre-store feeder
// hardcoded as demoEdgeRuleJSON, so seeding it here as a compile-gated store row
// changes nothing a fresh make dev shows — only where the rule now lives
// (authored + editable, not a wire-level constant, REL-062).
var seedDemoAutomationJSON = json.RawMessage(`{"id":"` + seedDemoAutomationID + `","mode":"single",` +
	`"triggers":[{"type":"state","entity_id":"` + seedRuleEntityID + `","to":["on"]}],` +
	`"conditions":[],` +
	`"actions":[{"type":"device_command","entity_id":"` + seedRuleEntityID + `","command":"launch","params":{"channel":"dev"}}]}`)

// SeedDemo inserts the make-dev demo as authored store rows: a two-node scope
// tree (a site + a screen under it), a playlist whose one item is assetRef, a
// schedule on the screen, two non-overlapping dayparts partitioning the day (a
// content daypart 06:00–22:00 playing the playlist and firing a rising-edge
// preset, and a blank overnight daypart 22:00–06:00), and one edge-classified
// automation (seedDemoAutomationJSON) — the same shape the former hardcoded
// feeder demo carried (both the schedule AND the edge_rules constant), so a
// fresh make dev still resolves a program and loads one edge rule, but now both
// are editable over the api.
//
// Rows are inserted in dependency order so each write's pre-commit
// datamodel.ValidateRows / BuildScopeTree sees a referentially complete set: the
// scope nodes first (a child after its parent), then the playlist + preset before
// the daypart that references them, the schedule before its dayparts. The
// automation row has no scheduling-core dependency, so its position is
// immaterial; it is inserted last. Every accepted Create bumps the store
// generation in its own transaction, so a freshly seeded store lands at
// generation 8. assetRef is the content id the demo playlist item points at (the
// same asset the feeder's screen_programs item shows). SeedDemo is meant for an
// empty store — the feeder seeds only when the generation is still 0.
func (s *Store) SeedDemo(ctx context.Context, assetRef string) error {
	orgParent := seedOrgAncestorScopeNodeID
	siteParent := seedSiteScopeNodeID
	tz := seedSiteTZ
	lat := seedSiteLat
	long := seedSiteLong

	rows := []struct {
		kind Kind
		row  any
	}{
		{KindScopeNode, datamodel.ScopeNode{
			ID: seedSiteScopeNodeID, Kind: "site", ParentID: &orgParent, Name: "Demo Site",
			TZ: &tz, Lat: &lat, Long: &long,
		}},
		{KindScopeNode, datamodel.ScopeNode{
			ID: seedScreenScopeNodeID, Kind: "screen", ParentID: &siteParent, Name: "Demo Screen",
		}},
		{KindPlaylist, datamodel.Playlist{
			ID: seedPlaylistID, ScopeNode: seedScreenScopeNodeID, Name: "Demo Content Playlist",
			Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: assetRef}},
		}},
		{KindSchedule, datamodel.Schedule{
			ID: seedScheduleID, ScopeNode: seedScreenScopeNodeID, Name: "Demo Two-Daypart Schedule",
		}},
		{KindPresetBatch, datamodel.PresetBatch{
			PresetID: seedPresetBatchID, ScopeNode: seedScreenScopeNodeID,
			Name: "Demo Content Rising-Edge Preset",
			Commands: []datamodel.PresetCommand{{
				EntityID: seedRuleEntityID, Command: "launch",
				Params: json.RawMessage(`{"channel":"schedule-demo"}`),
			}},
		}},
		{KindDaypart, datamodel.Daypart{
			ID: seedContentDaypartID, ScheduleID: seedScheduleID, ScopeNode: seedScreenScopeNodeID,
			DaysOfWeek: seedAllWeek, StartTime: seedContentStartTime, EndTime: seedContentEndTime,
			DisplayPower: "on", PlaylistID: seedPlaylistID, PresetBatchID: seedPresetBatchID,
			Name: "Content Hours",
		}},
		{KindDaypart, datamodel.Daypart{
			ID: seedOvernightDaypartID, ScheduleID: seedScheduleID, ScopeNode: seedScreenScopeNodeID,
			DaysOfWeek: seedAllWeek, StartTime: seedOvernightStartTime, EndTime: seedOvernightEndTime,
			DisplayPower: "blank", Name: "Overnight Blank",
		}},
		{KindAutomation, seedDemoAutomationJSON},
	}

	for _, r := range rows {
		body, err := json.Marshal(r.row)
		if err != nil {
			return fmt.Errorf("store: seed demo: marshal %s: %w", r.kind, err)
		}
		if _, err := s.Create(ctx, r.kind, body); err != nil {
			return fmt.Errorf("store: seed demo: create %s: %w", r.kind, err)
		}
	}
	return nil
}
