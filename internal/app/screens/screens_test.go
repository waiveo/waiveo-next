package screens

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screens_test.go covers the read model's two jobs: keeping each relay's report
// separate, and doing the age arithmetic HONESTLY — which is the whole feature.
// Every test drives the clock explicitly, because a model that stamped ages at
// write time rather than computing them at read time would pass any test that
// read immediately.

// clockAt returns a settable clock and a setter, so a test can advance time.
func clockAt(start int64) (func() int64, func(int64)) {
	now := start
	return func() int64 { return now }, func(v int64) { now = v }
}

func mustRegistry(t *testing.T, nowMs func() int64) *Registry {
	t.Helper()
	r, err := NewRegistry(nowMs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func statusFor(t *testing.T, r *Registry, screenID string) Status {
	t.Helper()
	for _, st := range r.Statuses() {
		if st.ScreenID == screenID {
			return st
		}
	}
	t.Fatalf("no status for %q; got %+v", screenID, r.Statuses())
	return Status{}
}

// TestAgesGrowWithTheReportEvenWithNoNewReport is the property the whole design
// exists for: a relay that goes silent must make its screens read staler, not
// freeze them at whatever the last report said.
//
// It is also the disconnected-relay case. Nothing in this model knows whether a
// relay is connected — it only knows nothing new has arrived — and that is
// exactly the right thing to report, because the app has genuinely stopped
// learning anything about those screens.
func TestAgesGrowWithTheReportEvenWithNoNewReport(t *testing.T) {
	now, set := clockAt(1_000_000)
	r := mustRegistry(t, now)

	// The ack is FRESHER than the pull, which is what a healthy screen presents:
	// the player acknowledges each Lease it materialises immediately after
	// pulling it. An ack older than the pull would mean the most recent pull is
	// still outstanding — a screen mid-transfer, which reachabilityOf reports as
	// `fetching`, not `stale`, and which is a different case from this one.
	if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{{
		ScreenID: "screen-a", Paired: true, LastPullAgeMs: 3_000, LastAckAgeMs: 2_500, LastRenderStartAgeMs: 4_000,
		ProgramRevision: "rev-1", Priority: "scheduled", Display: "content", ContentCount: 2,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}

	// Read at the instant of the report: the relay's own ages, unmodified.
	got := statusFor(t, r, "screen-a")
	if got.LastPullAgeMs != 3_000 || got.ReportAgeMs != 0 {
		t.Fatalf("at report time last_pull=%d report_age=%d, want 3000/0", got.LastPullAgeMs, got.ReportAgeMs)
	}
	if got.Reachability != ReachabilityLive {
		t.Fatalf("reachability at 3s stale = %q, want live", got.Reachability)
	}

	// A long silence, with no new report. Every age has grown by exactly that
	// silence and the screen has crossed the live window.
	//
	// The silence is expressed RELATIVE to LiveWindowMs rather than as a literal
	// two minutes, which is what this test used to say. That literal was fine
	// against a 45s window and became wrong the moment the window was derived
	// from the player's real pull cadence — a test asserting "stale" after a
	// silence shorter than the window is asserting a bug. Written this way it
	// stays a test of the ARITHMETIC (ages grow; the judgement follows them)
	// under any window the derivation produces.
	const silence = LiveWindowMs + 60_000
	set(1_000_000 + silence)
	got = statusFor(t, r, "screen-a")
	if got.LastPullAgeMs != 3_000+silence {
		t.Errorf("last_pull age after %dms of silence = %d, want %d — a status that does not age reports a dead fleet as healthy forever", silence, got.LastPullAgeMs, 3_000+silence)
	}
	if got.LastAckAgeMs != 2_500+silence || got.LastRenderStartAgeMs != 4_000+silence {
		t.Errorf("ack/render ages = %d/%d, want %d/%d", got.LastAckAgeMs, got.LastRenderStartAgeMs, 2_500+silence, 4_000+silence)
	}
	if got.ReportAgeMs != silence {
		t.Errorf("report_age = %d, want %d — the field that distinguishes a dead screen from a dead relay", got.ReportAgeMs, silence)
	}
	if got.Reachability != ReachabilityStale {
		t.Errorf("reachability after %dms of silence = %q, want stale", silence, got.Reachability)
	}
}

// TestNeverObservedIsPreservedThroughTheArithmetic: "never" must not become a
// number. Adding the report's own age to the sentinel would turn a screen that
// has never checked in into one that checked in two minutes ago, which is the
// single most misleading thing this surface could say.
func TestNeverObservedIsPreservedThroughTheArithmetic(t *testing.T) {
	now, set := clockAt(1_000_000)
	r := mustRegistry(t, now)

	if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{{
		ScreenID: "screen-new", Paired: true,
		LastPullAgeMs: NeverObserved, LastAckAgeMs: NeverObserved, LastRenderStartAgeMs: NeverObserved,
		ProgramRevision: "rev-waiting", ContentCount: 1,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}

	set(1_000_000 + 120_000)
	got := statusFor(t, r, "screen-new")
	if got.LastPullAgeMs != NeverObserved || got.LastAckAgeMs != NeverObserved || got.LastRenderStartAgeMs != NeverObserved {
		t.Fatalf("never-observed ages became (%d, %d, %d) after two minutes, want all %d",
			got.LastPullAgeMs, got.LastAckAgeMs, got.LastRenderStartAgeMs, NeverObserved)
	}
	if got.Reachability != ReachabilityNeverSeen {
		t.Errorf("reachability = %q for a screen that has never pulled, want never_seen (NOT stale — it never broke, it never started)", got.Reachability)
	}
}

// TestTheLiveWindowBoundary pins ALL THREE thresholds this model draws, at their
// edges, over the three observations the judgement is made from.
//
// The table is (pull age, ack age, unacked pulls) rather than pull age alone
// because the judgement is: live if either contact is recent; fetching if the
// most recent pull is still unacknowledged, the shipped player could still be
// transferring, AND the screen is not simply re-asking forever; stale otherwise.
// Each row below is the reason one of those clauses exists, named in its own
// comment — a table that only drove pull age would leave two clauses untested at
// their edges, which is precisely the half-of-a-pair this round is about.
//
// The `unacked` column is the one added this round, and the two rows at its
// boundary are the ones that would have caught the 2026-08 case: a screen with a
// pull inside the transfer window and an ack that never comes reads `fetching`
// while it has an outstanding Lease it might still be working on, and `stale`
// once it has abandoned more of them than a single transient failure explains.
func TestTheLiveWindowBoundary(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	maxUnacked := int(MaxFetchingUnackedPulls)

	cases := []struct {
		why     string
		pull    int64
		ack     int64
		unacked int
		want    Reachability
	}{
		{"just pulled", 0, NeverObserved, 1, ReachabilityLive},
		{"one ms inside the live window", LiveWindowMs - 1, NeverObserved, 1, ReachabilityLive},
		{"exactly at the live window", LiveWindowMs, NeverObserved, 1, ReachabilityLive},
		{"a healthy screen whose ack answered its pull", 3_000, 2_500, 0, ReachabilityLive},

		// The live window is judged on contact alone, so even a screen that has
		// abandoned pull after pull reads live for as long as one of them is
		// recent. That is correct and deliberate: it IS reachable. The column
		// this table pins is reachability, not content health.
		{"failing every pull, but one just landed", 1_000, NeverObserved, 50, ReachabilityLive},

		// The ack is a real round trip from the screen, so a pull that has aged
		// past the window but was acknowledged INSIDE it is a screen we heard
		// from inside the window. Without this the console flashes stale for one
		// poll interval after every long content transfer completes.
		{"pull past the window, ack inside it", LiveWindowMs + 1, LiveWindowMs, 0, ReachabilityLive},

		// Past the live window with the most recent pull still outstanding: the
		// gap the shipped player only occupies to materialise content.
		{"one ms past the window, pull unacknowledged", LiveWindowMs + 1, NeverObserved, 1, ReachabilityFetching},
		{"exactly at the transfer window", ContentTransferWindowMs, NeverObserved, 1, ReachabilityFetching},
		{"an earlier ack, this pull outstanding", ContentTransferWindowMs, ContentTransferWindowMs + 5_000, 1, ReachabilityFetching},

		// And it expires, or a screen that died between its pull and its ack
		// would hide in `fetching` forever.
		{"one ms past the transfer window", ContentTransferWindowMs + 1, NeverObserved, 1, ReachabilityStale},

		// The progress bound, at its edge. A transfer has ONE pull outstanding;
		// the allowance on top absorbs a single failed iteration, the same
		// tolerance the live window makes. Past it the screen is not fetching, it
		// is failing — and an age bound can never say so, because this screen's
		// age resets on every retry.
		{"at the outstanding-pull allowance", LiveWindowMs + 1, NeverObserved, maxUnacked, ReachabilityFetching},
		{"one pull past the allowance", LiveWindowMs + 1, NeverObserved, maxUnacked + 1, ReachabilityStale},
		{"the 2026-08 wall: pulling forever, acknowledging never", LiveWindowMs + 1, NeverObserved, 40, ReachabilityStale},

		// Acknowledged and then quiet: nothing is in flight, so no grace.
		{"pull past the window and acknowledged", LiveWindowMs + 1, LiveWindowMs + 1, 0, ReachabilityStale},
		{"long gone", 600_000, 599_000, 0, ReachabilityStale},

		{"never pulled", NeverObserved, NeverObserved, 0, ReachabilityNeverSeen},
	}
	for _, tc := range cases {
		if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{
			{ScreenID: "screen-a", LastPullAgeMs: tc.pull, LastAckAgeMs: tc.ack,
				LastRenderStartAgeMs: NeverObserved, UnackedPulls: tc.unacked},
		}); err != nil {
			t.Fatalf("ApplyScreenStatus: %v", err)
		}
		if got := statusFor(t, r, "screen-a").Reachability; got != tc.want {
			t.Errorf("%s (pull %d, ack %d, %d unacked) → reachability %q, want %q", tc.why, tc.pull, tc.ack, tc.unacked, got, tc.want)
		}
	}
}

// TestAScreenFailingEveryPullIsNeverCalledFetching is the 2026-08 case, driven
// the way it actually happened, and it is the regression test for the finding
// that `fetching` had permanently captured exactly the screen the live window
// was retuned for.
//
// The wall: a content-URL 403. Every program pull answers 200, so the relay
// re-stamps `lastPullMs` each time. `wvEnsureContent` then fails and
// `wvDoProgram` returns at Program.brs:337 — BEFORE `wvAckLease` at :365 — so no
// ack is ever sent. PlayerTask counts a failure and retries on a backoff that
// doubles to a 60 000 ms cap, forever.
//
// Every sample that screen produces has an unacknowledged pull, and its age
// never exceeds 60 000 + 8 000 + 10 000 = 78 000 ms, well inside the 172 000 ms
// transfer window. So the age bound alone could NEVER expire it: the console
// reported "Collecting content" for the rest of its life, and the fleet roll-up
// (which grades `down` on live == 0 && fetching == 0) could never call a whole
// site of them dark.
//
// Swept across the full age range rather than sampled at one point, and driven
// for both an ack that never happened and one that happened long ago, because
// those are the two shapes the reviewer found neither of which ever reached a
// non-fetching state.
func TestAScreenFailingEveryPullIsNeverCalledFetching(t *testing.T) {
	// The player's retry-backoff cap plus the program request timeout plus one
	// report interval: the largest age this screen can ever present.
	const worstAgeMs int64 = 60_000 + 8_000 + 10_000
	if worstAgeMs > ContentTransferWindowMs {
		t.Fatalf("fixture no longer models the finding: the worst age a screen in retry backoff presents (%d) is outside the transfer window (%d), so the age bound would expire it on its own",
			worstAgeMs, ContentTransferWindowMs)
	}

	for _, ackShape := range []struct {
		why string
		ack func(pull int64) int64
	}{
		{"never acknowledged anything", func(int64) int64 { return NeverObserved }},
		{"acknowledged once, long ago", func(pull int64) int64 { return pull + 600_000 }},
	} {
		t.Run(ackShape.why, func(t *testing.T) {
			now, _ := clockAt(1_000_000)
			r := mustRegistry(t, now)
			// The counter after a few minutes of 2s/4s/8s/…/60s retries. The
			// third consecutive unacknowledged pull is enough; this is what the
			// relay would actually be reporting by the time anyone looked.
			const unacked = 37

			for pull := int64(0); pull <= worstAgeMs; pull += 1_000 {
				if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{{
					ScreenID: "screen-403", Paired: true,
					LastPullAgeMs: pull, LastAckAgeMs: ackShape.ack(pull),
					LastRenderStartAgeMs: NeverObserved, UnackedPulls: unacked,
					ProgramRevision: "rev-new", ContentCount: 1,
				}}); err != nil {
					t.Fatalf("ApplyScreenStatus at pull age %d: %v", pull, err)
				}
				got := statusFor(t, r, "screen-403").Reachability
				if got == ReachabilityFetching {
					t.Fatalf("a screen that has failed %d consecutive pulls reads `fetching` at pull age %d.\n"+
						"Nothing is being fetched: the player abandons the Lease before the ack and retries forever, so this state "+
						"never expires and the console tells an operator a dead wall is downloading. It must read live (a pull did "+
						"just land) or stale (it has not), never fetching.", unacked, pull)
				}
				// And the two it IS allowed to be, so the assertion above cannot
				// be satisfied by a fourth state appearing.
				if got != ReachabilityLive && got != ReachabilityStale {
					t.Fatalf("at pull age %d the screen reads %q, want live or stale", pull, got)
				}
			}

			// The one that matters to the roll-up: at the top of its sawtooth it
			// must reach STALE, or `live == 0 && fetching == 0` never holds and a
			// dark site is never graded `down`.
			if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{{
				ScreenID: "screen-403", Paired: true,
				LastPullAgeMs: worstAgeMs, LastAckAgeMs: ackShape.ack(worstAgeMs),
				LastRenderStartAgeMs: NeverObserved, UnackedPulls: unacked,
			}}); err != nil {
				t.Fatalf("ApplyScreenStatus: %v", err)
			}
			if got := statusFor(t, r, "screen-403").Reachability; got != ReachabilityStale {
				t.Fatalf("at the worst age a broken screen presents (%d ms) it reads %q, want stale — this is the sample the fleet-dark alarm depends on reaching", worstAgeMs, got)
			}
		})
	}
}

// TestAScreenDownloadingNewContentIsNotCalledStale is the operator-visible
// property the second threshold exists for, driven the way it actually happens.
//
// A screen is handed a Lease naming a 60 MB video it does not hold. The player
// fetches it — serialised into the poll loop, before the ack, before the sleep —
// which on building wifi is the better part of a minute plus an on-device
// SHA-256. Throughout, never-wipe keeps the OUTGOING program on the wall: the
// screen is working, and an operator sent to investigate it has been sent
// nowhere. It must not read `stale`.
//
// The relay reports through the whole transfer; only the ages move.
func TestAScreenDownloadingNewContentIsNotCalledStale(t *testing.T) {
	now, set := clockAt(1_000_000)
	r := mustRegistry(t, now)

	// The pull that handed over the new Lease, acknowledged LAST cycle — so the
	// most recent pull is outstanding, which is what a transfer looks like from
	// the relay's side.
	const pullAtMs int64 = 1_000_000
	report := func(atMs int64) Status {
		t.Helper()
		if err := r.ApplyScreenStatus("relay-1", atMs, []wire.ScreenStatusEntry{{
			ScreenID: "screen-hanger", Paired: true,
			LastPullAgeMs:        atMs - pullAtMs,
			LastAckAgeMs:         atMs - pullAtMs + 10_000, // the PREVIOUS cycle's ack
			LastRenderStartAgeMs: NeverObserved,
			// ONE outstanding pull, which is what a transfer looks like: the
			// fetch is serialised inside the player's poll loop, so a screen
			// materialising content has made no further pull to be waiting on.
			UnackedPulls:    1,
			ProgramRevision: "rev-new", ContentCount: 1,
		}}); err != nil {
			t.Fatalf("ApplyScreenStatus at +%dms: %v", atMs-pullAtMs, err)
		}
		set(atMs)
		return statusFor(t, r, "screen-hanger")
	}

	// A 45-second transfer, watched every report interval.
	for elapsed := int64(0); elapsed <= 45_000; elapsed += 10_000 {
		got := report(pullAtMs + elapsed)
		if got.Reachability == ReachabilityStale {
			t.Fatalf("%d ms into a content transfer the screen reads stale (pull age %d, window %d): the wall is showing the previous program correctly and an operator has been sent to look at it for nothing",
				elapsed, got.LastPullAgeMs, LiveWindowMs)
		}
	}
	// And past the live window it reads `fetching` specifically — not `live`,
	// because nothing has confirmed anything.
	if got := report(pullAtMs + LiveWindowMs + 1_000); got.Reachability != ReachabilityFetching {
		t.Fatalf("past the live window a transferring screen reads %q, want fetching: `live` would be a claim no contact supports", got.Reachability)
	}
	// The transfer finishes and the player acknowledges. The next report carries
	// a fresh ack against the same old pull, and the screen is live again with no
	// stale flash in between.
	done := pullAtMs + 60_000
	if err := r.ApplyScreenStatus("relay-1", done, []wire.ScreenStatusEntry{{
		ScreenID: "screen-hanger", Paired: true,
		LastPullAgeMs: done - pullAtMs, LastAckAgeMs: 100, LastRenderStartAgeMs: NeverObserved,
		UnackedPulls: 0, // the ack cleared it
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus after the ack: %v", err)
	}
	set(done)
	if got := statusFor(t, r, "screen-hanger").Reachability; got != ReachabilityLive {
		t.Fatalf("the report carrying the completed transfer's ack reads %q, want live — a stale flash between the ack and the next pull is the flap this threshold exists to remove", got)
	}
}

// TestAScreenThatDiedBetweenAPullAndItsAckStillGoesStale is the AGE half of the
// expiry, and it is the reason `fetching` is bounded on time at all. The
// observation is IDENTICAL to a transfer in progress — pulled once, not
// acknowledged, nothing since — so the only thing that can separate them is
// time, and a state with no expiry would hide a dead screen behind a hopeful
// word forever.
//
// Its counterpart is TestAScreenFailingEveryPullIsNeverCalledFetching, which is
// the case this bound alone cannot reach: a screen that keeps pulling resets its
// own age and would sit inside this window permanently.
func TestAScreenThatDiedBetweenAPullAndItsAckStillGoesStale(t *testing.T) {
	now, set := clockAt(1_000_000)
	r := mustRegistry(t, now)

	if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{{
		ScreenID: "screen-dead", Paired: true,
		LastPullAgeMs: 0, LastAckAgeMs: NeverObserved, LastRenderStartAgeMs: NeverObserved,
		// One pull outstanding and no more coming: the screen lost power between
		// the pull and the ack, so the count stops here while the age climbs.
		UnackedPulls: 1,
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}
	set(1_000_000 + ContentTransferWindowMs)
	if got := statusFor(t, r, "screen-dead").Reachability; got != ReachabilityFetching {
		t.Fatalf("at exactly the transfer window the screen reads %q, want fetching", got)
	}
	set(1_000_000 + ContentTransferWindowMs + 1)
	if got := statusFor(t, r, "screen-dead").Reachability; got != ReachabilityStale {
		t.Fatalf("one ms past the transfer window a screen that never acknowledged its pull reads %q, want stale — an unbounded `fetching` is the withdrawn 180 000 ms window with a friendlier label", got)
	}
}

// TestReachabilityReadsThePullNotWhicheverContactWasLatest: a screen correctly
// showing a BLANK program never reports a render start, and a screen that pulls
// but never acknowledges is still checking in. Judging by "the most recent of the
// three" would call the first one dead and the second one alive for the wrong
// reason.
func TestReachabilityReadsThePullNotWhicheverContactWasLatest(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{{
		ScreenID: "screen-blank", Paired: true,
		LastPullAgeMs: 2_000, LastAckAgeMs: 2_000,
		// Never rendered anything: its program is display:blank.
		LastRenderStartAgeMs: NeverObserved,
		Display:              "blank",
	}}); err != nil {
		t.Fatalf("ApplyScreenStatus: %v", err)
	}
	if got := statusFor(t, r, "screen-blank").Reachability; got != ReachabilityLive {
		t.Errorf("a healthy screen showing a blank program reads %q, want live", got)
	}
}

// TestAReportReplacesOnlyItsOwnRelaysView: a report is a full-set replace of ONE
// relay's screens. A relay reporting must never blank another relay's — that
// would make a live wall read as never-checked-in, which is precisely the alarm
// this surface exists to raise honestly.
func TestAReportReplacesOnlyItsOwnRelaysView(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{
		{ScreenID: "screen-a", LastPullAgeMs: 1_000},
	}); err != nil {
		t.Fatalf("relay-1 report: %v", err)
	}
	if err := r.ApplyScreenStatus("relay-2", 1_000_000, []wire.ScreenStatusEntry{
		{ScreenID: "screen-b", LastPullAgeMs: 1_000},
	}); err != nil {
		t.Fatalf("relay-2 report: %v", err)
	}
	if got := len(r.Statuses()); got != 2 {
		t.Fatalf("%d statuses after two relays reported, want 2", got)
	}

	// relay-2 now reports NOTHING — its own screens go, relay-1's stay.
	if err := r.ApplyScreenStatus("relay-2", 1_000_000, nil); err != nil {
		t.Fatalf("relay-2 empty report: %v", err)
	}
	got := r.Statuses()
	if len(got) != 1 || got[0].ScreenID != "screen-a" {
		t.Fatalf("after relay-2 reported an empty set, statuses = %+v, want only screen-a", got)
	}
	if got[0].RelayID != "relay-1" {
		t.Errorf("relay_id = %q, want relay-1", got[0].RelayID)
	}
}

// TestForgetScreensDropsARevokedRelaysView: revocation means the relay no longer
// speaks for the site; leaving its last report serving the console would leave it
// speaking for the site anyway.
func TestForgetScreensDropsARevokedRelaysView(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{
		{ScreenID: "screen-a", LastPullAgeMs: 1_000},
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	r.ForgetScreens("relay-1")
	if got := r.Statuses(); len(got) != 0 {
		t.Fatalf("%d statuses after forgetting the reporting relay, want 0: %+v", len(got), got)
	}
}

// TestARefusedReportLeavesThePriorViewIntact: a full-set replace applied HALF way
// installs a view that is not the relay's actual view — silently blanking exactly
// the screens whose entries were malformed. Each refusal shape is covered because
// each is a different way for a relay to be wrong.
func TestARefusedReportLeavesThePriorViewIntact(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	good := []wire.ScreenStatusEntry{{ScreenID: "screen-a", LastPullAgeMs: 1_000}}
	if err := r.ApplyScreenStatus("relay-1", 1_000_000, good); err != nil {
		t.Fatalf("good report: %v", err)
	}

	bad := map[string][]wire.ScreenStatusEntry{
		"an entry naming no screen": {{ScreenID: "screen-b"}, {ScreenID: ""}},
		"the same screen twice":     {{ScreenID: "screen-b"}, {ScreenID: "screen-b"}},
		"an over-long field":        {{ScreenID: "screen-b", ProgramRevision: strings.Repeat("x", maxScreenFieldBytes+1)}},
	}
	for name, entries := range bad {
		if err := r.ApplyScreenStatus("relay-1", 2_000_000, entries); err == nil {
			t.Errorf("a report with %s was accepted, want a refusal", name)
		}
	}
	// An over-large report is refused too, without allocating per entry first.
	oversize := make([]wire.ScreenStatusEntry, maxScreensPerReport+1)
	if err := r.ApplyScreenStatus("relay-1", 2_000_000, oversize); err == nil {
		t.Error("an over-large report was accepted, want a refusal")
	}
	// An unauthenticated report is refused: nothing may be attributed to no relay.
	if err := r.ApplyScreenStatus("", 2_000_000, good); err == nil {
		t.Error("a report carrying no authenticated relay identity was accepted, want a refusal")
	}

	after := r.Statuses()
	if len(after) != 1 || after[0].ScreenID != "screen-a" {
		t.Fatalf("after five refused reports the view is %+v, want the prior view (screen-a) intact", after)
	}
}

// TestTheFreshestReportWinsForAScreenTwoRelaysClaim: it should not happen (a
// screen pairs with one relay) but it is reachable while migrating a screen
// between relays, and the freshest observation is the only defensible answer —
// the alternative is a console flickering between two relays' views of one wall.
func TestTheFreshestReportWinsForAScreenTwoRelaysClaim(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	// relay-old reported a while ago; relay-new reported just now.
	if err := r.ApplyScreenStatus("relay-old", 900_000, []wire.ScreenStatusEntry{
		{ScreenID: "screen-a", LastPullAgeMs: 1_000, ProgramRevision: "rev-old"},
	}); err != nil {
		t.Fatalf("old report: %v", err)
	}
	if err := r.ApplyScreenStatus("relay-new", 1_000_000, []wire.ScreenStatusEntry{
		{ScreenID: "screen-a", LastPullAgeMs: 1_000, ProgramRevision: "rev-new"},
	}); err != nil {
		t.Fatalf("new report: %v", err)
	}

	got := r.Statuses()
	if len(got) != 1 {
		t.Fatalf("%d statuses for one screen claimed by two relays, want 1: %+v", len(got), got)
	}
	if got[0].RelayID != "relay-new" || got[0].ProgramRevision != "rev-new" {
		t.Errorf("the winning report is %q/%q, want relay-new/rev-new (the freshest)", got[0].RelayID, got[0].ProgramRevision)
	}
}

// TestAReportStampedInTheFutureReadsAsBrandNew rather than as a negative age.
// The app's own clock can step backwards between an arrival and a read (an NTP
// correction), and a negative age would mis-order every comparison downstream and
// could collide with the never sentinel.
func TestAReportStampedInTheFutureReadsAsBrandNew(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	if err := r.ApplyScreenStatus("relay-1", 1_500_000, []wire.ScreenStatusEntry{
		{ScreenID: "screen-a", LastPullAgeMs: 2_000},
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	got := statusFor(t, r, "screen-a")
	if got.ReportAgeMs != 0 {
		t.Errorf("report_age for a future-stamped report = %d, want 0", got.ReportAgeMs)
	}
	if got.LastPullAgeMs != 2_000 {
		t.Errorf("last_pull age = %d, want the relay's own 2000 unchanged", got.LastPullAgeMs)
	}
}

// TestANegativeReportedAgeIsTreatedAsNeverObserved: -1 is the only negative this
// model defines, and any other negative is a relay reporting something that is
// not a measurement. Carrying it forward arithmetically would surface as a screen
// that pulled in the future.
func TestANegativeReportedAgeIsTreatedAsNeverObserved(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{
		{ScreenID: "screen-a", LastPullAgeMs: -5_000, LastAckAgeMs: NeverObserved, LastRenderStartAgeMs: NeverObserved},
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	got := statusFor(t, r, "screen-a")
	if got.LastPullAgeMs != NeverObserved {
		t.Errorf("a nonsense negative age surfaced as %d, want %d (never observed)", got.LastPullAgeMs, NeverObserved)
	}
	if got.Reachability != ReachabilityNeverSeen {
		t.Errorf("reachability = %q, want never_seen", got.Reachability)
	}
}

// TestNewRegistryRequiresAClock: every value this model produces is a duration,
// so a registry with no clock could only ever answer zero — which would read as
// "every screen checked in just now", the exact opposite of the truth.
func TestNewRegistryRequiresAClock(t *testing.T) {
	if _, err := NewRegistry(nil); err == nil {
		t.Fatal("NewRegistry(nil) succeeded, want a refusal")
	}
}

// replayAHealthyScreen drives the read model exactly as the field drives it — a
// player pulling on pullCadenceMs, a relay reporting on its own unsynchronised
// cadence, and a console reading at instants that line up with neither — and
// returns the worst `last_pull_age_ms` the console ever saw.
//
// It fails the test the moment a screen that has NEVER missed a pull reads
// `stale`, which is the property, rather than checking `age <= LiveWindowMs`,
// which would pass for any pair of numbers.
func replayAHealthyScreen(t *testing.T, pullCadenceMs int64) int64 {
	t.Helper()
	const (
		// Long enough to walk the whole beat pattern several times over: the two
		// tickers are free-running in the field, and the case that matters is the
		// read that lands just before a report which lands just before a pull.
		horizonMs = 15 * 60 * 1_000
		readEvery = 1_000
		start     = int64(1_752_537_600_000)
	)
	reportCadenceMs := wire.ScreenStatusReportIntervalMs

	now, set := clockAt(start)
	r := mustRegistry(t, now)

	var (
		lastPullAt   = int64(start)
		lastReportAt = int64(0)
		worstAge     int64
	)
	for t0 := int64(0); t0 <= horizonMs; t0 += readEvery {
		clock := start + t0

		// The player pulls on its own cadence, forever. This screen is HEALTHY:
		// it never misses one.
		if clock-lastPullAt >= pullCadenceMs {
			lastPullAt = clock
		}
		// The relay reports on its cadence, carrying the age IT measured.
		if lastReportAt == 0 || clock-lastReportAt >= reportCadenceMs {
			lastReportAt = clock
			if err := r.ApplyScreenStatus("relay-1", clock, []wire.ScreenStatusEntry{{
				ScreenID:      "screen-hanger",
				Paired:        true,
				LastPullAgeMs: clock - lastPullAt,
				// A slide cast: acked, rendering, nothing unusual.
				LastAckAgeMs:         clock - lastPullAt,
				LastRenderStartAgeMs: clock - lastPullAt,
				ProgramRevision:      "rev-7",
				Priority:             "scheduled",
				Display:              "content",
				ContentCount:         4,
			}}); err != nil {
				t.Fatalf("ApplyScreenStatus at +%dms: %v", t0, err)
			}
		}

		// The console reads, at an instant that lines up with neither ticker.
		set(clock)
		got := statusFor(t, r, "screen-hanger")
		if got.LastPullAgeMs > worstAge {
			worstAge = got.LastPullAgeMs
		}
		if got.Reachability != ReachabilityLive {
			t.Fatalf("at +%dms a screen pulling every %d ms — one that has NEVER missed a pull — reads %q (last_pull_age_ms = %d, window = %d).\nThe window must cover a healthy screen's worst honest age; see internal/shared/wire/screencadence.go.",
				t0, pullCadenceMs, got.Reachability, got.LastPullAgeMs, LiveWindowMs)
		}
	}

	// The replay must actually have exercised the top of the sawtooth, or a
	// future edit that quietly shrinks the horizon would leave this test passing
	// without ever testing anything.
	if worstAge < pullCadenceMs {
		t.Fatalf("the replay's worst observed age was %d ms, below one pull cadence (%d ms) — it never reached the top of the sawtooth and proves nothing", worstAge, pullCadenceMs)
	}
	return worstAge
}

// TestAScreenAtTheDERIVEDCADENCEBOUNDNeverReadsStale drives the replay at the
// worst cadence the derivation admits: the player's poll wait plus the whole of
// its program-request timeout. A screen slower than this is not on the healthy
// path at all — its pull failed and it is in retry backoff.
//
// This is the derivation's own claim, driven rather than asserted. The cadence
// comes from wire, never from a literal here: the previous version of this test
// hardcoded 55_100 to match a constant that was itself hand-entered, so the two
// agreed with each other and with nothing else (see screencadence.go's
// "correction of 2026-08").
func TestAScreenAtTheDERIVEDCADENCEBOUNDNeverReadsStale(t *testing.T) {
	worstAge := replayAHealthyScreen(t, wire.HealthyProgramPullCadenceMs)

	// And the window is not merely survived, it is survived with the margin the
	// derivation promises: a screen already at the top of the healthy cadence
	// that then loses one whole program request must still read live.
	//
	// This used to demand a WHOLE further pull cycle of headroom, and that demand
	// was only satisfiable because the cadence it was measured against omitted
	// the player's lease-ack round trip. With the honest 26 000 ms cadence,
	// "survives one whole missed cycle" (62 000 ms) and "a screen in retry
	// backoff reads stale" (under 60 000 ms) cannot both be true, and the second
	// one is the one that matters — see ScreenLiveWindowCadenceMultiple's own
	// doc, and screencadence_test.go's TestTheLiveWindowCoversAHealthyScreenAndOneFailedPull,
	// which does the same arithmetic against the player's real backoff schedule.
	if headroom := LiveWindowMs - worstAge; headroom < wire.ProgramPullRequestTimeoutMs {
		t.Fatalf("the worst age at the cadence bound was %d ms against a %d ms window: %d ms of headroom, less than the one lost program request (%d ms) the window must absorb",
			worstAge, LiveWindowMs, headroom, wire.ProgramPullRequestTimeoutMs)
	}
}

// TestTheFIELDMEASUREDSawtoothNeverReadsStale is the same replay at what a real
// screen actually does, so the model is checked against hardware and not only
// against itself.
//
// Measured on The Hanger, 2026-08, after the content-URL 403 that had that
// screen stuck in retry backoff was fixed: `last_pull_age_ms` sawtooths between
// ~9 400 ms and ~19 500 ms — a 10 s poll plus LAN latency. The assertions are
// two-sided on purpose. Never stale is the operator-visible property; the upper
// bound on the observed peak is what keeps this test honest, because a replay
// that silently stopped reproducing the measurement would otherwise go on
// passing forever.
func TestTheFIELDMEASUREDSawtoothNeverReadsStale(t *testing.T) {
	// A 10 s wait plus ~100 ms of real work per iteration on a LAN — the cadence
	// that produces the measured sawtooth against a 10 s report interval.
	const fieldPullCadenceMs = 10_100
	// The measured peak, with room for the replay's 1 s read granularity.
	const measuredPeakMs = 19_500
	const tolerance = 2_000

	worstAge := replayAHealthyScreen(t, fieldPullCadenceMs)

	if worstAge > measuredPeakMs+tolerance {
		t.Fatalf("the replay's worst age was %d ms, above the ~%d ms peak measured on real hardware (+%d tolerance); this replay no longer reproduces the field and its never-stale result proves less than it looks",
			worstAge, measuredPeakMs, tolerance)
	}
	if worstAge > LiveWindowMs/2 {
		t.Fatalf("a real healthy screen's worst age (%d ms) is more than half the live window (%d ms); the window has been tightened past what hardware actually presents and the console will flap",
			worstAge, LiveWindowMs)
	}
}

// TestLiveWindowIsTheSharedDerivedConstant: this package must not re-declare the
// threshold. It publishes wire's, so that changing the measured cadence in one
// place moves the console's line with it.
func TestLiveWindowIsTheSharedDerivedConstant(t *testing.T) {
	if LiveWindowMs != wire.ScreenLiveWindowMs {
		t.Fatalf("screens.LiveWindowMs = %d but wire.ScreenLiveWindowMs = %d; the read model has grown a second, independently-drifting threshold",
			LiveWindowMs, wire.ScreenLiveWindowMs)
	}
}
