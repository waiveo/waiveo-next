// Package player1 is the executable player/1 conformance driver: the §10
// differential oracle for the player/1 contract. It replays EVERY case of the
// frozen player-1 corpus (conformance/corpora/player-1) — the first-photon
// pairing subset (PLY-050/055/057) against a LIVE relay, driving a pluggable
// PlayerTarget (the player/1 client under test), plus the Phase-2/3 lease
// preemption, reconnect/relocate, revocation, and off-air liveness cases
// (PLY-101/130/136/155) against the exact committed decision functions the
// virtual player and relay themselves run — and diffs the observed behavior
// against each case's own declared `expected` block.
//
// The target is pluggable ON PURPOSE for the pairing subset (§10 "drivers
// take ANY target, diff behavior"): the first-photon target is the in-process
// virtual player (VirtualPlayerTarget, wrapping internal/virtualplayer.Photon),
// and the SAME driver validates a real BrightScript player in a later wave by
// plugging in a different PlayerTarget — the corpus, the assertions, and the
// oracle stay identical; only the client changes. Task 5's four Phase-2/3
// cases are deliberately driven at the decision-function level rather than
// through PlayerTarget (see each drivePLY10*/drivePLY136/drivePLY155's own
// doc): a BrightScript player-v3 exercising these same behaviors on-device is
// a deliberately deferred hardware target (this plan proves the player/1
// SEMANTICS in software); each Phase-2/3 case's Pass note also points at the
// package's own live end-to-end test proving the identical function wired to
// a real relay over the wire.
//
// §10 "no silent caps": Run drives every case in the frozen corpus — nothing
// is PENDING. TestPlayer1CorpusFullyAccountedFor independently re-derives the
// corpus directory's own case set and fails loudly if a future corpus
// addition is ever left untriaged.
package player1

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/virtualplayer"
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

// Run replays every player-1 corpus case and returns the differential-oracle
// Report: the first-photon pairing subset against relay, driving target, plus
// the Phase-2/3 cases against their own committed decision functions. It
// reads each case's `expected` block from the frozen corpus (never a value
// hard-coded here) and asserts every field the live surface can observe,
// noting the fields it cannot rather than faking a pass.
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

	// Phase-2/3 cases (Task 5): each drives the exact committed decision
	// function the virtual player / relay itself uses — no logic
	// reimplemented here — against the frozen corpus's own input, asserting
	// every field of its own expected block. §10 "no silent caps": nothing
	// remains PENDING.
	drivePLY101(&rep, cases)
	drivePLY130(&rep, cases)
	drivePLY136(&rep, cases)
	drivePLY155(&rep, cases)

	return rep
}

// decodeField re-marshals a corpus case's generic map[string]any input (or
// any sub-object of it) into a concrete typed value — the driver's own
// fields are JSON-tagged to match the frozen corpus's field names exactly,
// so this is a lossless re-decode, not a schema translation.
func decodeField(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal corpus field: %w", err)
	}
	return json.Unmarshal(b, v)
}

