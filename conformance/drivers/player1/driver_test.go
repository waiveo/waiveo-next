package player1_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/player1"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/shared/paircode"
)

// expectedPending is the enumerated PENDING set the player1 driver must
// report — the honesty anchor: if a future task implements one of these
// features but forgets to move its case from PENDING to driven, this test
// fails, and if it drives a case it should not yet, this test also fails.
//
// Task 5 drives every remaining Phase-2 case (PLY-101/130/136/155), so this
// set is now empty — full player/1 conformance, zero pending.
var expectedPending []string

// expectedDriven is the full set of cases the driver exercises against the
// live stack: the first-photon subset (PLY-050/055/057) plus every Phase-2
// case Task 5 wires in (PLY-101/130/136/155) — the entire frozen player-1
// corpus.
var expectedDriven = []string{
	"PLY-050-valid-pairing-happy-path-tofu-same-network",
	"PLY-055-valid-cross-vlan-manual-entry-pairing-code-commitment",
	"PLY-057-invalid-oob-authentication-mismatch-rejected",
	"PLY-101-valid-lease-preemption-interrupt-now",
	"PLY-130-valid-server-moved-relocate-never-wipe",
	"PLY-136-valid-token-revoked-reconnect-clears-token-only",
	"PLY-155-valid-power-schedule-interaction",
}

// TestPlayer1DriverGreen boots the live in-process feeder+relay and runs the
// player/1 driver against the first-photon virtual-player target: every
// driven case must PASS and the PENDING set must be exactly the enumerated
// list.
func TestPlayer1DriverGreen(t *testing.T) {
	relay, err := player1.NewInProcessRelay()
	if err != nil {
		t.Fatalf("NewInProcessRelay: %v", err)
	}
	defer relay.Close()

	rep := player1.Run(player1.NewVirtualPlayerTarget(), relay)
	t.Logf("\n%s", rep.String())

	if !reflect.DeepEqual(rep.Driven(), expectedDriven) {
		t.Errorf("driven set = %v, want %v", rep.Driven(), expectedDriven)
	}
	if got := rep.Failed(); len(got) != 0 {
		t.Errorf("driven cases FAILED against the live stack: %v", got)
	}
	if !reflect.DeepEqual(rep.PendingIDs(), expectedPending) {
		t.Errorf("pending set = %v, want %v (a feature moving from Phase-2 to driven must update this)", rep.PendingIDs(), expectedPending)
	}
	if !rep.OK() {
		t.Errorf("report not OK:\n%s", rep.String())
	}
}

// TestPlayer1DriverHasTeeth proves the driver can FAIL: it points the SAME
// driver at a deliberately-broken target that skips the commitment check and
// redeems any pairing code — the exact MITM vulnerability PLY-057 forbids.
// The driver MUST report PLY-057 as FAIL, not PASS. A conformance harness
// that cannot fail is worthless.
func TestPlayer1DriverHasTeeth(t *testing.T) {
	relay, err := player1.NewInProcessRelay()
	if err != nil {
		t.Fatalf("NewInProcessRelay: %v", err)
	}
	defer relay.Close()

	rep := player1.Run(brokenNoPinTarget{}, relay)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "PLY-057") {
		t.Errorf("expected PLY-057 to FAIL against a commitment-skipping target, but it did not; report:\n%s", rep.String())
	}
	if rep.OK() {
		t.Errorf("driver reported OK against a broken target — the oracle has no teeth")
	}
}

