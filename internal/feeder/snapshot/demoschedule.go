package snapshot

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// This file authors the Wave-2 demo schedule (REL-065): a two-scope-node
// tree (the first-photon site + its screen) and one schedule on the screen
// node with two non-overlapping dayparts partitioning the day (DAT-073) — a
// content daypart covering the bulk of the day and a blank overnight
// daypart. Build (snapshot.go) carries the result opaquely in
// sections.schedule, exactly as relay/1 (REL-065) requires: this package
// authors real data-model/1 rows but never resolves them — the relay
// derives (a later task, internal/relay/schedulehost), it does not
// re-implement (data-model/1 line 391).
//
// Every row's identity is a fixture-ULID-style constant, matching the
// existing first-photon fixtures (demoRuleEntityID etc, and the site scope
// node id cmd/waiveo-feeder's own firstPhotonSite/hello.SiteBinding already
// binds a relay to, REL-036) — so the demo schedule's site node is the SAME
// site a relay's hello handshake already knows, never a second, disagreeing
// one.
const (
	// demoOrgAncestorScopeNodeID is referenced only as the demo site node's
	// parent_id: a non-org scope node MUST declare a non-null parent_id
	// (DAT-002), but this carried section is a subtree (REL-065) — the org
	// root itself is out of scope, so this id is deliberately absent from
	// the carried scope-node set. BuildScopeTree treats an absent-parent
	// reference as a subtree boundary, not a referential-integrity error
	// (its own documented tolerance).
	demoOrgAncestorScopeNodeID = "01J8Z0DEMOORGANCESTORBOUND"

	// demoSiteScopeNodeID is the SAME site scope node id
	// cmd/waiveo-feeder's firstPhotonSite (REL-036) binds a relay to via
	// hello.SiteBinding — the demo schedule's site node carries the
	// identical tz/lat/long as firstPhotonSiteEffective (below), so the two
	// can never disagree about this site's placement.
	demoSiteScopeNodeID = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

	demoScreenScopeNodeID  = "01J8Z4DEMOSCREENFIRSTPHOTN"
	demoScheduleID         = "01J8Z5DEMOSCHEDULETWODAYPT"
	demoPlaylistID         = "01J8Z6DEMOPLAYLISTCONTENT1"
	demoContentDaypartID   = "01J8Z7DEMODAYPARTCONTENT01"
	demoOvernightDaypartID = "01J8Z7DEMODAYPARTOVERNIGHT"
	demoPresetBatchID      = "01J8Z8DEMOPRESETBATCHFIRE1"

	// demoRowTimestampMs is a fixed, deterministic created_at/updated_at for
	// every demo row (a fixture instant, not a real wall-clock read) —
	// matching the fixed-timestamp convention this package's own tests
	// already use for fixture data (e.g. TestBuildWithGrantsRidesAndVerifies's
	// IssuedAt).
	demoRowTimestampMs = 1752537000000

	// demoContentStartTime/demoContentEndTime/demoOvernightStartTime/
	// demoOvernightEndTime are the two dayparts' half-open coverage
	// (DAT-071-073): content 06:00-22:00, overnight 22:00-06:00 (wrapping),
	// every day of the week — the two partition every day with no gap and
	// no overlap (the same abutting-half-open pattern DAT-073's own
	// accepted-partition case uses).
	demoContentStartTime   = "06:00:00"
	demoContentEndTime     = "22:00:00"
	demoOvernightStartTime = "22:00:00"
	demoOvernightEndTime   = "06:00:00"
)

// demoAllWeek is every weekday (DAT-071's 0=Sunday..6=Saturday), so both
// dayparts hold every day — the demo partitions each individual day
// identically, with no weekday-dependent gap.
var demoAllWeek = []int{0, 1, 2, 3, 4, 5, 6}

