package api1_test

import (
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/api1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// expectedDriven is every api-1 corpus case this driver drives against live,
// HTTP-mounted handlers — which is now every case in the frozen corpus, none
// excepted. Almost all are driven against api.New; API-063 alone is driven
// across the relay/app SEAM (a real telemetry.Buffer record pushed through the
// live POST /telemetry/v1/push ingest), because that requirement's subject is
// what survives BETWEEN components rather than what one handler returns. A
// case's presence here says nothing about whether it currently PASSes: see
// expectedFailing.
var expectedDriven = []string{
	"API-010-valid-simple-problem",
	"API-013-valid-multi-field-validation-problem",
	"API-022-invalid-delete-org-scope-node-without-if-match",
	"API-022-invalid-if-match-missing",
	"API-023-invalid-if-match-conflict",
	"API-032-valid-pagination-roundtrip",
	"API-034-valid-identity-resource-create-then-select-and-page",
	"API-035-invalid-cursor-foreign-resource",
	"API-044-valid-selector-scope-subtree",
	"API-045-invalid-selector-malformed",
	"API-052-valid-idempotency-replay",
	"API-053-invalid-idempotency-key-reused-different-body",
	"API-063-valid-trace-id-propagated-into-durable-event",
	"API-101-invalid-external-id-cross-kind-conflict",
	"API-102-invalid-external-id-conflict",
	"API-111-valid-bulk-enable-202-job",
	"API-121-valid-export-workspace-job",
}

// expectedPending is EMPTY: every api-1 corpus case now has a mounted route to
// drive. Its one entry was API-121, pending only because api.New's mux had no
// /api/v1/workspace/export handler; that route exists now
// (internal/app/api/workspace.go) and the case is driven above.
//
// The variable stays rather than being deleted, for the same reason the
// driver's own pendingCaseIDs map does: §10's "no silent caps" rule needs a
// place to record the next undrivable case WITH a reason, and TestAPI1DriverGreen
// asserts the driver's pending set is exactly this one — an empty declaration
// is what makes a newly-pending case fail by name instead of passing quietly.
var expectedPending = []string{}

// expectedFailing maps every corpus case this driver drives that genuinely
// diverges from its own frozen expectation, to why. These are NOT driver
// bugs: each is a confirmed corpus-vs-shipped-code mismatch this driver
// surfaced by mounting the real api.New handler instead of the convention
// libraries directly (the 2026-07-26 reconciliation's finding that no
// conformance driver touched a single internal/app/ package). A case in this
// set moving to PASS is progress — update this map when it does, so this
// test keeps proving the driver has teeth on whatever remains broken, rather
// than silently going green over a still-broken case, or silently going red
// over a NEW regression this map hasn't been told about yet.
//
// It is currently EMPTY. Its one entry was API-111, whose Job `created_by`
// could not match the corpus while the live handler stamped that field from a
// fixed constant standing in for a deferred auth model. The handler now stamps
// it from the real authenticated principal, so the driver seeds that principal
// with the case's own pinned `created_by` — the same drive-it-from-the-fixture
// technique it already applies to the clock and the id source, and exactly what
// contracts/api-1.md's Conformance notes sanction ("cases that need a principal
// treat one as a given, opaque input"). The map itself stays: its job is to make
// a NEW divergence loud, not to record any particular one.
var expectedFailing = map[string]string{}

// TestAPI1DriverGreen replays every frozen api-1 corpus case against the LIVE
// api.New handler: the driven/pending sets are exactly as declared, every
// driven-and-not-in-expectedFailing case PASSes, and every case in
// expectedFailing actually FAILs (proving this test would catch either a
// silent regression on a previously-passing case, or a new gap papered over
// by quietly declaring it "expected to fail" without it actually failing).
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

	gotFailed := rep.Failed()
	sort.Strings(gotFailed)
	wantFailed := make([]string, 0, len(expectedFailing))
	for id := range expectedFailing {
		wantFailed = append(wantFailed, id)
	}
	sort.Strings(wantFailed)
	if !equalStrings(gotFailed, wantFailed) {
		t.Errorf("failed set = %v, want exactly the known-divergent set %v\n(a case leaving this set is progress — update expectedFailing; "+
			"a case newly appearing is a regression this test is designed to catch)\n%s", gotFailed, wantFailed, rep.String())
	}
}

// TestAPI1DriverHasTeeth proves the driver actually diffs against the live
// implementation rather than rubber-stamping every case: it takes the real
// API-023 case (whose corpus `expected.body.current_revision` truthfully says
// 3, the resource's real current revision after this driver's own seeding)
// and corrupts ONLY the declared expectation to 999, leaving the live
// handler's real, correct behavior untouched. A driver with teeth reports
// this case FAIL.
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
	brokenBody = deepCopyMap(brokenBody)
	brokenBody["current_revision"] = float64(999) // the real resource is at revision 3; this is a lie.
	brokenExpected["body"] = brokenBody
	broken.Expected = brokenExpected
	cases["API-023-invalid-if-match-conflict"] = broken

	rep := api1.RunCases(cases)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "API-023-invalid-if-match-conflict") {
		t.Errorf("expected API-023-invalid-if-match-conflict to FAIL against a corrupted (current_revision:999) expectation, but it did not; report:\n%s", rep.String())
	}
}

// TestAPI1SelectorParseErrorContentTypeHasTeeth proves the malformed-selector
// case (API-045) actually checks the corpus-pinned `content_type` field
// against a genuine HTTP response, rather than silently never reading it. It
// corrupts ONLY the declared `expected.content_type` from the real
// "application/problem+json" to a wrong value, leaving the live handler's
// real Content-Type header untouched. A driver with teeth reports this case
// FAIL.
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
}

// TestAPI1SelectorParseErrorBodyTypeHasTeeth is TestAPI1SelectorParseErrorContentTypeHasTeeth
// for `expected.body.type`.
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
