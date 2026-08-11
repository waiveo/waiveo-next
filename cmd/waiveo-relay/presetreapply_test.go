package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// The device plane is where a generation apply's cost is felt, and these tests
// measure it there: how many REAL device commands a governed scope node's
// daypart preset batch dispatches across a sequence of applies.
//
// They are driven through the FEEDER's own store -> snapshot path rather than a
// hand-built schedule section, because the defect they pin only exists on that
// path: the feeder re-mints every content URL twice a day
// (contenturl.SnapshotRemintInterval, cmd/waiveo-feeder/remint.go) and
// announces it by advancing the generation, so the relay sees a HIGHER
// generation carrying a DIFFERENT section hash — REL-070's same-hash
// suppression correctly does not fire — over a schedule section that is byte
// for byte what it already had. A fixture that stamps its own generation cannot
// demonstrate that the hash really differs while the schedule really does not.
//
// The import of internal/app/store here is the feeder's store, already a
// transitive dependency of internal/feeder/snapshot, which these tests use to
// build the snapshot the relay applies.

// presetApplyEntityID is the entity the seeded demo preset batch commands
// (store.SeedDemo's own seedRuleEntityID). The seeded "Content Hours" daypart
// (06:00-22:00 America/Chicago) binds that batch; the seeded "Overnight Blank"
// daypart binds none.
const presetApplyEntityID = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"

// seededPresetBatchID is the batch that daypart binds (store.SeedDemo's own
// seedPresetBatchID) — the row an operator edits to change what a daypart
// asserts on entry.
const seededPresetBatchID = "01J8Z8DEM0PRESETBATCHF1RE1"

// seededFeederStore is an in-memory app store carrying store.SeedDemo's
// two-daypart schedule — the ordinary signage shape: one daypart binding a
// preset batch, one not.
func seededFeederStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SeedDemo(context.Background(), signhash.ContentID([]byte("preset-apply-image-bytes"))); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	return s
}

// appliedFromFeederStore runs the REAL feeder build (snapshot.BuildFromStore,
// signing content URLs exactly as a deployment with a content-URL key does) over
// s's current generation and adapts the result into the desiredstate.Applied a
// verified pull hands the relay.
func appliedFromFeederStore(t *testing.T, s *store.Store, nowMs int64) desiredstate.Applied {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	rows, err := s.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	sign := contenturl.Signer{
		Base:  "https://origin.example",
		Key:   []byte("preset-apply-content-url-key"),
		TTL:   contenturl.SnapshotTTL,
		NowMs: nowMs,
	}
	snap, _, err := snapshot.BuildFromStore(rows, sign, id, nowMs)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	applied := desiredstate.Applied{
		Generation:     snap.Generation,
		Hash:           snap.Hash,
		ScreenPrograms: snap.Sections.ScreenPrograms,
		Schedule:       snap.Sections.Schedule,
		ContentOrigin:  snap.Sections.RevocationAndSite.ContentOrigin,
	}
	if len(snap.Sections.ScreenPrograms) > 0 {
		applied.ScreenID = snap.Sections.ScreenPrograms[0].ScreenID
		applied.ProgramRevision = snap.Sections.ScreenPrograms[0].ProgramRevision
		applied.Priority = snap.Sections.ScreenPrograms[0].Priority
		applied.Display = snap.Sections.ScreenPrograms[0].Display
	}
	return applied
}

// presetApplyDriver builds a scheduleDriver wired to a recording controller
// through the SAME automation.CommandSink -> deviceplane.CommandSurface path the
// running binary dispatches a preset batch through, with an entity resolver that
// resolves every entity — so what the recorder sees is what a resolvable device
// would have been sent.
//
// A permissive resolver is load-bearing, not convenience: the real
// deviceplane.CommandSurface refuses an entity nothing has adopted BEFORE the
// controller (command.go's resolveEntity gate), and the binary wires no
// CommandLog or CommandJournal on this surface — so on a deployment whose preset
// batch names an unadopted entity, a fired batch leaves no trace anywhere at
// all. Asserting at the controller is what makes the dispatch observable.
func presetApplyDriver(t *testing.T) (*scheduleDriver, *recordingController) {
	t.Helper()
	ctrl := &recordingController{}
	surface := deviceplane.NewCommandSurface(ctrl, deviceclass.Builtin(),
		func(entityID string) (string, string, bool) { return entityID + "-device", "media-player", true })
	srv, _ := newTestPlayerServer(t)
	return &scheduleDriver{
		srv:       srv,
		sink:      automation.NewCommandSink(surface, "01J8ZPRESETAPPLYRELAYID001"),
		site:      hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298},
		tickEvery: time.Hour, // never ticks within a test; every instant is driven explicitly
	}, ctrl
}

