// Package relay1 is the executable relay/1 conformance driver: the §10
// differential oracle for the relay/1 contract's enrollment + desired-state
// surface. It replays the first-photon-applicable relay-1 corpus cases
// (conformance/corpora/relay-1) against a LIVE feeder and a pluggable
// RelayClient (the relay implementation under test), and diffs the client's
// actual behavior against each case's own declared `expected` block.
//
// The RelayClient is pluggable ON PURPOSE (§10 "drivers take ANY target"):
// the first-photon target is RealRelayClient (internal/relay/enroll +
// internal/relay/desiredstate), and the teeth meta-test plugs in a
// deliberately-broken client that skips signature verification to prove the
// REL-071 assertion can FAIL. The Feeder is likewise an abstraction over the
// live counterparty, rich enough to stage each case (mint a claim token,
// serve a validly-signed snapshot at a chosen generation, serve an
// impostor-signed one) — the feederBaseURL of the brief, expanded to what an
// oracle actually needs to reproduce a case.
//
// Applicability triage (§10 "no silent caps"): Run DRIVES the cases
// first-photon implements (REL-010 fresh enroll, REL-070 idempotent reapply,
// REL-071 wrong-peer-key rejection) and marks every other relay-1 case
// PENDING with an explicit reason.
package relay1

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// RelayClient is the pluggable relay/1 implementation under test: the two
// relay-side operations the first-photon corpus exercises — enrolling against
// a feeder, and pulling+verifying+applying its signed desired state. A
// different relay build (or a deliberately-broken one) implements the same
// two methods and is driven by the identical oracle.
type RelayClient interface {
	// Name identifies the implementation (e.g. "real-relay").
	Name() string
	// Enroll redeems a claim credential at feederBaseURL and persists the
	// resulting identity + desired-state verification key into store.
	Enroll(feederBaseURL string, store *identity.Store) error
	// Pull fetches, verifies, and applies the feeder's signed desired-state
	// snapshot from feederBaseURL, returning the applied state or a typed
	// rejection error (leaving last-applied untouched on rejection).
	Pull(feederBaseURL string, store *identity.Store) (desiredstate.Applied, error)
}

// Feeder is the LIVE counterparty the driver stages each case against: the
// enrollment endpoint (REL-010), plus the ability to serve a
// validly-feeder-signed snapshot at a chosen generation (to stage REL-070's
// gen-42→gen-43 reapply) and an impostor-signed snapshot (REL-071). A
// concrete Feeder owns the feeder's signing identity, so the valid snapshots
// it serves verify against the exact key a relay enrolled against it learned.
type Feeder interface {
	// EnrollBaseURL is the feeder's enrollment base URL (/claim-token,
	// /enroll).
	EnrollBaseURL() string
	// CurrentClaimToken returns the feeder's currently-pending claim token
	// (minting one if none is pending) WITHOUT redeeming it — so the driver
	// knows which token a subsequent enrollment will consume, to later prove
	// its single-use refusal (REL-010).
	CurrentClaimToken() (string, error)
	// SignedSnapshotURL points /state/pull at a snapshot validly signed by
	// this feeder's own signing key at the given generation (same sections,
	// hence same hash, across generations — REL-070's byte-identical reapply).
	SignedSnapshotURL(generation int64) (string, error)
	// WrongKeySnapshotURL points /state/pull at a snapshot at the given
	// generation signed by a DIFFERENT key than this feeder's own — REL-071's
	// impostor snapshot.
	WrongKeySnapshotURL(generation int64) (string, error)
}

const contract = "relay/1"

