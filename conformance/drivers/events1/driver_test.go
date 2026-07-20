package events1_test

import (
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/events1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// expectedDriven is every first-half conformance/corpora/events-1 case this
// driver drives against the live internal/events implementation: the
// envelope/catalog/EVT-013 gate cases (EVT-010, both EVT-013 pair members),
// the four automation.run cases (EVT-040/041x3), and the three remaining
// platform schemas' single valid case each (EVT-050/060/070/080).
var expectedDriven = []string{
	"EVT-010-valid-entity-state-changed",
	"EVT-013-invalid-registered-schema-malformed-payload",
	"EVT-013-invalid-unregistered-schema-payload",
	"EVT-040-valid-automation-run",
	"EVT-041-valid-automation-run-misfire-caught",
	"EVT-041-valid-automation-run-restarted",
	"EVT-041-valid-automation-run-skipped-internal",
	"EVT-050-valid-content-played",
	"EVT-060-valid-device-heartbeat",
	"EVT-070-valid-box-vitals",
	"EVT-080-valid-audit-login-failure",
}

// expectedPending is the second-half delivery layer (WS/SSE bindings,
// resumable delivery, webhooks) — a separate follow-up plan, not a driver
// gap. §10 "no silent caps": each rides an explicit PENDING row with a
// reason, never a silently-absent one.
var expectedPending = []string{
	"EVT-091-valid-hello-fresh-subscribe",
	"EVT-134-invalid-resume-from-malformed",
	"EVT-140-valid-resume-with-gap",
	"EVT-151-valid-webhook-delivery-signed",
}

// TestEvents1DriverGreen replays every frozen events-1 corpus case against
// the live internal/events implementation: exactly the first-half cases are
// driven (none silently skipped), the delivery-layer cases are explicitly
// PENDING, and every driven case PASSes.
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
// truthfully true — internal/events.Validate really does deliver a
// well-formed automation.run event) and corrupts ONLY the declared
// expectation to false, leaving the live implementation's real, correct
// behavior untouched. A driver with teeth reports this case FAIL — the
// manufactured expectation of non-delivery is itself what's wrong, and the
// driver must say so rather than rubber-stamping every case PASS.
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
