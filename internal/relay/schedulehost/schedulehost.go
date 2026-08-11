// Package schedulehost is the relay-side driver for data-model/1's
// scheduling-core resolution engine (contracts/relay-1.md REL-065,
// contracts/data-model-1.md DAT-051/111/113-118): it parses the
// opaquely-carried `schedule` desired-state section into a
// datamodel.RowStore (BuildStore/Governs), resolves it per-instant into the
// player/1 Lease a screen is served (ProjectLease/Resolver, DAT-113-118),
// and — in a later file of this package — fires preset batches on daypart
// rising edges.
//
// This package DERIVES, it does not re-implement (data-model/1 line 391):
// every parse/validate/resolve step here calls straight through to
// internal/datamodel's own, corpus-proven functions (datamodel.BuildScopeTree,
// datamodel.ValidateRows, datamodel.Resolve, datamodel.PresetTransition). No
// scheduling semantics — precedence, holding, fallback, the display_power
// projection, or the terminal-default rule — are expressed in this package;
// ProjectLease reads the already-resolved datamodel.EffectiveState and only
// maps it onto the player/1 Lease field vocabulary.
package schedulehost

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Lease projection constants. These are the player/1 Lease field values a
// schedule-resolved program carries; ProjectLease maps a resolved
// datamodel.EffectiveState onto them without re-deriving any scheduling rule.
const (
	// leasePriorityScheduled is the Lease `priority` every schedule-resolved
	// program carries (player/1 PLY-108): schedule-driven, never the separate
	// emergency `preempt` path (out of this driver's scope).
	leasePriorityScheduled = "scheduled"

	// leaseDisplayContent is player/1's Lease `display` value for a powered
	// screen showing its playlist (PLY-093) — the value datamodel.Resolve
	// already projects a display_power:on daypart to (DAT-113). This package
	// reads that projection to decide whether to source content; it does not
	// re-map display_power itself (data-model/1 line 391).
	leaseDisplayContent = "content"

	// leaseContentTypeImage is the player/1 content `type` (PLY-083) an asset
	// item that states NO content_type of its own is served as — REL-061a's
	// stated default and this codebase's own historical implicit value, exactly
	// the default playerserver.SetServedProgram applies to the app-signed
	// baseline. An item that DOES state one is served as that type
	// (leaseContentTypeFor), which is how a scheduled video plays instead of
	// being drawn as a still.
	leaseContentTypeImage = "image"

	// leaseContentTypeSlide is the player/1 content `type` (PLY-083) of a native
	// slide item (native slide rendering, parity milestone 2), matching
	// playerserver.SetServedProgram's own leaseContentTypeSlide. A slide item's
	// content is its `layers`, not an asset_ref/url.
	leaseContentTypeSlide = "slide"

	// sourceSlide is the data-model/1 playlist-item `source` value (DAT-041) of a
	// native-slide item — the same value the app-side projection keys on
	// (snapshot.sourceSlide), so both sides recognize a slide identically.
	sourceSlide = "slide"

	// terminalProgramRevision is the stable programRevision for the DAT-118
	// terminal default (a governing schedule holds nothing): a fixed sentinel,
	// so a screen parked at the terminal blank never spuriously re-swaps.
	//
	// It is single-sourced from playerserver, which serves the SAME DAT-118
	// terminal default to a screen it holds no program for at all: both paths
	// arrive at one contract-defined state, and two sentinels for it would read
	// to a player as a real program change every time it crossed between them.
	terminalProgramRevision = playerserver.TerminalProgramRevision
)

// BuildStore parses a carried `schedule` desired-state section (REL-065)
// into a data-model/1 RowStore: it unmarshals the carried scope-node array
// and the six scheduling-core row arrays, then runs them through
// datamodel's own parse/validate path — datamodel.BuildScopeTree for the
// scope-node tree, datamodel.ValidateRows for the six row kinds — never
// re-expressing either's rules here (data-model/1 line 391).
//
// BuildStore is degrade-safe: it MUST NOT panic or brick on a bad schedule
// section. A scope-node entry that fails to unmarshal is recorded as a
// ROW_MALFORMED error and excluded from the tree; every other parse or
// validation failure datamodel.BuildScopeTree/ValidateRows themselves
// report is passed through unchanged. The returned RowStore is always
// usable — built from whatever good nodes and rows survived — even when
// the returned error slice is non-empty; the caller (the relay boot path,
// a later task) logs the errors and degrades to serving the app-authored
// program rather than treating a bad schedule as fatal.
func BuildStore(sec wire.ScheduleSection) (datamodel.RowStore, []datamodel.Error) {
	var errs []datamodel.Error

	nodes := make([]datamodel.ScopeNode, 0, len(sec.ScopeNodes))
	for _, raw := range sec.ScopeNodes {
		var n datamodel.ScopeNode
		if err := json.Unmarshal(raw, &n); err != nil {
			errs = append(errs, datamodel.Error{
				Field:   "scope_nodes",
				Code:    "ROW_MALFORMED",
				Message: err.Error(),
			})
			continue
		}
		nodes = append(nodes, n)
	}

	tree, treeErrs := datamodel.BuildScopeTree(nodes)
	errs = append(errs, treeErrs...)

	raw := datamodel.RawRows{
		Playlists:       sec.Playlists,
		Casts:           sec.Casts,
		Schedules:       sec.Schedules,
		ValidityWindows: sec.ValidityWindows,
		Dayparts:        sec.Dayparts,
		Fallbacks:       sec.Fallbacks,
		PresetBatches:   sec.PresetBatches,
	}
	rows, rowErrs := datamodel.ValidateRows(raw)
	errs = append(errs, rowErrs...)

	return datamodel.RowStore{Tree: tree, Rows: rows}, errs
}