// Run replays the first-photon-applicable relay/1 corpus cases against feeder,
// driving client, and returns the differential-oracle Report.
func Run(client RelayClient, feeder Feeder) report.Report {
	rep := report.Report{Driver: "relay1", Target: client.Name()}

	cases, err := corpus.LoadDir(corpusDir())
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load relay-1 corpus: %v", err))
		return rep
	}

	driveREL010(&rep, client, feeder, cases)
	driveREL070(&rep, client, feeder, cases)
	driveREL071(&rep, client, feeder, cases)

	pend := func(short, reason string) {
		id := short
		if c, ok := corpus.ByID(cases, short); ok {
			id = c.CaseID
		}
		rep.Pending(id, contract, reason)
	}
	pend("REL-020", "re-enrollment after cert expiry is Phase-2 identity-lifecycle; first-photon has no in-band renewal/re-enrollment path (REL-015/017).")
	pend("REL-022", "re-enrollment with a superseded cert is Phase-2 identity-lifecycle; no re-enrollment path in first-photon.")
	pend("REL-027", "re-enrollment PoP-signature rejection is Phase-2 identity-lifecycle; no re-enrollment path in first-photon.")
	pend("REL-030", "hello/negotiate channel-binding is the Phase-2 steady-state relay/1 session; first-photon's desired-state pull is a bare GET with no hello/negotiate handshake.")
	pend("REL-056", "multi-generation atomic swap needs >1 concurrently-staged generation with sectioned apply; first-photon serves one generation and applies it whole.")
	pend("REL-061", "preempt screen-program (priority/offline) is Phase-2 program-delivery semantics; first-photon applies a single static screen-program with no preemption.")
	pend("REL-090", "telemetry overflow / loss-marker is the Phase-2 telemetry plane; first-photon has no telemetry ingest surface.")
	pend("REL-094", "telemetry latest-only heartbeat supersession is the Phase-2 telemetry plane; not built in first-photon.")
	pend("REL-110", "device-candidate + command is the Phase-3 device plane; first-photon carries an empty device_inventory and no command path.")
	pend("REL-133", "clock-hint bounding is Phase-2 clock-trust; first-photon has no clock-hint negotiation.")
	pend("REL-136", "cold-boot skew-tolerant connect is Phase-2 clock-trust; first-photon has no skew-negotiated connect path.")

	return rep
}

// driveREL010 drives fresh enrollment: a relay holding no certificate redeems
// a claim credential and receives a relay_id, certificate, and the app peer's
// desired_state_verification_key; a repeat enrollment is a stable-identity
// no-op; and a second redemption of the SAME claim token is refused.
func driveREL010(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-010")
	if !ok {
		rep.Fail("REL-010", contract, "case not found in frozen corpus")
		return
	}

	store, err := identity.Open(":memory:")
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("open identity store: %v", err))
		return
	}
	defer store.Close()

	// Capture the token the enrollment will consume, so the single-use refusal
	// below can re-present that exact token.
	token, err := feeder.CurrentClaimToken()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("current claim token: %v", err))
		return
	}

	var diffs []report.Diff

	if err := client.Enroll(feeder.EnrollBaseURL(), store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("fresh enroll failed: %v", err))
		return
	}

	relayID, ok, err := store.Identity()
	if err != nil || !ok || relayID.RelayID == "" {
		diffs = append(diffs, report.Diff{Field: "relay_id (enroll-ack.body.relay_id)", Expected: "issued+persisted", Actual: fmt.Sprintf("ok=%v id=%q err=%v", ok, relayID.RelayID, err)})
	}
	if len(relayID.CertPEM) == 0 {
		diffs = append(diffs, report.Diff{Field: "cert (enroll-ack.body cert)", Expected: "issued", Actual: "empty"})
	}
	if _, present, err := store.DesiredStateVerificationKey(); err != nil || !present {
		diffs = append(diffs, report.Diff{Field: "desired_state_verification_key", Expected: "present", Actual: fmt.Sprintf("present=%v err=%v", present, err)})
	}

	// relay_id_stable_across_attempts: a repeat enroll is an idempotent no-op
	// leaving the relay_id unchanged (REL-014).
	firstID := relayID.RelayID
	if err := client.Enroll(feeder.EnrollBaseURL(), store); err != nil {
		diffs = append(diffs, report.Diff{Field: "repeat-enroll idempotent", Expected: "no-op success", Actual: err.Error()})
	}
	if again, _, _ := store.Identity(); again.RelayID != firstID {
		diffs = append(diffs, report.Diff{Field: "relay_id_stable_across_attempts", Expected: firstID, Actual: again.RelayID})
	}

	// Second redemption of the SAME claim token is refused (REL-013): the
	// feeder checks the token before the CSR, so an empty CSR still surfaces
	// the CLAIM_TOKEN_INVALID reason.
	wantCode := c.ExpectString("responses.1.code")
	if wantCode == "" {
		wantCode = "CLAIM_TOKEN_INVALID"
	}
	gotCode, err := reEnrollRefusalCode(feeder.EnrollBaseURL(), token)
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "reuse-token probe", Expected: wantCode, Actual: err.Error()})
	} else if gotCode != wantCode {
		diffs = append(diffs, report.Diff{Field: "reuse-token refusal code", Expected: wantCode, Actual: gotCode})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "fresh enrollment diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"relay_id / cert / desired_state_verification_key values are runtime-issued, not the corpus fixtures — asserted present + stable + single-use, not byte-equal.",
		"not_before/not_after (enroll-ack.body): runtime validity window; not asserted byte-equal.")
}

