package securitymodel1_test

import (
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/conformance/drivers/securitymodel1"
)

// expectedDriven is every security-model corpus case this driver executes
// against live internal/app code — which is every case in the frozen corpus,
// none excepted. A case's presence here says nothing about whether it PASSes:
// see expectedFailing.
var expectedDriven = []string{
	"SEC-034-valid-grant-audit-carries-purpose-and-issued-via",
	"SEC-035-invalid-grant-expired-rejected",
	"SEC-035a-invalid-grant-refusals-on-the-redemption-endpoint",
	"SEC-050-valid-credential-reset-grant-flow",
	"SEC-066-valid-monotonic-floor-survives-restart",
	"SEC-067-invalid-unauthenticated-time-claim-does-not-advance-floor",
	"SEC-067a-invalid-unauthenticated-claim-below-a-verifiable-value",
	"SEC-072-valid-console-admission-uid0",
	"SEC-072a-invalid-console-peer-not-root",
	"SEC-075-invalid-console-verb-not-allowed",
	"SEC-120-invalid-first-boot-claim-outside-window",
	"SEC-120a-invalid-unclaimed-box-claimed-without-the-setup-code",
	"SEC-121-valid-factory-reset-destroys-key-material",
}

// expectedPending is EMPTY on any non-root runner, which is what this repo's
// gates and CI are. SEC-072a is the one case that can become pending at runtime:
// it models a peer whose effective uid is NOT 0 connecting to the real console
// socket, so a conformance process running as uid 0 cannot produce the
// connection the case describes and reports it pending with that reason rather
// than asserting a refusal that did not occur.
//
// The declaration stays rather than being deleted, for the same reason the
// driver's own pendingCaseIDs map does: §10's "no silent caps" rule needs a
// place to record the next undrivable case WITH a reason, and an empty
// declaration is what makes a newly-pending case fail by NAME instead of
// passing quietly.
var expectedPending = []string{}

// expectedFailing maps every driven case that genuinely diverges from its own
// frozen expectation, to why. It is currently EMPTY and stays in place to make
// a NEW divergence loud — a case appearing here without this map being told is
// a regression, and a case leaving it is progress that must be recorded.
var expectedFailing = map[string]string{}

// TestSecurityModel1DriverGreen replays every frozen case against the live
// implementation and asserts the driven/pending/failed sets are exactly as
// declared.
func TestSecurityModel1DriverGreen(t *testing.T) {
	rep := securitymodel1.Run()
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
		t.Errorf("failed set = %v, want exactly the known-divergent set %v\n"+
			"(a case leaving this set is progress — update expectedFailing; a case newly appearing is a regression this test is designed to catch)\n%s",
			gotFailed, wantFailed, rep.String())
	}
}

// TestSecurityModel1ClockFloorHasTeeth proves the driver actually diffs the
// live clock floor's behavior against the frozen expectation rather than
// rubber-stamping it: it corrupts ONLY SEC-066's declared `adopted_time_ms`,
// leaving the live component untouched. A driver with teeth reports FAIL.
func TestSecurityModel1ClockFloorHasTeeth(t *testing.T) {
	cases, err := securitymodel1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	const id = "SEC-066-valid-monotonic-floor-survives-restart"
	broken, ok := cases[id]
	if !ok {
		t.Fatalf("%s missing from the frozen corpus", id)
	}
	expected := deepCopyMap(broken.Expected)
	if expected["adopted_time_ms"] != float64(1752537600000) {
		t.Fatalf("test bug: expected adopted_time_ms 1752537600000 in the real corpus, got %v", expected["adopted_time_ms"])
	}
	expected["adopted_time_ms"] = float64(1700000000000) // the rolled-back host reading; the app must NOT adopt it.
	broken.Expected = expected
	cases[id] = broken

	rep := securitymodel1.RunCases(cases)
	t.Logf("\n%s", rep.String())
	if !caseFailed(rep, id) {
		t.Errorf("expected %s to FAIL against a corrupted (adopted_time_ms:1700000000000) expectation, but it did not; report:\n%s", id, rep.String())
	}
}

// TestSecurityModel1ConsoleVerbHasTeeth is the same proof on the console
// binding's refusal path: corrupting SEC-075's expected `admitted` to true —
// leaving the live dispatcher untouched — must FAIL.
func TestSecurityModel1ConsoleVerbHasTeeth(t *testing.T) {
	cases, err := securitymodel1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	const id = "SEC-075-invalid-console-verb-not-allowed"
	broken, ok := cases[id]
	if !ok {
		t.Fatalf("%s missing from the frozen corpus", id)
	}
	expected := deepCopyMap(broken.Expected)
	if expected["admitted"] != false {
		t.Fatalf("test bug: expected admitted:false in the real corpus, got %v", expected["admitted"])
	}
	expected["admitted"] = true // the live dispatcher refuses an unlisted verb; this is a lie.
	broken.Expected = expected
	cases[id] = broken

	rep := securitymodel1.RunCases(cases)
	t.Logf("\n%s", rep.String())
	if !caseFailed(rep, id) {
		t.Errorf("expected %s to FAIL against a corrupted (admitted:true) expectation, but it did not; report:\n%s", id, rep.String())
	}
}