// Governs reports whether the carried schedule governs screenNodeID, per
// the relay's additive serving policy (Global Constraints): the screen's
// scope node MUST be present in the carried scope tree, AND at least one
// schedule row MUST be applicable to it per DAT-051 — its own scope_node is
// the screen itself OR any ancestor of the screen on the parent_id chain
// (contracts/data-model-1.md DAT-051: "a site-wide base schedule governs
// every screen beneath it"). Governs delegates the ancestor walk to
// datamodel.ScopeTree.AncestorChain, the same cascade ApplicableSchedules
// itself walks, rather than re-deriving it here (data-model/1 line 391).
//
// Governs is a purely structural, time-independent check — it says nothing
// about whether a schedule is currently in force (DAT-052) or a daypart
// currently HOLDS (that per-instant question is Resolve's, a later task) —
// it only decides whether the relay should treat this screen as
// schedule-driven at all.
//
// When Governs is false (no scope node carried for the screen, or no
// schedule is applicable to it at any ancestor distance), the relay's
// stated policy is to keep serving the app-authored screen_programs
// program unchanged (REL-061) — behavior is UNCHANGED for an empty or
// non-applicable schedule. Governs does not itself decide what to serve;
// callers do.
func Governs(store datamodel.RowStore, screenNodeID string) bool {
	chain := store.Tree.AncestorChain(screenNodeID)
	if chain == nil {
		return false
	}
	inChain := make(map[string]bool, len(chain))
	for _, id := range chain {
		inChain[id] = true
	}
	for _, s := range store.Rows.Schedules {
		if inChain[s.ScopeNode] {
			return true
		}
	}
	return false
}

// ProjectLease resolves screenNodeID's effective state at nowMs
// (datamodel.Resolve) and maps it onto the player/1 Lease fields a relay serves
// (DAT-113-118):
//
//   - display is datamodel.Resolve's already-projected Lease `display` — content
//     for a display_power:on daypart (DAT-113), blank for a blank/off daypart
//     (DAT-114/115) or the terminal default (DAT-118). This package does not
//     re-derive the display_power mapping; it reads Resolve's projection.
//   - content, for a `content` display, is the effective daypart's/fallback's
//     playlist projected item-by-item to player/1 Lease content refs
//     (playlistContent); a blank/terminal display carries no content. Each
//     asset item's fetchable `url` is derived from contentOrigin — the
//     desired-state content-origin base (desiredstate.Applied.ContentOrigin,
//     REL-061) — in the same `<base>/content/<hex>` form snapshot.Build uses;
//     an empty contentOrigin degrades to a url-less content item (REL-140).
//   - priority is always `scheduled` — this is the schedule-driven serve path;
//     the emergency `preempt` priority is a separate path, out of scope here.
//   - programRevision is a deterministic function of the effective-daypart
//     IDENTITY (programRevisionFor): byte-identical while the same daypart holds
//     (no spurious re-swap) and different across a daypart change (the player
//     swaps).
//
// It returns a non-nil error exactly when datamodel.Resolve does — i.e. an
// unresolvable effective tz (DAT-034) — and in that case returns no Lease
// fields: resolution NEVER substitutes box-local state, so the caller degrades
// rather than serving a guessed one.
func ProjectLease(store datamodel.RowStore, screenNodeID string, nowMs int64, contentOrigin string, contentURLKey []byte) (display string, priority string, content []wire.LeaseContent, programRevision string, err error) {
	state, err := datamodel.Resolve(store, screenNodeID, nowMs)
	if err != nil {
		return "", "", nil, "", err
	}
	display, priority, content, programRevision = projectState(store, state, contentSigner{origin: contentOrigin, key: contentURLKey, nowMs: nowMs})
	return display, priority, content, programRevision, nil
}

// projectState maps an ALREADY-resolved datamodel.EffectiveState onto the
// player/1 Lease fields. It is the shared projection ProjectLease (which calls
// Resolve first) and Resolver.ResolveNow (which keeps the resolved state for the
// preset rising-edge check) both use, so the two cannot drift on how a resolved
// state becomes a Lease.
func projectState(store datamodel.RowStore, state datamodel.EffectiveState, sign contentSigner) (display string, priority string, content []wire.LeaseContent, programRevision string) {
	display = state.Display
	priority = leasePriorityScheduled
	programRevision = programRevisionFor(state)
	if state.Display == leaseDisplayContent {
		content = playlistContent(store, state.PlaylistID, sign)
	}
	return display, priority, content, programRevision
}

// programRevisionFor derives a Lease programRevision from the effective state's
// identity: the schedule + effective daypart id while a daypart holds, the
// schedule + fallback id under a fallback (DAT-117), or the fixed terminal
// sentinel at the DAT-118 terminal default. It is a pure function of identity,
// so it is stable while the same state holds and changes when it changes — the
// property a player relies on to swap its program only on a real change.
func programRevisionFor(state datamodel.EffectiveState) string {
	switch {
	case state.Daypart != nil:
		return state.ScheduleID + ":dp:" + state.Daypart.ID
	case state.Fallback != nil:
		return state.ScheduleID + ":fb:" + state.Fallback.ID
	default:
		return terminalProgramRevision
	}
}

