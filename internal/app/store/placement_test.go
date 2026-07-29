package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// placement_test.go covers the delete-guard seam from the outside: that a
// DeleteGuard actually gates the removal (and its error reaches the caller
// unwrapped), that it reads the store as it still stands, and that the reverse
// placement lookup it reads through sees a row of every kind that carries a
// scope_node. The lookup's own table list is asserted structurally in
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
