package api

import "testing"

// The staleness rule is the whole reason pack health is not just a stored
// string, and it is unexported logic, so it is exercised here rather than
// through the route.

const healthNow int64 = 1_752_537_600_000

// A fresh report reads back exactly as the pack sent it.
func TestAFreshHealthReportIsReportedAsGiven(t *testing.T) {
	r := newPackHealthRegistry()
	r.put("acme/one", packHealthReport{Status: healthDegraded, Detail: "credentials expire soon", ReportedAt: healthNow})

	got := r.snapshot(healthNow + 1_000)
	if len(got) != 1 {
		t.Fatalf("snapshot = %+v, want one line", got)
	}
	if got[0].Status != healthDegraded || got[0].Detail != "credentials expire soon" {
		t.Fatalf("line = %+v, want the pack's own words", got[0])
	}
}

// THE rule. A report that never expired would leave a green line for an
// extension that wedged an hour ago — on the page an operator opened to find out
// what was wrong.
func TestAStaleHealthReportDegradesToUnknown(t *testing.T) {
	r := newPackHealthRegistry()
	r.put("acme/one", packHealthReport{Status: healthOK, Detail: "all good", ReportedAt: healthNow})

	got := r.snapshot(healthNow + packHealthTTLMs + 1)
	if len(got) != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
	if got[0].Status != healthUnknown {
		t.Fatalf("stale report reads %q, want unknown — an ok that never expires is a page that lies", got[0].Status)
	}
	if got[0].Detail == "all good" {
		t.Fatal("a stale line still shows the pack's old detail; it must say that nothing has been heard since")
	}
}

// Stale is UNKNOWN, not DOWN. A missed report is evidence about the reporting,
// not about the pack: grading a busy extension as down would train an operator
// to ignore the colour.
func TestAStaleReportIsNotGradedAsDown(t *testing.T) {
	r := newPackHealthRegistry()
	r.put("acme/one", packHealthReport{Status: healthOK, Detail: "all good", ReportedAt: healthNow})
	if got := r.snapshot(healthNow + packHealthTTLMs + 1); got[0].Status == healthDown {
		t.Fatal("a silent pack was graded down; silence is not failure")
	}
}

// A report exactly AT the ttl is still current. The boundary matters because a
// pack reporting on an interval equal to the ttl would otherwise flap between
// ok and unknown on every read.
func TestAReportAtExactlyTheTTLIsStillCurrent(t *testing.T) {
	r := newPackHealthRegistry()
	r.put("acme/one", packHealthReport{Status: healthOK, Detail: "all good", ReportedAt: healthNow})
	if got := r.snapshot(healthNow + packHealthTTLMs); got[0].Status != healthOK {
		t.Fatalf("a report at exactly the ttl reads %q, want it still current", got[0].Status)
	}
}

// The newest report wins. A pack that recovers must be able to say so, or its
// first bad moment would define it forever.
func TestALaterReportReplacesAnEarlierOne(t *testing.T) {
	r := newPackHealthRegistry()
	r.put("acme/one", packHealthReport{Status: healthDown, Detail: "broken", ReportedAt: healthNow})
	r.put("acme/one", packHealthReport{Status: healthOK, Detail: "recovered", ReportedAt: healthNow + 10})

	got := r.snapshot(healthNow + 20)
	if len(got) != 1 || got[0].Status != healthOK || got[0].Detail != "recovered" {
		t.Fatalf("snapshot = %+v, want only the newer report", got)
	}
}

// Lines are ordered by pack, so a health page does not reshuffle between reads.
func TestHealthLinesAreOrderedByPack(t *testing.T) {
	r := newPackHealthRegistry()
	for _, id := range []string{"acme/zebra", "acme/apple", "acme/mango"} {
		r.put(id, packHealthReport{Status: healthOK, Detail: "fine", ReportedAt: healthNow})
	}
	got := r.snapshot(healthNow)
	if len(got) != 3 || got[0].Name > got[1].Name || got[1].Name > got[2].Name {
		t.Fatalf("snapshot is not sorted by pack: %+v", got)
	}
}

// Every status a pack may report is one the page can RANK. An unranked status
// counts as the best one, so an unrankable status would let an extension report
// its own failure in a way that improves the summary.
func TestEveryReportableStatusIsRankable(t *testing.T) {
	for _, s := range []string{healthOK, healthDegraded, healthDown} {
		if !validPackHealthStatus(s) {
			t.Fatalf("%q is rankable but not reportable", s)
		}
		if _, ok := healthRank[s]; !ok {
			t.Fatalf("a pack may report %q and the page cannot rank it — it would count as the best status", s)
		}
	}
	if _, ok := healthRank[healthUnknown]; !ok {
		t.Fatal("a stale report reads unknown and the page cannot rank it")
	}
}
