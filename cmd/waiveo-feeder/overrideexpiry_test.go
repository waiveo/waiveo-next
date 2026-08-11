package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// overrideexpiry_test.go drives the half of a screen override's TTL that was
// declared everywhere and performed nowhere: the LAPSE.
//
// data-model/1 DAT-004d says a lapsed override is treated as absent "with no
// write required to retire it", the openapi description says an alert
// self-limits, and the shipped code evaluated expiry only inside
// snapshot.DeriveScreenPrograms — behind a snapshot cache keyed on the store
// GENERATION, which advances only on a write. A sixty-second alert therefore
// stayed `priority=preempt pinned=true` for as long as nobody edited anything
// else.
//
// So the whole test is: advance ONLY the clock, write NOTHING, and assert the
// screen falls back to its schedule.

// ovxScreenOverride patches a screen row's `override` member, the one way an
// override is stored (DAT-004c). It is a direct store write so the clock the
// assertions run on stays entirely under the test's control.
func ovxSetOverride(t *testing.T, st *store.Store, screenID string, o *datamodel.ScreenOverride) {
	t.Helper()
	ctx := context.Background()
	res, found, err := st.Get(ctx, store.KindScreen, screenID)
	if err != nil || !found {
		t.Fatalf("read screen %s: found=%v err=%v", screenID, found, err)
	}
	patch, err := json.Marshal(map[string]any{"override": o})
	if err != nil {
		t.Fatalf("marshal override: %v", err)
	}
	if _, err := st.Update(ctx, store.KindScreen, screenID, res.Revision, patch); err != nil {
		t.Fatalf("write override: %v", err)
	}
}

// ovxProgram returns the screen's entry in a derived snapshot's
// `screen_programs` section.
func ovxProgram(t *testing.T, snap wire.StateSnapshotBody, screenID string) wire.ScreenProgram {
	t.Helper()
	for _, sp := range snap.Sections.ScreenPrograms {
		if sp.ScreenID == screenID {
			return sp
		}
	}
	t.Fatalf("no screen_programs entry for %s in %+v", screenID, snap.Sections.ScreenPrograms)
	return wire.ScreenProgram{}
}

