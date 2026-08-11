package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// pushnow_test.go is the relay half of PUSH-NOW (parity row 5.7) driven end to
// end on the wire a screen actually reads: an operator's override arrives as a
// `preempt` screen_programs entry, and what a paired player is SERVED changes to
// it — and, critically, STAYS changed while the relay keeps re-resolving the
// screen's schedule underneath it.
//
// That second half is the whole reason the priority fence exists. This site is
// the one topology where a schedule resolution IS attributed to a screen (one
// governed scope node, one screen — soleServedScreenID), so a resolver writes
// this screen's program every 30 seconds for as long as the generation stands.
// Without the fence the very first tick after every push silently put the
// schedule back, and the console would have reported a successful push against a
// screen that reverted within half a minute — a surface that accepts work it
// never performs, arriving through the back door.

// Single-screen fixture identities. One site, ONE governed screen scope node and
// ONE screen identity row, which is the topology soleServedScreenID attributes
// on — deliberately, because it is the topology where the schedule resolver is
// the competing writer this feature has to survive.
const (
	pnOrgID       = "01J8ZPUSHNOWORGB0UND00001"
	pnSiteID      = "01J8ZPUSHNOWSITE00000001"
	pnScopeNode   = "01J8ZPUSHNOWSC0PEN0DE0001"
	pnScreenRow   = "01J8ZPUSHNOWSCREENR0W0001"
	pnScheduleID  = "01J8ZPUSHNOWSCHEDULE00001"
	pnDaypartID   = "01J8ZPUSHNOWDAYPART000001"
	pnPlaylistID  = "01J8ZPUSHNOWPLAYLIST00001"
	pnScheduleURL = "https://origin.example/content/scheduled"
	pnPushURL     = "https://origin.example/content/pushed-now"
)

// pnSiteNode is the fixture site carrying the tz/geo the subtree resolves
// against (DAT-034).
func pnSiteNode() datamodel.ScopeNode {
	tz := "America/Chicago"
	lat := 41.8781
	long := -87.6298
	parent := pnOrgID
	return datamodel.ScopeNode{ID: pnSiteID, Kind: "site", ParentID: &parent, Name: "Push Now Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1}
}

// pnApplied builds a one-screen site whose all-day schedule governs the screen's
// scope node, with the screen's own `screen_programs` entry supplied by the
// caller — so a test can hand it either the ordinary scheduled baseline or the
// preempt entry an operator's push produces, at whatever generation.
func pnApplied(t *testing.T, gen int64, prog wire.ScreenProgram) desiredstate.Applied {
	t.Helper()
	schedule, daypart, playlist := msScheduleRows(pnScheduleID, pnDaypartID, pnPlaylistID, pnScopeNode)
	node := msScreenNode(pnScopeNode, "Push Now Screen")
	// The scope node hangs off THIS fixture's site, not the multiscreen one.
	parent := pnSiteID
	node.ParentID = &parent

	sec := wire.ScheduleSection{
		ScopeNodes: marshalRows(t, pnSiteNode(), node),
		Schedules:  marshalRows(t, schedule),
		Dayparts:   marshalRows(t, daypart),
		Playlists:  marshalRows(t, playlist),
	}.Normalized()

	return desiredstate.Applied{
		Generation:     gen,
		Schedule:       sec,
		ContentOrigin:  "https://origin.example",
		ScreenPrograms: []wire.ScreenProgram{prog},
		PairingGrants:  []wire.PairingGrant{twoScreenGrant(pnScreenRow)},
	}
}

// pnScheduledProgram is the ordinary app-authored baseline for this screen: what
// the feeder derives when NO override is set.
func pnScheduledProgram(revision string) wire.ScreenProgram {
	return wire.ScreenProgram{
		ScreenID:        pnScreenRow,
		ProgramRevision: revision,
		Priority:        playerserver.PriorityScheduled,
		Display:         "content",
		Content:         []wire.ContentRef{{AssetRef: msScheduleeAst, URL: pnScheduleURL}},
	}
}

// pnPushedProgram is what snapshot.overrideProgram derives for this screen once
// an operator pushes a cast at it: same screen, `preempt` priority, the pushed
// content, and a revision of its own so a player sees a real program change.
func pnPushedProgram(revision string) wire.ScreenProgram {
	return wire.ScreenProgram{
		ScreenID:        pnScreenRow,
		ProgramRevision: revision,
		Priority:        playerserver.PriorityPreempt,
		Display:         "content",
		Content:         []wire.ContentRef{{AssetRef: "sha256:pushed", URL: pnPushURL}},
	}
}

// pnBoot installs and applies gen over a fresh player server, returning the
// server, its HTTP test surface, a paired channel token for the fixture screen,
// and the resolvers the apply built — exactly the sequence main's boot path runs.
func pnBoot(t *testing.T, ctx context.Context, applied desiredstate.Applied) (*playerserver.Server, *httptest.Server, string, *scheduleDriver, []*schedulehost.Resolver) {
	t.Helper()
	srv := newPlayerServerWithGrants(t, twoScreenGrant(pnScreenRow))
	driver := &scheduleDriver{
		srv:       srv,
		sink:      fakeScheduleSink(),
		site:      hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298},
		tickEvery: scheduleResolverTickInterval,
	}
	nowMs := demoContentHourInstant(t)
	serveAppAuthoredPrograms(srv, applied.Generation, applied.ScreenPrograms)
	_, resolvers := driver.apply(ctx, applied, nowMs)

	ts := newPlayerHTTP(t, srv)
	token := pairForToken(t, ts, "grant-offline-"+pnScreenRow)
	return srv, ts, token, driver, resolvers
}

