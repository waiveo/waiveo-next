package relay1_test

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/relay1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

var expectedDriven = []string{
	"REL-010-valid-fresh-enroll",
	"REL-070-valid-generation-reapply-idempotent-noop",
	"REL-071-invalid-wrong-peer-key-snapshot-rejected",
}

var expectedPending = []string{
	"REL-020-valid-re-enroll-after-cert-expiry",
	"REL-022-invalid-re-enroll-superseded-cert",
	"REL-027-invalid-re-enroll-pop-signature-invalid",
	"REL-030-valid-hello-negotiate-channel-binding",
	"REL-056-valid-generation-apply-atomic-swap",
	"REL-061-valid-preempt-priority-screen-program-offline",
	"REL-090-valid-telemetry-overflow-loss-marker",
	"REL-094-valid-telemetry-latest-only-heartbeat-superseded",
	"REL-110-valid-device-candidate-and-command",
	"REL-133-valid-clock-hint-bounded",
	"REL-136-valid-coldboot-skew-tolerant-connect",
}

// TestRelay1DriverGreen boots the live in-process feeder and runs the relay/1
// driver against the real relay client: every driven case PASSes and the
// PENDING set is exactly the enumerated list.
func TestRelay1DriverGreen(t *testing.T) {
	feeder, err := relay1.NewInProcessFeeder()
	if err != nil {
		t.Fatalf("NewInProcessFeeder: %v", err)
	}
	defer feeder.Close()

	rep := relay1.Run(relay1.NewRealRelayClient(), feeder)
	t.Logf("\n%s", rep.String())

	if !reflect.DeepEqual(rep.Driven(), expectedDriven) {
		t.Errorf("driven set = %v, want %v", rep.Driven(), expectedDriven)
	}
	if got := rep.Failed(); len(got) != 0 {
		t.Errorf("driven cases FAILED against the live stack: %v", got)
	}
	if !reflect.DeepEqual(rep.PendingIDs(), expectedPending) {
		t.Errorf("pending set = %v, want %v (a feature moving to driven must update this)", rep.PendingIDs(), expectedPending)
	}
	if !rep.OK() {
		t.Errorf("report not OK:\n%s", rep.String())
	}
}

// TestRelay1DriverHasTeeth points the SAME driver at a deliberately-broken
// relay client that applies snapshots WITHOUT verifying their signature — the
// exact vulnerability REL-071 forbids. The driver MUST report REL-071 as
// FAIL: a broken relay that accepts an impostor-signed snapshot advances its
// last-applied generation, which the driver detects.
func TestRelay1DriverHasTeeth(t *testing.T) {
	feeder, err := relay1.NewInProcessFeeder()
	if err != nil {
		t.Fatalf("NewInProcessFeeder: %v", err)
	}
	defer feeder.Close()

	rep := relay1.Run(brokenSkipVerifyClient{}, feeder)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "REL-071") {
		t.Errorf("expected REL-071 to FAIL against a signature-skipping relay, but it did not; report:\n%s", rep.String())
	}
	if rep.OK() {
		t.Errorf("driver reported OK against a broken client — the oracle has no teeth")
	}
}

// TestRelay1CorpusFullyAccountedFor extends the §10 "no silent caps"
// guarantee to cases nobody has wired into the driver yet: it enumerates
// every case_id actually present in the frozen relay-1 corpus DIRECTORY
// (independent of expectedDriven/expectedPending above) and asserts that set
// is EXACTLY Driven() ∪ PendingIDs(). Freezing a new corpus/relay-1/*.json
// case without triaging it (driving it, or adding it to the driver's Pending
// list with a reason) fails this test by name, instead of silently shipping
// uncovered.
func TestRelay1CorpusFullyAccountedFor(t *testing.T) {
	feeder, err := relay1.NewInProcessFeeder()
	if err != nil {
		t.Fatalf("NewInProcessFeeder: %v", err)
	}
	defer feeder.Close()

	rep := relay1.Run(relay1.NewRealRelayClient(), feeder)

	cases, err := relay1.LoadCorpus()
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
		t.Errorf("corpus case(s) frozen under conformance/corpora/relay-1 but NEITHER driven NOR pending in the relay1 driver — triage: drive it in Run, or mark it Pending with a reason: %v", uncovered)
	}
	if len(phantom) > 0 {
		t.Errorf("driver Driven()/PendingIDs() name case id(s) that do not exist in the frozen relay-1 corpus (phantom id, or corpus file renamed/removed): %v", phantom)
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

// brokenSkipVerifyClient enrolls normally (so it holds an anchor and can pull)
// but its Pull blindly persists whatever generation/hash the feeder serves,
// with NO signature verification — accepting an impostor snapshot. It exists
// only to prove the REL-071 assertion has teeth.
type brokenSkipVerifyClient struct{}

func (brokenSkipVerifyClient) Name() string { return "broken-skip-verify" }

func (brokenSkipVerifyClient) Enroll(feederBaseURL string, store *identity.Store) error {
	return relayenroll.Run(feederBaseURL, store)
}

func (brokenSkipVerifyClient) Pull(feederBaseURL string, store *identity.Store) (desiredstate.Applied, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // broken teeth strawman, test-only.
		},
	}
	resp, err := client.Get(feederBaseURL + "/state/pull")
	if err != nil {
		return desiredstate.Applied{}, err
	}
	defer resp.Body.Close()
	var body wire.StateSnapshotBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return desiredstate.Applied{}, err
	}
	// The vulnerability: apply without verifying the signature.
	if err := store.SetLastAppliedGeneration(body.Generation, body.Hash); err != nil {
		return desiredstate.Applied{}, err
	}
	return desiredstate.Applied{Generation: body.Generation}, nil
}