// TestAnAlertsTTLLapsesWithNoWriterRunning is the regression. A sixty-second
// alert is imposed, then the CLOCK ALONE moves past its expiry: no api write, no
// store write of any kind, nothing bumping the generation. The next pull must
// answer with the screen's schedule again.
//
// It drives desiredStateSource.current(), which is what a relay's state.pull is
// answered from — not snapshot.DeriveScreenPrograms, which was always right and
// was never the thing that was broken. The distinction is the whole bug: the
// derivation evaluated expiry correctly and the cache in front of it never asked
// again.
func TestAnAlertsTTLLapsesWithNoWriterRunning(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	img := placeholderImage()
	if err := st.SeedDemo(ctx, signhash.ContentID(img)); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}

	// The injected clock. Everything below moves it by hand; nothing sleeps and
	// nothing reads the wall.
	base := feederContentInstant(t)()
	now := base
	src := &desiredStateSource{
		store:          st,
		content:        origin.New(),
		contentBaseURL: "https://192.0.2.12:7420",
		id:             id,
		nowMs:          func() int64 { return now },
	}

	// Baseline: the screen resolves through its schedule, so nothing below can
	// pass vacuously.
	before, err := src.current()
	if err != nil {
		t.Fatalf("current() before the alert: %v", err)
	}
	if got := ovxProgram(t, before, store.SeedScreenID); got.Priority != "scheduled" || got.Pinned {
		t.Fatalf("fixture: the un-overridden screen derives priority=%q pinned=%v, want scheduled/false", got.Priority, got.Pinned)
	}

	// A sixty-second fire-drill alert, exactly as ShowAlert composes one: an
	// absolute expires_at derived from the imposing instant.
	const ttlMs = 60_000
	ovxSetOverride(t, st, store.SeedScreenID, &datamodel.ScreenOverride{
		Mode:      datamodel.ScreenOverrideModeAlert,
		Message:   "FIRE DRILL — leave by the east door",
		SetAt:     now,
		ExpiresAt: now + ttlMs,
	})

	during, err := src.current()
	if err != nil {
		t.Fatalf("current() during the alert: %v", err)
	}
	alert := ovxProgram(t, during, store.SeedScreenID)
	if alert.Priority != "preempt" || !alert.Pinned {
		t.Fatalf("during the alert the screen derives priority=%q pinned=%v, want preempt/true", alert.Priority, alert.Pinned)
	}

	// Still inside the window: it must NOT have lapsed early. This is the half a
	// cache-invalidate-always fix would break, and it would break silently — an
	// alert that vanished a second after it was raised looks like a delivery
	// problem, not like a bug here.
	now = base + ttlMs - 1
	still := ovxProgram(t, mustCurrent(t, src), store.SeedScreenID)
	if still.Priority != "preempt" || !still.Pinned {
		t.Fatalf("1ms before expiry the screen derives priority=%q pinned=%v, want the alert still in force", still.Priority, still.Pinned)
	}

	// --- The clock reaches the expiry. NOTHING ELSE HAPPENS.
	genBefore, err := st.Generation(ctx)
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}
	// EXACTLY at expires_at, not a millisecond past it. Applies is
	// `ExpiresAt > tMs`, so the override has already stopped applying at this
	// instant — and a cache window written `now <= cachedUntil` would serve the
	// alert for one more pull while the derivation behind it disagreed. One
	// millisecond is invisible in production and is exactly the kind of
	// boundary this whole defect was made of, so it is pinned here rather than
	// stepped over.
	now = base + ttlMs

	after := ovxProgram(t, mustCurrent(t, src), store.SeedScreenID)
	if after.Pinned {
		t.Error("after its TTL the alert is still Pinned — the relay goes on refusing to re-resolve this screen, so it never returns to its schedule")
	}
	if after.Priority != "scheduled" {
		t.Errorf("after its TTL the screen derives priority=%q, want scheduled — a sixty-second alert that never lapses is a fire-drill notice left on the wall", after.Priority)
	}
	if before.Sections.ScreenPrograms == nil || ovxProgram(t, before, store.SeedScreenID).ProgramRevision != after.ProgramRevision {
		t.Errorf("after the TTL the program_revision is %q, want the pre-alert %q — the same schedule at a comparable instant must derive the same program, and a player only re-fetches when the revision changes",
			after.ProgramRevision, ovxProgram(t, before, store.SeedScreenID).ProgramRevision)
	}

	// And the row is untouched: DAT-004d retires the override by TIME, not by a
	// writer nulling it out. A fix that swept the member away would pass every
	// assertion above and quietly delete an operator's override.
	if genAfter, gerr := st.Generation(ctx); gerr != nil || genAfter != genBefore {
		t.Errorf("generation moved from %d to %d (err %v) with no write — the lapse must be a re-derivation, not a mutation", genBefore, genAfter, gerr)
	}
	res, found, err := st.Get(ctx, store.KindScreen, store.SeedScreenID)
	if err != nil || !found {
		t.Fatalf("re-read the screen: found=%v err=%v", found, err)
	}
	var row struct {
		Override *datamodel.ScreenOverride `json:"override"`
	}
	if err := json.Unmarshal(res.Body, &row); err != nil {
		t.Fatalf("decode screen row: %v", err)
	}
	if row.Override == nil {
		t.Error("the lapsed override was REMOVED from the screen row — DAT-004d says a lapsed override is treated as absent, not deleted; a console must still be able to show an operator what lapsed and when")
	}
}

