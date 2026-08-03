package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// placement_test.go covers both directions of DAT-006's `scope_node` reference
// from the outside.
//
// The REFERENCED side (a scope node a row is placed at must not be deleted): that
// a DeleteGuard actually gates the removal (and its error reaches the caller
// unwrapped), that it reads the store as it still stands, and that the reverse
// lookup it reads through sees a row of every kind that carries a scope_node.
//
// The REFERRING side (a row must not be placed at a node that is not there): that
// a write carrying an unresolvable placement is refused, for every one of those
// same kinds, on create and on re-place alike — and that an EMPTY placement is not
// mistaken for one.
//
// The table list both directions rest on is asserted structurally in
// placement_internal_test.go — see the note on placementProbes.

// errGuardRefused is a sentinel a test guard returns, so the assertion is that
// the CALLER'S OWN error came back — not something the store minted that merely
// looks like a refusal.
var errGuardRefused = errors.New("guard refused this delete")

// TestDeleteGuardRefusalRollsBack: a guard that returns an error leaves the row
// in place, leaves the generation where it was, and hands its own error back
// verbatim — the same three properties a WriteGuard's refusal has.
func TestDeleteGuardRefusalRollsBack(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)
	genBefore := gen(t, s)

	err := s.Delete(ctx, store.KindScopeNode, screenNodeID, 1, func(store.DeleteView) error {
		return errGuardRefused
	})
	if !errors.Is(err, errGuardRefused) {
		t.Fatalf("delete error = %v, want the guard's own error", err)
	}
	if g := gen(t, s); g != genBefore {
		t.Fatalf("generation moved on a refused delete: %d -> %d", genBefore, g)
	}
	if _, ok, _ := s.Get(ctx, store.KindScopeNode, screenNodeID); !ok {
		t.Fatalf("row was removed despite a refusing guard")
	}
}

// TestDeleteGuardsRunInOrderUntilOneRefuses: guards are evaluated in the order
// given and the first refusal short-circuits, so a caller supplying one guard per
// rule controls which rule answers when several apply at once.
func TestDeleteGuardsRunInOrderUntilOneRefuses(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)

	var ran []string
	err := s.Delete(ctx, store.KindScopeNode, screenNodeID, 1,
		func(store.DeleteView) error { ran = append(ran, "first"); return nil },
		func(store.DeleteView) error { ran = append(ran, "second"); return errGuardRefused },
		func(store.DeleteView) error { ran = append(ran, "third"); return nil },
	)
	if !errors.Is(err, errGuardRefused) {
		t.Fatalf("delete error = %v, want the second guard's refusal", err)
	}
	if len(ran) != 2 || ran[0] != "first" || ran[1] != "second" {
		t.Fatalf("guards ran %v, want [first second] — evaluation must stop at the first refusal", ran)
	}
}

