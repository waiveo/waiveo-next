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
	"SEC-035-invalid-grant-expired-rejected",
	"SEC-050-valid-credential-reset-grant-flow",
	"SEC-066-valid-monotonic-floor-survives-restart",
	"SEC-067-invalid-unauthenticated-time-claim-does-not-advance-floor",
	"SEC-072-valid-console-admission-uid0",
	"SEC-075-invalid-console-verb-not-allowed",
	"SEC-120-invalid-first-boot-claim-outside-window",
	"SEC-121-valid-factory-reset-destroys-key-material",
}

// expectedPending is EMPTY: every frozen case has a live surface to drive.
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
