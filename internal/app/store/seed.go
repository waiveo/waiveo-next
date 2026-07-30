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
	seedOrgAncestorScopeNodeID = "01J8Z0DEM00RGANCEST0RB0VND"
	seedSiteScopeNodeID        = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	seedScreenScopeNodeID      = "01J8Z4DEM0SCREENF1RSTPH0TN"
	seedScheduleID             = "01J8Z5DEM0SCHEDV1ETW0DAYPT"

	// SeedScreenID is the id of the demo SCREEN IDENTITY ROW (DAT-004/DAT-004a)
	// — the row a `screen_id` names, and the id every derived screen_programs
	// entry for the demo deployment carries (relay/1 REL-061).
	//
	// It is exported because the fixture snapshot path
	// (internal/feeder/snapshot.Build, used by the conformance drivers and the
	// virtual-player first-light proof) has no store to read a screen out of and
	// must still name a REAL screen row rather than an invented placeholder
	// string. Single-sourcing it here is what keeps the fixture path and a
	// freshly-seeded store naming the same screen instead of two.
	//
	// It is deliberately NOT seedScreenScopeNodeID: a `screen`-kind scope node
	// is a placement classification and never a screen identity (DAT-004a), and
	// a `screen_id` that resolves to a node rather than to a screen row MUST be
	// rejected.
	SeedScreenID = "01J8Z9DEM0SCREENR0WF1RSTPH"

	seedScreenName         = "Demo Screen"
	seedPlaylistID         = "01J8Z6DEM0P1AY11STC0NTENT1"
	seedContentDaypartID   = "01J8Z7DEM0DAYPARTC0NTENT01"
	seedOvernightDaypartID = "01J8Z7DEM0DAYPART0VERN1GHT"
	seedPresetBatchID      = "01J8Z8DEM0PRESETBATCHF1RE1"
	seedRuleEntityID       = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"

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

// seedDemoAutomationName is the demo automation's resource-envelope name. The
// openapi Automation schema declares `name` and `scope_node` REQUIRED, and
// neither is defaultable — a name and a placement are authored facts, not
// something a server can invent — so the seed states both rather than shipping a
// demo row that no generated client could read.
const seedDemoAutomationName = "Demo Screen-On Launch"

// seedDemoAutomationJSON is the demo edge rule as an authored automation body: a
// state trigger on seedRuleEntityID rising to "on" firing a device_command
// launch on that same entity (RUL-002 edge, the same shape a fixture-registry
// media-player resolves). Its rules/1 vocabulary is byte-for-byte the rule the
// pre-store feeder hardcoded as demoEdgeRuleJSON, so seeding it here as a
// compile-gated store row changes nothing a fresh make dev shows — only where
// the rule now lives (authored + editable, not a wire-level constant, REL-062).
//
// `enabled` is stated EXPLICITLY, and true. An automation created without it
// comes back disabled (declaredmembers.go), which is the right default for a
// half-finished authoring call and the wrong one for a fixture whose entire
// purpose is to fire: the demo is meant to be live, so the seed says so rather
// than leaning on a default.
var seedDemoAutomationJSON = json.RawMessage(`{"id":"` + seedDemoAutomationID + `",` +
	`"name":"` + seedDemoAutomationName + `","scope_node":"` + seedScreenScopeNodeID + `","enabled":true,` +
	`"mode":"single",` +
	`"triggers":[{"type":"state","entity_id":"` + seedRuleEntityID + `","to":["on"]}],` +
	`"conditions":[],` +
	`"actions":[{"type":"device_command","entity_id":"` + seedRuleEntityID + `","command":"launch","params":{"channel":"dev"}}]}`)

// SeedDemo inserts the make-dev demo as authored store rows: a three-node scope
// tree (the org root, a site, a screen-kind node under it), a SCREEN IDENTITY ROW placed on
// that node, a playlist whose items are assetRefs in order, a schedule on the
// screen node, two non-overlapping dayparts partitioning the day (a content
// daypart 06:00–22:00 playing the playlist and firing a rising-edge preset, and
// a blank overnight daypart 22:00–06:00), and one edge-classified automation
// (seedDemoAutomationJSON) — the same shape the former hardcoded feeder demo
// carried (both the schedule AND the edge_rules constant), so a fresh make dev
// still resolves a program and loads one edge rule, but now all of it is
// editable over the api.
//
// The screen row is what makes the demo deployment produce a screen_programs
// entry at all: that section is resolved one entry per screen ROW (REL-061,
// DAT-004a), so a store with a screen-kind scope node but no screen row has a
// schedule governing nothing and delivers no program.
//
// Rows are inserted in dependency order so each write's pre-commit
// datamodel.ValidateRows / BuildFullScopeTree / ValidateIdentityRows sees a
// referentially complete set: the scope nodes first (a child after its parent),
// then the screen row on the node it is placed under, then the playlist + preset
// before the daypart that references them, the schedule before its dayparts. The
// automation row has no scheduling-core dependency, so its position is
// immaterial; it is inserted last. Every accepted Create bumps the store
// generation in its own transaction.
//
// assetRefs are the content ids the demo playlist's items point at, IN PLAYBACK
// ORDER — one item per ref, so a caller that added N assets to the content origin
// gets an N-item playlist and therefore an N-item derived screen program. Passing
// none is refused rather than seeding a content daypart pointing at an empty
// playlist. SeedDemo is meant for an empty store — the feeder seeds only when the
// generation is still 0.
func (s *Store) SeedDemo(ctx context.Context, assetRefs ...string) error {
	if len(assetRefs) == 0 {
		return fmt.Errorf("store: seed demo: at least one asset ref is required")
	}
	items := make([]datamodel.PlaylistItem, 0, len(assetRefs))
	for _, ref := range assetRefs {
		items = append(items, datamodel.PlaylistItem{Source: "asset", AssetRef: ref})
	}

	orgParent := seedOrgAncestorScopeNodeID
	siteParent := seedSiteScopeNodeID
	tz := seedSiteTZ
	lat := seedSiteLat
	long := seedSiteLong

	rows := []struct {
		kind Kind
		row  any
	}{
		// The org root is a REAL row, not a tolerated boundary: the store
		// validates the full tree, where DAT-002 demands every parent resolve
		// and exactly one org-kind node exist. An org carries no geo (DAT-032).
		{KindScopeNode, datamodel.ScopeNode{
			ID: seedOrgAncestorScopeNodeID, Kind: "org", Name: "Demo Org",
		}},
		{KindScopeNode, datamodel.ScopeNode{
			ID: seedSiteScopeNodeID, Kind: "site", ParentID: &orgParent, Name: "Demo Site",
			TZ: &tz, Lat: &lat, Long: &long,
		}},
		{KindScopeNode, datamodel.ScopeNode{
			ID: seedScreenScopeNodeID, Kind: "screen", ParentID: &siteParent, Name: seedScreenName,
		}},
		{KindScreen, datamodel.Screen{
			ID: SeedScreenID, ScopeNode: seedScreenScopeNodeID, Name: seedScreenName,
		}},
		{KindPlaylist, datamodel.Playlist{
			ID: seedPlaylistID, ScopeNode: seedScreenScopeNodeID, Name: "Demo Content Playlist",
			Items: items,
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
