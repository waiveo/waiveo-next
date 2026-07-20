package api1_test

import (
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/api1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// expectedDriven is every api-1 sync-convention corpus case this driver
// drives: optimistic concurrency (API-022/023), keyset pagination (API-032),
// the label-selector grammar (API-044/045), and client-assignable external_id
// (API-102).
var expectedDriven = []string{
	"API-022-invalid-if-match-missing",
	"API-023-invalid-if-match-conflict",
	"API-032-valid-pagination-roundtrip",
	"API-044-valid-selector-scope-subtree",
	"API-045-invalid-selector-malformed",
	"API-102-invalid-external-id-conflict",
}

// expectedPending is every other case frozen under conformance/corpora/api-1:
// the async-half cases plus api/1's own error-shape cases, deliberately not
// driven here (§10 "no silent caps" — recorded PENDING with a reason, never
// silently absent).
var expectedPending = []string{
	"API-010-valid-simple-problem",
	"API-013-valid-multi-field-validation-problem",
	"API-052-valid-idempotency-replay",
	"API-053-invalid-idempotency-key-reused-different-body",
	"API-111-valid-bulk-enable-202-job",
	"API-121-valid-export-workspace-job",
}

// TestAPI1DriverGreen replays every frozen api-1 corpus case against the live
// internal/shared/apihttp + internal/shared/apiselector implementations: the
// six sync-convention cases are driven and PASS; the async-half + error-shape
// cases are explicitly PENDING, never silently missing.
func TestAPI1DriverGreen(t *testing.T) {
	rep := api1.Run()
	t.Logf("\n%s", rep.String())

	gotDriven := rep.Driven()
	sort.Strings(gotDriven)
	wantDriven := append([]string(nil), expectedDriven...)
	sort.Strings(wantDriven)
	if !equalStrings(gotDriven, wantDriven) {
		t.Errorf("driven set = %v, want %v", gotDriven, wantDriven)
	}

	gotPending := rep.PendingIDs()
	sort.Strings(gotPending)
	wantPending := append([]string(nil), expectedPending...)
	sort.Strings(wantPending)
	if !equalStrings(gotPending, wantPending) {
		t.Errorf("pending set = %v, want %v", gotPending, wantPending)
	}

	if !rep.OK() {
		t.Errorf("report not OK:\n%s", rep.String())
	}
}

// TestAPI1DriverHasTeeth proves the driver actually diffs against the live
// implementation rather than rubber-stamping every case: it takes the real
// API-023 case (whose corpus `expected.body.current_revision` truthfully says
// 3, the resource's real current revision) and corrupts ONLY the declared
// expectation to 999, leaving CheckIfMatch's real, correct behavior
// untouched. A driver with teeth reports this case FAIL — the real revision
// really is 3, so the manufactured expectation of 999 is itself what's wrong,
// and the driver must say so.
func TestAPI1DriverHasTeeth(t *testing.T) {
	cases, err := api1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	broken, ok := cases["API-023-invalid-if-match-conflict"]
	if !ok {
		t.Fatal("API-023-invalid-if-match-conflict case missing from the frozen corpus")
	}
	brokenExpected := deepCopyMap(broken.Expected)
	brokenBody, ok := brokenExpected["body"].(map[string]any)
	if !ok {
		t.Fatal("API-023 corpus case has no expected.body map")
	}
	if brokenBody["current_revision"] != float64(3) {
		t.Fatalf("test bug: expected current_revision 3 in the real corpus, got %v", brokenBody["current_revision"])
	}
	brokenBody["current_revision"] = float64(999) // the real resource is at revision 3; this is a lie.
	brokenExpected["body"] = brokenBody
	broken.Expected = brokenExpected
	cases["API-023-invalid-if-match-conflict"] = broken

	rep := api1.RunCases(cases)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "API-023-invalid-if-match-conflict") {
		t.Errorf("expected API-023-invalid-if-match-conflict to FAIL against a corrupted (current_revision:999) expectation, but it did not; report:\n%s", rep.String())
	}
	if rep.OK() {
		t.Errorf("driver reported OK against a corrupted corpus expectation — the oracle has no teeth")
	}
}

// TestAPI1CorpusFullyAccountedFor extends the "no silent caps" guarantee to
// any case nobody has triaged yet: it enumerates every case_id actually
// present in the frozen api-1 corpus DIRECTORY and asserts that set is
// EXACTLY Driven() ∪ PendingIDs(). A new corpus/*.json case frozen without
// wiring it into the driver (driven or explicitly Pending with a reason)
// fails this test by name.
func TestAPI1CorpusFullyAccountedFor(t *testing.T) {
	rep := api1.Run()

	cases, err := api1.LoadCorpus()
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
		t.Errorf("corpus case(s) frozen under conformance/corpora/api-1 but NEITHER driven NOR pending: %v", uncovered)
	}
	if len(phantom) > 0 {
		t.Errorf("driver names case id(s) absent from the frozen corpus (phantom id, or corpus file renamed/removed): %v", phantom)
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
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