// expectInt64 returns an integer expected field at path, defaulting to 0
// when absent — JSON numbers decode as float64 through corpus.Case.Expect,
// so this is the int64 counterpart to ExpectBool/ExpectString.
func expectInt64(c corpus.Case, path string) int64 {
	v, ok := c.Expect(path)
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// ply101Input mirrors conformance/corpora/player-1/
// PLY-101-valid-lease-preemption-interrupt-now.json's own `input` block,
// field for field.
type ply101Input struct {
	ActiveLeaseBefore struct {
		LeaseID         string `json:"lease_id"`
		Priority        string `json:"priority"`
		ProgramRevision string `json:"program_revision"`
	} `json:"active_lease_before"`
	CurrentlyRendering struct {
		AssetRef      string `json:"asset_ref"`
		RenderStartTS int64  `json:"render_start_ts"`
	} `json:"currently_rendering"`
	NewLease struct {
		LeaseID         string              `json:"lease_id"`
		ScreenID        string              `json:"screen_id"`
		ProgramRevision string              `json:"program_revision"`
		Priority        string              `json:"priority"`
		Display         string              `json:"display"`
		Content         []wire.LeaseContent `json:"content"`
		IssuedAt        int64               `json:"issued_at"`
		ValidUntil      int64               `json:"valid_until"`
	} `json:"new_lease"`
	DeliveryTime int64 `json:"delivery_time"`
}

// drivePLY101 drives virtualplayer.AdoptionOf — the exact, committed,
// network-free priority-adoption decision a player runs on every delivered
// Lease (PLY-094/100/101/102/107) — against the frozen PLY-101 case's own
// input, and diffs every field of its expected block: the lease
// acknowledgement (PLY-091), the interrupt-now timing (PLY-101), the
// interrupted asset's render/end (PLY-107/111/112), the takeover asset's
// render/start (PLY-110), and the new sole active lease (PLY-094).
func drivePLY101(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "PLY-101")
	if !ok {
		rep.Fail("PLY-101", contract, "case not found in frozen corpus")
		return
	}

	var in ply101Input
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	active := virtualplayer.ActiveRender{
		LeaseID:         in.ActiveLeaseBefore.LeaseID,
		Priority:        in.ActiveLeaseBefore.Priority,
		ProgramRevision: in.ActiveLeaseBefore.ProgramRevision,
		AssetRef:        in.CurrentlyRendering.AssetRef,
		RenderStartTS:   in.CurrentlyRendering.RenderStartTS,
	}
	next := wire.Lease{
		LeaseID:         in.NewLease.LeaseID,
		ScreenID:        in.NewLease.ScreenID,
		ProgramRevision: in.NewLease.ProgramRevision,
		Priority:        in.NewLease.Priority,
		Display:         in.NewLease.Display,
		Content:         in.NewLease.Content,
		IssuedAt:        in.NewLease.IssuedAt,
		ValidUntil:      in.NewLease.ValidUntil,
	}

	got := virtualplayer.AdoptionOf(active, next, in.DeliveryTime)

	var diffs []report.Diff
	assertStr := func(field, want, actual string) {
		if want != actual {
			diffs = append(diffs, report.Diff{Field: field, Expected: want, Actual: actual})
		}
	}
	assertBool := func(field string, want, actual bool) {
		if want != actual {
			diffs = append(diffs, report.Diff{Field: field, Expected: want, Actual: actual})
		}
	}
	assertInt := func(field string, want, actual int64) {
		if want != actual {
			diffs = append(diffs, report.Diff{Field: field, Expected: want, Actual: actual})
		}
	}

	assertStr("lease_ack.lease_id", c.ExpectString("lease_ack.lease_id"), got.AckLeaseID)
	assertBool("lease_ack.accepted", c.ExpectBool("lease_ack.accepted"), got.AckAccepted)
	assertBool("interrupted_immediately", c.ExpectBool("interrupted_immediately"), got.InterruptedImmediately)
	assertBool("waited_for_natural_end", c.ExpectBool("waited_for_natural_end"), got.WaitedForNaturalEnd)
	assertStr("previous_asset_render_end.asset_ref", c.ExpectString("previous_asset_render_end.asset_ref"), got.PreviousRenderEnd.AssetRef)
	assertInt("previous_asset_render_end.t_end", expectInt64(c, "previous_asset_render_end.t_end"), got.PreviousRenderEnd.TEnd)
	assertStr("previous_asset_render_end.cause", c.ExpectString("previous_asset_render_end.cause"), got.PreviousRenderEnd.Cause)
	assertStr("previous_asset_render_end.completion", c.ExpectString("previous_asset_render_end.completion"), got.PreviousRenderEnd.Completion)
	assertStr("takeover_render_start.lease_id", c.ExpectString("takeover_render_start.lease_id"), got.TakeoverRenderStart.LeaseID)
	assertStr("takeover_render_start.asset_ref", c.ExpectString("takeover_render_start.asset_ref"), got.TakeoverRenderStart.AssetRef)
	assertInt("takeover_render_start.ts", expectInt64(c, "takeover_render_start.ts"), got.TakeoverRenderStart.TS)
	assertStr("takeover_render_end_cause", c.ExpectString("takeover_render_end_cause"), got.TakeoverCause)
	assertStr("active_lease_after", c.ExpectString("active_lease_after"), got.ActiveLeaseAfter)

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "preempt-priority interrupt-now adoption diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"driven via virtualplayer.AdoptionOf directly — the exact, committed, network-free priority-adoption decision (PLY-094/100/101/102/107); the live wire round-trip (redeem, pull, ack, render/start+end over a real preempt-priority relay) is exercised end to end in internal/virtualplayer/preempt_test.go's TestPreemptInterruptNowEndToEnd, reusing the same function.")
}

// ply130Input mirrors conformance/corpora/player-1/
// PLY-130-valid-server-moved-relocate-never-wipe.json's own `input` block.
type ply130Input struct {
	PlayerPersistedState struct {
		RelayAddress struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"relay_address"`
		TrustAnchorPresent  bool `json:"trust_anchor_present"`
		ChannelTokenPresent bool `json:"channel_token_present"`
	} `json:"player_persisted_state"`
	ConnectionAttempts []struct {
		Attempt int `json:"attempt"`
		Address struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"address"`
		Result               string `json:"result"`
		ReDiscoveryTriggered bool   `json:"re_discovery_triggered"`
		DiscoveryResponse    *struct {
			ST       string `json:"st"`
			Location string `json:"location"`
		} `json:"discovery_response"`
	} `json:"connection_attempts"`
}