// presetApplyInstant is a fixed Unix-ms instant at h:m America/Chicago — a
// deterministic "now" so a resolve lands in the intended daypart regardless of
// when the test runs.
func presetApplyInstant(t *testing.T, h, m int) int64 {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return time.Date(2026, time.January, 15, h, m, 0, 0, loc).UnixMilli()
}

// TestReMintReapplyDoesNotRefireTheEffectiveDaypartsPresetBatch is the
// device-plane cost of the content-URL re-mint, pinned.
//
// The feeder advances the desired-state generation every
// contenturl.SnapshotRemintInterval so the fleet re-pulls freshly minted content
// URLs (cmd/waiveo-feeder/remint.go). Nothing is authored by that tick, but every
// minted url changes, so the snapshot's section hash changes, so REL-070's
// same-hash suppression correctly does not fire and a FULL apply proceeds — which
// rebuilds every governed node's schedule resolver.
//
// Without a carried rising-edge baseline that rebuild looked like a boot to
// datamodel.PresetTransition (a nil prev is the empty daypart identity), so the
// already-effective daypart's preset batch — display_power, input select, volume
// on an ordinary signage site — was re-dispatched to real hardware twice a day,
// forever, at whatever wall-clock instant the feeder process last started at.
//
// The assertion is a dispatch COUNT at the device plane: one apply, two ordinary
// live ticks, a re-mint apply at the same effective daypart with nothing
// authored — exactly one device command in total.
func TestReMintReapplyDoesNotRefireTheEffectiveDaypartsPresetBatch(t *testing.T) {
	s := seededFeederStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	driver, ctrl := presetApplyDriver(t)

	// Apply #1, inside the seeded content daypart: the genuine resume edge fires
	// its bound preset batch once (DAT-075).
	noon := presetApplyInstant(t, 12, 0)
	first := appliedFromFeederStore(t, s, noon)
	_, resolvers := driver.apply(ctx, first, noon)
	if len(resolvers) != 1 {
		t.Fatalf("apply #1 built %d resolver(s), want 1 (the seeded schedule governs exactly one screen)", len(resolvers))
	}
	if calls := ctrl.calls(); len(calls) != 1 || calls[0] != presetApplyEntityID+"/launch" {
		t.Fatalf("apply #1 dispatched %v, want exactly [%s/launch] — the resume edge DAT-075 names", calls, presetApplyEntityID)
	}

	// Two ordinary live ticks inside the SAME daypart: level-triggered state, no
	// edge, nothing dispatched.
	resolvers[0].Tick(presetApplyInstant(t, 12, 30), driver.sink)
	resolvers[0].Tick(presetApplyInstant(t, 13, 0), driver.sink)
	if calls := ctrl.calls(); len(calls) != 1 {
		t.Fatalf("two live ticks inside the same daypart dispatched %v, want no additional command", calls)
	}

	// The re-mint: the generation advances with NOTHING authored, and the feeder
	// rebuilds the snapshot. This is remintLoop's whole effect.
	if err := s.AdvanceGeneration(context.Background()); err != nil {
		t.Fatalf("AdvanceGeneration: %v", err)
	}
	remintAt := presetApplyInstant(t, 13, 30)
	second := appliedFromFeederStore(t, s, remintAt)

	// The two premises the defect stands on, asserted rather than assumed: the
	// hash really does differ (so REL-070 does not suppress the apply), and the
	// schedule section really is unchanged (so nothing about the dayparting was
	// authored).
	if second.Hash == first.Hash {
		t.Fatalf("re-mint produced the same section hash %s — this test no longer exercises a full apply", second.Hash)
	}
	if second.Generation <= first.Generation {
		t.Fatalf("re-mint generation %d did not advance past %d", second.Generation, first.Generation)
	}
	assertSameScheduleSection(t, first, second)

	driver.apply(ctx, second, remintAt)
	if calls := ctrl.calls(); len(calls) != 1 {
		t.Fatalf("the re-mint apply dispatched %v (%d commands), want the original 1: a generation apply over a node the relay was ALREADY resolving is not a resume, and must not re-assert the daypart's device state",
			calls, len(calls))
	}
}

