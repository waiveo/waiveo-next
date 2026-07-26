package api1_test

import (
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/api1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// expectedDriven is every api-1 corpus case this driver drives against the
// LIVE, HTTP-mounted api.New handler — every case in the frozen corpus except
// API-121 (see expectedPending). A case's presence here says nothing about
// whether it currently PASSes: see expectedFailing.
var expectedDriven = []string{
	"API-010-valid-simple-problem",
	"API-013-valid-multi-field-validation-problem",
	"API-022-invalid-if-match-missing",
	"API-023-invalid-if-match-conflict",
	"API-032-valid-pagination-roundtrip",
	"API-044-valid-selector-scope-subtree",
	"API-045-invalid-selector-malformed",
	"API-052-valid-idempotency-replay",
	"API-053-invalid-idempotency-key-reused-different-body",
	"API-102-invalid-external-id-conflict",
	"API-111-valid-bulk-enable-202-job",
}

// expectedPending is the one api-1 corpus case with no mounted route to
// drive: api.New's mux has no /api/v1/workspace/export handler (§10 "no
// silent caps" — recorded PENDING with a reason, never silently absent).
var expectedPending = []string{
	"API-121-valid-export-workspace-job",
}

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
var expectedFailing = map[string]string{
	"API-010-valid-simple-problem": "live rs.notFound (api.go) emits the resource-kind-agnostic detail " +
		"\"No resource exists at this identifier.\" for every kind's 404; the corpus pins the scope-node-specific " +
		"\"No scope node exists with this identifier.\" — a wording mismatch, not a missing route.",
	"API-013-valid-multi-field-validation-problem": "status/code/message mismatch: the corpus pins 400/ENUM_MISMATCH/TOO_SHORT " +
		"for an invalid kind + empty name; the live scope-node validator (datamodel.BuildScopeTree) always emits 422 and has no " +
		"TOO_SHORT check on `name` at all (only SCOPE_NODE_KIND_INVALID for kind) — the corpus predates (or was never reconciled " +
		"with) the 422-uniformly decision the later scheduling-core plan settled on.",
	"API-032-valid-pagination-roundtrip": "the corpus pins the UNSCOPED bare-ULID cursor form for the automations list " +
		"(\"...pinned to the unscoped bare-ULID cursor form\", ADR embedded in the old driver's own comment); the live " +
		"automations list (api.go's automationsConfig.resourceType=\"automations\") scopes every cursor uniformly like every " +
		"other resource — there is no unscoped list route in the shipped code, so the corpus's second-page request (built with " +
		"the bare-ULID cursor) is rejected 400/CURSOR_INVALID by the live handler instead of returning page 2.",
	"API-052-valid-idempotency-replay": "the fixture's create body is `{kind:\"site\",name:\"New Site\"}` with no tz/lat/long; " +
		"DAT-031 requires a site to declare all three, so the live store rejects the very first request 422/VALIDATION_FAILED " +
		"instead of creating it 201 — the Idempotency-Key fixtures were never reconciled with the site-geo-required rule.",
	"API-053-invalid-idempotency-key-reused-different-body": "same DAT-031 site-geo-required mismatch as API-052: the first " +
		"request in the case is rejected 422 instead of succeeding 201.",
	"API-102-invalid-external-id-conflict": "wording mismatch only: the live apihttp.CheckExternalIDUnique detail names the " +
		"resource by resourceConfig.resourceType (\"scope-nodes\", the generic tag every scope-node kind shares in api.go); the " +
		"corpus pins the row's own `kind` (\"screen\"). The conflict IS correctly detected (400/EXTERNAL_ID_CONFLICT, write not " +
		"executed) — only the human-readable detail string differs.",
	"API-111-valid-bulk-enable-202-job": "two of the Job resource's fields can never equal the corpus's arbitrary pinned " +
		"values: the live handler mints the Job id via ulid.New() (automations.go bulkEnableExec) with no injection seam, and " +
		"stamps created_by from the fixed pocPrincipal constant (auth is deferred — api.go's own doc comment). created_at DOES " +
		"match (this driver sets the harness clock to the case's own pinned created_at) — confirming those are the only two " +
		"genuine divergences, not a systemic Job-shape bug.",
}

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