// playlistContent projects the playlist named by playlistID (the effective
// daypart's or fallback's playlist_id, already resolved onto state.PlaylistID)
// into player/1 Lease content refs, one per projectable item (DAT-041), IN
// ORDER — an N-item playlist therefore yields an N-item Lease `content` array a
// player cycles per PLY-083a, not just its first item. An `asset` item projects
// to a plain `image` content item; a `slide` item (native slide rendering,
// parity milestone 2) projects to a `type:"slide"` item carrying the authored
// layer stack (resolveSlideLayers, which drops a slide whose layers do not pass
// wire.ValidateSlideLayers); a `cast` item (DAT-043) projects to ONE such item
// PER SLIDE of the referenced cast, in authored order (castContent). A
// `playable` (pack) item has no direct Lease content ref and is skipped. An
// empty or unknown playlist id yields no content.
//
// A slide projects here IDENTICALLY to the app-signed baseline
// (snapshot.playlistContent): both derive image-layer URLs from the same origin
// and gate on the same wire.ValidateSlideLayers, so a screen sees the same slide
// whether it is playing the signed baseline or the relay's re-resolution of a
// daypart boundary. TestDerivedContentMatchesRelaySideProjection pins that.
//
// An item's own `duration_seconds` override (DAT-042), when present and
// non-zero, is carried onto the projected content item's `duration_ms`
// (PLY-083b) as duration_seconds*1000 — the same per-item dwell-time override
// relay/1's own ContentRef.DurationMS carries for the app-authored direct
// path (REL-061a), so a schedule-resolved multi-item cast honors an authored
// per-item duration exactly as a direct one does. An item with no override
// (DurationSeconds nil or 0) carries no `duration_ms` at all (omitempty) —
// unchanged from this function's pre-existing single-item output, so an
// existing one-item playlist with no duration override still projects a
// byte-identical Lease content item.
//
// contentOrigin is the desired-state's content-origin base URL
// (desiredstate.Applied.ContentOrigin, from revocation_and_site.content_origin,
// REL-061/066). Each asset item's Lease `url` is MINTED by contenturl.Signer
// under that base (contentSigner.urlFor) — the same minter the app-authored path
// calls, so the two sides cannot grow a second URL grammar (REL-061). The PATH
// is identical to what the app emits for the same asset + base; the query is
// not, and must not be: each side signs its own deadline, measured from its own
// clock, which is exactly what lets an offline relay keep re-minting through an
// outage (REL-050/066d).
//
// `expires_at` on the Lease item is still 0 here, and that is now a KNOWN GAP
// rather than the accurate statement it once was. A minted url has a real
// deadline, so a Lease claiming none understates it; the app-authored path
// (snapshot) carries the true one. Closing it here means deciding what a
// re-minted item's deadline says to a player mid-Lease (PLY-086/092) and is left
// to the same work that finishes REL-066d.
//
// When contentOrigin is "" — the desired state carried no content origin — the
// url is left EMPTY exactly as before: the relay derives content URLs only from
// the base carried in desired-state, never a relay-local guess, so a missing
// base degrades to a url-less content item rather than fabricating a box-local
// origin (REL-140).
func playlistContent(store datamodel.RowStore, playlistID string, sign contentSigner) []wire.LeaseContent {
	if playlistID == "" {
		return nil
	}
	for i := range store.Rows.Playlists {
		p := store.Rows.Playlists[i]
		if p.ID != playlistID {
			continue
		}
		content := make([]wire.LeaseContent, 0, len(p.Items))
		for _, item := range p.Items {
			var durationMS int64
			if item.DurationSeconds != nil && *item.DurationSeconds != 0 {
				durationMS = int64(*item.DurationSeconds) * 1000
			}
			if item.Source == datamodel.PlaylistSourceCast {
				content = append(content, castContent(store, item.CastID, durationMS, sign)...)
				continue
			}
			if item.Source == sourceSlide {
				// A slide re-resolved on the relay must project IDENTICALLY to the
				// app-signed baseline (snapshot.playlistContent), or a screen would
				// see one thing in its signed program and another the moment a
				// daypart boundary made the relay re-resolve. So the same rule holds
				// here: derive each image layer's URL from the origin, admit the
				// stack only if wire.ValidateSlideLayers accepts it, and DROP a slide
				// that does not validate rather than serve a player a malformed layer.
				layers, ok := resolveSlideLayers(item.Slide, sign)
				if !ok {
					continue
				}
				content = append(content, wire.LeaseContent{
					Type:       leaseContentTypeSlide,
					Layers:     layers,
					DurationMS: durationMS,
				})
				continue
			}
			if item.AssetRef == "" {
				continue // a pack `playable` has no direct Lease content ref.
			}
			content = append(content, wire.LeaseContent{
				Type:       leaseContentTypeFor(item.ContentType),
				AssetRef:   item.AssetRef,
				URL:        sign.urlFor(item.AssetRef),
				DurationMS: durationMS,
			})
		}
		if len(content) == 0 {
			return nil
		}
		return content
	}
	return nil
}

// leaseContentTypeFor resolves an asset playlist item's authored `content_type`
// (datamodel.PlaylistItem.ContentType) to the player/1 Lease content `type`
// (PLY-083) this projection stamps on it: the stated value when there is one,
// `image` when there is not.
//
// A Lease content item's `type` is REQUIRED on the wire, so unlike the app-side
// projection — which carries the authored value verbatim, empty string
// included, because a REL-061 ContentRef's own content_type is optional — this
// side must substitute the default itself. That asymmetry is deliberate and it
// is the reason this is a named function on both sides of one rule rather than
// an inline expression: playerserver.SetServedProgram applies the IDENTICAL
// default when converting the app-signed baseline, so a screen sees the same
// `type` whether it is playing the signed program or the relay's own
// re-resolution of a daypart boundary.
func leaseContentTypeFor(authored string) string {
	if authored == "" {
		return leaseContentTypeImage
	}
	return authored
}

// resolveSlideLayers projects an authored slide item's layer stack into the
// wire.Layer slice a slide Lease item carries, or reports it is not projectable.
// It mirrors snapshot.resolveSlideLayers exactly: each image layer's fetch URL
// is minted from the relay's own content signer (the same urlFor a plain asset
// item uses, so the two content-URL grammars stay single-sourced), and the
// stack is admitted only if wire.ValidateSlideLayers accepts it. A nil slide, or
// one whose layers do not validate, is not projectable (ok=false) and the caller
// drops the item — a player is never handed a malformed layer.
func resolveSlideLayers(slide *datamodel.Slide, sign contentSigner) ([]wire.Layer, bool) {
	if slide == nil {
		return nil, false
	}
	return resolveLayers(slide.Layers, sign)
}