// TestTheSnapshotCacheStillServesFromCacheWhenNothingCanChange pins the other
// side of the same seam. The expiry-aware invalidation must not degenerate into
// "rebuild on every pull": the cache is what keeps a busy site from re-signing a
// whole snapshot per relay per pull, and losing it would be an invisible
// regression (every assertion about CONTENT would still pass).
func TestTheSnapshotCacheStillServesFromCacheWhenNothingCanChange(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SeedDemo(ctx, signhash.ContentID(placeholderImage())); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}

	base := feederContentInstant(t)()
	now := base
	src := &desiredStateSource{
		store:          st,
		content:        origin.New(),
		contentBaseURL: "https://192.0.2.12:7420",
		id:             id,
		nowMs:          func() int64 { return now },
	}

	if _, err := src.current(); err != nil {
		t.Fatalf("current(): %v", err)
	}

	// POISON the cache rather than comparing outputs. Two builds of the same
	// store at the same generation are byte-identical (Ed25519 signing is
	// deterministic), so "the snapshot did not change" is satisfied just as well
	// by rebuilding it every time — which is precisely the regression this test
	// exists to catch, and which output comparison cannot see. Replacing the
	// cached body with a sentinel makes "was the cache consulted?" directly
	// observable: the sentinel comes back only if it was.
	const sentinel = "cache-was-consulted"
	src.mu.Lock()
	if !src.haveCache || src.cachedUntil != 0 {
		src.mu.Unlock()
		t.Fatalf("after a build with no override anywhere: haveCache=%v cachedUntil=%d, want true/0 — a store with no TTL'd override must leave the window unbounded",
			src.haveCache, src.cachedUntil)
	}
	src.cached.Hash = sentinel
	src.mu.Unlock()

	// No override anywhere, so cachedUntil is zero and only a write may
	// invalidate. Move the clock a long way and the cached snapshot must stand.
	now = base + 86_400_000
	second := mustCurrent(t, src)
	if second.Hash != sentinel {
		t.Fatalf("a day of clock with no override rebuilt the snapshot instead of serving the cache — the generation cache is gone and every relay pull now re-derives and re-signs a whole snapshot")
	}
}

// TestNextOverrideExpiryPicksTheEarliestStillPendingOne covers the deadline
// arithmetic itself — the value both the cache window and the publishing timer
// are built on.
func TestNextOverrideExpiryPicksTheEarliestStillPendingOne(t *testing.T) {
	const now = int64(1_000_000)
	screen := func(expiresAt int64) datamodel.Screen {
		if expiresAt < 0 {
			return datamodel.Screen{} // no override at all
		}
		return datamodel.Screen{Override: &datamodel.ScreenOverride{
			Mode: datamodel.ScreenOverrideModeAlert, Message: "x", ExpiresAt: expiresAt,
		}}
	}

	cases := []struct {
		name    string
		screens []datamodel.Screen
		want    int64
	}{
		{"no screens at all", nil, 0},
		{"no overrides", []datamodel.Screen{screen(-1), screen(-1)}, 0},
		{"an override with no expiry never wakes the timer", []datamodel.Screen{screen(0)}, 0},
		{"one pending expiry", []datamodel.Screen{screen(now + 500)}, now + 500},
		{"the EARLIEST of several", []datamodel.Screen{screen(now + 900), screen(now + 5), screen(now + 400)}, now + 5},
		{"an already-lapsed one is not a deadline", []datamodel.Screen{screen(now - 1)}, 0},
		{
			"an expiry EQUAL to now has already lapsed (Applies is strictly greater)",
			[]datamodel.Screen{screen(now)}, 0,
		},
		{
			"lapsed ones are skipped in favour of the pending one",
			[]datamodel.Screen{screen(now - 10), screen(now + 30), screen(now - 5)}, now + 30,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextOverrideExpiry(tc.screens, now); got != tc.want {
				t.Errorf("nextOverrideExpiry = %d, want %d", got, tc.want)
			}
		})
	}
}

func mustCurrent(t *testing.T, src *desiredStateSource) wire.StateSnapshotBody {
	t.Helper()
	snap, err := src.current()
	if err != nil {
		t.Fatalf("current(): %v", err)
	}
	return snap
}

// --- The publishing loop's own decision -------------------------------------
//
// The first cut of overrideExpiryLoop asked "is any override with an expiry
// set?" to decide whether a lapse still needed announcing. That is true forever
// after the first alert — a lapsed override stays on its row until an operator
// clears it — so the loop advanced the generation on EVERY poll for the rest of
// the process's life, nudging every relay in the site once a minute for a change
// that had already happened. It was caught on a running stack, not here, because
// the loop had no test at all. It has one now.