// buildDemoScheduleSection authors the Wave-2 demo schedule (REL-065): a
// two-scope-node tree (the first-photon site + screen) and a schedule on
// the screen node with two non-overlapping dayparts (DAT-073) — a content
// daypart (display_power:on, playlist_id -> a playlist whose single item is
// assetRef, the SAME asset_ref the screen_programs item shows) covering
// 06:00-22:00, and a blank overnight daypart (display_power:blank) covering
// 22:00-06:00. The content daypart's preset_batch_id fires one
// device_command against the demoRuleEntityID fixture entity on its rising
// edge (DAT-075, a later relay task fires it). Every row carries the api/1
// resource baseline (id/preset_id + revision) and scope_node; none carries
// a pack-ownership field (DAT-101). The returned section is Normalized
// (REL-060: every array marshals as `[]`, never `null`).
func buildDemoScheduleSection(assetRef string) (wire.ScheduleSection, error) {
	siteTZ := firstPhotonSiteEffective.TZ
	siteLat := firstPhotonSiteEffective.Lat
	siteLong := firstPhotonSiteEffective.Long
	orgParentID := demoOrgAncestorScopeNodeID
	siteParentID := demoSiteScopeNodeID

	siteNode := datamodel.ScopeNode{
		ID:        demoSiteScopeNodeID,
		Kind:      "site",
		ParentID:  &orgParentID,
		Name:      "Demo Site",
		TZ:        &siteTZ,
		Lat:       &siteLat,
		Long:      &siteLong,
		Revision:  1,
		CreatedAt: demoRowTimestampMs,
		UpdatedAt: demoRowTimestampMs,
	}
	screenNode := datamodel.ScopeNode{
		ID:        demoScreenScopeNodeID,
		Kind:      "screen",
		ParentID:  &siteParentID,
		Name:      "Demo Screen",
		Revision:  1,
		CreatedAt: demoRowTimestampMs,
		UpdatedAt: demoRowTimestampMs,
	}

	schedule := datamodel.Schedule{
		ID:        demoScheduleID,
		ScopeNode: demoScreenScopeNodeID,
		Name:      "Demo Two-Daypart Schedule",
		Revision:  1,
		CreatedAt: demoRowTimestampMs,
		UpdatedAt: demoRowTimestampMs,
	}
	playlist := datamodel.Playlist{
		ID:        demoPlaylistID,
		ScopeNode: demoScreenScopeNodeID,
		Name:      "Demo Content Playlist",
		Items: []datamodel.PlaylistItem{
			{Source: "asset", AssetRef: assetRef},
		},
		Revision:  1,
		CreatedAt: demoRowTimestampMs,
		UpdatedAt: demoRowTimestampMs,
	}
	presetBatch := datamodel.PresetBatch{
		PresetID:  demoPresetBatchID,
		ScopeNode: demoScreenScopeNodeID,
		Name:      "Demo Content Rising-Edge Preset",
		Commands: []datamodel.PresetCommand{
			{
				EntityID: demoRuleEntityID,
				Command:  "launch",
				Params:   json.RawMessage(`{"channel":"schedule-demo"}`),
			},
		},
		Revision:  1,
		CreatedAt: demoRowTimestampMs,
		UpdatedAt: demoRowTimestampMs,
	}
	contentDaypart := datamodel.Daypart{
		ID:            demoContentDaypartID,
		ScheduleID:    demoScheduleID,
		ScopeNode:     demoScreenScopeNodeID,
		DaysOfWeek:    demoAllWeek,
		StartTime:     demoContentStartTime,
		EndTime:       demoContentEndTime,
		DisplayPower:  "on",
		PlaylistID:    demoPlaylistID,
		PresetBatchID: demoPresetBatchID,
		Name:          "Content Hours",
		Revision:      1,
		CreatedAt:     demoRowTimestampMs,
		UpdatedAt:     demoRowTimestampMs,
	}
	overnightDaypart := datamodel.Daypart{
		ID:           demoOvernightDaypartID,
		ScheduleID:   demoScheduleID,
		ScopeNode:    demoScreenScopeNodeID,
		DaysOfWeek:   demoAllWeek,
		StartTime:    demoOvernightStartTime,
		EndTime:      demoOvernightEndTime,
		DisplayPower: "blank",
		Name:         "Overnight Blank",
		Revision:     1,
		CreatedAt:    demoRowTimestampMs,
		UpdatedAt:    demoRowTimestampMs,
	}

	scopeNodesRaw, err := marshalEachRow(siteNode, screenNode)
	if err != nil {
		return wire.ScheduleSection{}, err
	}
	schedulesRaw, err := marshalEachRow(schedule)
	if err != nil {
		return wire.ScheduleSection{}, err
	}
	playlistsRaw, err := marshalEachRow(playlist)
	if err != nil {
		return wire.ScheduleSection{}, err
	}
	daypartsRaw, err := marshalEachRow(contentDaypart, overnightDaypart)
	if err != nil {
		return wire.ScheduleSection{}, err
	}
	presetBatchesRaw, err := marshalEachRow(presetBatch)
	if err != nil {
		return wire.ScheduleSection{}, err
	}

	return wire.ScheduleSection{
		ScopeNodes:    scopeNodesRaw,
		Playlists:     playlistsRaw,
		Schedules:     schedulesRaw,
		Dayparts:      daypartsRaw,
		PresetBatches: presetBatchesRaw,
	}.Normalized(), nil
}

// marshalEachRow marshals each value to json.RawMessage, for building a
// ScheduleSection's opaquely-carried row arrays (REL-065: relay/1 does not
// own these row shapes, and never interprets them — it only forwards them
// for the relay's own data-model/1 parser to unmarshal).
func marshalEachRow(vs ...any) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(vs))
	for _, v := range vs {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}