// resolveLayers is the layer-level half of that job, shared with the cast
// expansion (castContent) for the same reason snapshot.resolveLayers is: a
// cast's slides carry the same wire.Layer stack an inline `slide` item does and
// must reach a Lease through the identical URL minting and the identical
// validation gate.
func resolveLayers(authored []wire.Layer, sign contentSigner) ([]wire.Layer, bool) {
	layers := make([]wire.Layer, len(authored))
	for i, l := range authored {
		// wire.LayerFetchesContent, not an inline `== LayerKindImage`: it names
		// the content-bearing kinds (image and video) in ONE place, shared with
		// the validator and with snapshot.resolveLayers, so a kind cannot be
		// admitted by validation on one side and left url-less by a projection
		// on the other — which would drop the whole slide at serve time.
		if wire.LayerFetchesContent(l.Kind) {
			l.URL = sign.urlFor(l.AssetRef)
		}
		layers[i] = l
	}
	if err := wire.ValidateSlideLayers(layers); err != nil {
		return nil, false
	}
	return layers, true
}

// castContent expands ONE `source: "cast"` playlist item (DAT-041) into the
// player/1 Lease content items its cast's slides project to: one `type:"slide"`
// item per slide, in authored order, each carrying that slide's own layer stack.
//
// It is the relay-side twin of snapshot.castContent and MUST stay its twin —
// same order, same per-slide dwell-time resolution, same drop of a slide whose
// layers do not validate — because these two functions are the two answers a
// screen can receive for the same authored cast: the app-signed baseline it is
// handed on a generation, and the relay's own re-resolution the moment a daypart
// boundary passes. A divergence between them is a screen whose content changes
// at a boundary for no authored reason. TestDerivedContentMatchesRelaySideProjection
// pins the two together.
//
// itemDurationMS is the referencing playlist item's own `duration_seconds`
// override already converted to ms (0 when it stated none); the resolution from
// there — slide `duration_ms`, that override, the CAST's `default_duration_ms`,
// then nothing (omitempty, the player's own default) — is datamodel.SlideDwellMS,
// the SAME function internal/feeder/snapshot's projection calls. It is one
// function rather than a copy on each side precisely because these two
// projections must agree: a divergence here is a screen whose slide timings
// change at a daypart boundary for no authored reason.
//
// An unknown cast id contributes nothing: a carried schedule section that does
// not resolve is a degraded input, and the honest projection of absent content
// is no content rather than a placeholder a screen would stall on.
func castContent(store datamodel.RowStore, castID string, itemDurationMS int64, sign contentSigner) []wire.LeaseContent {
	var out []wire.LeaseContent
	if castID == "" {
		return out
	}
	for i := range store.Rows.Casts {
		c := store.Rows.Casts[i]
		if c.ID != castID {
			continue
		}
		for _, slide := range c.Slides {
			layers, ok := resolveLayers(slide.Layers, sign)
			if !ok {
				continue
			}
			out = append(out, wire.LeaseContent{
				Type:       leaseContentTypeSlide,
				Layers:     layers,
				DurationMS: datamodel.SlideDwellMS(slide, c, itemDurationMS),
				// The cast-local slide id, carried for `nav` jump targeting
				// exactly as snapshot.castContent carries it onto its own
				// ContentRef. This projection produces a LeaseContent directly
				// (no ContentRef hop), so forgetting it here would make a nav
				// menu work on the app-authored path and silently dead-end on the
				// relay-resolved one — the two-projections-disagree failure this
				// file's own twin note exists to prevent.
				SlideID: slide.ID,
			})
		}
		return out
	}
	return out
}

// contentURL builds a schedule-resolved content item's Lease `url` from the
// desired-state content-origin base and a content-addressed asset_ref
// (`sha256:<hex>`): `contentOrigin + "/content/" + <hex>`, mirroring
// snapshot.Build's app-authored form byte-for-byte (REL-061). An empty
// contentOrigin yields an empty url — the relay never fabricates a box-local
// origin (REL-140); the caller degrades to a url-less content item.
// contentSigner carries everything minting one content URL needs: the origin
// base, the key (REL-066a) and the clock the deadline is measured from.
//
// It replaces the bare origin string that was threaded through this file
// because the three travel together — a URL minted from one relay's origin
// under another's key, or against a clock other than the one the deadline is
// read against, is not a URL that verifies.
type contentSigner struct {
	origin string
	key    []byte
	nowMs  int64
}

// contentURLTTL is how long a minted URL stays fetchable.
//
// Minting happens at SERVE time (REL-066d), so this bounds how long after a
// screen was handed a URL it may still fetch it — not how long a snapshot is
// good for. It is generous because the cost of it being too short is a screen
// that cannot fetch content it was told to play, and the cost of it being too
// long is bounded by the key's own rotation.
//
// It is contenturl.ServeTTL rather than its own number, so that this relay's
// lifetime and the app side's are stated side by side in one place and can be
// compared. They are equal today (contenturl.SnapshotTTL collapsed onto ServeTTL
// once the feeder began re-minting its snapshot on a timer); the app side is the
// one that would have to argue for diverging again, and its constant is where
// that argument lives.
const contentURLTTL = contenturl.ServeTTL

// urlFor mints the URL for one asset_ref.
//
// With no key it returns the unsigned form, which is what a deployment whose app
// peer delivers no key has always had. With no origin it returns empty, and the
// relay never fabricates a box-local one (REL-140) — the caller degrades to a
// url-less content item, which a screen surfaces as unresolvable rather than
// fetching from somewhere nobody authorized. A digest that will not sign returns
// empty too: the UNSIGNED form would be worse than none, since against a
// verifying origin it 403s, which reads to an operator as an authorization fault
// rather than as the malformed asset_ref it is.
//
// The three-case rule is contenturl.Signer.Mint's, called rather than restated,
// so this relay's minting and the app's cannot drift on the URL grammar or on
// what an unmintable ref degrades to. This projection carries no `expires_at`
// onto its Lease items today, so the deadline is dropped here; the app side,
// which does carry one, uses it.
func (c contentSigner) urlFor(assetRef string) string {
	url, _ := contenturl.Signer{Base: c.origin, Key: c.key, TTL: contentURLTTL, NowMs: c.nowMs}.Mint(assetRef)
	return url
}

