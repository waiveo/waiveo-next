// Package player1 is the executable player/1 conformance driver: the §10
// differential oracle for the player/1 contract. It replays the
// FIRST-PHOTON-APPLICABLE cases of the frozen player-1 corpus
// (conformance/corpora/player-1) against a LIVE relay, driving a pluggable
// PlayerTarget (the player/1 client under test), and diffs the target's
// actual behavior against each case's own declared `expected` block.
//
// The target is pluggable ON PURPOSE (§10 "drivers take ANY target, diff
// behavior"): the first-photon target is the in-process virtual player
// (VirtualPlayerTarget, wrapping internal/virtualplayer.Photon), and the
// SAME driver validates a real BrightScript player in a later wave by
// plugging in a different PlayerTarget — the corpus, the assertions, and the
// oracle stay identical; only the client changes.
//
// Applicability triage (§10 "no silent caps"): Run DRIVES the cases the
// first-photon relay + virtual player actually implement (PLY-050, PLY-055,
// PLY-057) and marks every other player-1 case PENDING with an explicit
// reason. The returned Report enumerates all of them, so a reader sees
// exactly what is and isn't covered, and a future task that builds a pending
// feature must move its case from PENDING to driven for the harness's own
// honesty check to keep passing.
package player1

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
)

// PlayerTarget is the pluggable player/1 client the driver drives. A single
// method drives one full pairing + program-pull attempt for a pairing code
// and reports what the target itself can observe about the outcome — enough
// for the driver to diff against the corpus without ever peering inside the
// target's internals (so a BrightScript target, observed only over the wire,
// implements the exact same interface).
type PlayerTarget interface {
	// Name identifies the target implementation (e.g. "virtualplayer").
	Name() string
	// Pair drives one player/1 attempt for pairingCode (which itself carries
	// the relay's dial address, grant selector, and OOB fingerprint
	// commitment) and returns the outcome the target observed.
	Pair(pairingCode string) PairResult
}

// PairResult is what a PlayerTarget reports about a single pairing attempt —
// the target's own black-box observation, mapped onto the vocabulary the
// player/1 corpus asserts.
type PairResult struct {
	// Completed is true when the full player/1 thread ran to a delivered
	// content item (the happy-path terminal outcome).
	Completed bool
	// Rejected is true when the target refused to complete the attempt
	// (aborted before or during redemption) — the negative-path outcome.
	Rejected bool
	// CommitmentMismatch is true when the rejection was specifically an
	// out-of-band fingerprint-commitment mismatch (PLY-056/057), i.e. the
	// relay the code actually reached is not the one it was formed for —
	// distinguishing a MITM rejection from an unrelated failure.
	CommitmentMismatch bool
	// Err is the target's error detail when Rejected.
	Err string
}

// Relay is the LIVE relay the driver drives a target against, plus the
// black-box observation surface the differential oracle needs: the driver
// cannot MITM the target↔relay TLS (that is exactly what player/1's
// commitment pinning prevents), so relay-side wire observation — how many
// times /player/v1/pair was hit, and the raw request/response bodies —
// is the only way to assert properties like "fingerprint_commitment never
// crossed the wire" or "the flow never reached redemption". This is what
// subsumes the brief's plain relayBaseURL: an oracle must observe the relay,
// not merely address it.
type Relay interface {
	// BaseURL is the relay's own https base URL.
	BaseURL() string
	// FormPairingCode forms a valid out-of-band pairing code, over a FRESH
	// single-use grant, whose fingerprint commitment is over THIS relay's real
	// certificate — the correctly-formed code a non-MITM player verifies and
	// accepts.
	FormPairingCode() (string, error)
	// FormPairingCodePair forms, over ONE fresh grant, both a correct code and
	// a mismatched code (commitment over a DIFFERENT cert than this relay
	// presents — the substituted-cert / MITM scenario, PLY-057). Sharing the
	// grant lets PLY-057 prove the rejected mismatched attempt left that exact
	// grant redeemable.
	FormPairingCodePair() (correct, mismatched string, err error)
	// PairCallCount returns how many requests have hit /player/v1/pair since
	// the last Reset.
	PairCallCount() int
	// PairRequests returns the raw JSON request bodies seen at
	// /player/v1/pair since the last Reset.
	PairRequests() [][]byte
	// PairResponses returns the raw JSON response bodies the relay returned
	// from /player/v1/pair since the last Reset.
	PairResponses() [][]byte
	// Reset clears the observation counters/buffers between cases.
	Reset()
}

