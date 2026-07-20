package deviceclass1_test

import (
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/deviceclass1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// expectedDriven is every conformance/corpora/device-class-registry case this
// driver drives: the original structural happy-path fixture plus the six new
// negative/resolution cases this task adds (REG-011/012/021/031/042/052).
var expectedDriven = []string{
	"REG-011-invalid-origin",
	"REG-012-invalid-class-identifier-collision",
	"REG-021-valid-whitelist-classification-unrecognized-falls-to-off",
	"REG-031-invalid-group-name-collision",
	"REG-042-invalid-attribute-unit-not-applicable",
	"REG-052-invalid-unknown-device-class-command-unresolved",
	"roku-media-player",
}

// TestDeviceClass1DriverGreen replays every frozen device-class-registry/1
// corpus case against the live internal/deviceclass implementation: every
// case is driven (none deferred), and every driven case PASSes.
func TestDeviceClass1DriverGreen(t *testing.T) {
	rep := deviceclass1.Run()
	t.Logf("\n%s", rep.String())

	got := rep.Driven()
	sort.Strings(got)
	if !equalStrings(got, expectedDriven) {
		t.Errorf("driven set = %v, want %v", got, expectedDriven)
	}
	if len(rep.PendingIDs()) != 0 {
		t.Errorf("pending set = %v, want none — device-class-registry/1's structural + resolution surface has no hardware/dep-gated cases", rep.PendingIDs())
	}
	if !rep.OK() {
		t.Errorf("report not OK:\n%s", rep.String())
	}
}

// TestDeviceClass1DriverHasTeeth proves the driver actually diffs against the
// live implementation rather than rubber-stamping every case: it takes the
// real roku-media-player case (whose corpus `expected` truthfully says
// valid:true) and corrupts ONLY the declared expectation to valid:false,
// leaving deviceclass.Validate's real, correct behavior untouched. A driver
// with teeth reports this case FAIL — internal/deviceclass's Builtin()
// registry really does validate clean, so the manufactured expectation of
// failure is itself what's wrong, and the driver must say so.
func TestDeviceClass1DriverHasTeeth(t *testing.T) {
	cases, err := deviceclass1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	broken, ok := cases["roku-media-player"]
	if !ok {
		t.Fatal("roku-media-player case missing from the frozen corpus")
	}
	brokenExpected := make(map[string]any, len(broken.Expected))
	for k, v := range broken.Expected {
		brokenExpected[k] = v
	}
	brokenExpected["valid"] = false // the real registry validates clean; this is a lie.
	broken.Expected = brokenExpected
	cases["roku-media-player"] = broken

	rep := deviceclass1.RunCases(cases)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "roku-media-player") {
		t.Errorf("expected roku-media-player to FAIL against a corrupted (valid:false) expectation, but it did not; report:\n%s", rep.String())
	}
	if rep.OK() {
		t.Errorf("driver reported OK against a corrupted corpus expectation — the oracle has no teeth")
	}
}

// TestDeviceClass1CorpusFullyAccountedFor extends the "no silent caps"
// guarantee to any case nobody has triaged yet: it enumerates every case_id
// actually present in the frozen device-class-registry corpus DIRECTORY and
// asserts that set is EXACTLY Driven() ∪ PendingIDs(). A new corpus/*.json
// case frozen without wiring it into the driver (driven or explicitly
// Pending with a reason) fails this test by name.
func TestDeviceClass1CorpusFullyAccountedFor(t *testing.T) {
	rep := deviceclass1.Run()

	cases, err := deviceclass1.LoadCorpus()
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
		t.Errorf("corpus case(s) frozen under conformance/corpora/device-class-registry but NEITHER driven NOR pending: %v", uncovered)
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