// TestDeleteGuardSeesTheRowItIsAboutToRemove: the guard reads the store as it
// STILL stands. A guard that ran after the DELETE statement would see a store
// with the row already gone and could not decide anything about it.
func TestDeleteGuardSeesTheRowItIsAboutToRemove(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)

	var sawTarget bool
	if err := s.Delete(ctx, store.KindScopeNode, screenNodeID, 1, func(v store.DeleteView) error {
		rows, err := v.Rows(store.KindScopeNode)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.ID == screenNodeID {
				sawTarget = true
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !sawTarget {
		t.Fatalf("the guard did not see the row being deleted — it must run BEFORE the removal")
	}
	if _, ok, _ := s.Get(ctx, store.KindScopeNode, screenNodeID); ok {
		t.Fatalf("row survived a delete no guard refused")
	}
}

// TestPlacedAtSeesEveryKindThatCarriesAPlacement seeds one row of every kind
// that carries a scope_node, each on its own pair of probe nodes, and asserts
// the lookup reports the empty node as free and the populated one as in use —
// naming the table it found the row in.
func TestPlacedAtSeesEveryKindThatCarriesAPlacement(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)

	for i, probe := range placementProbes(t) {
		free, used := placementNodeID(2*i), placementNodeID(2*i+1)
		createProbeNode(t, s, free)
		createProbeNode(t, s, used)
		for _, row := range probe.rows(used) {
			if _, err := s.Create(ctx, row.kind, row.body); err != nil {
				t.Fatalf("%s: create %s row: %v", probe.kind, row.kind, err)
			}
		}

		// The empty node first: a lookup that answered "in use" unconditionally
		// could not pass this half, so the positive half below means something.
		if err := s.Delete(ctx, store.KindScopeNode, free, 1, expectPlacedAt(t, probe.kind, free, "")); err != nil {
			t.Fatalf("%s: probe delete of an empty node: %v", probe.kind, err)
		}
		if err := s.Delete(ctx, store.KindScopeNode, used, 1, expectPlacedAt(t, probe.kind, used, probe.wantTable)); err != nil {
			t.Fatalf("%s: probe delete of a referenced node: %v", probe.kind, err)
		}
	}
}

func createProbeNode(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if _, err := s.Create(context.Background(), store.KindScopeNode, mustJSON(t, datamodel.ScopeNode{
		ID: id, Kind: "screen", ParentID: strp(siteNodeID), Name: "Placement Probe",
	})); err != nil {
		t.Fatalf("create probe node %s: %v", id, err)
	}
}

// expectPlacedAt returns a guard asserting PlacedAt's verdict for node: wantTable
// empty means "nothing is placed here", otherwise the table the hit must name. It
// always returns nil, so the probing delete itself succeeds.
func expectPlacedAt(t *testing.T, kind store.Kind, node, wantTable string) store.DeleteGuard {
	t.Helper()
	return func(v store.DeleteView) error {
		p, found, err := v.PlacedAt(node)
		if err != nil {
			return err
		}
		if found != (wantTable != "") {
			t.Errorf("%s probe: PlacedAt(%s) found = %v (%q), want %v", kind, node, found, p.Table, wantTable != "")
			return nil
		}
		if found && p.Table != wantTable {
			t.Errorf("%s probe: PlacedAt(%s) reported table %q, want %q", kind, node, p.Table, wantTable)
		}
		return nil
	}
}

// placementNodeID mints a distinct, valid fixture ULID per probe node (no
// secrets; Crockford-base32 only).
func placementNodeID(i int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	return "01J8ZP1ACEMENTPR0BEN0DE0" + string([]byte{alphabet[(i/32)%32], alphabet[i%32]})
}

// placementRowID mints a fixture ULID for a referencing row: a 6-character tag
// naming its kind, suffixed by the last two characters of the node it is placed
// at, so every probe's rows carry ids of their own (a shared id across two probes
// is a duplicate-identity write, not a placement).
func placementRowID(tag, node string) string {
	return "01J8ZP1ACEMENTR0WS" + tag + node[len(node)-2:]
}

// placementRow is one row a probe writes: which kind it is and its body.
type placementRow struct {
	kind store.Kind
	body json.RawMessage
}

type placementProbe struct {
	kind store.Kind
	// rows are every row this probe places at the node — the kind under test
	// plus whatever rows its own write-time validation requires beside it.
	rows func(node string) []placementRow
	// wantTable is the table PlacedAt must name for the populated node: the
	// FIRST placement table in scan order carrying a row there, which for a kind
	// that cannot be written alone is one of its companions rather than itself.
	wantTable string
}

// placementProbes is one minimal, VALID row per kind that carries a scope_node —
// every store.Kind but scope_nodes.
//
// Two kinds cannot be written alone: DAT-007 requires a daypart's and a
// validity-window's scope_node to EQUAL its owning schedule's, so probing either
// necessarily places a schedule at the same node, and the lookup then reports the
// earlier arm. Their own arms are therefore covered structurally rather than
// behaviourally here — placement_internal_test.go asserts the scanned table list
// against allKinds itself, which is the property that would actually break.
func placementProbes(t *testing.T) []placementProbe {
	t.Helper()
	// one wraps a single-row body builder: the common case, where the kind under
	// test needs no companion row to be valid.
	one := func(kind store.Kind, build func(node string) map[string]any) func(string) []placementRow {
		return func(n string) []placementRow {
			return []placementRow{{kind, mustJSON(t, build(n))}}
		}
	}
	scheduleAt := func(n string) placementRow {
		return placementRow{store.KindSchedule, mustJSON(t, datamodel.Schedule{
			ID: placementRowID("SCHED0", n), ScopeNode: n, Name: "Owning"})}
	}
	playlistAt := func(n string) placementRow {
		return placementRow{store.KindPlaylist, mustJSON(t, datamodel.Playlist{
			ID: placementRowID("P1AY11", n), ScopeNode: n, Name: "Placed",
			Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:cafef00d"}}})}
	}
	return []placementProbe{
		{store.KindPlaylist, func(n string) []placementRow {
			return []placementRow{playlistAt(n)}
		}, "playlists"},
		{store.KindSchedule, func(n string) []placementRow {
			return []placementRow{scheduleAt(n)}
		}, "schedules"},
		{store.KindDaypart, func(n string) []placementRow {
			return []placementRow{scheduleAt(n), playlistAt(n), {store.KindDaypart, mustJSON(t, datamodel.Daypart{
				ID: placementRowID("DAYPRT", n), ScheduleID: placementRowID("SCHED0", n), ScopeNode: n,
				DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "06:00:00", EndTime: "22:00:00",
				DisplayPower: "on", PlaylistID: placementRowID("P1AY11", n),
			})}}
		}, "playlists"},
		{store.KindValidityWindow, func(n string) []placementRow {
			return []placementRow{scheduleAt(n), {store.KindValidityWindow, mustJSON(t, map[string]any{
				"id": placementRowID("VA11DW", n), "schedule_id": placementRowID("SCHED0", n), "scope_node": n,
			})}}
		}, "schedules"},
		{store.KindFallback, one(store.KindFallback, func(n string) map[string]any {
			return map[string]any{"id": placementRowID("FA11BK", n), "scope_node": n, "display_power": "blank"}
		}), "fallbacks"},
		{store.KindPresetBatch, one(store.KindPresetBatch, func(n string) map[string]any {
			return map[string]any{
				"preset_id": placementRowID("PRESET", n), "scope_node": n, "name": "Placed",
				"commands": []any{map[string]any{"entity_id": placementRowID("ENT1TY", n), "command": "power_on"}},
			}
		}), "preset_batches"},
		{store.KindAutomation, one(store.KindAutomation, func(n string) map[string]any {
			return placedAutomation(placementRowID("AT0MAT", n), n)
		}), "automations"},
		{store.KindWebhookEndpoint, one(store.KindWebhookEndpoint, func(n string) map[string]any {
			return map[string]any{
				"id": placementRowID("WEBH00", n), "scope_node": n,
				"url": "https://webhook.example/ingest", "schemas": []any{"content.played"},
			}
		}), "webhook_endpoints"},
		{store.KindScreen, one(store.KindScreen, func(n string) map[string]any {
			return map[string]any{"id": placementRowID("SCREEN", n), "scope_node": n, "name": "Placed"}
		}), "screens"},
		{store.KindAdoptedDevice, one(store.KindAdoptedDevice, func(n string) map[string]any {
			return map[string]any{
				"id": placementRowID("DEV1CE", n), "scope_node": n, "name": "Placed",
				"driver": "roku-ecp", "native_id": "10.0.0.41", "entities": []any{map[string]any{
					"entity_id": placementRowID("ENT1TY", n), "device_class": "media-player", "enabled": true,
					"hidden": false, "display_name": "Placed", "category": "primary",
				}},
			}
		}), "adopted_devices"},
	}
}

// placedAutomation is a compile-clean rule row (the automations kind is gated by
// the rules compiler, not by ValidateRows, so a stub body would be rejected
// before it could be placed anywhere).
func placedAutomation(id, node string) map[string]any {
	entity := placementRowID("ENT1TY", node)
	return map[string]any{
		"id": id, "scope_node": node, "name": "Placed Automation", "enabled": true, "mode": "single",
		"triggers": []any{map[string]any{
			"type": "state", "entity_id": entity, "attribute": "power", "to": "on",
		}},
		"actions": []any{map[string]any{
			"type": "device_command", "entity_id": entity, "command": "power_on",
		}},
	}
}

// ---- the forward direction: a placement that names no scope node ----------

// absentPlacementNode is a fixture ULID (no secrets) that names NO scope node in
// any test below. It is the whole subject of the cases that follow: DAT-006 makes
// a row's scope_node the id of the node it is placed under, and this is not one.
const absentPlacementNode = "01J8ZN0SVCHN0DEANYWHERE001"

// assertUnresolvedPlacement requires err to be the store's DAT-006 refusal, and
// requires it to be the ONLY error carried.
//
// "Only" is the load-bearing half. The check runs ahead of every per-kind
// validator, so a row refused for its placement must not ALSO be reported against
// rules it could not possibly satisfy while unplaced (a daypart's schedule_id, say,
// naming a schedule whose own create was refused for the same reason). A caller
// reading the 422 gets the one fixable thing.
func assertUnresolvedPlacement(t *testing.T, err error, what string) {
	t.Helper()
	var verr *store.ValidationError
	if !errors.As(err, &verr) {
		t.Errorf("%s placed at a node that does not exist: err = %v, want a *store.ValidationError", what, err)
		return
	}
	// The placement fault must be PRESENT and must LEAD — not be the only thing
	// reported. Insisting on exactly one error was how this helper came to pin a
	// regression: it made a short-circuit look correct, and a short-circuit
	// collapses api/1 API-013's published multi-field answer to a single error
	// whenever a row has an unrelated fault as well.
	if len(verr.Errors) == 0 {
		t.Errorf("%s: refusal carries no errors", what)
		return
	}
	if e := verr.Errors[0]; e.Field != "scope_node" || e.Code != "ROW_SCOPE_NODE_UNRESOLVED" {
		t.Errorf("%s: leading refusal = {field:%q code:%q}, want {scope_node ROW_SCOPE_NODE_UNRESOLVED}; all = %+v",
			what, e.Field, e.Code, verr.Errors)
	}
}

// TestEveryPlacementTableRefusesAnUnresolvablePlacement is the forward direction's
// completeness check, run over the SAME probe set the reverse lookup's own test
// uses — one minimal, valid row per kind that carries a scope_node.
//
// Those probes are known-good at a real node: TestPlacedAtSeesEveryKindThatCarriesA
// Placement creates every one of them successfully. So a refusal here is
// attributable to the placement and to nothing else, and the tree the store holds
// is a complete, conformant one — the only thing wrong in the whole store is where
// these rows say they are.
//
// pack_rows gets an arm of its own because it is not a Kind and is written through
// its own method, which is exactly why placement.go names it by hand; the structural
// half of the claim (that these are ALL the placement tables) is
// placement_internal_test.go's.
func TestEveryPlacementTableRefusesAnUnresolvablePlacement(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)

	for _, probe := range placementProbes(t) {
		for _, row := range probe.rows(absentPlacementNode) {
			_, err := s.Create(ctx, row.kind, row.body)
			assertUnresolvedPlacement(t, err, string(row.kind)+" (probing "+string(probe.kind)+")")
			// Nothing was written: the table is still empty, so the refusal is a
			// rollback rather than a stored row plus an error.
			if stored, err := s.List(ctx, row.kind, store.ListFilter{}); err != nil {
				t.Fatalf("list %s: %v", row.kind, err)
			} else if len(stored) != 0 {
				t.Errorf("%s: a refused create left %d row(s) behind", row.kind, len(stored))
			}
		}
	}

	if _, _, err := s.InstallPack(ctx, packSpec("acme/placement", "1.0.0", 1)); err != nil {
		t.Fatalf("install the pack whose collection row is probed: %v", err)
	}
	_, err := s.CreatePackRow(ctx, "acme/placement", "menu_items",
		store.PackRow{LifecycleState: "published", ScopeNode: absentPlacementNode, Body: json.RawMessage(`{"name":"Burger"}`)})
	assertUnresolvedPlacement(t, err, "pack_rows")
	rows, err := s.ListPackRows(ctx, "acme/placement", "menu_items")
	if err != nil {
		t.Fatalf("list pack rows: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused pack-row create left %d row(s) behind", len(rows))
	}
}

// TestRePlacingOntoAnAbsentNodeIsRefused is the UPDATE half. A create is not the
// only way a row acquires a placement: a patch may move one, and the effective
// post-merge scope_node is what has to resolve — otherwise a row could be created
// correctly and then walked off the tree in a second request.
func TestRePlacingOntoAnAbsentNodeIsRefused(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)

	pl := datamodel.Playlist{
		ID: playlistID, ScopeNode: screenNodeID, Name: "Placed",
		Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: "sha256:cafef00d"}},
	}
	created, err := s.Create(ctx, store.KindPlaylist, mustJSON(t, pl))
	if err != nil {
		t.Fatalf("create the playlist to be re-placed: %v", err)
	}

	_, err = s.Update(ctx, store.KindPlaylist, playlistID, created.Revision,
		mustJSON(t, map[string]any{"scope_node": absentPlacementNode}))
	assertUnresolvedPlacement(t, err, "playlists (re-placed)")

	// The refusal rolled back: the row is still at the node it was authored at,
	// still at the revision the caller held.
	after, found, err := s.Get(ctx, store.KindPlaylist, playlistID)
	if err != nil || !found {
		t.Fatalf("the refused update removed the row (found=%v err=%v)", found, err)
	}
	if after.ScopeNode != screenNodeID || after.Revision != created.Revision {
		t.Fatalf("after a refused re-place: scope_node=%q revision=%d, want %q/%d",
			after.ScopeNode, after.Revision, screenNodeID, created.Revision)
	}
}

