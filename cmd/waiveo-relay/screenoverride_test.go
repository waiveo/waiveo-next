package main

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screenoverride_test.go covers the relay's side of a screen program OVERRIDE
// (data-model/1 DAT-004c/DAT-004d): the app peer has pinned one specific
// screen's program — an automation's `play_cast` or `show_alert`, or an
// operator's push-now — and the relay must not take it back.
//
// The hazard is narrow and easy to miss, which is why it gets its own file. The
// relay only re-resolves a schedule onto a SCREEN where the attribution is
// forced: exactly one governed scope node and exactly one screen
// (soleServedScreenID). That is the small-site shape — one TV, one schedule —
// and it is the shape where an override would have been silently reverted at
// the very next resolver tick, seconds after the operator's write landed. Every
// larger site was accidentally safe, so the defect would have been invisible in
// any multi-screen test and would have shown up only on the smallest deployment
// this platform has.

// TestPinnedProgramIsNeverServedOverByScheduleResolution asserts the refusal at
// the decision itself: a screen whose app-authored program is pinned is not a
// candidate for a governed node's schedule resolution, even when it is the only
// screen on the site.
func TestPinnedProgramIsNeverServedOverByScheduleResolution(t *testing.T) {
	pinned := func(id string) wire.ScreenProgram {
		p := msScreenProgram(id, "override", "gen3")
		p.Pinned = true
		return p
	}
	plain := func(id string) wire.ScreenProgram { return msScreenProgram(id, "sched", "gen3") }

	cases := []struct {
		name     string
		governed []string
		programs []wire.ScreenProgram
		want     string
	}{
		{
			// The control: without a pin, this is the one topology that DOES
			// attribute. If this case ever stops returning the screen, the test
			// below proves nothing — it would be passing because attribution
			// broke generally, not because the pin was honored.
			name:     "one governed node, one unpinned screen — still attributed",
			governed: []string{msScopeNodeA}, programs: []wire.ScreenProgram{plain(msScreenRowA)},
			want: msScreenRowA,
		},
		{
			name:     "one governed node, one PINNED screen — the override stands",
			governed: []string{msScopeNodeA}, programs: []wire.ScreenProgram{pinned(msScreenRowA)},
			want: "",
		},
		{
			// A pin on a screen the node was never going to be attributed to
			// changes nothing: the answer was already "" for its own reason.
			name:     "two screens, one of them pinned — still unattributable",
			governed: []string{msScopeNodeA}, programs: []wire.ScreenProgram{pinned(msScreenRowA), plain(msScreenRowB)},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := soleServedScreenID(tc.governed, tc.programs); got != tc.want {
				t.Errorf("soleServedScreenID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPinnedProgramSurvivesTheBootResolverOnTheWire is the same refusal observed
// where it matters: what a paired screen is actually served after the relay has
// booted, installed the app-authored programs, and run its schedule resolver.
//
// The site is deliberately the forced-attribution one — a single governed scope
// node and a single screen — because that is the only site where the resolver
// would otherwise write over the pin. Without the Pinned flag the screen here
// serves the schedule's own program revision and the operator's alert never
// appears; with it, the alert stands and the resolver still runs (it keeps
// firing the node's preset batches, which have nothing to do with any screen).
func TestPinnedProgramSurvivesTheBootResolverOnTheWire(t *testing.T) {
	site := hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298}
	nowMs := demoContentHourInstant(t)

	srv := newPlayerServerWithGrants(t, twoScreenGrant(msScreenRowA))
	applied := buildMultiScreenApplied(t, 3, "gen3")

	// One governed scope node, one screen: the topology that forces attribution.
	schedA, dayA, listA := msScheduleRows(msScheduleAID, msDaypartAID, msPlaylistAID, msScopeNodeA)
	applied.Schedule = wire.ScheduleSection{
		ScopeNodes: marshalRows(t, msSiteNode(), msScreenNode(msScopeNodeA, "Screen A")),
		Schedules:  marshalRows(t, schedA),
		Dayparts:   marshalRows(t, dayA),
		Playlists:  marshalRows(t, listA),
	}.Normalized()

	// The app peer's own pinned program for that screen — what an automation's
	// show_alert projects to.
	pinnedProgram := msScreenProgram(msScreenRowA, "override", "gen3")
	pinnedProgram.Pinned = true
	pinnedProgram.Priority = "preempt"
	applied.ScreenPrograms = []wire.ScreenProgram{pinnedProgram}

	serveAppAuthoredPrograms(srv, applied.Generation, applied.ScreenPrograms)
	resolvers := bootScheduleResolverAt(applied, srv, fakeScheduleSink(), site, nowMs)
	if len(resolvers) != 1 {
		t.Fatalf("fixture: built %d resolver(s), want exactly 1 governed scope node — the topology under test", len(resolvers))
	}

	ts := newPlayerHTTP(t, srv)
	lease := pullProgram(t, ts, pairForToken(t, ts, "grant-offline-"+msScreenRowA))
	if lease.ProgramRevision != pinnedProgram.ProgramRevision {
		t.Fatalf("screen serves program_revision %q, want the pinned override's %q — the schedule resolver took back a program the app peer pinned (DAT-004d)",
			lease.ProgramRevision, pinnedProgram.ProgramRevision)
	}
	if lease.Priority != "preempt" {
		t.Errorf("lease priority = %q, want preempt — the alert's takeover class did not reach the wire", lease.Priority)
	}
}
