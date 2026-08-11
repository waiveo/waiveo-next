package events1_test

import (
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/events1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// expectedDriven is every conformance/corpora/events-1 case this driver
// drives — against the LIVE, HTTP-mounted internal/app/eventingest (POST
// /telemetry/v1/push) and internal/app/eventsse (GET /events/v1) handlers
// wherever a real producer/transport path exists, and directly against
// internal/events for the two schemas confirmed to have no producer or HTTP
// endpoint anywhere in the tree (audit.event, the internal-origin
// automation.run case) — see driver.go's package doc and each drive
// function's own comment for which is which and why.
var expectedDriven = []string{
	"EVT-010-valid-entity-state-changed",
	"EVT-013-invalid-registered-schema-malformed-payload",
	"EVT-013-invalid-unregistered-schema-payload",
	"EVT-040-valid-automation-run",
	"EVT-041-valid-automation-run-misfire-caught",
	"EVT-041-valid-automation-run-restarted",
	"EVT-041-valid-automation-run-skipped-internal",
	"EVT-050-valid-content-played",
	"EVT-055-valid-screen-interaction",
	"EVT-060-valid-device-heartbeat",
	"EVT-070-valid-box-vitals",
	"EVT-080-valid-audit-login-failure",
	"EVT-091-valid-hello-fresh-subscribe",
	"EVT-101-valid-sse-selector-and-schemas",
	"EVT-120-valid-scope-filtered-subscription",
	"EVT-134-invalid-resume-from-malformed",
	"EVT-134-invalid-resume-from-out-of-scope",
	"EVT-140-valid-resume-with-gap",
	"EVT-142-valid-mid-stream-buffer-exceeded-gap",
	"EVT-142a-invalid-deferral-exceeds-its-bound",
	"EVT-142a-valid-deferral-does-not-hold-the-delivery-position",
	"EVT-142a-valid-deferred-marker-resolves-inside-the-bound",
	"EVT-151-valid-webhook-delivery-signed",
}

// expectedPending is empty: every frozen events-1 case is driven.
var expectedPending = []string{}

// TestEvents1DriverGreen replays every frozen events-1 corpus case against
// the live handlers: every case in the corpus is driven (none pending, none
// silently skipped), and every driven case PASSes — unlike api1 (its sibling
// driver), mounting the real events/1 handlers surfaced no genuine
// corpus-vs-code divergence: the live eventingest/eventsse behavior matches
// the frozen corpus exactly, for everything a live handler exists to drive.
func TestEvents1DriverGreen(t *testing.T) {
	rep := events1.Run()
	t.Logf("\n%s", rep.String())

	got := sortedCopy(rep.Driven())
	want := sortedCopy(expectedDriven)
	if !equalStrings(got, want) {
		t.Errorf("driven set = %v, want %v", got, want)
	}

	gotPending := sortedCopy(rep.PendingIDs())
	wantPending := sortedCopy(expectedPending)
	if !equalStrings(gotPending, wantPending) {
		t.Errorf("pending set = %v, want %v", gotPending, wantPending)
	}

	if !rep.OK() {
		t.Errorf("report not OK:\n%s", rep.String())
	}
}

// TestEvents1DriverHasTeeth proves the driver actually diffs against the live
// implementation rather than rubber-stamping every case: it takes the real
// EVT-040-valid-automation-run case (whose corpus `expected.delivered` is
// truthfully true — the live eventingest handler really does append a
// well-formed automation.run event) and corrupts ONLY the declared
// expectation to false, leaving the live implementation's real, correct
// behavior untouched. A driver with teeth reports this case FAIL.
func TestEvents1DriverHasTeeth(t *testing.T) {
	cases, err := events1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	const target = "EVT-040-valid-automation-run"
	broken, ok := cases[target]
	if !ok {
		t.Fatalf("%s case missing from the frozen corpus", target)
	}
	brokenExpected := make(map[string]any, len(broken.Expected))
	for k, v := range broken.Expected {
		brokenExpected[k] = v
	}
	brokenExpected["delivered"] = false // the real automation.run event really delivers; this is a lie.
	broken.Expected = brokenExpected
	cases[target] = broken

	rep := events1.RunCases(cases)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, target) {
		t.Errorf("expected %s to FAIL against a corrupted (delivered:false) expectation, but it did not; report:\n%s", target, rep.String())
	}
	if rep.OK() {
		t.Errorf("driver reported OK against a corrupted corpus expectation — the oracle has no teeth")
	}
}

