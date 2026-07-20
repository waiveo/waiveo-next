package manifest1_test

import (
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/manifest1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// expectedDriven is every conformance/corpora/manifest-1 case this driver
// drives: the original identity/compat/capability/egress/data-model happy-path
// and negative fixtures, plus the five new negative cases this task adds
// (MAN-052/072/081/090/091) turning their traceability rows from a
// positive-only "covered" into a genuinely negative-tested one.
var expectedDriven = []string{
	"MAN-001-valid-minimal",
	"MAN-020-valid-full-featured",
	"MAN-021-invalid-unknown-capability",
	"MAN-030-invalid-egress-wildcard",
	"MAN-051-valid-data-model",
	"MAN-052-invalid-two-title-fields",
	"MAN-072-invalid-devices-without-compat-relay",
	"MAN-081-invalid-fixed-without-duration-seconds",
	"MAN-090-invalid-non-namespaced-event",
	"MAN-091-invalid-automation-action-not-in-actions",
}

// TestManifest1DriverGreen replays every frozen manifest-1 corpus case
// against the live internal/manifest.Validate implementation: every case is
// driven (none deferred), and every driven case PASSes.
func TestManifest1DriverGreen(t *testing.T) {
	rep := manifest1.Run()
	t.Logf("\n%s", rep.String())

	got := rep.Driven()
	sort.Strings(got)
	if !equalStrings(got, expectedDriven) {
		t.Errorf("driven set = %v, want %v", got, expectedDriven)
	}
	if len(rep.PendingIDs()) != 0 {
		t.Errorf("pending set = %v, want none — manifest/1's structural surface has no hardware/dep-gated cases", rep.PendingIDs())
	}
	if !rep.OK() {
		t.Errorf("report not OK:\n%s", rep.String())
	}
}

// TestManifest1DriverHasTeeth proves the driver actually diffs against the
// live implementation rather than rubber-stamping every case: it takes the
// real MAN-001-valid-minimal case (whose corpus `expected` truthfully says
// valid:true) and corrupts ONLY the declared expectation to valid:false,
// leaving manifest.Validate's real, correct behavior untouched. A driver
// with teeth reports this case FAIL — the minimal manifest really does
// validate clean against the fixture host, so the manufactured expectation
// of failure is itself what's wrong, and the driver must say so.
func TestManifest1DriverHasTeeth(t *testing.T) {
	cases, err := manifest1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	broken, ok := cases["MAN-001-valid-minimal"]
	if !ok {
		t.Fatal("MAN-001-valid-minimal case missing from the frozen corpus")
	}
	brokenExpected := make(map[string]any, len(broken.Expected))
	for k, v := range broken.Expected {
		brokenExpected[k] = v
	}
	brokenExpected["valid"] = false // the real manifest validates clean; this is a lie.
	broken.Expected = brokenExpected
	cases["MAN-001-valid-minimal"] = broken

	rep := manifest1.RunCases(cases)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "MAN-001-valid-minimal") {
		t.Errorf("expected MAN-001-valid-minimal to FAIL against a corrupted (valid:false) expectation, but it did not; report:\n%s", rep.String())
	}
	if rep.OK() {
		t.Errorf("driver reported OK against a corrupted corpus expectation — the oracle has no teeth")
	}
}

// TestManifest1CorpusFullyAccountedFor extends the "no silent caps" guarantee
// to any case nobody has triaged yet: it enumerates every case_id actually
// present in the frozen manifest-1 corpus DIRECTORY and asserts that set is
// EXACTLY Driven() ∪ PendingIDs(). A new corpus/*.json case frozen without
// wiring it into the driver (driven or explicitly Pending with a reason)
// fails this test by name.
func TestManifest1CorpusFullyAccountedFor(t *testing.T) {
	rep := manifest1.Run()

	cases, err := manifest1.LoadCorpus()
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
		t.Errorf("corpus case(s) frozen under conformance/corpora/manifest-1 but NEITHER driven NOR pending: %v", uncovered)
	}
	if len(phantom) > 0 {
		t.Errorf("driver names case id(s) absent from the frozen corpus (phantom id, or corpus file renamed/removed): %v", phantom)
	}
}

func caseFailed(rep report.Report, short string) bool {
	for _, c := range rep.Cases {
		if len(c.CaseID) >= len(short) && c.CaseID[:len(short)] == short {
			return c.Status == report.FAIL
		}
	}
	return false
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
