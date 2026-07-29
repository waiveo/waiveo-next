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
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
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

	// leaseContentTypeImage is the single player/1 content `type` (PLY-083)
	// the first-photon carries, matching playerserver.SetServedProgram's own
	// constant `image` annotation. A real per-item content-type lookup is a
	// later concern.
	leaseContentTypeImage = "image"

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
func ProjectLease(store datamodel.RowStore, screenNodeID string, nowMs int64, contentOrigin string) (display string, priority string, content []wire.LeaseContent, programRevision string, err error) {
	state, err := datamodel.Resolve(store, screenNodeID, nowMs)
	if err != nil {
		return "", "", nil, "", err
	}
	display, priority, content, programRevision = projectState(store, state, contentOrigin)
	return display, priority, content, programRevision, nil
}

// projectState maps an ALREADY-resolved datamodel.EffectiveState onto the
// player/1 Lease fields. It is the shared projection ProjectLease (which calls
// Resolve first) and Resolver.ResolveNow (which keeps the resolved state for the
// preset rising-edge check) both use, so the two cannot drift on how a resolved
// state becomes a Lease.
func projectState(store datamodel.RowStore, state datamodel.EffectiveState, contentOrigin string) (display string, priority string, content []wire.LeaseContent, programRevision string) {
	display = state.Display
	priority = leasePriorityScheduled
	programRevision = programRevisionFor(state)
	if state.Display == leaseDisplayContent {
		content = playlistContent(store, state.PlaylistID, contentOrigin)
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
// into player/1 Lease content refs, one per asset item (DAT-041), IN ORDER —
// an N-item playlist therefore yields an N-item Lease `content` array a
// player cycles per PLY-083a, not just its first item. Only items carrying
// an asset_ref project to a plain image content item; a `playable` (pack)
// item has no direct Lease content ref and is skipped. An empty or unknown
// playlist id yields no content.
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
// REL-061/066). Each asset item's Lease `url` is stamped as
// `contentOrigin + "/content/" + hex(asset_ref)` — the sha256: prefix stripped
// off the content-addressed ref — which is BYTE-IDENTICAL to the URL form the
// app-authored path (snapshot.Build) emits for the same asset + base, so the
// two content-URL grammars are single-sourced (REL-061), never a second shape.
// `expires_at` is 0 for now, matching snapshot.Build (no content-URL TTL policy
// is defined yet).
//
// When contentOrigin is "" — the desired state carried no content origin — the
// url is left EMPTY exactly as before: the relay derives content URLs only from
// the base carried in desired-state, never a relay-local guess, so a missing
// base degrades to a url-less content item rather than fabricating a box-local
// origin (REL-140).
func playlistContent(store datamodel.RowStore, playlistID string, contentOrigin string) []wire.LeaseContent {
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
			if item.AssetRef == "" {
				continue // a pack `playable` has no direct Lease content ref.
			}
			var durationMS int64
			if item.DurationSeconds != nil && *item.DurationSeconds != 0 {
				durationMS = int64(*item.DurationSeconds) * 1000
			}
			content = append(content, wire.LeaseContent{
				Type:       leaseContentTypeImage,
				AssetRef:   item.AssetRef,
				URL:        contentURL(contentOrigin, item.AssetRef),
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

// contentURL builds a schedule-resolved content item's Lease `url` from the
// desired-state content-origin base and a content-addressed asset_ref
// (`sha256:<hex>`): `contentOrigin + "/content/" + <hex>`, mirroring
// snapshot.Build's app-authored form byte-for-byte (REL-061). An empty
// contentOrigin yields an empty url — the relay never fabricates a box-local
// origin (REL-140); the caller degrades to a url-less content item.
func contentURL(contentOrigin, assetRef string) string {
	if contentOrigin == "" {
		return ""
	}
	return contentOrigin + "/content/" + strings.TrimPrefix(assetRef, "sha256:")
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

	// generation is the desired-state generation this resolver was built for
	// (relay/1 REL-052/056). It is stamped onto every playerserver.SetProgram
	// write so a superseded generation's still-in-flight resolver goroutine —
	// cancelled but mid-resolve when a higher generation is applied — cannot
	// revert the served program to its stale generation: SetProgram fences a
	// strictly-older write.
	generation int64

	// prev is the effective state the last successful ResolveNow projected, or
	// nil before the first — the edge datamodel.PresetTransition keys a preset
	// firing on. It is advanced only on a successful resolve, so a resolution
	// error never spuriously changes the rising-edge baseline.
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
func NewResolver(store datamodel.RowStore, screenNodeID, servedScreenID string, srv *playerserver.Server, generation int64, contentOrigin string) *Resolver {
	return &Resolver{
		store:          store,
		screenNodeID:   screenNodeID,
		servedScreenID: servedScreenID,
		srv:            srv,
		generation:     generation,
		contentOrigin:  contentOrigin,
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

	display, priority, content, programRevision := projectState(r.store, state, r.contentOrigin)
	// Served to THIS resolver's own screen alone. A resolver with no screen to
	// serve (servedScreenID "") still resolves and still reports the rising edge
	// below — the preset batch is a scope-node concern (DAT-075) and needs no
	// screen identity — it simply installs no program.
	if r.srv != nil && r.servedScreenID != "" {
		r.srv.SetProgram(r.generation, r.servedScreenID, programRevision, priority, display, content)
	}

	fired = datamodel.PresetTransition(r.prev, &state)
	r.prev = &state
	r.lastResolveMs = nowMs
	return fired, nil
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

// TickBoot is the ONE resume-governed tick a Resolver performs — at boot,
// generation apply, or clock-trust resume (DAT-075's final sentence: "On
// boot, generation apply, or clock-trust resume the current effective
// daypart's preset batch fires once, governed by its effective misfire"). It
// resolves and serves the current instant's Lease exactly as Tick does (the
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