// TestEvents1ResumeGapHasTeeth proves the live-transport EVT-140 case actually
// reads the real streams — it is driven over both bindings — rather than
// rubber-stamping: it corrupts ONLY the declared gap-marker reason, leaving the
// live handler's real buffer_exceeded/retention_expired marker untouched.
func TestEvents1ResumeGapHasTeeth(t *testing.T) {
	cases, err := events1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	const target = "EVT-140-valid-resume-with-gap"
	broken, ok := cases[target]
	if !ok {
		t.Fatalf("%s case missing from the frozen corpus", target)
	}
	brokenExpected := make(map[string]any, len(broken.Expected))
	for k, v := range broken.Expected {
		brokenExpected[k] = v
	}
	frames, ok := brokenExpected["frames"].([]any)
	if !ok || len(frames) != 2 {
		t.Fatalf("test bug: expected.frames must have 2 entries, got %v", brokenExpected["frames"])
	}
	gapFrame, ok := frames[1].(map[string]any)
	if !ok || gapFrame["reason"] != "retention_expired" {
		t.Fatalf("test bug: expected frames[1].reason retention_expired in the real corpus, got %v", gapFrame["reason"])
	}
	brokenGapFrame := make(map[string]any, len(gapFrame))
	for k, v := range gapFrame {
		brokenGapFrame[k] = v
	}
	brokenGapFrame["reason"] = "wrong_reason" // the live gap marker really says retention_expired; this is a lie.
	brokenFrames := append([]any(nil), frames...)
	brokenFrames[1] = brokenGapFrame
	brokenExpected["frames"] = brokenFrames
	broken.Expected = brokenExpected
	cases[target] = broken

	rep := events1.RunCases(cases)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, target) {
		t.Errorf("expected %s to FAIL against a corrupted (gap reason: wrong_reason) expectation, but it did not; report:\n%s", target, rep.String())
	}
}

// TestEvents1CorpusFullyAccountedFor extends the "no silent caps" guarantee
// to any case nobody has triaged yet: it enumerates every case_id actually
// present in the frozen events-1 corpus DIRECTORY and asserts that set is
// EXACTLY Driven() ∪ PendingIDs(). A new corpus/*.json case frozen without
// wiring it into the driver (driven or explicitly Pending with a reason)
// fails this test by name.
func TestEvents1CorpusFullyAccountedFor(t *testing.T) {
	rep := events1.Run()

	cases, err := events1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	inCorpus := make(map[string]bool, len(cases))
	for id := range cases {
		inCorpus[id] = true
	}

	accounted := map[string]bool{}
	for _, id := range rep.Driven() {
		accounted[id] = true
	}
	for _, id := range rep.PendingIDs() {
		accounted[id] = true
	}

	var uncovered, phantom []string
	for id := range inCorpus {
		if !accounted[id] {
			uncovered = append(uncovered, id)
		}
	}
	for id := range accounted {
		if !inCorpus[id] {
			phantom = append(phantom, id)
		}
	}
	sort.Strings(uncovered)
	sort.Strings(phantom)

	if len(uncovered) > 0 {
		t.Errorf("corpus case(s) frozen under conformance/corpora/events-1 but NEITHER driven NOR pending: %v", uncovered)
	}
	if len(phantom) > 0 {
		t.Errorf("driver names case id(s) absent from the frozen corpus (phantom id, or corpus file renamed/removed): %v", phantom)
	}
}

func caseFailed(rep report.Report, id string) bool {
	for _, c := range rep.Cases {
		if c.CaseID == id {
			return c.Status == report.FAIL
		}
	}
	return false
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