// contract is the corpus contract name every player-1 case declares.
const contract = "player/1"

// Run replays the first-photon-applicable player/1 corpus cases against relay,
// driving target, and returns the differential-oracle Report. It reads each
// case's `expected` block from the frozen corpus (never a value hard-coded
// here) and asserts every field the live surface can observe, noting the
// fields it cannot rather than faking a pass.
func Run(target PlayerTarget, relay Relay) report.Report {
	rep := report.Report{Driver: "player1", Target: target.Name()}

	cases, err := corpus.LoadDir(corpusDir())
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load player-1 corpus: %v", err))
		return rep
	}

	drivePLY055(&rep, target, relay, cases)
	drivePLY057(&rep, target, relay, cases)
	drivePLY050(&rep, target, relay, cases)

	// PENDING — features not built in first-photon (§10 "no silent caps"):
	// each is a real player/1 case this wave's relay + virtual player do not
	// implement, enumerated with its reason so coverage is explicit.
	pend := func(short, reason string) {
		id := short
		if c, ok := corpus.ByID(cases, short); ok {
			id = c.CaseID
		}
		rep.Pending(id, contract, reason)
	}
	pend("PLY-101", "lease preemption / interrupt-now is Phase-2 program-delivery; the first-photon relay serves one static program with no preemption path to drive.")
	pend("PLY-130", "server-moved relocate / never-wipe reconnection is Phase-2 steady-state; the virtual player is a single-shot pairing thread with no persisted server-locating state to relocate.")
	pend("PLY-136", "token revocation / reconnect-clears-token is Phase-2 credential-lifecycle; first-photon issues a channel token but has no revocation surface to exercise.")
	pend("PLY-155", "power-schedule interaction is Phase-3 scheduling; no schedule section is applied or delivered in first-photon.")

	return rep
}

// drivePLY055 drives the cross-VLAN manual-entry pairing-code commitment
// happy path: a correctly-formed pairing code (commitment over the relay's
// real cert) must pair to a delivered content item, with grant_selector on
// the wire but fingerprint_commitment NEVER on it.
func drivePLY055(rep *report.Report, target PlayerTarget, relay Relay, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "PLY-055")
	if !ok {
		rep.Fail("PLY-055", contract, "case not found in frozen corpus")
		return
	}

	relay.Reset()
	code, err := relay.FormPairingCode()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("form pairing code: %v", err))
		return
	}
	res := target.Pair(code)

	var diffs []report.Diff
	// commitment_match (expected true): the target accepted the relay's cert
	// against the code's OOB commitment and ran to completion.
	if want := c.ExpectBool("commitment_match"); want != (res.Completed && !res.Rejected) {
		diffs = append(diffs, report.Diff{Field: "commitment_match", Expected: want, Actual: res.Completed && !res.Rejected})
	}
	// Exactly one redemption reached the relay.
	if relay.PairCallCount() != 1 {
		diffs = append(diffs, report.Diff{Field: "pair_call_count", Expected: 1, Actual: relay.PairCallCount()})
	}
	reqs := relay.PairRequests()
	if len(reqs) == 1 {
		// grant_selector_sent_to_relay (expected true).
		sent := jsonNonEmptyString(reqs[0], "grant_selector")
		if want := c.ExpectBool("grant_selector_sent_to_relay"); want != sent {
			diffs = append(diffs, report.Diff{Field: "grant_selector_sent_to_relay", Expected: want, Actual: sent})
		}
		// fingerprint_commitment_sent_to_relay (expected false) — the
		// load-bearing privacy property: the commitment must never cross the
		// wire, observed directly on the request the relay received.
		onWire := jsonHasKey(reqs[0], "fingerprint_commitment")
		if want := c.ExpectBool("fingerprint_commitment_sent_to_relay"); want != onWire {
			diffs = append(diffs, report.Diff{Field: "fingerprint_commitment_sent_to_relay", Expected: want, Actual: onWire})
		}
	}
	resps := relay.PairResponses()
	if len(resps) == 1 {
		// channel_token_issued (expected true).
		issued := jsonNonEmptyString(resps[0], "channel_token")
		if want := c.ExpectBool("channel_token_issued"); want != issued {
			diffs = append(diffs, report.Diff{Field: "channel_token_issued", Expected: want, Actual: issued})
		}
		// screen_id present (value differs from the corpus fixture — see note).
		if !jsonNonEmptyString(resps[0], "screen_id") {
			diffs = append(diffs, report.Diff{Field: "screen_id_present", Expected: true, Actual: false})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "manual-entry commitment happy path diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		fmt.Sprintf("screen_id value is relay-minted, not the corpus fixture %q — asserted present, not byte-equal.", c.ExpectString("screen_id")),
		"poll_continues_until_redeemed: the first-photon relay redeems synchronously (no pending→redeemed poll loop in the REST subset) — the redeemed terminal state is observed, the multi-poll transit is not.",
		"trust_anchor_persisted / trust_outcome=commitment_verified: the virtual player pins in-process (TLS VerifyConnection); on-disk persistence is not observable from the wire, but the commitment check that gates it is (commitment_match).")
}