// TestAnAuthoredPresetEditIsDispatchedByTheApplyThatCarriesIt is the MIRROR of
// the re-mint test above, driven through the same real feeder path, and the two
// together are the property. The carry must suppress an apply that changed
// nothing — and only that.
//
// Keying the carry on effective-daypart identity alone fails this. The seeded
// "Content Hours" daypart holds 06:00-22:00, so between the edit and 22:00
// there is no rising edge left for it to ride: an apply that declines to
// dispatch is not deferring the edit, it is dropping it. Nothing about editing
// a preset batch ties to a rules/1 trigger either, so the operator has no other
// route — the display keeps the OLD scene until someone restarts the relay.
//
// The edit here is the smallest real one: the same batch, same daypart, same
// window, one command re-authored. Every id involved is unchanged, which is
// exactly why identity cannot see it.
func TestAnAuthoredPresetEditIsDispatchedByTheApplyThatCarriesIt(t *testing.T) {
	s := seededFeederStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	driver, ctrl := presetApplyDriver(t)

	// Apply #1 inside content hours: the resume edge fires the authored batch.
	noon := presetApplyInstant(t, 12, 0)
	driver.apply(ctx, appliedFromFeederStore(t, s, noon), noon)
	if calls := ctrl.calls(); len(calls) != 1 || calls[0] != presetApplyEntityID+"/launch" {
		t.Fatalf("apply #1 dispatched %v, want exactly [%s/launch]", calls, presetApplyEntityID)
	}

	// The operator re-authors the effective daypart's bound preset batch. A store
	// Update advances the generation by itself — this IS what an edit looks like
	// on the wire, no synthetic AdvanceGeneration needed.
	batch, ok, err := s.Get(context.Background(), store.KindPresetBatch, seededPresetBatchID)
	if err != nil || !ok {
		t.Fatalf("Get(preset batch %s) = ok %v, err %v", seededPresetBatchID, ok, err)
	}
	edit := json.RawMessage(`{"commands":[{"entity_id":"` + presetApplyEntityID + `","command":"home"}]}`)
	if _, err := s.Update(context.Background(), store.KindPresetBatch, seededPresetBatchID, batch.Revision, edit); err != nil {
		t.Fatalf("Update(preset batch): %v", err)
	}

	// Apply #2, still inside the SAME daypart at a later instant — the shape the
	// relay sees for that edit. The newly authored command must be dispatched.
	edited := presetApplyInstant(t, 13, 30)
	driver.apply(ctx, appliedFromFeederStore(t, s, edited), edited)
	calls := ctrl.calls()
	if len(calls) != 2 || calls[1] != presetApplyEntityID+"/home" {
		t.Fatalf("the apply carrying the edited preset batch dispatched %v, want a second command [%s/home] — the effective daypart's identity never changes again inside its window, so an apply that swallows the edit never delivers it at all",
			calls, presetApplyEntityID)
	}

	// And the carry is not simply disabled by having seen one edit: a further
	// apply with nothing authored (a re-mint) is silent again.
	if err := s.AdvanceGeneration(context.Background()); err != nil {
		t.Fatalf("AdvanceGeneration: %v", err)
	}
	after := presetApplyInstant(t, 14, 0)
	driver.apply(ctx, appliedFromFeederStore(t, s, after), after)
	if calls := ctrl.calls(); len(calls) != 2 {
		t.Fatalf("a re-mint apply after the edit dispatched %v (%d total), want the 2 already dispatched — the carry must resume once the rows stop changing", calls, len(calls))
	}
}

// TestABootStillFiresTheEffectiveDaypartsPresetBatch is the mirror guard: the
// fix above must suppress a REBUILD's spurious edge and nothing else. A relay
// booting inside an already-active preset-bound daypart has no prior resolver to
// carry a baseline from, has genuinely missed every edge while it was down, and
// MUST fire once (DAT-075 governed by DAT-121's catch_up_once default).
func TestABootStillFiresTheEffectiveDaypartsPresetBatch(t *testing.T) {
	s := seededFeederStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	noon := presetApplyInstant(t, 12, 0)
	applied := appliedFromFeederStore(t, s, noon)

	// A fresh driver IS the boot: nothing was resolving this node before it.
	driver, ctrl := presetApplyDriver(t)
	driver.apply(ctx, applied, noon)
	if calls := ctrl.calls(); len(calls) != 1 || calls[0] != presetApplyEntityID+"/launch" {
		t.Fatalf("boot dispatched %v, want exactly [%s/launch] — a boot resume edge must still fire", calls, presetApplyEntityID)
	}

	// And the boot path proper (bootScheduleResolverAt, which passes no carried
	// state at all) fires it too.
	bootCtrl := &recordingController{}
	bootSurface := deviceplane.NewCommandSurface(bootCtrl, deviceclass.Builtin(),
		func(entityID string) (string, string, bool) { return entityID + "-device", "media-player", true })
	bootSrv, _ := newTestPlayerServer(t)
	bootScheduleResolverAt(applied, bootSrv, automation.NewCommandSink(bootSurface, "01J8ZPRESETAPPLYRELAYID001"),
		hello.SiteBinding{TZ: "America/Chicago"}, noon)
	if calls := bootCtrl.calls(); len(calls) != 1 || calls[0] != presetApplyEntityID+"/launch" {
		t.Fatalf("bootScheduleResolverAt dispatched %v, want exactly [%s/launch]", calls, presetApplyEntityID)
	}
}

