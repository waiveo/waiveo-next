package api

import (
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// casts.go mounts the authored slidecast (data-model/1 DAT-043) as an ORDINARY
// api/1 resource family, through the same generic resourceConfig every other
// authored kind goes through.
//
// # Why a cast is its own family
//
// A cast is the unit an operator actually works in: the ordered set of slides
// they build, name, and hand to a screen. Everything else in the scheduling core
// is the machinery that decides WHEN it plays — a playlist item references it, a
// daypart plays that playlist, a schedule holds that daypart. Modelling it as a
// row rather than as a shape inlined into a playlist item is what makes one edit
// reach every screen playing it; an inlined copy would make "change the menu" an
// edit of every playlist that shows it, which is the operator experience this
// rewrite exists to replace rather than reproduce.
//
// Mounting it here rather than as a bespoke surface is what gives it, in one
// step and with no per-kind branch, the label selector, the keyset cursor scoped
// to its own resource type, If-Match/revision, external_id uniqueness under its
// placement, the visible-set read filter, placement authorization on writes, the
// audit record every mutation files, and the 422/400 validation split.
//
// # Why the FULL schema gate, where the scheduling-core families run the member half
//
// The three families beside it (schedules, dayparts, playlists) declare no
// request body at all in api/openapi.yaml and so run only the member half
// (scheduling.go explains that trade: their per-field rules live in the
// datamodel validators, which report EVERY failing member at once, and a
// fail-fast whole-schema gate ahead of them would replace that with one poorer
// message).
//
// A cast declares its full request schema, so it runs the whole gate — and the
// multi-error argument is not lost by doing so, because the two checks divide
// the rules cleanly rather than overlapping. The document expresses the SHAPE (a
// closed member set, the layer-kind enum, a `#RRGGBB` color, a non-empty slides
// array), and a violation of shape is a client that is malformed rather than an
// operator whose slide is subtly wrong. Everything an operator can plausibly get
// wrong while holding a well-formed editor open — a layer that runs off the
// canvas, a duplicate slide id, an image layer with no asset_ref, a cast_id
// naming nothing — is a data-model rule, validated inside the store write, and
// still answered as API-013's multi-field `errors[]` with every failing slide
// named at once (datamodel.checkCastSlides).
//
// That is also why no `validate` hook appears below. The layer rules are
// wire.ValidateAuthoredSlideLayers's, applied by datamodel.ValidateRows over the
// resulting full row-set — the SAME gate the two content projections re-apply
// before serving a slide — so there is exactly one copy of them and an accepted
// cast is by construction a cast a screen can be served.

// castsConfig is the resource configuration for the cast kind. Its own
// scope_node is its placement, its external_id grouping, and the node a write of
// it is authorized at — the same three-way coincidence every kind but a scope
// node has (scheduling.go explains why they are nonetheless separate
// projections).
//
// No deleteGuards: the rule that a cast a playlist still references cannot be
// deleted is enforced where every other referential rule in the scheduling core
// is, by datamodel.ValidateRows re-running over the row-set the delete would
// leave behind (store.Delete's own post-write revalidation). A guard here would
// be a second copy of a rule the validator already owns, and the two would
// answer differently the day one of them changed.
func castsConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindCast,
		path:         "casts",
		resourceType: "casts",
		displayName:  "cast",
		createSchema: "CastCreate",
		updateSchema: "CastUpdate",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
		writeScope:   func(f resourceFields) string { return f.ScopeNode },
	}
}
