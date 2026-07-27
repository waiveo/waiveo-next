package relay1

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// driveREL051 drives REL-051's ahead-of-current clause on the wire, plus the
// REL-054 correlation-id requirement on the acknowledgment it produces:
//
//   - The relay applies generation 44, then the feeder's CURRENT generation
//     is staged BEHIND it at 42 (the app peer stays stateless about relay
//     history), and the relay pulls with since_generation=44 — GREATER than
//     the app peer's current generation.
//   - REL-051: the answer MUST be the full state.snapshot (sections
//     included) at the app peer's own current generation — never a
//     state.unchanged asserting agreement that does not exist.
//   - REL-052: the relay's own gate then refuses the regressed generation —
//     last-applied stays 44, nothing is applied.
//   - REL-054: the state.ack for the refused snapshot reports error rather
//     than an advanced applied_generation, and carries the correlation id
//     of the state.pull exchange that delivered it — asserted against the
//     WIRE frame the app peer recorded, id included.
//
// The exchange rides the real production dialer (internal/relay/relayconn)
// against the live in-process feeder, exactly as driveREL057 does.
func driveREL051(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-051")
	if !ok {
		rep.Fail("REL-051", contract, "case not found in frozen corpus")
		return
	}

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	defer store.Close()
	relayIdent, ok, err := store.Identity()
	if err != nil || !ok {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read enrolled identity: ok=%v err=%v", ok, err))
		return
	}

	// Precondition: the relay's last-applied generation is AHEAD of what the
	// app peer will serve (input.relay_last_applied vs
	// input.app_peer_current_generation).
	aheadGen, present := expectIntInput(c, "relay_last_applied.generation")
	if !present {
		rep.Fail(c.CaseID, contract, "corpus input carries no relay_last_applied.generation")
		return
	}
	currentGen, present := expectIntInput(c, "app_peer_current_generation")
	if !present {
		rep.Fail(c.CaseID, contract, "corpus input carries no app_peer_current_generation")
		return
	}

	conn, err := relayconn.Dial(relayconn.Config{
		URL: feeder.EnrollBaseURL(), Store: store, Declaration: driverDeclaration,
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("dial persistent connection: %v", err))
		return
	}
	defer conn.Close()

	if err := feeder.StageSnapshot(aheadGen, false); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-%d snapshot: %v", aheadGen, err))
		return
	}
	reply, err := conn.Pull("", nil)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("baseline state.pull: %v", err))
		return
	}
	body, raw, err := relayconn.SnapshotFromFrame(reply)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("baseline snapshot: %v", err))
		return
	}
	if _, err := desiredstate.VerifyAndApply(store, body, raw); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply baseline gen-%d: %v", aheadGen, err))
		return
	}

	// The app peer's current generation regresses behind the relay's
	// last-applied one; the relay pulls claiming since_generation=44.
	if err := feeder.StageSnapshot(currentGen, false); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-%d snapshot: %v", currentGen, err))
		return
	}
	since := aheadGen
	reply, err = conn.Pull("", &since)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("ahead-of-current state.pull: %v", err))
		return
	}

	var diffs []report.Diff

	// response_type + state_unchanged_sent: the full snapshot, never the
	// lying unchanged (REL-051's ahead-of-current clause).
	if want := c.ExpectString("response_type"); want == "" {
		diffs = append(diffs, report.Diff{Field: "response_type", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if string(reply.Type) != want {
		diffs = append(diffs, report.Diff{Field: "response_type (REL-051: since_generation GREATER than current)", Expected: want, Actual: string(reply.Type)})
	}
	if wantUnchanged, present := c.Expect("state_unchanged_sent"); !present {
		diffs = append(diffs, report.Diff{Field: "state_unchanged_sent", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if wantUnchanged == false && reply.Type == wire.FrameTypeStateUnchanged {
		diffs = append(diffs, report.Diff{Field: "state_unchanged_sent", Expected: false, Actual: "state.unchanged asserting agreement that does not exist (REL-051)"})
	}

	// snapshot_carries_sections: the FULL snapshot, asserted on the raw
	// wire bytes (a decoded struct cannot reveal an omitted key).
	var bodyKeys map[string]json.RawMessage
	if err := json.Unmarshal(reply.Body, &bodyKeys); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode reply body keys: %v", err))
		return
	}
	if c.ExpectBool("snapshot_carries_sections") {
		if _, hasSections := bodyKeys["sections"]; !hasSections {
			diffs = append(diffs, report.Diff{Field: "snapshot_carries_sections", Expected: true, Actual: fmt.Sprintf("reply body carries no sections key: %s", reply.Body)})
		}
	}
	snapBody, rawSections, err := relayconn.SnapshotFromFrame(reply)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode ahead-of-current reply: %v", err))
		return
	}
	if wantGen, present := expectInt(c, "snapshot_generation"); !present {
		diffs = append(diffs, report.Diff{Field: "snapshot_generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if snapBody.Generation != wantGen {
		diffs = append(diffs, report.Diff{Field: "snapshot_generation (the app peer's own current)", Expected: wantGen, Actual: snapBody.Generation})
	}

	// relay_refuses_regressed_generation: the divergence surfaces at the
	// relay's own REL-052 gate, typed, with nothing applied.
	_, verifyErr := desiredstate.VerifyAndApply(store, snapBody, rawSections)
	if c.ExpectBool("relay_refuses_regressed_generation") && !errors.Is(verifyErr, desiredstate.ErrGenerationRegressed) {
		diffs = append(diffs, report.Diff{Field: "relay_refuses_regressed_generation (REL-052)", Expected: "ErrGenerationRegressed", Actual: fmt.Sprintf("%v", verifyErr)})
	}
	priorGen, _, _, _ := store.LastAppliedGeneration()
	if wantGen, present := expectInt(c, "persisted_last_applied_unchanged.generation"); !present {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied_unchanged.generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if priorGen != wantGen {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied_unchanged.generation", Expected: wantGen, Actual: priorGen})
	}

	// The acknowledgment for the refused snapshot, sent exactly as the real
	// relay sends it (REL-054/072: error + the UNADVANCED generation,
	// correlated to the pull exchange).
	if verifyErr != nil {
		if err := conn.SendStateAck(reply.ID, reply.TraceID, wire.StateAckBody{
			AppliedGeneration: priorGen,
			Error:             &wire.AckErrorBody{Code: ackErrorCode(verifyErr), Message: verifyErr.Error()},
		}); err != nil {
			diffs = append(diffs, report.Diff{Field: "state.ack send", Expected: "sent", Actual: err.Error()})
		}
	}

	// state_ack: diffed against the WIRE frame the app peer recorded — the
	// envelope id MUST be the state.pull exchange's own correlation id
	// (REL-054), the body MUST report error rather than an advanced
	// applied_generation.
	wantAckGen, genPresent := expectInt(c, "state_ack.body.applied_generation")
	if !genPresent {
		diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	}
	wantCorrelated := c.ExpectBool("state_ack.correlates_to_pull_id")
	wantErrPresent := c.ExpectBool("state_ack.body.error_present")
	if genPresent {
		if observed, ok := awaitWireAckFrame(feeder, relayIdent.RelayID, func(f wire.Frame, body wire.StateAckBody) bool {
			if wantCorrelated && f.ID != reply.ID {
				return false
			}
			if wantErrPresent && body.Error == nil {
				return false
			}
			return body.AppliedGeneration == wantAckGen
		}); !ok {
			diffs = append(diffs, report.Diff{
				Field:    "state_ack (wire, REL-054: correlation id of the pull exchange + error, unadvanced applied_generation)",
				Expected: fmt.Sprintf("id=%s applied_generation=%d error present", reply.ID, wantAckGen),
				Actual:   observed,
			})
		}
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "ahead-of-current since_generation diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"relay_id / frame ids: runtime-issued (the enrolled identity and fresh correlation ids), asserted self-consistent on the wire (the recorded state.ack's envelope id equals the pull exchange's) rather than byte-equal to the corpus's static fixtures; generations 44/42 are staged live and match the corpus values exactly.",
		"state_ack.body.error_present is asserted as presence + the unadvanced applied_generation, not a specific taxonomy code: the taxonomy defines no dedicated regressed-generation code — REL-051's own text resolves this divergence by operator re-anchoring (REL-017), not by a retryable code.")
}

// expectIntInput reads an integer from the corpus case's INPUT block by
// dotted path (JSON numbers decode as float64) — the input-side counterpart
// of expectInt, which reads the expected block.
func expectIntInput(c corpus.Case, path string) (int64, bool) {
	var cur any = c.Input
	for _, key := range strings.Split(path, ".") {
		node, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		if cur, ok = node[key]; !ok {
			return 0, false
		}
	}
	f, ok := cur.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// awaitWireAckFrame is awaitWireAck with the whole wire FRAME exposed to the
// predicate, for assertions on the envelope itself (REL-054's correlation
// id) and not just the decoded body.
func awaitWireAckFrame(feeder Feeder, relayID string, pred func(f wire.Frame, body wire.StateAckBody) bool) (observed string, ok bool) {
	deadline := time.Now().Add(ackWaitTimeout)
	for time.Now().Before(deadline) {
		if f, present := feeder.LastStateAck(relayID); present {
			var body wire.StateAckBody
			if f.DecodeBody(&body) == nil && pred(f, body) {
				return "", true
			}
			observed = fmt.Sprintf("last wire state.ack: id=%s body=%s", f.ID, string(f.Body))
		} else {
			observed = "no wire state.ack recorded at the app peer"
		}
		time.Sleep(5 * time.Millisecond)
	}
	return observed, false
}
