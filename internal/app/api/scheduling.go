package api

import "github.com/maaxton/waiveo-next/internal/app/store"

// schedulesConfig, daypartsConfig, and playlistsConfig are the resource
// configurations for the three data-model/1 scheduling-core kinds this task
// exposes over api/1 (the openapi's authoring surface for schedules/dayparts/
// playlists — the remaining scheduling-core kinds, validity-windows/fallbacks/
// preset-batches, are a deferred follow-up using this identical pattern).
//
// Every scheduling-core row's OWN scope_node is both its placement (what a
// selector's `scope_node`/`scope_node subtree` term evaluates against) and its
// external_id uniqueness grouping (API-101: two rows may share an external_id
// only under different scope nodes) — unlike a scope node, whose placement is
// itself but whose external_id groups by its PARENT (scopenodes.go).
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
		kind:         store.KindSchedule,
		path:         "schedules",
		resourceType: "schedules",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
	}
}

func daypartsConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindDaypart,
		path:         "dayparts",
		resourceType: "dayparts",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
	}
}

func playlistsConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindPlaylist,
		path:         "playlists",
		resourceType: "playlists",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
	}
}
