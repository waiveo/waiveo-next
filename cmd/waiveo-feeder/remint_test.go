package main

import (
	"context"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// remint_test.go pins BOTH halves of the re-mint publication, because either one
// alone is a defect that passes every other gate.
//
// The first half is that it happens at all. desiredStateSource rebuilding its
// cached snapshot is invisible outside the feeder: relayconn answers
// `state.unchanged` with no body when a pull's `since_generation` matches, and
// the relay no-ops on a generation it already holds, so a re-mint that does not
// advance the generation is a re-mint the fleet never receives. That is HV-1
// again, on a timer — every image and video 403ing at T+SnapshotTTL on a site
// nobody authored, recoverable only by restarting the relay.
//
// The second half is that it costs nothing on screen. A generation advance
// re-applies every screen's program on every relay, and a player swaps — and
// restarts the rotation — when program_revision changes (PLY-090/108). The
// revision is stable across a re-mint only because snapshot.revisionContent
// strips `exp`/`sig` from the digest; if that reduction were removed, this loop
// would restart every screen in the site twice a day. The two halves are
// asserted together, in one test, because the fix is only correct as a pair.

// TestARemintReachesTheFleetWithoutRestartingAScreen drives the real loop
// against a real store and a real signing origin.
func TestARemintReachesTheFleetWithoutRestartingAScreen(t *testing.T) {
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
	// A SIGNING origin: against a keyless one the urls never expire, nothing
	// would rot, and this test would prove nothing.
	oc := origin.New(origin.WithSigningKey([]byte("a-32-byte-test-key-for-hmac-0001")))
	if _, err := oc.Add(img); err != nil {
		t.Fatalf("origin.Add: %v", err)
	}

	base := feederContentInstant(t)()
	now := base
	src := &desiredStateSource{
		store:          st,
		content:        oc,
		contentBaseURL: "https://192.0.2.12:7420",
		id:             id,
		nowMs:          func() int64 { return now },
	}

	before := mustCurrent(t, src)
	if len(before.Sections.ScreenPrograms) == 0 {
		t.Fatalf("the seeded store derives no screen programs — this test would prove nothing")
	}
	firstItem := ovxProgram(t, before, store.SeedScreenID).Content
	if len(firstItem) == 0 || firstItem[0].ExpiresAt == 0 {
		t.Fatalf("the seeded program carries no expiring content url (%+v) — this test would prove nothing", firstItem)
	}
	genBefore, err := st.Generation(ctx)
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}

	// The loop, at a cadence a test can wait for. NOTHING else writes: no api
	// call, no override, no seed. Whatever moves the generation is the re-mint.
	loopCtx, stop := context.WithCancel(ctx)
	defer stop()
	go remintLoop(loopCtx, st, 5*time.Millisecond)

	genAfter := waitForGenerationPast(t, st, genBefore)
	// Stop the loop before deriving, so the generation the assertions are made
	// about is the one that was read.
	stop()

	// Half one: the fleet is told. A relay pulling with since=genBefore is
	// answered with a snapshot BODY rather than `state.unchanged`, which is the
	// only way a re-minted url ever reaches a screen.
	if genAfter <= genBefore {
		t.Fatalf("the generation is still %d — a re-mint that does not advance it is answered `state.unchanged` with no "+
			"body (relayconn), the relay's pull no-ops on it (rePuller.tick), and the freshly minted urls never leave "+
			"this process. Every image and video 403s at T+SnapshotTTL on any site nobody authors.", genAfter)
	}

	// A full day, not exactly the interval: it lands back inside the seeded
	// content daypart (06:00–22:00 local), so the schedule resolves to the same
	// program and the only difference between the two builds is the mint.
	now = base + 24*time.Hour.Milliseconds()
	after := mustCurrent(t, src)

	if after.Generation <= before.Generation {
		t.Errorf("the derived snapshot's generation is still %d (was %d) — the advance did not reach the desired state a "+
			"pull is answered from", after.Generation, before.Generation)
	}

	// The premise, asserted: the urls really were re-minted. Without this the
	// invariant below would hold over two identical builds and mean nothing.
	secondItem := ovxProgram(t, after, store.SeedScreenID).Content
	if len(secondItem) == 0 {
		t.Fatalf("the re-minted program carries no content")
	}
	if secondItem[0].URL == firstItem[0].URL || secondItem[0].ExpiresAt <= firstItem[0].ExpiresAt {
		t.Fatalf("PREMISE FALSE: the content url did not change across the re-mint (%q exp=%d vs %q exp=%d), so the "+
			"revision invariant below is about two identical builds",
			firstItem[0].URL, firstItem[0].ExpiresAt, secondItem[0].URL, secondItem[0].ExpiresAt)
	}

	// Half two: no screen pays for it. EVERY screen, not just the seeded one —
	// the property is about the derivation, not about a position.
	wasRevision := map[string]string{}
	for _, sp := range before.Sections.ScreenPrograms {
		wasRevision[sp.ScreenID] = sp.ProgramRevision
	}
	if len(after.Sections.ScreenPrograms) != len(before.Sections.ScreenPrograms) {
		t.Fatalf("the re-mint changed how many screens are delivered (%d, was %d)",
			len(after.Sections.ScreenPrograms), len(before.Sections.ScreenPrograms))
	}
	for _, sp := range after.Sections.ScreenPrograms {
		was, known := wasRevision[sp.ScreenID]
		if !known {
			t.Errorf("the re-mint introduced a screen_programs entry for %s that was not there before", sp.ScreenID)
			continue
		}
		if sp.ProgramRevision != was {
			t.Errorf("screen %s: program_revision moved from %q to %q across a re-mint.\n"+
				"A player treats a changed revision as a new program and restarts the rotation (PLY-090/108), so this "+
				"loop would visibly restart every screen in the site twice a day. The revision must be digested through "+
				"snapshot.revisionContent, which drops the `exp`/`sig` a re-mint changes by construction.",
				sp.ScreenID, was, sp.ProgramRevision)
		}
	}
}

// waitForGenerationPast polls until the store's generation exceeds from, and
// returns it. The budget is generous relative to the loop's cadence, so the test
// fails on the LOOP not advancing rather than on a slow machine.
func waitForGenerationPast(t *testing.T, st *store.Store, from int64) int64 {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		gen, err := st.Generation(ctx)
		if err != nil {
			t.Fatalf("read generation: %v", err)
		}
		if gen > from {
			return gen
		}
		if time.Now().After(deadline) {
			return gen
		}
		time.Sleep(2 * time.Millisecond)
	}
}