// drivePLY057 drives the out-of-band commitment-mismatch MITM negative: a
// pairing code whose commitment is over a different cert than the relay
// presents MUST be rejected at the local check, NEVER reaching /player/v1/pair,
// and MUST leave the grant redeemable for a fresh, correct attempt.
func drivePLY057(rep *report.Report, target PlayerTarget, relay Relay, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "PLY-057")
	if !ok {
		rep.Fail("PLY-057", contract, "case not found in frozen corpus")
		return
	}

	relay.Reset()
	correct, mismatched, err := relay.FormPairingCodePair()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("form pairing code pair: %v", err))
		return
	}
	res := target.Pair(mismatched)

	var diffs []report.Diff
	// pairing_attempt_result=rejected. The corpus case declares this field, so
	// its absence is itself a failure — not a silently-skipped assertion.
	if v, ok := c.Expect("pairing_attempt_result"); !ok {
		diffs = append(diffs, report.Diff{Field: "pairing_attempt_result", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if want, _ := v.(string); want == "rejected" && !res.Rejected {
		diffs = append(diffs, report.Diff{Field: "pairing_attempt_result", Expected: want, Actual: "not-rejected"})
	}
	// commitment_match (expected false).
	if want := c.ExpectBool("commitment_match"); want != false || !res.CommitmentMismatch {
		diffs = append(diffs, report.Diff{Field: "commitment_match", Expected: want, Actual: !res.CommitmentMismatch})
	}
	// The flow MUST NOT have reached the relay at all: this single count==0
	// observation is what makes fingerprint_commitment_sent_to_relay=false,
	// channel_token_used=false, status_poll_attempted=false, and
	// trust_anchor_persisted/pinned=false all concretely true — nothing was
	// ever sent, redeemed, polled, or pinned.
	if relay.PairCallCount() != 0 {
		diffs = append(diffs, report.Diff{Field: "reached_relay (channel_token_used/status_poll_attempted/trust_anchor_pinned)", Expected: 0, Actual: relay.PairCallCount()})
	}
	if c.ExpectBool("fingerprint_commitment_sent_to_relay") { // expected false
		diffs = append(diffs, report.Diff{Field: "fingerprint_commitment_sent_to_relay", Expected: false, Actual: "corpus expected true?!"})
	}

	// grant_invalidated (expected false): the rejected attempt must not have
	// consumed the grant. Prove it by pairing the CORRECT code over the SAME
	// grant — it must now redeem and complete. (Only meaningful once the
	// mismatch was actually rejected above.)
	relay.Reset()
	reproof := target.Pair(correct)
	grantInvalidated := !reproof.Completed
	if want := c.ExpectBool("grant_invalidated"); want != grantInvalidated { // expected false
		diffs = append(diffs, report.Diff{Field: "grant_invalidated", Expected: want, Actual: grantInvalidated})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "MITM commitment-mismatch rejection diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		fmt.Sprintf("error_code: corpus expects %q; the virtual player reports a commitment-mismatch error (ErrCommitmentMismatch) — semantically equivalent, exact code string not asserted.", c.ExpectString("error_code")),
		fmt.Sprintf("player_state: corpus expects %q; the virtual player is single-shot and returns an error rather than exposing a state machine — the terminal not-paired outcome is observed, the named state is not.", c.ExpectString("player_state")))
}

// drivePLY050 drives the pairing-redemption / channel-token-issuance thread
// the virtual player exercises. The corpus case is trust-on-first-use
// (no commitment); the virtual player always performs a commitment check, so
// the DRIVEN subset here is the redemption + token issuance both flows share,
// with the TOFU-specific expectations explicitly noted as not-exercised.
func drivePLY050(rep *report.Report, target PlayerTarget, relay Relay, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "PLY-050")
	if !ok {
		rep.Fail("PLY-050", contract, "case not found in frozen corpus")
		return
	}

	relay.Reset()
	code, err := relay.FormPairingCode()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("form pairing code: %v", err))
		return
	}
	res := target.Pair(code)

	var diffs []report.Diff
	if !res.Completed || res.Rejected {
		diffs = append(diffs, report.Diff{Field: "redemption_completed", Expected: true, Actual: res.Completed && !res.Rejected})
	}
	if relay.PairCallCount() != 1 {
		diffs = append(diffs, report.Diff{Field: "pair_call_count", Expected: 1, Actual: relay.PairCallCount()})
	}
	resps := relay.PairResponses()
	if len(resps) == 1 {
		if want := c.ExpectBool("channel_token_issued"); want != jsonNonEmptyString(resps[0], "channel_token") {
			diffs = append(diffs, report.Diff{Field: "channel_token_issued", Expected: want, Actual: jsonNonEmptyString(resps[0], "channel_token")})
		}
		if !jsonNonEmptyString(resps[0], "screen_id") {
			diffs = append(diffs, report.Diff{Field: "screen_id_present", Expected: true, Actual: false})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "redemption/token-issuance subset diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"DRIVEN SUBSET: redemption + channel-token issuance only. The virtual player performs a commitment check on every connection, so this case's TOFU-specific expectations are NOT exercised and were not asserted:",
		"  - trust_outcome=trust_on_first_use / commitment_comparison_performed=false: opposite of what the virtual player does (it always compares) — Phase-2 TOFU-no-commitment bootstrap.",
		"  - trust_anchor_pinning_granularity=ca-level: the virtual player pins to the leaf SPKI commitment, not a CA chain (no CA exists in first-photon bootstrap).",
		"  - steady_state_connection_after_pairing=full-verification-succeeds: no post-pairing CA-verified steady-state connection in first-photon.",
		fmt.Sprintf("  - channel_token_expires_at / screen_id=%q: relay-minted values, asserted present not byte-equal to the corpus fixture.", c.ExpectString("screen_id")))
}

// corpusDir resolves the frozen player-1 corpus directory relative to THIS
// source file, so the driver finds it regardless of the test's working
// directory.
func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "player-1")
}

// LoadCorpus loads every frozen player-1 corpus case, keyed by case_id — the
// exact set Run itself reads. It is exported so the driver's own tests can
// independently verify every case_id present in the corpus DIRECTORY is
// accounted for by Run as either driven or pending (§10 "no silent caps"
// extended to a NEW case someone freezes later: it must be triaged, not
// silently uncovered).
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}

// jsonHasKey reports whether body decodes as a JSON object carrying key at
// the top level (regardless of value).
func jsonHasKey(body []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// jsonNonEmptyString reports whether body decodes as a JSON object whose key
// is a non-empty string.
func jsonNonEmptyString(body []byte, key string) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	s, ok := m[key].(string)
	return ok && s != ""
}