// drivePLY130 drives virtualplayer.RelocateDecision — the committed,
// network-free Reconnect/relocate never-wipe state machine
// (PLY-022/026/130/131/132/133) — against the frozen PLY-130 case's own
// connection-attempt sweep, and diffs every field of its expected block.
func drivePLY130(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "PLY-130")
	if !ok {
		rep.Fail("PLY-130", contract, "case not found in frozen corpus")
		return
	}

	var in ply130Input
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	state := virtualplayer.PersistedState{
		RelayAddress: virtualplayer.RelayAddress{
			Host: in.PlayerPersistedState.RelayAddress.Host,
			Port: in.PlayerPersistedState.RelayAddress.Port,
		},
		TrustAnchorPresent:  in.PlayerPersistedState.TrustAnchorPresent,
		ChannelTokenPresent: in.PlayerPersistedState.ChannelTokenPresent,
	}
	attempts := make([]virtualplayer.ConnectionAttempt, 0, len(in.ConnectionAttempts))
	for _, a := range in.ConnectionAttempts {
		at := virtualplayer.ConnectionAttempt{
			Attempt:              a.Attempt,
			Address:              virtualplayer.RelayAddress{Host: a.Address.Host, Port: a.Address.Port},
			Result:               a.Result,
			ReDiscoveryTriggered: a.ReDiscoveryTriggered,
		}
		if a.DiscoveryResponse != nil {
			at.DiscoveryResponse = &virtualplayer.DiscoveryResponse{
				ST:       a.DiscoveryResponse.ST,
				Location: a.DiscoveryResponse.Location,
			}
		}
		attempts = append(attempts, at)
	}

	got := virtualplayer.RelocateDecision(state, attempts)

	var diffs []report.Diff
	if want := c.ExpectString("failure_classification_attempts_1_to_4"); got.FailureClassificationBeforeRelocate != want {
		diffs = append(diffs, report.Diff{Field: "failure_classification_attempts_1_to_4", Expected: want, Actual: got.FailureClassificationBeforeRelocate})
	}
	if want := c.ExpectBool("backoff_capped"); got.BackoffCapped != want {
		diffs = append(diffs, report.Diff{Field: "backoff_capped", Expected: want, Actual: got.BackoffCapped})
	}
	if want := int(expectInt64(c, "re_discovery_fired_on_attempt")); got.ReDiscoveryFiredOnAttempt != want {
		diffs = append(diffs, report.Diff{Field: "re_discovery_fired_on_attempt", Expected: want, Actual: got.ReDiscoveryFiredOnAttempt})
	}
	wantAddr := virtualplayer.RelayAddress{
		Host: c.ExpectString("relay_address_updated_to.host"),
		Port: int(expectInt64(c, "relay_address_updated_to.port")),
	}
	if got.RelayAddressUpdatedTo != wantAddr {
		diffs = append(diffs, report.Diff{Field: "relay_address_updated_to", Expected: wantAddr, Actual: got.RelayAddressUpdatedTo})
	}
	if want := c.ExpectBool("trust_anchor_cleared"); got.TrustAnchorCleared != want {
		diffs = append(diffs, report.Diff{Field: "trust_anchor_cleared", Expected: want, Actual: got.TrustAnchorCleared})
	}
	if want := c.ExpectBool("channel_token_cleared"); got.ChannelTokenCleared != want {
		diffs = append(diffs, report.Diff{Field: "channel_token_cleared", Expected: want, Actual: got.ChannelTokenCleared})
	}
	if want := c.ExpectBool("retries_indefinitely"); got.RetriesIndefinitely != want {
		diffs = append(diffs, report.Diff{Field: "retries_indefinitely", Expected: want, Actual: got.RetriesIndefinitely})
	}
	if want := c.ExpectString("reconnect_result"); got.ReconnectResult != want {
		diffs = append(diffs, report.Diff{Field: "reconnect_result", Expected: want, Actual: got.ReconnectResult})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "reconnect/relocate never-wipe sweep diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"driven via virtualplayer.RelocateDecision directly — the exact, committed, network-free Reconnect/relocate state machine (PLY-022/026/130-133); the live wire round-trip (a real address move across two listeners on one relay identity) is exercised end to end in internal/virtualplayer/relocate_test.go's TestRelocateLiveNeverWipeAcrossMovedAddress, reusing virtualplayer.Pair/Session.PullProgram/Session.Relocate.")
}