// TestPushNowChangesTheServedLeaseAndSurvivesTheScheduleResolver is parity row
// 5.7's central assertion, in the only place that settles it: what the screen is
// handed.
//
// It drives the real path — a generation carrying a `preempt` entry, applied
// through the same scheduleDriver.apply the running relay uses, observed through
// player/1's own pair → program surface with the SAME channel token the screen
// keeps across the apply — and then runs the schedule resolver's own tick over
// the top, which is the thing that used to undo it.
func TestPushNowChangesTheServedLeaseAndSurvivesTheScheduleResolver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Boot on the ordinary schedule. This baseline matters: if the fixture were
	// already serving the pushed content, every assertion below would pass with
	// the feature deleted.
	//
	// What is actually served here is the RESOLVER's own resolution, not the
	// app-authored baseline — this site is attributable, so the resolver replaces
	// the baseline at the same generation, which is correct and is precisely the
	// competing writer the rest of this test is about. Asserted by the schedule's
	// own asset ref rather than by the baseline's URL for that reason.
	_, ts, token, driver, _ := pnBoot(t, ctx, pnApplied(t, 5, pnScheduledProgram("rev-scheduled")))
	boot := pullProgram(t, ts, token)
	if boot.Priority != playerserver.PriorityScheduled {
		t.Fatalf("fixture: screen boots at priority %q, want %q", boot.Priority, playerserver.PriorityScheduled)
	}
	if len(boot.Content) != 1 || boot.Content[0].AssetRef != msScheduleeAst {
		t.Fatalf("fixture: screen boots serving %+v, want the schedule's own asset", boot.Content)
	}

	// The operator's push: a strictly higher generation whose entry for this
	// screen is `preempt`.
	nowMs := demoContentHourInstant(t)
	pushed := pnApplied(t, 6, pnPushedProgram("rev-pushed"))
	_, resolvers := driver.apply(ctx, pushed, nowMs)

	lease := pullProgram(t, ts, token)
	if lease.Priority != playerserver.PriorityPreempt {
		t.Errorf("served Lease priority = %q, want %q — a push-now must reach the player as a takeover, so it interrupts rather than waiting out the current item (PLY-100/101)",
			lease.Priority, playerserver.PriorityPreempt)
	}
	if lease.ProgramRevision != "rev-pushed" {
		t.Errorf("served program_revision = %q, want rev-pushed — the push never reached the screen", lease.ProgramRevision)
	}
	if len(lease.Content) != 1 || lease.Content[0].URL != pnPushURL {
		t.Fatalf("served content = %+v, want the pushed asset at %s", lease.Content, pnPushURL)
	}

	// THE REGRESSION THIS FEATURE IS ABOUT. The schedule resolver keeps
	// re-resolving this screen at the same generation, every 30s, for as long as
	// the generation stands. One tick used to be enough to put the schedule back.
	if len(resolvers) != 1 {
		t.Fatalf("fixture: apply built %d resolver(s), want exactly 1 — this test is meaningless without a resolver competing for the screen", len(resolvers))
	}
	for i := 0; i < 3; i++ {
		resolvers[0].Tick(nowMs, fakeScheduleSink())
	}

	after := pullProgram(t, ts, token)
	if after.Priority != playerserver.PriorityPreempt || len(after.Content) != 1 || after.Content[0].URL != pnPushURL {
		t.Fatalf("after 3 schedule-resolver ticks the screen serves priority=%q content=%+v, want the pushed asset still — the schedule silently reverted an operator's push within half a minute",
			after.Priority, after.Content)
	}
}

