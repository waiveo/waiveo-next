package api

import (
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// schedulesConfig, daypartsConfig, and playlistsConfig are the resource
// configurations for the three data-model/1 scheduling-core kinds this task
// exposes over api/1 (the openapi's authoring surface for schedules/dayparts/
// playlists — the remaining scheduling-core kinds, validity-windows/fallbacks/
// preset-batches, are a deferred follow-up using this identical pattern).
//
// Every scheduling-core row's OWN scope_node is both its placement (what a
// selector's `scope_node`/`scope_node subtree` term evaluates against) and its
// external_id uniqueness grouping (API-101: two rows may share an external_id
// only under different scope nodes) AND the node a write of it is authorized at
// — unlike a scope node, whose placement is itself but whose external_id groups
// by, and whose writes authorize at, its PARENT (scopenodes.go).
//
// A daypart row carries neither external_id nor labels (data-model/1 DAT-070);
// selLabels/extScope simply read the zero value for it through the SAME generic
// resourceFields projection every kind shares (parseFields leaves them "" / nil
// when the JSON keys are absent), so no daypart-specific branch is needed
// anywhere in this file or in api.go — this is what "the SAME resource-handler
// helper applied ... identical conventions" means in practice.
//
// None of the three configs below special-case validation: a create/update whose
// datamodel validation fails — DAT-073 daypart overlap, a dangling schedule_id/
// playlist_id/preset_batch_id reference, a DAT-074/120 closed-vocabulary
// violation, ... — is mapped to 422/VALIDATION_FAILED carrying the datamodel
// error list as the api/1 `errors` extension member (API-013) by
// writeStoreError (api.go), which every mounted resource already shares.
func schedulesConfig() resourceConfig {
	return resourceConfig{
		// The MEMBER half of schema enforcement, not the whole schema
		// (bodyschema.go undeclaredMemberRejected). These families' per-field rules
		// live in the datamodel validators, which report EVERY failing member at once
		// under data-model/1's published codes; a fail-fast whole-schema gate ahead of
		// them would replace that with one poorer message. What the datamodel cannot
		// see is a member it does not define — by the time it validates, an undeclared
		// member has already vanished into the decoded row — so it was accepted,
		// stored, and served back on every read.
		createMembers: "ScheduleCreate",
		updateMembers: "ScheduleUpdate",
		kind:          store.KindSchedule,
		path:          "schedules",
		resourceType:  "schedules",
		displayName:   "schedule",
		selLabels:     func(f resourceFields) map[string]string { return f.Labels },
		placement:     func(f resourceFields) string { return f.ScopeNode },
		extScope:      func(f resourceFields) string { return f.ScopeNode },
		writeScope:    func(f resourceFields) string { return f.ScopeNode },
	}
}

func daypartsConfig() resourceConfig {
	return resourceConfig{
		// The MEMBER half of schema enforcement, not the whole schema
		// (bodyschema.go undeclaredMemberRejected). These families' per-field rules
		// live in the datamodel validators, which report EVERY failing member at once
		// under data-model/1's published codes; a fail-fast whole-schema gate ahead of
		// them would replace that with one poorer message. What the datamodel cannot
		// see is a member it does not define — by the time it validates, an undeclared
		// member has already vanished into the decoded row — so it was accepted,
		// stored, and served back on every read.
		createMembers: "DaypartCreate",
		updateMembers: "DaypartUpdate",
		kind:          store.KindDaypart,
		path:          "dayparts",
		resourceType:  "dayparts",
		displayName:   "daypart",
		selLabels:     func(f resourceFields) map[string]string { return f.Labels },
		placement:     func(f resourceFields) string { return f.ScopeNode },
		extScope:      func(f resourceFields) string { return f.ScopeNode },
		writeScope:    func(f resourceFields) string { return f.ScopeNode },
	}
}

func playlistsConfig() resourceConfig {
	return resourceConfig{
		// The MEMBER half of schema enforcement, not the whole schema
		// (bodyschema.go undeclaredMemberRejected). These families' per-field rules
		// live in the datamodel validators, which report EVERY failing member at once
		// under data-model/1's published codes; a fail-fast whole-schema gate ahead of
		// them would replace that with one poorer message. What the datamodel cannot
		// see is a member it does not define — by the time it validates, an undeclared
		// member has already vanished into the decoded row — so it was accepted,
		// stored, and served back on every read.
		createMembers: "PlaylistCreate",
		updateMembers: "PlaylistUpdate",
		kind:          store.KindPlaylist,
		path:          "playlists",
		resourceType:  "playlists",
		displayName:   "playlist",
		selLabels:     func(f resourceFields) map[string]string { return f.Labels },
		placement:     func(f resourceFields) string { return f.ScopeNode },
		extScope:      func(f resourceFields) string { return f.ScopeNode },
		writeScope:    func(f resourceFields) string { return f.ScopeNode },
		validate:      validatePlaylistAssets,
		writeGuards:   playlistAssetGuards,
	}
}

// playlistAssetGuards and validatePlaylistAssets are the playlist kind's
// mounting of the ONE asset-reference rule, whose implementation and full
// rationale live in assetrefs.go. They are named functions rather than inline
// closures so the guard set a playlist write assembles is greppable from the
// config above, and so the in-transaction guard has a name the retention sweep's
// own doc can point at.
//
// The projection they check is store.RowAssetReferences, which for a playlist
// covers BOTH an item's own `asset_ref` and the image layers of an inline
// `source: "slide"` item — an item whose content is its layer stack carries no
// item-level asset_ref, so the hand-written check this replaced saw an inline
// slide as referencing nothing at all and let its images through un-gated.
func validatePlaylistAssets(srv *server, body []byte) []datamodel.Error {
	return validateRowAssets(srv, store.KindPlaylist, body)
}

func playlistAssetGuards(srv *server, body []byte) []store.WriteGuard {
	return rowAssetGuards(srv, store.KindPlaylist, body)
}