// Resolver owns the per-instant serving of ONE scope node's schedule-resolved
// program: it resolves that node's effective state, projects it onto the
// player/1 Lease the player server issues (Resolver.ResolveNow), and tracks the
// previous effective state so a daypart's rising edge can fire its preset batch
// (a later task dispatches the returned datamodel.PresetFire). A site with
// several governed scope nodes runs one Resolver each.
//
// # Two id spaces, deliberately separate
//
// screenNodeID is the SCOPE NODE resolution happens at (data-model/1 DAT-001,
// what datamodel.Resolve and Governs take). servedScreenID is the SCREEN
// IDENTITY ROW whose player/1 program the projection is installed as
// (DAT-004a, what a channel token resolves to and playerserver.SetProgram
// keys on). DAT-004a is explicit that these are distinct rows with distinct
// ids and that "a `screen`-kind scope node is a placement classification —
// never a screen identity in its own right", so they are two parameters rather
// than one: passing a scope-node id where a screen id belongs installs the
// program under a key no channel token can ever reach.
//
// servedScreenID MAY be empty, and then this Resolver serves no player at all
// — it still resolves and still returns the preset-batch rising edge, which is
// a scope-node-level concern (DAT-075) needing no screen identity. That is the
// mode a caller uses when it cannot say which screen a governed node's
// resolution belongs to: relay/1's carried `schedule` section (REL-065) is
// keyed by scope node and carries no screen identity rows, so the relay has no
// screen -> scope-node placement to join on. Firing that node's presets while
// serving nobody is strictly better than serving the resolution to a screen it
// might not be for.
type Resolver struct {
	store        datamodel.RowStore
	screenNodeID string

	// servedScreenID is the screen identity row (DAT-004a) this resolver's
	// projection is served to, or "" to resolve and fire presets without
	// serving any player — see the type doc.
	servedScreenID string

	srv *playerserver.Server

	// contentOrigin is the desired-state content-origin base URL
	// (desiredstate.Applied.ContentOrigin, from revocation_and_site.content_origin,
	// REL-061/066) this resolver derives schedule-resolved content-item Lease URLs
	// from — the SAME base the app-authored snapshot.Build path uses, so both
	// produce byte-identical `<base>/content/<hex>` URLs. Empty when the applied
	// desired-state carried no content origin, in which case resolved content
	// items degrade to a url-less ref (REL-140) rather than a box-local guess.
	contentOrigin string

	// contentURLKey is the key this resolver MINTS content URLs with
	// (REL-066a/066d), from the applied desired state. Empty means the app peer
	// delivered none and URLs go out unsigned.
	contentURLKey []byte

	// generation is the desired-state generation this resolver was built for
	// (relay/1 REL-052/056). It is stamped onto every playerserver.SetProgram
	// write so a superseded generation's still-in-flight resolver goroutine —
	// cancelled but mid-resolve when a higher generation is applied — cannot
	// revert the served program to its stale generation: SetProgram fences a
	// strictly-older write.
	generation int64

	// mu guards prev alone. Every other field is fixed for a Resolver's life,
	// and prev would be too — it is written and read only by the one goroutine
	// driving this resolver — were it not for CarryState, which the APPLY
	// goroutine calls on the resolver it is replacing. Cancelling that
	// resolver's loop cannot interrupt a resolve already in flight
	// (scheduleDriver.apply's own doc), so that read genuinely races the
	// in-flight write and needs the lock.
	mu sync.Mutex

	// prev is the effective state the last successful ResolveNow projected, or
	// nil before the first — the edge datamodel.PresetTransition keys a preset
	// firing on. It is advanced only on a successful resolve, so a resolution
	// error never spuriously changes the rising-edge baseline.
	//
	// A resolver REPLACING another over the same scope node adopts that one's
	// prev (AdoptCarriedState) instead of starting from nil: a rebuild is not a
	// boot, and a nil baseline would make every rebuild a rising edge. See
	// AdoptCarriedState.
	prev *datamodel.EffectiveState

	// lastResolveMs is the instant the last successful ResolveNow resolved at —
	// the evaluated_at a preset batch fired for that resolution stamps into its
	// DAT-092 outcome (FirePreset has no nowMs of its own; Tick calls ResolveNow
	// immediately before FirePreset, so this is that tick's instant).
	lastResolveMs int64
}

// NewResolver builds a Resolver resolving scope node screenNodeID from store
// and writing each resolved Lease to srv as servedScreenID's program
// (playerserver.Server.SetProgram). It carries no signing key: the Lease-signing
// identity is the relay's own, established once via
// playerserver.Server.SetSigningKey, and a schedule resolution has no authority
// over it (playerserver.SetProgram's own doc).
//
// screenNodeID and servedScreenID are two different id spaces (DAT-004a) and
// the caller MUST NOT pass one for the other — see the Resolver type doc.
// servedScreenID may be "" for a resolver that fires a node's presets without
// serving any player, which is what a caller that cannot join the node to a
// screen passes rather than guessing.
//
// generation is the desired-state generation store was applied for (relay/1
// REL-052/056): it is carried onto every SetProgram write as the fence key, so
// a resolver from a superseded generation cannot revert the served program a
// newer generation installed. The live re-pull path (cmd/waiveo-relay) passes
// the applied generation; tests exercising a single generation may pass any
// non-decreasing value.
//
// contentOrigin is the applied desired-state's content-origin base URL
// (desiredstate.Applied.ContentOrigin, REL-061/066): every schedule-resolved
// content item this resolver serves gets a fetchable `<contentOrigin>/content/
// <hex>` URL derived from it, byte-identical to the app-authored snapshot.Build
// form. An empty contentOrigin degrades resolved content to url-less refs
// (REL-140) — the live re-pull path passes applied.ContentOrigin, so a feeder
// that carried no content origin degrades rather than fabricating one.
func NewResolver(store datamodel.RowStore, screenNodeID, servedScreenID string, srv *playerserver.Server, generation int64, contentOrigin string, contentURLKey []byte) *Resolver {
	return &Resolver{
		store:          store,
		screenNodeID:   screenNodeID,
		servedScreenID: servedScreenID,
		srv:            srv,
		generation:     generation,
		contentOrigin:  contentOrigin,
		contentURLKey:  contentURLKey,
	}
}