// TestADaypartBoundaryCrossedAtAnApplyStillFires is the other mirror guard: a
// carried baseline suppresses only an UNCHANGED effective daypart. When the
// apply lands after the node's effective daypart genuinely changed, the carried
// baseline names the OLD daypart, the rebuild resolves a different identity, and
// the newly effective daypart's preset batch fires — exactly as a live tick
// across the same boundary would.
//
// The seeded schedule is the shape that makes this observable: "Overnight Blank"
// (22:00-06:00) binds no preset batch, "Content Hours" (06:00-22:00) binds one.
func TestADaypartBoundaryCrossedAtAnApplyStillFires(t *testing.T) {
	s := seededFeederStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	driver, ctrl := presetApplyDriver(t)

	// Apply #1 overnight: the effective daypart binds no preset, so nothing fires
	// — and the resolver's carried baseline becomes the overnight daypart.
	overnight := presetApplyInstant(t, 2, 0)
	driver.apply(ctx, appliedFromFeederStore(t, s, overnight), overnight)
	if calls := ctrl.calls(); len(calls) != 0 {
		t.Fatalf("the overnight apply dispatched %v, want nothing (that daypart binds no preset batch)", calls)
	}

	// Apply #2 inside content hours, again with nothing authored: the effective
	// daypart CHANGED, so this is a real rising edge and must fire.
	if err := s.AdvanceGeneration(context.Background()); err != nil {
		t.Fatalf("AdvanceGeneration: %v", err)
	}
	noon := presetApplyInstant(t, 12, 0)
	driver.apply(ctx, appliedFromFeederStore(t, s, noon), noon)
	if calls := ctrl.calls(); len(calls) != 1 || calls[0] != presetApplyEntityID+"/launch" {
		t.Fatalf("an apply landing after a genuine daypart boundary dispatched %v, want exactly [%s/launch] — carrying the baseline must not swallow a real edge",
			calls, presetApplyEntityID)
	}
}

// assertSameScheduleSection fails unless a and b carry byte-identical schedule
// sections — the premise that makes the re-mint apply "nothing authored".
func assertSameScheduleSection(t *testing.T, a, b desiredstate.Applied) {
	t.Helper()
	rowSets := []struct {
		name string
		x, y [][]byte
	}{
		{"scope_nodes", rawBytes(a.Schedule.ScopeNodes), rawBytes(b.Schedule.ScopeNodes)},
		{"playlists", rawBytes(a.Schedule.Playlists), rawBytes(b.Schedule.Playlists)},
		{"casts", rawBytes(a.Schedule.Casts), rawBytes(b.Schedule.Casts)},
		{"schedules", rawBytes(a.Schedule.Schedules), rawBytes(b.Schedule.Schedules)},
		{"validity_windows", rawBytes(a.Schedule.ValidityWindows), rawBytes(b.Schedule.ValidityWindows)},
		{"dayparts", rawBytes(a.Schedule.Dayparts), rawBytes(b.Schedule.Dayparts)},
		{"fallbacks", rawBytes(a.Schedule.Fallbacks), rawBytes(b.Schedule.Fallbacks)},
		{"preset_batches", rawBytes(a.Schedule.PresetBatches), rawBytes(b.Schedule.PresetBatches)},
	}
	for _, rs := range rowSets {
		if len(rs.x) != len(rs.y) {
			t.Fatalf("schedule section %s changed across the re-mint (%d -> %d rows) — the test premise (nothing authored) no longer holds", rs.name, len(rs.x), len(rs.y))
		}
		for i := range rs.x {
			if string(rs.x[i]) != string(rs.y[i]) {
				t.Fatalf("schedule section %s[%d] changed across the re-mint — the test premise (nothing authored) no longer holds:\n old %s\n new %s", rs.name, i, rs.x[i], rs.y[i])
			}
		}
	}
}

func rawBytes[T ~[]byte](rows []T) [][]byte {
	out := make([][]byte, 0, len(rows))
	for _, r := range rows {
		out = append(out, []byte(r))
	}
	return out
}
