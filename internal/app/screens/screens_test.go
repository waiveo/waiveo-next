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

	if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{{
		ScreenID: "screen-a", Paired: true, LastPullAgeMs: 3_000, LastAckAgeMs: 3_500, LastRenderStartAgeMs: 4_000,
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

	// Two minutes later, with no new report. Every age has grown by two minutes
	// and the screen has crossed the live window.
	set(1_000_000 + 120_000)
	got = statusFor(t, r, "screen-a")
	if got.LastPullAgeMs != 123_000 {
		t.Errorf("last_pull age two minutes later = %d, want 123000 — a status that does not age reports a dead fleet as healthy forever", got.LastPullAgeMs)
	}
	if got.LastAckAgeMs != 123_500 || got.LastRenderStartAgeMs != 124_000 {
		t.Errorf("ack/render ages = %d/%d, want 123500/124000", got.LastAckAgeMs, got.LastRenderStartAgeMs)
	}
	if got.ReportAgeMs != 120_000 {
		t.Errorf("report_age = %d, want 120000 — the field that distinguishes a dead screen from a dead relay", got.ReportAgeMs)
	}
	if got.Reachability != ReachabilityStale {
		t.Errorf("reachability at 123s stale = %q, want stale", got.Reachability)
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

// TestTheLiveWindowBoundary pins the one threshold this model draws, at its
// edges. Exactly at the window is live; one millisecond past it is not.
func TestTheLiveWindowBoundary(t *testing.T) {
	now, _ := clockAt(1_000_000)
	r := mustRegistry(t, now)

	cases := []struct {
		age  int64
		want Reachability
	}{
		{0, ReachabilityLive},
		{LiveWindowMs - 1, ReachabilityLive},
		{LiveWindowMs, ReachabilityLive},
		{LiveWindowMs + 1, ReachabilityStale},
		{NeverObserved, ReachabilityNeverSeen},
	}
	for _, tc := range cases {
		if err := r.ApplyScreenStatus("relay-1", 1_000_000, []wire.ScreenStatusEntry{
			{ScreenID: "screen-a", LastPullAgeMs: tc.age, LastAckAgeMs: NeverObserved, LastRenderStartAgeMs: NeverObserved},
		}); err != nil {
			t.Fatalf("ApplyScreenStatus: %v", err)
		}
		if got := statusFor(t, r, "screen-a").Reachability; got != tc.want {
			t.Errorf("last_pull age %d → reachability %q, want %q", tc.age, got, tc.want)
		}
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