// TestClearingThePushReturnsTheScreenToItsSchedule is the other half of the
// capability, and the half a fence makes easy to get wrong: an override that
// could not be undone would be worse than no override at all.
//
// Clearing produces a NEW generation whose entry for the screen is `scheduled`
// again. A strictly higher generation always wins over a preempt program, which
// is exactly what makes the fence safe to have — it holds WITHIN a generation
// and never across one.
func TestClearingThePushReturnsTheScreenToItsSchedule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, ts, token, driver, _ := pnBoot(t, ctx, pnApplied(t, 5, pnPushedProgram("rev-pushed")))
	if got := pullProgram(t, ts, token).Content; len(got) != 1 || got[0].URL != pnPushURL {
		t.Fatalf("fixture: screen boots serving %+v, want the pushed asset", got)
	}

	nowMs := demoContentHourInstant(t)
	cleared := pnApplied(t, 6, pnScheduledProgram("rev-scheduled-again"))
	driver.apply(ctx, cleared, nowMs)

	lease := pullProgram(t, ts, token)
	if lease.Priority != playerserver.PriorityScheduled {
		t.Errorf("after clearing, served Lease priority = %q, want %q", lease.Priority, playerserver.PriorityScheduled)
	}
	// The schedule's OWN asset, which is the real claim: the screen is back on
	// what the schedule resolves, not merely off the push. (The resolver's
	// resolution replaces the app-authored baseline at this generation, as it
	// does at boot — see the sibling test's own note.)
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != msScheduleeAst {
		t.Fatalf("after clearing, served content = %+v, want the schedule's own asset — the override could not be undone", lease.Content)
	}
}

// TestASecondPushReplacesTheFirstWithinOneGeneration guards the one way a
// priority fence can be too strong: if `preempt` were treated as un-overwritable
// rather than as out-ranking `scheduled`, the SECOND push to a screen would be
// swallowed and the operator would be stuck with the first one until they cleared
// it — a worse failure than the one the fence fixes, because it is silent on the
// success path.
func TestASecondPushReplacesTheFirstWithinOneGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, ts, token, _, _ := pnBoot(t, ctx, pnApplied(t, 5, pnPushedProgram("rev-push-one")))

	// Both writes at the SAME generation, which is the case the fence acts on.
	second := pnPushedProgram("rev-push-two")
	second.Content = []wire.ContentRef{{AssetRef: "sha256:pushed2", URL: "https://origin.example/content/pushed-twice"}}
	srv.SetServedProgram(5, second)

	lease := pullProgram(t, ts, token)
	if lease.ProgramRevision != "rev-push-two" {
		t.Fatalf("after a second push at the same generation the screen serves %q, want rev-push-two — a preempt program must be replaceable by another preempt program", lease.ProgramRevision)
	}
}