// ResolveNow resolves the screen's effective state at nowMs, serves the
// projected Lease (playerserver.Server.SetProgram), and returns the preset batch
// to fire on this instant's rising edge (datamodel.PresetTransition of the
// previous effective state to the current) — nil when nothing fires; a later
// task dispatches it through the device plane.
//
// This is the level-triggered STATE projection (DAT-119): the display/content
// Lease is re-derived and re-served on EVERY call, whether or not the effective
// daypart changed — a daypart is a holding state, not a one-shot event. Only the
// returned preset fire is edge-triggered (DAT-075), keyed on effective-daypart
// identity by datamodel.PresetTransition.
//
// On a resolution error — an unresolvable effective tz (DAT-034) above all — it
// does NOT call SetProgram: the currently-served program (the app-authored one,
// or a prior resolved one) stays in place and the error is surfaced for the
// caller to log and degrade. Resolution NEVER substitutes box-local state, so a
// bad tz degrades rather than serving a guessed program.
func (r *Resolver) ResolveNow(nowMs int64) (fired *datamodel.PresetFire, err error) {
	state, err := datamodel.Resolve(r.store, r.screenNodeID, nowMs)
	if err != nil {
		return nil, err
	}

	display, priority, content, programRevision := projectState(r.store, state, contentSigner{origin: r.contentOrigin, key: r.contentURLKey, nowMs: nowMs})
	// Served to THIS resolver's own screen alone. A resolver with no screen to
	// serve (servedScreenID "") still resolves and still reports the rising edge
	// below — the preset batch is a scope-node concern (DAT-075) and needs no
	// screen identity — it simply installs no program.
	if r.srv != nil && r.servedScreenID != "" {
		r.srv.SetProgram(r.generation, r.servedScreenID, programRevision, priority, display, content)
	}

	r.mu.Lock()
	fired = datamodel.PresetTransition(r.prev, &state)
	r.prev = &state
	r.mu.Unlock()
	r.lastResolveMs = nowMs
	return fired, nil
}

// ScopeNodeID is the scope node this Resolver resolves — the key a caller
// rebuilding a generation's resolvers matches an outgoing resolver to its
// incoming replacement on (CarryState/AdoptCarriedState).
func (r *Resolver) ScopeNodeID() string { return r.screenNodeID }

// CarriedBaseline is what one generation's Resolver hands the Resolver
// replacing it over the same scope node: the rising-edge baseline it reached,
// plus the authored rows that baseline's desired device state was read from.
//
// The rows travel with the state because the replacement cannot recover them —
// it holds only the NEW generation's store, and "the same daypart is still
// effective" and "the same device state is still desired" are different
// questions. AdoptCarriedState answers the second by comparing these rows
// against its own generation's; State alone can only answer the first.
//
// Nothing here is copied. State is the last-resolved value itself, which
// nothing mutates after ResolveNow publishes it (each resolve publishes a fresh
// value rather than editing the previous one), and PresetBatch points into the
// outgoing resolver's store, which is fixed for that resolver's life.
type CarriedBaseline struct {
	// State is the effective state the outgoing resolver's last successful
	// resolve projected — the value datamodel.PresetTransition keys the next
	// rising edge against. Never nil in a CarriedBaseline CarryState returns.
	State *datamodel.EffectiveState

	// PresetBatch is the preset-batch row State's effective daypart bound in the
	// OUTGOING generation — the batch that would have fired, and so the desired
	// device state the carried baseline actually stands for. Nil when the
	// carried state has no effective daypart, when that daypart binds no batch,
	// or when the outgoing store carried no such row (a degraded store).
	PresetBatch *datamodel.PresetBatch
}

// CarryState is the rising-edge baseline this Resolver reached — the effective
// state its last successful resolve projected together with the preset-batch
// row that state bound — or nil if it never resolved.
//
// It exists for exactly one caller — the apply path replacing this Resolver
// with a new one over the SAME scope node — which hands it to that new
// resolver's AdoptCarriedState. Read under the lock because the apply path
// calls it after cancelling this resolver's loop, and cancellation cannot
// interrupt a resolve already in flight.
func (r *Resolver) CarryState() *CarriedBaseline {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prev == nil {
		return nil
	}
	carried := &CarriedBaseline{State: r.prev}
	if r.prev.Daypart != nil && r.prev.Daypart.PresetBatchID != "" {
		// This resolver's OWN store, which is what the baseline was resolved
		// against and is immutable for its life — so reading it under mu costs
		// nothing and cannot deadlock.
		carried.PresetBatch = r.presetBatch(r.prev.Daypart.PresetBatchID)
	}
	return carried
}