// TestPlayer1CorpusFullyAccountedFor extends the §10 "no silent caps"
// guarantee to cases nobody has wired into the driver yet: it enumerates
// every case_id actually present in the frozen player-1 corpus DIRECTORY
// (independent of expectedDriven/expectedPending above) and asserts that set
// is EXACTLY Driven() ∪ PendingIDs(). Freezing a new corpus/player-1/*.json
// case without triaging it (driving it, or adding it to the driver's Pending
// list with a reason) fails this test by name, instead of silently shipping
// uncovered.
func TestPlayer1CorpusFullyAccountedFor(t *testing.T) {
	relay, err := player1.NewInProcessRelay()
	if err != nil {
		t.Fatalf("NewInProcessRelay: %v", err)
	}
	defer relay.Close()

	rep := player1.Run(player1.NewVirtualPlayerTarget(), relay)

	cases, err := player1.LoadCorpus()
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
		t.Errorf("corpus case(s) frozen under conformance/corpora/player-1 but NEITHER driven NOR pending in the player1 driver — triage: drive it in Run, or mark it Pending with a reason: %v", uncovered)
	}
	if len(phantom) > 0 {
		t.Errorf("driver Driven()/PendingIDs() name case id(s) that do not exist in the frozen player-1 corpus (phantom id, or corpus file renamed/removed): %v", phantom)
	}
}

// TestPlayer1DriverPLY057PendingWhenTargetNotCapable proves Run's
// target-capability gate (driver.go's commitmentMismatchCapable/
// pendPLY057NotCapable) end to end against a REAL in-process relay: a target
// that completes every pairing attempt but reports itself unable to detect a
// commitment mismatch (player1.CommitmentMismatchCapable
// .SupportsCommitmentMismatchDetection()==false — the exact shape
// RemoteECPPlayerTarget's relay-observed mode reports, see remotetarget.go's
// own doc) drives PLY-050/055 plus every Phase-2/3 case as usual, but records
// PLY-057 PENDING instead of a guaranteed-wrong FAIL.
func TestPlayer1DriverPLY057PendingWhenTargetNotCapable(t *testing.T) {
	relay, err := player1.NewInProcessRelay()
	if err != nil {
		t.Fatalf("NewInProcessRelay: %v", err)
	}
	defer relay.Close()

	rep := player1.Run(notCapableTarget{}, relay)
	t.Logf("\n%s", rep.String())

	wantPending := []string{"PLY-057-invalid-oob-authentication-mismatch-rejected"}
	if !reflect.DeepEqual(rep.PendingIDs(), wantPending) {
		t.Errorf("pending set = %v, want %v", rep.PendingIDs(), wantPending)
	}
	wantDriven := []string{
		"PLY-050-valid-pairing-happy-path-tofu-same-network",
		"PLY-055-valid-cross-vlan-manual-entry-pairing-code-commitment",
		"PLY-101-valid-lease-preemption-interrupt-now",
		"PLY-130-valid-server-moved-relocate-never-wipe",
		"PLY-136-valid-token-revoked-reconnect-clears-token-only",
		"PLY-155-valid-power-schedule-interaction",
	}
	if !reflect.DeepEqual(rep.Driven(), wantDriven) {
		t.Errorf("driven set = %v, want %v", rep.Driven(), wantDriven)
	}
	if got := rep.Failed(); len(got) != 0 {
		t.Errorf("no case should FAIL against an always-completing target: %v", got)
	}
}

// notCapableTarget wraps the REAL VirtualPlayerTarget (so PLY-050/055 still
// genuinely redeem against the live relay, exactly as TestPlayer1DriverGreen
// drives them) but reports itself unable to distinguish an OOB
// commitment-mismatch rejection from an ordinary successful pairing
// (player1.CommitmentMismatchCapable) — the exact shape RemoteECPPlayerTarget's
// relay-observed mode reports (see remotetarget.go's own doc). It exists to
// prove TestPlayer1DriverPLY057PendingWhenTargetNotCapable's PENDING gate
// without needing a real device.
type notCapableTarget struct {
	player1.VirtualPlayerTarget
}

func (notCapableTarget) Name() string                              { return "virtualplayer-not-capable-fake" }
func (notCapableTarget) SupportsCommitmentMismatchDetection() bool { return false }