// ply136Input mirrors conformance/corpora/player-1/
// PLY-136-valid-token-revoked-reconnect-clears-token-only.json's own `input`
// block.
type ply136Input struct {
	PlayerPersistedState struct {
		RelayAddress struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"relay_address"`
		TrustAnchorPresent  bool `json:"trust_anchor_present"`
		ChannelTokenPresent bool `json:"channel_token_present"`
	} `json:"player_persisted_state"`
	ReconnectAttempt struct {
		Classification string `json:"classification"`
		RelayResponse  struct {
			ErrorCode string `json:"error_code"`
			Reason    string `json:"reason"`
		} `json:"relay_response"`
	} `json:"reconnect_attempt"`
}

// drivePLY136 drives virtualplayer.ReconnectDecision — the committed,
// network-free credential-clearing decision (PLY-072/073/133/135/136) — against
// the frozen PLY-136 case's own reconnect attempt, and diffs every field of its
// expected block: CHANNEL_TOKEN_REVOKED clears ONLY the channel token, keeping
// the persisted relay address and trust anchor (never-wipe), and returns the
// player to Pairing redemption.
func drivePLY136(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "PLY-136")
	if !ok {
		rep.Fail("PLY-136", contract, "case not found in frozen corpus")
		return
	}

	var in ply136Input
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	state := virtualplayer.PersistedState{
		RelayAddress: virtualplayer.RelayAddress{
			Host: in.PlayerPersistedState.RelayAddress.Host,
			Port: in.PlayerPersistedState.RelayAddress.Port,
		},
		TrustAnchorPresent:  in.PlayerPersistedState.TrustAnchorPresent,
		ChannelTokenPresent: in.PlayerPersistedState.ChannelTokenPresent,
	}
	attempt := virtualplayer.ReconnectAttempt{
		Classification: in.ReconnectAttempt.Classification,
		RelayResponse: virtualplayer.RelayResponse{
			ErrorCode: in.ReconnectAttempt.RelayResponse.ErrorCode,
			Reason:    in.ReconnectAttempt.RelayResponse.Reason,
		},
	}

	got := virtualplayer.ReconnectDecision(state, attempt)

	var diffs []report.Diff
	if want := c.ExpectString("failure_classification"); got.FailureClassification != want {
		diffs = append(diffs, report.Diff{Field: "failure_classification", Expected: want, Actual: got.FailureClassification})
	}
	if want := c.ExpectBool("network_level_failure"); got.NetworkLevelFailure != want {
		diffs = append(diffs, report.Diff{Field: "network_level_failure", Expected: want, Actual: got.NetworkLevelFailure})
	}
	if want := c.ExpectBool("channel_token_cleared"); got.ChannelTokenCleared != want {
		diffs = append(diffs, report.Diff{Field: "channel_token_cleared", Expected: want, Actual: got.ChannelTokenCleared})
	}
	if want := c.ExpectBool("trust_anchor_cleared"); got.TrustAnchorCleared != want {
		diffs = append(diffs, report.Diff{Field: "trust_anchor_cleared", Expected: want, Actual: got.TrustAnchorCleared})
	}
	if want := c.ExpectBool("relay_address_cleared"); got.RelayAddressCleared != want {
		diffs = append(diffs, report.Diff{Field: "relay_address_cleared", Expected: want, Actual: got.RelayAddressCleared})
	}
	if want := c.ExpectString("player_state"); got.PlayerState != want {
		diffs = append(diffs, report.Diff{Field: "player_state", Expected: want, Actual: got.PlayerState})
	}
	if want := c.ExpectString("error_code"); got.ErrorCode != want {
		diffs = append(diffs, report.Diff{Field: "error_code", Expected: want, Actual: got.ErrorCode})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "token-revocation reconnect decision diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"driven via virtualplayer.ReconnectDecision directly — the exact, committed, network-free credential-clearing decision (PLY-072/073/133/135/136); the live wire round-trip (a real relay-side revocation via RevokeScreen, surfaced through Session.PullProgram as CHANNEL_TOKEN_REVOKED) is exercised end to end in internal/virtualplayer/revocation_test.go's TestReconnectRevokedClearsTokenOnlyEndToEnd, reusing this same function.")
}

// ply155Input mirrors conformance/corpora/player-1/
// PLY-155-valid-power-schedule-interaction.json's own `input` block.
type ply155Input struct {
	ActiveLease struct {
		LeaseID  string              `json:"lease_id"`
		Priority string              `json:"priority"`
		Display  string              `json:"display"`
		Content  []wire.LeaseContent `json:"content"`
	} `json:"active_lease"`
	DeviceStatus struct {
		State   string `json:"state"`
		AppType string `json:"app_type"`
	} `json:"device_status"`
	DisplayPowerScheduleState   string `json:"display_power_schedule_state"`
	RecoveryEvaluationTriggered bool   `json:"recovery_evaluation_triggered"`
	IncomingPreemptLease        struct {
		LeaseID  string              `json:"lease_id"`
		Priority string              `json:"priority"`
		Display  string              `json:"display"`
		Content  []wire.LeaseContent `json:"content"`
	} `json:"incoming_preempt_lease"`
}

// drivePLY155 drives BOTH committed relay-side decisions the case exercises —
// playerserver.EvaluateRecovery (the screen-liveness recovery gate,
// PLY-150-157) and playerserver.AdoptPreemptDisplay (the preempt-into-blank
// never-force-visible rule, PLY-104) — against the frozen PLY-155 case's own
// input, and diffs every field of its expected block: an intentional
// display:blank lease suppresses foreground recovery even though the device's
// own state/app-type gates would otherwise pass, and a preempt lease targeting
// that same screen is still accepted but does not force it visible.
func drivePLY155(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "PLY-155")
	if !ok {
		rep.Fail("PLY-155", contract, "case not found in frozen corpus")
		return
	}

	var in ply155Input
	if err := decodeField(c.Input, &in); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input: %v", err))
		return
	}

	sig := playerserver.LivenessSignal{
		ActiveDisplay: in.ActiveLease.Display,
		DeviceStatus: playerserver.DeviceStatus{
			State:   in.DeviceStatus.State,
			AppType: in.DeviceStatus.AppType,
		},
		PowerScheduleState: in.DisplayPowerScheduleState,
	}
	evalGot := playerserver.EvaluateRecovery(sig)

	incoming := wire.Lease{
		LeaseID:  in.IncomingPreemptLease.LeaseID,
		Priority: in.IncomingPreemptLease.Priority,
		Display:  in.IncomingPreemptLease.Display,
		Content:  in.IncomingPreemptLease.Content,
	}
	preemptGot := playerserver.AdoptPreemptDisplay(in.ActiveLease.Display, incoming)

	var diffs []report.Diff
	if want := c.ExpectBool("recovery_would_pass_state_check"); evalGot.WouldPassStateCheck != want {
		diffs = append(diffs, report.Diff{Field: "recovery_would_pass_state_check", Expected: want, Actual: evalGot.WouldPassStateCheck})
	}
	if want := c.ExpectBool("recovery_would_pass_app_type_check"); evalGot.WouldPassAppTypeCheck != want {
		diffs = append(diffs, report.Diff{Field: "recovery_would_pass_app_type_check", Expected: want, Actual: evalGot.WouldPassAppTypeCheck})
	}
	if want := c.ExpectBool("recovery_suppressed_due_to_blank_display"); evalGot.SuppressedDueToBlankDisplay != want {
		diffs = append(diffs, report.Diff{Field: "recovery_suppressed_due_to_blank_display", Expected: want, Actual: evalGot.SuppressedDueToBlankDisplay})
	}
	if want := c.ExpectBool("recovery_attempted"); evalGot.RecoveryAttempted != want {
		diffs = append(diffs, report.Diff{Field: "recovery_attempted", Expected: want, Actual: evalGot.RecoveryAttempted})
	}
	if want := c.ExpectBool("preempt_lease_accepted"); preemptGot.PreemptLeaseAccepted != want {
		diffs = append(diffs, report.Diff{Field: "preempt_lease_accepted", Expected: want, Actual: preemptGot.PreemptLeaseAccepted})
	}
	if want := c.ExpectString("display_remains"); preemptGot.DisplayRemains != want {
		diffs = append(diffs, report.Diff{Field: "display_remains", Expected: want, Actual: preemptGot.DisplayRemains})
	}
	if want := c.ExpectBool("preempt_forces_visible"); preemptGot.PreemptForcesVisible != want {
		diffs = append(diffs, report.Diff{Field: "preempt_forces_visible", Expected: want, Actual: preemptGot.PreemptForcesVisible})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "off-air liveness-recovery / preempt-into-blank interaction diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"driven via playerserver.EvaluateRecovery + playerserver.AdoptPreemptDisplay directly — the exact, committed, network-free relay-side decisions (PLY-093/104/150-157); this case's recovery_evaluation_triggered input gates WHEN a relay runs the evaluation (a scheduling concern outside these pure functions' own scope) and is not itself asserted as a field these functions produce.")
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