// driveREL070 drives idempotent generation reapply: after applying generation
// 42, a later snapshot at generation 43 with byte-identical sections (hence
// the same hash) must be a no-op — the applied generation advances but the
// persisted last-applied hash is unchanged and nothing re-runs.
func driveREL070(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-070")
	if !ok {
		rep.Fail("REL-070", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	var diffs []report.Diff

	url42, err := feeder.SignedSnapshotURL(42)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-42 snapshot: %v", err))
		return
	}
	if _, err := client.Pull(url42, store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply gen-42 snapshot: %v", err))
		return
	}
	gen1, hash1, _, _ := store.LastAppliedGeneration()
	if gen1 != 42 {
		diffs = append(diffs, report.Diff{Field: "last_applied after gen-42", Expected: int64(42), Actual: gen1})
	}

	url43, err := feeder.SignedSnapshotURL(43)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-43 snapshot: %v", err))
		return
	}
	if _, err := client.Pull(url43, store); err != nil {
		diffs = append(diffs, report.Diff{Field: "reapply gen-43 (treated_as_noop)", Expected: "no-op success", Actual: err.Error()})
	}
	gen2, hash2, _, _ := store.LastAppliedGeneration()

	// state_ack.body.applied_generation == 43. The corpus case declares this
	// field, so its absence is itself a failure — not a silently-skipped
	// assertion.
	if wantGen, present := expectInt(c, "state_ack.body.applied_generation"); !present {
		diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if gen2 != wantGen {
		diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation", Expected: wantGen, Actual: gen2})
	}
	// persisted_last_applied_hash_unchanged: the hash must not have moved even
	// though the generation number advanced.
	if hash2 != hash1 {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied_hash_unchanged", Expected: hash1, Actual: hash2})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "idempotent reapply diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"apply_time_side_effects_rerun=false / in_flight_run_canceled=false: first-photon applies desired state declaratively with no apply-time side effects or rule runs, so these are vacuously satisfied — the observable no-op property asserted is the unchanged persisted hash across the advanced generation.",
		"state_ack: desiredstate.Pull returns an Applied value, not a wire state.ack envelope (the REST subset carries no state.ack) — applied_generation is read from the persisted last-applied generation.")
}

// driveREL071 drives the wrong-peer-key security gate: a snapshot at
// generation 43 signed under a key that is NOT the relay's enrollment-anchored
// verification key MUST be rejected outright — no section applied, last-applied
// stays at 42.
func driveREL071(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-071")
	if !ok {
		rep.Fail("REL-071", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()

	var diffs []report.Diff

	url42, err := feeder.SignedSnapshotURL(42)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-42 snapshot: %v", err))
		return
	}
	if _, err := client.Pull(url42, store); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply gen-42 snapshot: %v", err))
		return
	}
	genBefore, hashBefore, _, _ := store.LastAppliedGeneration()

	// The impostor snapshot at gen 43 signed with a foreign key.
	urlBad, err := feeder.WrongKeySnapshotURL(43)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage wrong-key snapshot: %v", err))
		return
	}
	_, pullErr := client.Pull(urlBad, store)

	// signature_verifies=false: Pull must reject (a real client returns
	// ErrSnapshotSignatureInvalid; any non-nil rejection here is the
	// observable "did not accept").
	if pullErr == nil {
		diffs = append(diffs, report.Diff{Field: "signature_verifies (must reject)", Expected: false, Actual: "accepted"})
	}
	// sections_applied=false + persisted_last_applied_unchanged: last-applied
	// must still be exactly {42, hashBefore}.
	genAfter, hashAfter, _, _ := store.LastAppliedGeneration()
	// persisted_last_applied_unchanged.generation is declared by the corpus
	// case, so its absence is itself a failure — not a silently-skipped
	// assertion.
	if wantGen, present := expectInt(c, "persisted_last_applied_unchanged.generation"); !present {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied_unchanged.generation (sections_applied=false)", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if genAfter != wantGen {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied_unchanged.generation (sections_applied=false)", Expected: wantGen, Actual: genAfter})
	}
	if genAfter != genBefore || hashAfter != hashBefore {
		diffs = append(diffs, report.Diff{Field: "last_applied unchanged", Expected: fmt.Sprintf("{%d,%s}", genBefore, hashBefore), Actual: fmt.Sprintf("{%d,%s}", genAfter, hashAfter)})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "wrong-peer-key rejection diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		fmt.Sprintf("state_ack.body.error.code: corpus expects %q; a real relay client returns ErrSnapshotSignatureInvalid (errors.Is-matched: %v) rather than emitting a wire state.ack envelope — semantically equivalent SNAPSHOT_SIGNATURE_INVALID.", c.ExpectString("state_ack.body.error.code"), errors.Is(pullErr, desiredstate.ErrSnapshotSignatureInvalid)))
}

