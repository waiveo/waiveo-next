package api

import (
	"encoding/json"
	"fmt"
	"strings"

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
		displayName:  "schedule",
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
		displayName:  "daypart",
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
		displayName:  "playlist",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
		validate:     validatePlaylistAssets,
	}
}

// validatePlaylistAssets is the playlist kind's pre-write guard (wired as
// resourceConfig.validate): every item carrying an asset_ref MUST name content
// already present in the shared content origin (origin.Store.Has) — you cannot
// schedule content that was never uploaded, so a resolved Lease can never point a
// screen at a byte range this origin cannot serve (data-model/1 DAT-041). The
// asset_ref is content-addressed (`sha256:<hex>`); the hex, minus the prefix, is
// the origin's key.
//
// A missing asset yields a per-field REFERENCE_INVALID error NAMING the offending
// asset_ref (rendered as the api/1 `errors` extension by writeValidationFailed, so
// the create/update is refused 422 before it reaches the store). An item without
// an asset_ref (a `playable` pack item) needs no origin content and is skipped,
// mirroring schedulehost.playlistContent's own asset-only projection.
func validatePlaylistAssets(srv *server, body []byte) []datamodel.Error {
	var pl struct {
		Items []datamodel.PlaylistItem `json:"items"`
	}
	if err := json.Unmarshal(body, &pl); err != nil {
		return nil // a malformed body surfaces its real error on the store write.
	}
	var errs []datamodel.Error
	for i, item := range pl.Items {
		if item.AssetRef == "" {
			continue // a pack `playable` item has no origin content to resolve.
		}
		hexDigest := strings.TrimPrefix(item.AssetRef, "sha256:")
		if srv.content == nil || !srv.content.Has(hexDigest) {
			errs = append(errs, datamodel.Error{
				Field: fmt.Sprintf("items[%d].asset_ref", i),
				Code:  "REFERENCE_INVALID",
				Message: fmt.Sprintf(
					"asset_ref %s is not present in the content origin; upload the asset before scheduling it",
					item.AssetRef),
			})
		}
	}
	return errs
}
