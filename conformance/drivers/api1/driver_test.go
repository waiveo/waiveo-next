package api1_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/api1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// expectedDriven is every api-1 convention corpus case this driver drives: the
// sync conventions — optimistic concurrency (API-022/023), keyset pagination
// (API-032), the label-selector grammar (API-044/045), and client-assignable
// external_id (API-102) — and the async conventions — Idempotency-Key
// replay/reuse (API-052/053) and the 202 + Job resource (API-111/121).
var expectedDriven = []string{
	"API-022-invalid-if-match-missing",
	"API-023-invalid-if-match-conflict",
	"API-032-valid-pagination-roundtrip",
	"API-044-valid-selector-scope-subtree",
	"API-045-invalid-selector-malformed",
	"API-052-valid-idempotency-replay",
	"API-053-invalid-idempotency-key-reused-different-body",
	"API-102-invalid-external-id-conflict",
	"API-111-valid-bulk-enable-202-job",
	"API-121-valid-export-workspace-job",
}

// expectedPending is the remaining case frozen under conformance/corpora/api-1
// that no driver exercises yet: api/1's own Problem error-shape cases,
// deliberately not driven here (§10 "no silent caps" — recorded PENDING with a
// reason, never silently absent).
var expectedPending = []string{
	"API-010-valid-simple-problem",
	"API-013-valid-multi-field-validation-problem",
}

// TestAPI1DriverGreen replays every frozen api-1 corpus case against the live
// internal/shared/apihttp + apiselector + apijob implementations: the sync- and
// async-convention cases are driven and PASS; only api/1's own error-shape
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

// TestAPI1SelectorParseErrorContentTypeHasTeeth proves driveSelectorParseError
// actually checks the corpus-pinned `content_type` field for a malformed-
// selector case (API-045) against a genuine HTTP response, rather than
// silently never reading it. It corrupts ONLY the declared
// `expected.content_type` from the real "application/problem+json" to a
// wrong value, leaving apiselector.Parse's real ParseError (and the real
// apihttp.WriteProblemExt round-trip) untouched. A driver with teeth reports
// this case FAIL — the live Content-Type header really is
// application/problem+json, so the manufactured expectation is what's wrong.
func TestAPI1SelectorParseErrorContentTypeHasTeeth(t *testing.T) {
	cases, err := api1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	broken, ok := cases["API-045-invalid-selector-malformed"]
	if !ok {
		t.Fatal("API-045-invalid-selector-malformed case missing from the frozen corpus")
	}
	brokenExpected := deepCopyMap(broken.Expected)
	if brokenExpected["content_type"] != "application/problem+json" {
		t.Fatalf("test bug: expected content_type application/problem+json in the real corpus, got %v", brokenExpected["content_type"])
	}
	brokenExpected["content_type"] = "application/json" // the real handler writes application/problem+json; this is a lie.
	broken.Expected = brokenExpected
	cases["API-045-invalid-selector-malformed"] = broken

	rep := api1.RunCases(cases)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "API-045-invalid-selector-malformed") {
		t.Errorf("expected API-045-invalid-selector-malformed to FAIL against a corrupted (content_type:application/json) expectation, but it did not; report:\n%s", rep.String())
	}
	if rep.OK() {
		t.Errorf("driver reported OK against a corrupted content_type expectation — driveSelectorParseError never checks content_type")
	}
}

// TestAPI1SelectorParseErrorBodyTypeHasTeeth proves driveSelectorParseError's
// `body.type` check is tied to the JSON body a live apihttp.WriteProblemExt
// round-trip actually emits, not merely to the corpus fixture's own
// self-consistency. It corrupts ONLY the declared `expected.body.type` away
// from the real "about:blank", leaving the live emission untouched. A driver
// with teeth reports this case FAIL.
func TestAPI1SelectorParseErrorBodyTypeHasTeeth(t *testing.T) {
	cases, err := api1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	broken, ok := cases["API-045-invalid-selector-malformed"]
	if !ok {
		t.Fatal("API-045-invalid-selector-malformed case missing from the frozen corpus")
	}
	brokenExpected := deepCopyMap(broken.Expected)
	brokenBody, ok := brokenExpected["body"].(map[string]any)
	if !ok {
		t.Fatal("API-045 corpus case has no expected.body map")
	}
	if brokenBody["type"] != "about:blank" {
		t.Fatalf("test bug: expected body.type about:blank in the real corpus, got %v", brokenBody["type"])
	}
	brokenBody = deepCopyMap(brokenBody)
	brokenBody["type"] = "https://example.invalid/wrong-type" // the live emission is always about:blank; this is a lie.
	brokenExpected["body"] = brokenBody
	broken.Expected = brokenExpected
	cases["API-045-invalid-selector-malformed"] = broken

	rep := api1.RunCases(cases)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "API-045-invalid-selector-malformed") {
		t.Errorf("expected API-045-invalid-selector-malformed to FAIL against a corrupted (body.type:wrong-type) expectation, but it did not; report:\n%s", rep.String())
	}
	if rep.OK() {
		t.Errorf("driver reported OK against a corrupted body.type expectation")
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

// TestAPI1PendingAPI010013NoOtherCoverage locks in the corrected claim made by
// pendingCaseIDs and the package doc: that the two api/1 Problem error-shape
// fixtures (API-010, API-013) this driver records PENDING are NOT exercised
// by any other driver or test in the repo today. It scans every .go file in
// the module — excluding this package itself, which is expected to name
// these case_ids in order to record them PENDING — for a reference to either
// fixture's case_id. A hit outside this package means the "no other driver
// exercises this fixture" rationale is now stale: some other test has grown
// coverage and pendingCaseIDs must be updated to say so (or the case
// promoted out of pendingCaseIDs entirely).
func TestAPI1PendingAPI010013NoOtherCoverage(t *testing.T) {
	root := repoRoot(t)
	selfDir := filepath.Join(root, "conformance", "drivers", "api1")
	needles := []string{
		"API-010-valid-simple-problem",
		"API-013-valid-multi-field-validation-problem",
	}

	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if filepath.Dir(path) == selfDir {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		content := string(b)
		for _, needle := range needles {
			if strings.Contains(content, needle) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, fmt.Sprintf("%s references %q", rel, needle))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(hits) > 0 {
		t.Errorf("API-010/API-013 fixtures are now referenced outside conformance/drivers/api1 — the pendingCaseIDs rationale (\"no other driver in the repo exercises this fixture\") is stale and must be updated to name the driver that now covers them:\n%s", strings.Join(hits, "\n"))
	}
}

// repoRoot finds the module root (the directory containing go.mod) by
// walking up from this test file's own location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this test file's path")
	}
	dir := filepath.Dir(self)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate module root (go.mod) walking up from this test file")
		}
		dir = parent
	}
}