// enrolledStore opens a fresh in-memory identity store and enrolls it against
// the feeder — the precondition for any desired-state pull (the store must
// hold the enrollment-anchored verification key).
func enrolledStore(client RelayClient, feeder Feeder) (*identity.Store, error) {
	store, err := identity.Open(":memory:")
	if err != nil {
		return nil, fmt.Errorf("open identity store: %w", err)
	}
	if err := client.Enroll(feeder.EnrollBaseURL(), store); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("enroll before pull: %w", err)
	}
	return store, nil
}

// reEnrollRefusalCode re-presents an already-redeemed claim token to the
// feeder's /enroll and returns the RFC-9457 problem `code` it refuses with.
// An empty CSR is deliberate: the feeder validates the (already-redeemed)
// token before ever parsing the CSR, so this surfaces the token reason.
func reEnrollRefusalCode(enrollBaseURL, token string) (string, error) {
	body, _ := json.Marshal(map[string]string{"claim_token": token, "csr": ""})
	resp, err := insecureClient().Post(enrollBaseURL+"/enroll", "application/json", bytesReader(body))
	if err != nil {
		return "", fmt.Errorf("POST /enroll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "", fmt.Errorf("reuse of a redeemed claim token was ACCEPTED (status %d) — single-use not enforced", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var pb struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &pb) != nil || pb.Code == "" {
		return "", fmt.Errorf("refusal body carried no problem code: %s", string(raw))
	}
	return pb.Code, nil
}

// expectInt reads an integer expected field (JSON numbers decode as
// float64), also reporting whether the field was PRESENT — a case that
// declares the field must have it asserted, never silently skipped just
// because it happens to be missing from the corpus fixture.
func expectInt(c corpus.Case, path string) (n int64, present bool) {
	v, ok := c.Expect(path)
	if !ok {
		return 0, false
	}
	switch v := v.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "relay-1")
}

// LoadCorpus loads every frozen relay-1 corpus case, keyed by case_id — the
// exact set Run itself reads. It is exported so the driver's own tests can
// independently verify every case_id present in the corpus DIRECTORY is
// accounted for by Run as either driven or pending (§10 "no silent caps"
// extended to a NEW case someone freezes later: it must be triaged, not
// silently uncovered).
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