// TestPlayer1DriverAgainstRealRokuTarget drives the ENTIRE player/1 corpus
// against an actual on-LAN player-v3 device instead of VirtualPlayerTarget —
// the remote-target mode WAIVEO_CONF_PLAYER_TARGET=roku enables (see
// player1.RemoteEnvFromEnv's own doc for the full env var list, and
// remotetarget.go's package doc for per-case remote-driveability). CI never
// sets WAIVEO_CONF_PLAYER_TARGET (see .github/workflows/pr-tier.yml and
// merge-tier.yml's plain `go test ./conformance/drivers/...`), so this test
// SKIPs there — TestPlayer1DriverGreen above (hardcoded to
// VirtualPlayerTarget, independent of any env var) is what CI's PASS/PENDING
// set is actually pinned to. This test is the operator-driven path onto real
// hardware: point WAIVEO_CONF_PLAYER_ROKU_HOST/WAIVEO_CONF_RELAY_DIAL_HOST at
// a signage Roku on the same LAN as the box running `go test` and rerun it.
func TestPlayer1DriverAgainstRealRokuTarget(t *testing.T) {
	env, enabled, err := player1.RemoteEnvFromEnv()
	if err != nil {
		t.Fatalf("RemoteEnvFromEnv: %v", err)
	}
	if !enabled {
		t.Skip("WAIVEO_CONF_PLAYER_TARGET != \"roku\": remote-target mode not requested — this is the CI-default path (nothing here runs in CI)")
	}

	relay, err := player1.NewInProcessRelay(
		player1.WithBindHost(env.RelayBindHost),
		player1.WithDialHost(env.RelayDialHost),
	)
	if err != nil {
		t.Fatalf("NewInProcessRelay(bind=%q, dial=%q): %v", env.RelayBindHost, env.RelayDialHost, err)
	}
	defer relay.Close()

	target := player1.NewRemoteECPPlayerTarget(env.Target, relay)
	rep := player1.Run(target, relay)
	t.Logf("\n%s", rep.String())

	if got := rep.Failed(); len(got) != 0 {
		t.Errorf("driven case(s) FAILED against the real device %q: %v", env.Target.Host, got)
	}

	var wantPending []string
	if !target.SupportsCommitmentMismatchDetection() {
		// Relay-observed mode (WAIVEO_CONF_PLAYER_ROKU_DEBUG_PORT=off): PLY-057
		// cannot be distinguished from a successful pairing over the wire alone
		// — see player1.CommitmentMismatchCapable's own doc.
		wantPending = []string{"PLY-057-invalid-oob-authentication-mismatch-rejected"}
	}
	if !reflect.DeepEqual(rep.PendingIDs(), wantPending) {
		t.Errorf("pending set = %v, want %v", rep.PendingIDs(), wantPending)
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

// brokenNoPinTarget is a deliberately-vulnerable PlayerTarget: it decodes the
// pairing code and redeems the grant against the relay over TLS with NO
// commitment/pin check at all — it will happily complete against a
// substituted (MITM) certificate. It exists only to prove the driver's
// PLY-057 assertion has teeth.
type brokenNoPinTarget struct{}

func (brokenNoPinTarget) Name() string { return "broken-no-pin" }

func (brokenNoPinTarget) Pair(pairingCode string) player1.PairResult {
	host, port, grantSelector, _, err := paircode.Decode(pairingCode)
	if err != nil {
		return player1.PairResult{Rejected: true, Err: err.Error()}
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // deliberately-broken target: no pin check (the vulnerability PLY-057 forbids), test-only teeth strawman.
		},
	}
	body, _ := json.Marshal(map[string]any{
		"hardware_id":    "broken-no-pin",
		"grant_selector": grantSelector,
		"capabilities":   map[string]any{"content_types": []string{"image"}, "player_version": "0"},
	})
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	resp, err := client.Post("https://"+addr+"/player/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		return player1.PairResult{Rejected: true, Err: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return player1.PairResult{Rejected: true, Err: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	var out map[string]any
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return player1.PairResult{Rejected: true}
	}
	tok, _ := out["channel_token"].(string)
	return player1.PairResult{Completed: tok != ""}
}