// TestAnUnplacedRowIsNotARowPlacedAtNothing: an EMPTY scope_node is left alone by
// the reference check. Every row's column defaults to the empty string, so ""
// means unplaced — and whether a given kind may BE unplaced is DAT-006's presence
// half, which the per-kind validators decide (a screen row is refused
// ROW_SCOPE_NODE_MISSING; a webhook endpoint is not). A reference check that
// refused "" would be quietly deciding that rule for every kind at once.
func TestAnUnplacedRowIsNotARowPlacedAtNothing(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, store.KindWebhookEndpoint, mustJSON(t, map[string]any{
		"id": "01J8ZN0P1ACEDWEBH00KR0W001", "url": "https://webhook.example/ingest",
		"schemas": []any{"content.played"},
	})); err != nil {
		t.Fatalf("create an endpoint carrying no placement: %v", err)
	}

	// And the presence half answers for a kind that DOES require one. When this
	// test was written that made the split look like a division of labour; it was
	// not — only the two identity kinds enforced presence, while all six
	// scheduling-core kinds accepted an unplaced row. Both a screen (identity) and
	// a schedule (scheduling core) are checked below, so the claim is now true of
	// both halves of DAT-006's own list rather than of one example.
	_, err := s.Create(ctx, store.KindScreen, mustJSON(t, map[string]any{
		"id": "01J8ZN0P1ACEDSCREENR0W0001", "name": "Unplaced Screen",
	}))
	var verr *store.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("a screen row carrying no placement: err = %v, want a *store.ValidationError", err)
	}
	var sawMissing bool
	for _, e := range verr.Errors {
		if e.Code == "ROW_SCOPE_NODE_MISSING" {
			sawMissing = true
		}
	}
	if !sawMissing {
		t.Fatalf("a screen row carrying no placement was refused %+v, want a ROW_SCOPE_NODE_MISSING among them", verr.Errors)
	}
}