// AdoptCarriedState seeds this Resolver's rising-edge baseline from the
// resolver it REPLACES over the same scope node (that resolver's CarryState) —
// but only when this generation still carries the SAME desired device state for
// that baseline's effective daypart (datamodel.MayCarryAcrossApply). A nil
// carried, a carried baseline whose rows this generation re-authored, or a call
// on a Resolver that has already resolved, all do nothing.
//
// # Why a rebuild must not look like a boot
//
// A resolver is rebuilt on every desired-state generation apply, not only at
// boot (cmd/waiveo-relay's scheduleDriver). Without this, each rebuild started
// from a nil baseline, and datamodel.PresetTransition reads a nil prev as the
// empty identity — so the currently effective daypart looked like a rising edge
// on EVERY apply and TickBoot re-dispatched its whole preset batch to real
// devices. With the feeder re-minting content URLs on a 12-hourly timer
// (contenturl.SnapshotRemintInterval), that turned a resume rule into a cron:
// twice a day, at a wall-clock instant nobody chose, every governed node
// re-asserted its daypart's device state — powering a display an operator had
// just switched off, or yanking an input back mid-presentation.
//
// DAT-075's resume clause names boot, generation apply, and clock-trust resume
// because each is a discontinuity in the consumer's ability to observe: it may
// have MISSED an edge, and `misfire` says what to do about the one it missed.
// A relay that has been continuously resolving this node has missed nothing, so
// a re-apply over it is not a resume and there is nothing to catch up. Carrying
// the baseline across the rebuild is what makes the code say that.
//
// It suppresses no genuine edge. A real boot has no outgoing resolver to carry
// from, so its baseline is nil and it fires. A daypart boundary crossed between
// the last live tick and the rebuild leaves the carried baseline naming the OLD
// daypart, so the rebuild resolves a DIFFERENT identity and fires, still
// governed by misfire. An apply that begins governing a node nobody was
// resolving carries nothing for it and fires.
//
// # Why identity alone is the wrong key, and rows are the right one
//
// The paragraphs above argue only that a re-apply of the SAME desired state
// must not re-fire. Keying the carry on effective-daypart identity alone
// answers a different question than that, and gets the mirror case exactly
// backwards: it also swallows an apply whose desired state CHANGED.
//
// The ordinary 24/7 signage shape is one all-day daypart, and its identity
// never changes again after boot. An operator editing that daypart's bound
// preset batch — a different `launch` channel, a different volume — advances
// the generation, the relay applies it, and an identity-keyed carry reads "same
// node, same daypart id" as "nothing to do". No boundary is ever crossed
// afterwards for the edit to ride, so nothing dispatches it, ever; only a relay
// restart delivers it, and nothing logs that it was withheld. Suppressing "the
// same desired state, re-applied" and suppressing "a changed desired state" are
// opposite obligations that identity cannot tell apart.
//
// So the carry is keyed at DAYPART-ROW granularity, which is
// datamodel.MayCarryAcrossApply's own rule (DAT-075) and is called through to
// here rather than restated: adopt only when the carried baseline's own daypart
// row AND the preset-batch row it binds are unchanged in this generation. That
// is exactly the desired device state a fire would assert, and nothing wider —
// a slide's text or a playlist's items changing leaves both rows untouched and
// is still carried, so an unrelated edit never yanks a display. It also does
// not reinstate the re-mint cost: a re-mint re-emits a byte-identical schedule
// section, so both rows compare equal and the carry applies exactly as before.
func (r *Resolver) AdoptCarriedState(carried *CarriedBaseline) {
	if carried == nil || carried.State == nil {
		return
	}
	if !datamodel.MayCarryAcrossApply(carried.State, carried.PresetBatch, r.store) {
		// This generation re-authored what the carried baseline stood for. Leaving
		// the baseline nil makes the effective daypart a rising edge for TickBoot,
		// which is what delivers the edit — still governed by that daypart's own
		// misfire, so a site declaring `skip` is still obeyed.
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prev != nil {
		return // already resolved — its own baseline is newer than any carried one.
	}
	r.prev = carried.State
}

// FirePreset dispatches a rising-edge preset batch's device commands through the
// device plane and collects its DAT-092 batch outcome. fire is the
// datamodel.PresetFire ResolveNow/Tick returned — nil when this tick was not a
// rising edge (or the effective daypart's preset was masked away), in which case
// nothing dispatches and an empty outcome is returned.
//
// When fire is non-nil, FirePreset looks up its bound preset batch
// (fire.PresetBatchID, resolved against the store's already-validated
// PresetBatches — referential integrity is datamodel.ValidateRows's guarantee,
// DAT-075) and dispatches EACH command through sink (the SAME
// automation.CommandSink the edge-rules engine fires through, so a preset command
// gets the identical device-class vocabulary resolution, per-device
// serialization, and credential hygiene an app- or rule-dispatched command does,
// REL-113/114/115). Every command is attempted independently — one failure
// neither halts the batch nor discards the successes — and the per-command
// dispositions are classified into the complete/partial/failed outcome
// datamodel.BatchOutcome fixes (DAT-092). FirePreset does NOT re-derive which
// daypart is effective or whether a preset should fire — that edge decision is
// datamodel.PresetTransition's, read off fire; this method only executes it.
func (r *Resolver) FirePreset(fire *datamodel.PresetFire, sink *automation.CommandSink) datamodel.PresetBatchOutcome {
	if fire == nil {
		return datamodel.PresetBatchOutcome{} // not a rising edge — nothing fires (DAT-075).
	}
	batch := r.presetBatch(fire.PresetBatchID)
	if batch == nil {
		// A validated store never dangles a preset reference (DAT-075); a missing
		// batch here means a degraded store, so fire nothing rather than panic.
		return datamodel.BatchOutcome(nil, r.lastResolveMs)
	}

	results := make([]datamodel.CommandResult, 0, len(batch.Commands))
	for _, cmd := range batch.Commands {
		params, perr := decodeParams(cmd.Params)
		if perr != nil {
			results = append(results, datamodel.CommandResult{Target: cmd.EntityID, Command: cmd.Command, OK: false, Error: perr.Error()})
			continue
		}
		err := sink.Dispatch(cmd.EntityID, cmd.Command, params)
		results = append(results, datamodel.CommandResult{
			Target:  cmd.EntityID,
			Command: cmd.Command,
			OK:      err == nil,
			Error:   errString(err),
		})
	}
	return datamodel.BatchOutcome(results, r.lastResolveMs)
}

// presetBatch returns the preset batch identified by presetID (its DAT-090
// preset_id), or nil when the store carries none — the referential lookup
// FirePreset resolves a datamodel.PresetFire against.
func (r *Resolver) presetBatch(presetID string) *datamodel.PresetBatch {
	for i := range r.store.Rows.PresetBatches {
		if r.store.Rows.PresetBatches[i].PresetID == presetID {
			return &r.store.Rows.PresetBatches[i]
		}
	}
	return nil
}

// decodeParams unmarshals a preset command's opaque params (DAT-091) into the
// map[string]any the device-command surface dispatches — nil for an absent
// params object, an error only for malformed JSON (which fails just that one
// command, not the batch).
func decodeParams(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// errString is err.Error() or "" for a nil error — the Error field a
// datamodel.CommandResult carries for a failed command (empty on success).
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Tick is one turn of the resolution loop for this screen: it re-resolves and
// re-serves the current instant's Lease (ResolveNow — the level-triggered STATE
// projection, done EVERY tick, DAT-119) and then fires the rising-edge preset
// (FirePreset on the transition ResolveNow returned — edge-triggered ONLY on an
// effective-daypart-identity change, DAT-075). A masked daypart never fires,
// because the transition is computed on the node-level effective state, not
// per-schedule candidates (DAT-111).
//
// Tick is degrade-safe: on a resolution error (an unresolvable effective tz,
// DAT-034) ResolveNow leaves the served program untouched and returns the error,
// which Tick absorbs — the screen keeps its last-served program rather than
// resolving against a box-local substitute, and no preset fires. The store and
// screen are fixed for a Resolver's life, so a tz that resolves at boot resolves
// on every tick; a genuinely unresolvable one is surfaced by the boot-time
// resolve, not silently lost here.
func (r *Resolver) Tick(nowMs int64, sink *automation.CommandSink) {
	fired, err := r.ResolveNow(nowMs)
	if err != nil {
		return
	}
	r.FirePreset(fired, sink)
}

// TickBoot is the ONE resume-governed tick a Resolver performs — the first
// tick of a newly built Resolver, which is a boot, a clock-trust resume, or a
// generation apply that begins resolving this node (DAT-075's final sentence:
// "On boot, on a generation apply that begins resolving a scope node the
// consumer was not already resolving, or on clock-trust resume, the current
// effective daypart's preset batch fires once, governed by its effective
// misfire").
//
// A generation apply over a node this relay was ALREADY resolving is not one of
// those: the replacement Resolver adopts the outgoing one's rising-edge
// baseline (AdoptCarriedState), so TickBoot sees the unchanged effective
// daypart it actually is and fires nothing. Nothing was missed, so there is
// nothing to catch up. AdoptCarriedState's own doc owns why.
//
// It resolves and serves the current instant's Lease exactly as Tick does (the
// level-triggered STATE projection, DAT-119, via ResolveNow) — that part is
// never suppressed — but the preset-batch rising edge ResolveNow surfaces is
// dispatched only when the newly-effective daypart's own effective misfire
// (datamodel.Daypart.EffectiveMisfire, DAT-076/094/121) is NOT "skip": a
// "skip" misfire means a site declared this daypart's preset must never fire
// late, so the resume edge that would otherwise re-dispatch its device
// commands on every relay restart landing inside the daypart's window is
// suppressed instead. "catch_up_once" (the DAT-121 default) and "fire_each"
// both still fire — for a resume, where at most one currently-effective
// daypart is observable, both collapse to the same single fire TickBoot
// already performs.
//
// TickBoot is used ONLY for this one boot resolve; every ordinary live tick
// afterward (Resolver.Tick, driven by Loop) fires unconditionally on every
// rising edge regardless of misfire — misfire governs only the resume edge
// named in DAT-075, never an ordinary dayparting transition.
func (r *Resolver) TickBoot(nowMs int64, sink *automation.CommandSink) {
	fired, err := r.ResolveNow(nowMs)
	if err != nil {
		return
	}
	if fired != nil && r.misfireFor(fired.DaypartID) == "skip" {
		return // DAT-075/076/121: a "skip" misfire suppresses this resume-edge fire.
	}
	r.FirePreset(fired, sink)
}

// misfireFor resolves daypartID's effective misfire (datamodel.Daypart.
// EffectiveMisfire, DAT-076/121) by looking the daypart and its owning
// schedule up in the store — a referential lookup over already-validated
// rows, not a re-derivation of the misfire-default rule itself (data-model/1
// line 391, that rule lives in datamodel.Daypart.EffectiveMisfire alone). A
// daypartID datamodel.PresetTransition just returned always resolves here
// (ValidateRows's own referential-integrity guarantee covers schedule_id,
// DAT-075); a lookup miss (a degraded store) falls back to catch_up_once —
// TickBoot's safe default of firing rather than silently suppressing.
func (r *Resolver) misfireFor(daypartID string) string {
	for _, d := range r.store.Rows.Dayparts {
		if d.ID != daypartID {
			continue
		}
		for _, s := range r.store.Rows.Schedules {
			if s.ID == d.ScheduleID {
				return d.EffectiveMisfire(s)
			}
		}
		return d.EffectiveMisfire(datamodel.Schedule{})
	}
	return "catch_up_once"
}

// Loop drives Tick once per tick delivered on ticks, resolving the screen's
// effective state at the wall-clock instant each tick carries (t.UnixMilli()) —
// the periodic re-resolve that catches daypart boundaries (a daypart is a
// holding STATE re-resolved every tick, DAT-119, never a one-shot event). The
// tick CADENCE is injected: a time.Ticker's channel in the relay binary, a
// manual channel in tests, so nothing here sleeps on the wall clock. Loop
// returns when ctx is cancelled or ticks is closed. It mirrors the automation
// host's own drive loop (internal/relay/automationhost.Host.Run) — the STATE
// engine's counterpart to the EVENT engine's observation loop.
func (r *Resolver) Loop(ctx context.Context, ticks <-chan time.Time, sink *automation.CommandSink) {
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-ticks:
			if !ok {
				return
			}
			r.Tick(t.UnixMilli(), sink)
		}
	}
}