// TestSecurityModel1FactoryResetHasTeeth is the same proof on the destruction
// path, and it is the one that matters most: SEC-121's whole value is that the
// key material is genuinely gone afterwards. Corrupting the expected
// `data_key_present` to true — leaving the live destruction untouched — must
// FAIL, which proves the driver READS the key's state rather than assuming it.
func TestSecurityModel1FactoryResetHasTeeth(t *testing.T) {
	cases, err := securitymodel1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	const id = "SEC-121-valid-factory-reset-destroys-key-material"
	broken, ok := cases[id]
	if !ok {
		t.Fatalf("%s missing from the frozen corpus", id)
	}
	expected := deepCopyMap(broken.Expected)
	post, ok := expected["post_reset_state"].(map[string]any)
	if !ok {
		t.Fatal("SEC-121 corpus case has no expected.post_reset_state map")
	}
	if post["data_key_present"] != false {
		t.Fatalf("test bug: expected post_reset_state.data_key_present false in the real corpus, got %v", post["data_key_present"])
	}
	post = deepCopyMap(post)
	post["data_key_present"] = true // the reset really destroys it; this is a lie.
	expected["post_reset_state"] = post
	broken.Expected = expected
	cases[id] = broken

	rep := securitymodel1.RunCases(cases)
	t.Logf("\n%s", rep.String())
	if !caseFailed(rep, id) {
		t.Errorf("expected %s to FAIL against a corrupted (data_key_present:true) expectation, but it did not; report:\n%s", id, rep.String())
	}
}

// TestSecurityModel1ProvenanceGateHasTeeth is the same proof on the advance
// gate SEC-067a exists to isolate: corrupting only the expected
// `unauthenticated_claim_advanced_floor` to true — leaving the live floor
// untouched — must FAIL.
func TestSecurityModel1ProvenanceGateHasTeeth(t *testing.T) {
	cases, err := securitymodel1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	const id = "SEC-067a-invalid-unauthenticated-claim-below-a-verifiable-value"
	broken, ok := cases[id]
	if !ok {
		t.Fatalf("%s missing from the frozen corpus", id)
	}
	expected := deepCopyMap(broken.Expected)
	if expected["unauthenticated_claim_advanced_floor"] != false {
		t.Fatalf("test bug: expected unauthenticated_claim_advanced_floor false in the real corpus, got %v", expected["unauthenticated_claim_advanced_floor"])
	}
	expected["unauthenticated_claim_advanced_floor"] = true // the live gate refuses it; this is a lie.
	broken.Expected = expected
	cases[id] = broken

	rep := securitymodel1.RunCases(cases)
	t.Logf("\n%s", rep.String())
	if !caseFailed(rep, id) {
		t.Errorf("expected %s to FAIL against a corrupted (unauthenticated_claim_advanced_floor:true) expectation, but it did not; report:\n%s", id, rep.String())
	}
}

// TestSecurityModel1ProvenanceGateGuardsItsOwnIsolation is the guard on the
// property that makes SEC-067a worth more than the case it supplements: its
// unauthenticated candidate must stay SMALLER than its verifiable one, so the
// two differ only in provenance. Raising the unauthenticated candidate above the
// verifiable value — an INPUT edit, not an expectation edit — collapses the case
// back into the two-variable shape that decides nothing, and the driver must
// report FAIL rather than quietly keep passing.
func TestSecurityModel1ProvenanceGateGuardsItsOwnIsolation(t *testing.T) {
	cases, err := securitymodel1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	const id = "SEC-067a-invalid-unauthenticated-claim-below-a-verifiable-value"
	broken, ok := cases[id]
	if !ok {
		t.Fatalf("%s missing from the frozen corpus", id)
	}
	input := deepCopyMap(broken.Input)
	candidates, ok := input["candidates"].([]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("SEC-067a input carries %v, want a two-candidate array", input["candidates"])
	}
	unauth, ok := candidates[0].(map[string]any)
	if !ok {
		t.Fatal("SEC-067a's first candidate is not an object")
	}
	unauth = deepCopyMap(unauth)
	unauth["ts_ms"] = float64(1900000000000) // far ABOVE the verifiable value: isolation lost.
	input["candidates"] = []any{unauth, candidates[1]}
	broken.Input = input
	cases[id] = broken

	rep := securitymodel1.RunCases(cases)
	t.Logf("\n%s", rep.String())
	if !caseFailed(rep, id) {
		t.Errorf("expected %s to FAIL once its unauthenticated candidate outgrew its verifiable one (the case no longer isolates provenance), but it did not; report:\n%s", id, rep.String())
	}
}

