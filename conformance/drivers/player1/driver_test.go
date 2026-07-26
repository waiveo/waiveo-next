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
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
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

// TestNewInProcessRelayRejectsWildcardBindWithoutDialHost proves
// NewInProcessRelay refuses a wildcard WithBindHost given with no
// WithDialHost, rather than silently defaulting the dial address embedded
// into every formed pairing code and content-origin URL to "0.0.0.0" — an
// address no real player could ever dial.
func TestNewInProcessRelayRejectsWildcardBindWithoutDialHost(t *testing.T) {
	_, err := player1.NewInProcessRelay(player1.WithBindHost("0.0.0.0"))
	if err == nil {
		t.Fatal("NewInProcessRelay(WithBindHost(\"0.0.0.0\")) with no WithDialHost: err = nil, want a configuration error")
	}
}

// TestNewInProcessRelayLoopbackBindStillDefaultsDialHost proves the fix above
// does not disturb the ordinary default (no options at all, or a concrete,
// already-dialable bindHost): dialHost still silently defaults to bindHost.
func TestNewInProcessRelayLoopbackBindStillDefaultsDialHost(t *testing.T) {
	relay, err := player1.NewInProcessRelay()
	if err != nil {
		t.Fatalf("NewInProcessRelay(): %v", err)
	}
	defer relay.Close()
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
// target-capability gate (driver.go's ply057Driveable/
// pendPLY057NotDriveable) end to end against a REAL in-process relay: a
// target that completes every pairing attempt but reports itself unable to
// guarantee drivePLY057's relay-side assertions (player1.PLY057Driveable
// .DriveablePLY057()==false — the exact shape RemoteECPPlayerTarget reports
// in BOTH observation modes, see remotetarget.go's own doc) drives
// PLY-050/055 plus every Phase-2/3 case as usual, but records PLY-057
// PENDING instead of a guaranteed-wrong FAIL.
func TestPlayer1DriverPLY057PendingWhenTargetNotCapable(t *testing.T) {
	relay, err := player1.NewInProcessRelay()
	if err != nil {
		t.Fatalf("NewInProcessRelay: %v", err)
	}
	defer relay.Close()

	rep := player1.Run(notDriveableTarget{}, relay)
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

// notDriveableTarget wraps the REAL VirtualPlayerTarget (so PLY-050/055 still
// genuinely redeem against the live relay, exactly as TestPlayer1DriverGreen
// drives them) but reports itself unable to guarantee drivePLY057's
// relay-side assertions (player1.PLY057Driveable) — the exact shape
// RemoteECPPlayerTarget reports in BOTH observation modes (see
// remotetarget.go's own doc). It exists to prove
// TestPlayer1DriverPLY057PendingWhenTargetNotCapable's PENDING gate without
// needing a real device.
type notDriveableTarget struct {
	player1.VirtualPlayerTarget
}

func (notDriveableTarget) Name() string          { return "virtualplayer-not-driveable-fake" }
func (notDriveableTarget) DriveablePLY057() bool { return false }

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

	// PLY-057 is PENDING against a real device in EITHER observation mode —
	// RemoteECPPlayerTarget.DriveablePLY057() reports false unconditionally
	// (see remotetarget.go's own package doc for why debug-console mode's
	// ability to OBSERVE the mismatch does not make it safe to drive).
	wantPending := []string{"PLY-057-invalid-oob-authentication-mismatch-rejected"}
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

// postHocRejectingTarget models what a REAL player-v3 device actually does
// for PLY-057 (player-v3/source/Pairing.brs): it completes the bootstrap
// POST to /player/v1/pair over TLS with NO peer verification
// (InsecureSkipVerify — mirroring Pairing.brs's peerVerify:false/
// hostVerify:false), so the relay is reached UNCONDITIONALLY, and only AFTER
// that response is in hand does it compute the SHA-256 SPKI digest over the
// connection's own peer certificate and compare it, locally, against the
// pairing code's own fingerprint_commitment (Pairing.brs's PLY-052 check) —
// discarding the response and refusing to proceed on a mismatch, but never
// avoiding the relay round trip itself, exactly like real hardware.
//
// It exists to prove the exact gap driver.go/remotetarget.go's fix closes:
// driving PLY-057's relay-side assertions (Relay.PairCallCount()==0, the
// shared one-time grant surviving a follow-up correct attempt) against a
// target with this — entirely real, entirely contract-compliant
// (PLY-040/041/052/057/058) — architecture produces a guaranteed-wrong FAIL
// regardless of whether the mismatch itself is correctly detected (see
// TestPlayer1DriverPLY057FailsAgainstPostHocTargetNotDeclaringItself), and
// that reporting DriveablePLY057()==false (RemoteECPPlayerTarget's actual,
// fixed behavior) avoids it (see
// TestPlayer1DriverPLY057PendingAgainstPostHocTarget).
type postHocRejectingTarget struct {
	driveable bool
}

func (t postHocRejectingTarget) Name() string { return "post-hoc-rejecting (real player-v3 shape)" }

func (t postHocRejectingTarget) DriveablePLY057() bool { return t.driveable }

func (postHocRejectingTarget) Pair(pairingCode string) player1.PairResult {
	host, port, grantSelector, commitment, err := paircode.Decode(pairingCode)
	if err != nil {
		return player1.PairResult{Rejected: true, Err: err.Error()}
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // models Pairing.brs's disabled bootstrap-fetch verification (PLY-040/041) — the commitment check below is the real, local check.
		},
	}
	body, _ := json.Marshal(map[string]any{
		"hardware_id":    "post-hoc-rejecting-fake",
		"grant_selector": grantSelector,
		"capabilities":   map[string]any{"content_types": []string{"image"}, "player_version": "3.0.0"},
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
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return player1.PairResult{Rejected: true, Err: "no peer certificate observed"}
	}

	// PLY-052: the commitment comparison, run only AFTER the bootstrap POST
	// above already completed and reached the relay — exactly Pairing.brs's
	// own ordering (verify trust_anchors AFTER the response is in hand).
	ok, verr := tlsboot.VerifyCommitmentForCertDER(resp.TLS.PeerCertificates[0].Raw, commitment)
	if verr != nil {
		return player1.PairResult{Rejected: true, Err: verr.Error()}
	}
	if !ok {
		return player1.PairResult{Rejected: true, CommitmentMismatch: true, Err: "fingerprint commitment MISMATCH — refusing to pair (possible MITM)"}
	}

	var out map[string]any
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return player1.PairResult{Rejected: true}
	}
	tok, _ := out["channel_token"].(string)
	return player1.PairResult{Completed: tok != ""}
}

// TestPlayer1DriverPLY057FailsAgainstPostHocTargetNotDeclaringItself proves,
// against the REAL in-process relay, the exact failure mode the
// PLY057Driveable fix exists to prevent: a target that behaves like real
// player-v3 hardware (reaches the relay, then rejects a commitment mismatch
// locally — postHocRejectingTarget) but does NOT declare itself
// not-driveable is, by ply057Driveable's own "not-implementing = assumed
// driveable" default, driven through drivePLY057's full assertion set —
// and legitimately FAILS it, with precisely the two diffs the finding this
// test guards against described: the relay WAS reached
// (Relay.PairCallCount()==1, not 0), and the shared one-time grant was
// genuinely consumed (a follow-up correct-code attempt over the same grant
// no longer redeems, since internal/relay/playerserver's redeem() marks a
// one-time grant redeemed unconditionally, with no notion of commitment at
// all). This is the "zero test coverage" gap the review flagged: before this
// fix, RemoteECPPlayerTarget's debug-console mode reported exactly this
// shape (SupportsCommitmentMismatchDetection()==true while still reaching
// the relay) and so would have produced this same guaranteed-wrong FAIL the
// first time it ran against real hardware.
func TestPlayer1DriverPLY057FailsAgainstPostHocTargetNotDeclaringItself(t *testing.T) {
	relay, err := player1.NewInProcessRelay()
	if err != nil {
		t.Fatalf("NewInProcessRelay: %v", err)
	}
	defer relay.Close()

	rep := player1.Run(postHocRejectingTarget{driveable: true}, relay)
	t.Logf("\n%s", rep.String())

	if !caseFailed(rep, "PLY-057") {
		t.Errorf("expected PLY-057 to FAIL against a target that reaches the relay but claims itself driveable, but it did not; report:\n%s", rep.String())
	}
}

// TestPlayer1DriverPLY057PendingAgainstPostHocTarget proves the fix itself:
// the SAME postHocRejectingTarget, correctly reporting
// DriveablePLY057()==false (RemoteECPPlayerTarget's actual, fixed behavior
// in both of its observation modes), is recorded PENDING for PLY-057 rather
// than driven to the guaranteed-wrong FAIL
// TestPlayer1DriverPLY057FailsAgainstPostHocTargetNotDeclaringItself proves
// — while PLY-050/055 and every Phase-2/3 case still drive and PASS
// normally, since only PLY-057's own relay-side assertions are unsound for
// this target's architecture.
func TestPlayer1DriverPLY057PendingAgainstPostHocTarget(t *testing.T) {
	relay, err := player1.NewInProcessRelay()
	if err != nil {
		t.Fatalf("NewInProcessRelay: %v", err)
	}
	defer relay.Close()

	rep := player1.Run(postHocRejectingTarget{driveable: false}, relay)
	t.Logf("\n%s", rep.String())

	if got := rep.Failed(); len(got) != 0 {
		t.Errorf("no case should FAIL: %v", got)
	}
	wantPending := []string{"PLY-057-invalid-oob-authentication-mismatch-rejected"}
	if !reflect.DeepEqual(rep.PendingIDs(), wantPending) {
		t.Errorf("pending set = %v, want %v", rep.PendingIDs(), wantPending)
	}
}
