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

// playlistAssetGuards re-checks, INSIDE the store's write transaction, that every
// asset_ref the row being written names is still present in the content origin.
//
// It is the same rule validatePlaylistAssets applies before the write, and it is
// not a redundant second copy of it. The pre-write check runs in its own critical
// section: between it and the store write, the content retention sweep
// (internal/feeder/contentgc) can reclaim an asset that was present when the
// check ran. That interleaving stores a playlist whose item resolves to a URL the
// origin 404s — the api answers 201, and every screen the playlist plays on shows
// nothing. It is exactly the check-then-write race the external_id guard beside
// it was introduced to close, on a rule whose failure is visible in a shop window.
//
// The sweep holds the store's WRITE LOCK across its reference read and its
// deletions (store.WithContentReferences), and this guard runs inside a write
// transaction under that same lock, so the two are mutually exclusive: either the
// playlist commits first and the sweep then sees the asset referenced and keeps
// it, or the sweep deletes first and this guard refuses the playlist with the
// REFERENCE_INVALID the client would have got a moment earlier. There is no
// interleaving in which a stored row references reclaimed bytes.
//
// The check is deliberately scoped to the row BEING WRITTEN rather than to the
// resulting full playlist set (which is how the datamodel validators judge a
// write). An asset that goes missing outside this path — a corrupted file Open
// declines to load — would, under a whole-set check, make every subsequent
// playlist write anywhere in the workspace fail on account of an unrelated row:
// a write-dead store, from a fault this rule was not written to detect.
//
// existing is ignored: presence is a fact about the content origin, not about the
// other rows. The parameter is the WriteGuard contract's, and taking it is what
// buys the in-transaction position.
func playlistAssetGuards(srv *server, body []byte) []store.WriteGuard {
	return []store.WriteGuard{func([]store.Resource) error {
		if errs := validatePlaylistAssets(srv, body); len(errs) > 0 {
			return &store.ValidationError{Errors: errs}
		}
		return nil
	}}
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