// TestSecurityModel1UnclaimedBoxHasTeeth is the same proof on SEC-120's own
// clause: corrupting SEC-120a's second attempt (a wrong code against an
// UNCLAIMED box) to an expected `claimed: true` — leaving the live handler
// untouched — must FAIL.
func TestSecurityModel1UnclaimedBoxHasTeeth(t *testing.T) {
	cases, err := securitymodel1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	const id = "SEC-120a-invalid-unclaimed-box-claimed-without-the-setup-code"
	broken, ok := cases[id]
	if !ok {
		t.Fatalf("%s missing from the frozen corpus", id)
	}
	expected := deepCopyMap(broken.Expected)
	attempts, ok := expected["claim_attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("SEC-120a expects %v, want a two-attempt array", expected["claim_attempts"])
	}
	second, ok := attempts[1].(map[string]any)
	if !ok {
		t.Fatal("SEC-120a's second expected attempt is not an object")
	}
	if second["claimed"] != false {
		t.Fatalf("test bug: expected claim_attempts[1].claimed false in the real corpus, got %v", second["claimed"])
	}
	second = deepCopyMap(second)
	second["claimed"] = true // the live handler refuses a wrong code; this is a lie.
	expected["claim_attempts"] = []any{attempts[0], second}
	broken.Expected = expected
	cases[id] = broken

	rep := securitymodel1.RunCases(cases)
	t.Logf("\n%s", rep.String())
	if !caseFailed(rep, id) {
		t.Errorf("expected %s to FAIL against a corrupted (claim_attempts[1].claimed:true) expectation, but it did not; report:\n%s", id, rep.String())
	}
}

// TestSecurityModel1WireRefusalCodeHasTeeth is the proof that SEC-035a reads the
// refusal code off the RESPONSE rather than off a mapping of the driver's own:
// corrupting the expected code for the expired attempt — leaving the live
// handler untouched — must FAIL.
func TestSecurityModel1WireRefusalCodeHasTeeth(t *testing.T) {
	cases, err := securitymodel1.LoadCorpus()
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	const id = "SEC-035a-invalid-grant-refusals-on-the-redemption-endpoint"
	broken, ok := cases[id]
	if !ok {
		t.Fatalf("%s missing from the frozen corpus", id)
	}
	expected := deepCopyMap(broken.Expected)
	attempts, ok := expected["attempts"].([]any)
	if !ok || len(attempts) != 3 {
		t.Fatalf("SEC-035a expects %v, want a three-attempt array", expected["attempts"])
	}
	first, ok := attempts[0].(map[string]any)
	if !ok {
		t.Fatal("SEC-035a's first expected attempt is not an object")
	}
	problem, ok := first["error"].(map[string]any)
	if !ok || problem["code"] != "GRANT_EXPIRED" {
		t.Fatalf("test bug: expected attempts[0].error.code GRANT_EXPIRED in the real corpus, got %v", first["error"])
	}
	first = deepCopyMap(first)
	first["error"] = map[string]any{"code": "GRANT_ALREADY_REDEEMED"} // the wire says GRANT_EXPIRED; this is a lie.
	expected["attempts"] = []any{first, attempts[1], attempts[2]}
	broken.Expected = expected
	cases[id] = broken

	rep := securitymodel1.RunCases(cases)
	t.Logf("\n%s", rep.String())
	if !caseFailed(rep, id) {
		t.Errorf("expected %s to FAIL against a corrupted (attempts[0].error.code) expectation, but it did not; report:\n%s", id, rep.String())
	}
}

// TestSecurityModel1CorpusFullyAccountedFor extends "no silent caps" to any
// case nobody has triaged: every case_id present in the frozen corpus DIRECTORY
// must be exactly Driven() ∪ PendingIDs(). A newly frozen case that nobody
// wired into the driver fails this test by name.
func TestSecurityModel1CorpusFullyAccountedFor(t *testing.T) {
	rep := securitymodel1.Run()
	cases, err := securitymodel1.LoadCorpus()
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
		t.Errorf("corpus case(s) frozen under conformance/corpora/security-model but NEITHER driven NOR pending: %v", uncovered)
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

func caseFailed(rep report.Report, id string) bool {
	for _, c := range rep.Cases {
		if c.CaseID == id {
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
