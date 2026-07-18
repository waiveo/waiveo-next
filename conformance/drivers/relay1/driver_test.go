package relay1_test

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"reflect"
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