// TestALapseIsPublishedExactlyOnce walks the whole sequence a real alert goes
// through — imposed, lapsed, announced, sitting there lapsed, cleared, replaced
// — and asserts the generation is advanced only at the one step that is news.
func TestALapseIsPublishedExactlyOnce(t *testing.T) {
	const (
		screenA = "01J8Z0SCREEN0000000000000A"
		screenB = "01J8Z0SCREEN0000000000000B"
		t0      = int64(1_000_000)
	)
	alert := func(id string, expiresAt int64) datamodel.Screen {
		return datamodel.Screen{ID: id, Override: &datamodel.ScreenOverride{
			Mode: datamodel.ScreenOverrideModeAlert, Message: "x", ExpiresAt: expiresAt,
		}}
	}

	// published is carried across the steps exactly as the loop carries it.
	published := map[string]int64{}
	step := func(t *testing.T, screens []datamodel.Screen, now int64, wantAdvance bool, why string) {
		t.Helper()
		lapsed := lapsedOverrides(screens, now)
		got := hasUnpublishedLapse(lapsed, published)
		published = lapsed
		if got != wantAdvance {
			t.Fatalf("advance=%v, want %v — %s", got, wantAdvance, why)
		}
	}

	a := []datamodel.Screen{alert(screenA, t0+100)}
	step(t, a, t0, false, "the alert is still in force; nothing has lapsed")
	step(t, a, t0+99, false, "one millisecond before expiry it is still in force")
	step(t, a, t0+100, true, "AT expires_at the override has stopped applying (Applies is strictly greater) and the fleet has not been told")

	// The row still carries the lapsed override, because DAT-004d retires it by
	// time rather than by deleting it. This is the step the first cut got wrong.
	step(t, a, t0+101, false, "the same lapse a millisecond later must not be announced twice")
	step(t, a, t0+60_000, false, "a minute later — the poll bound — it must STILL not be announced again; this is the nudge storm")
	step(t, a, t0+86_400_000, false, "a day later, still nothing new")

	// A second screen's alert lapses: news, and only that one.
	b := []datamodel.Screen{alert(screenA, t0+100), alert(screenB, t0+200)}
	step(t, b, t0+150, false, "screen B's alert has not lapsed yet, and screen A's is already published")
	step(t, b, t0+200, true, "screen B's alert lapsed")
	step(t, b, t0+201, false, "and is not re-announced")

	// The operator clears screen A's lapsed override. That write advanced the
	// generation itself, so this loop must stay quiet.
	cleared := []datamodel.Screen{{ID: screenA}, alert(screenB, t0+200)}
	step(t, cleared, t0+300, false, "clearing a lapsed override is the operator's own write; announcing it again is a second nudge for one change")

	// A fresh alert on screen A, with a different expiry: a new pair, so news
	// once when it lapses.
	fresh := []datamodel.Screen{alert(screenA, t0+400), alert(screenB, t0+200)}
	step(t, fresh, t0+350, false, "the fresh alert is in force")
	step(t, fresh, t0+400, true, "the fresh alert lapsed — a different expires_at is a different lapse")
	step(t, fresh, t0+500, false, "and is not re-announced either")
}

// TestLapsedOverridesSelectsExactlyTheLapsedOnes pins the classification the
// loop and the nudge both stand on.
func TestLapsedOverridesSelectsExactlyTheLapsedOnes(t *testing.T) {
	const now = int64(5_000)
	screens := []datamodel.Screen{
		{ID: "no-override"},
		{ID: "no-expiry", Override: &datamodel.ScreenOverride{Mode: "play", CastID: "c", ExpiresAt: 0}},
		{ID: "pending", Override: &datamodel.ScreenOverride{Mode: "alert", Message: "x", ExpiresAt: now + 1}},
		{ID: "exactly-now", Override: &datamodel.ScreenOverride{Mode: "alert", Message: "x", ExpiresAt: now}},
		{ID: "lapsed", Override: &datamodel.ScreenOverride{Mode: "alert", Message: "x", ExpiresAt: now - 1}},
	}
	got := lapsedOverrides(screens, now)
	want := map[string]int64{"exactly-now": now, "lapsed": now - 1}
	if len(got) != len(want) {
		t.Fatalf("lapsedOverrides = %v, want %v", got, want)
	}
	for id, exp := range want {
		if got[id] != exp {
			t.Errorf("lapsedOverrides[%q] = %d, want %d", id, got[id], exp)
		}
	}
	// An override with NO expiry never lapses — it stands until it is cleared,
	// and treating it as lapsed would advance the generation forever for the one
	// override kind that is meant to be permanent.
	if _, has := got["no-expiry"]; has {
		t.Error("an override with no expires_at was reported as lapsed")
	}
}