// TestAnUnplacedSchedulingRowIsRefusedToo is the other half of DAT-006's list.
// The six scheduling-core kinds DAT-005 enumerates require a placement exactly as
// the identity rows do; before checkRowPlacement they accepted one at 201.
func TestAnUnplacedSchedulingRowIsRefusedToo(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	_, err := s.Create(ctx, store.KindSchedule, mustJSON(t, map[string]any{
		"id": "01J8ZN0P1ACEDSCHEDR0W0001", "name": "Unplaced Schedule",
		"timezone": "America/Chicago",
	}))
	var verr *store.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("a schedule carrying no placement: err = %v, want a *store.ValidationError", err)
	}
	var found bool
	for _, e := range verr.Errors {
		if e.Field == "scope_node" && e.Code == "ROW_SCOPE_NODE_MISSING" {
			found = true
		}
	}
	if !found {
		t.Fatalf("refusal does not name scope_node/ROW_SCOPE_NODE_MISSING: %+v", verr.Errors)
	}
}

// TestAScopeNodeIsNotItselfAPlacement pins the two halves of a regression that
// shipped and was caught in review: the placement check ran for KindScopeNode.
//
// DAT-006 is explicit that every row "OTHER THAN A SCOPE NODE ITSELF" carries a
// scope_node, so a scope node is the thing a placement points AT. Running the
// check on it refused a scope-node write with a message naming `table
// scope_nodes` — the one table that never holds a placement — and, worse, it put
// scope_nodes on the FORWARD list while placedAt keeps it off the REVERSE one.
// placement.go's own doc names that asymmetry as the defect to avoid: a table on
// one list and not the other is a store where a node cannot be deleted but a row
// may point at nothing.
func TestAScopeNodeIsNotItselfAPlacement(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	seedSiteScreen(t, s)

	// A screen-kind node under the seeded site, carrying a stray `scope_node`
	// that resolves to nothing. It must not be refused FOR THAT: a scope node has
	// no placement to be wrong about.
	body := mustJSON(t, screenNode())
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw["id"] = "01J8ZSTRAYSCREENNDE0000001" // distinct from seedSiteScreen's own screen node
	raw["scope_node"] = absentPlacementNode
	withStray, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, err := s.Create(ctx, store.KindScopeNode, withStray); err != nil {
		var verr *store.ValidationError
		if errors.As(err, &verr) {
			for _, e := range verr.Errors {
				if e.Field == "scope_node" {
					t.Fatalf("a scope-node create was refused for its own scope_node (%s: %s) — a scope node is what a placement points at, not a row that carries one",
						e.Code, e.Message)
				}
			}
		}
		t.Logf("refused for an unrelated reason, which is this fixture's business and not the placement rule's: %v", err)
	}
}
