package relay1

import (
	"fmt"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// ackWaitTimeout bounds how long a drive waits for an async wire observation
// (a state.ack landing at the app peer's read loop, a nudge-triggered apply
// completing) before declaring the divergence. Generous for an in-process
// loopback exchange.
const ackWaitTimeout = 5 * time.Second

// driveREL057 drives the server-initiated state.changed nudge (REL-057) on
// ONE established persistent connection — enroll (HTTP bootstrap via the
// pluggable client), dial the real connection, apply the staged generation
// 42, then advance the feeder to 43 and push the nudge — asserting the
// corpus's expectations against the wire:
//
//   - the nudge delivered carries exactly the advanced generation (a nudge
//     is generation-only, no payload);
//   - the relay responds with its OWN state.pull, and every state.snapshot
//     received on the connection correlates to a pull the relay sent —
//     desired state still moves exclusively as a pull response, the app
//     peer never sends an unsolicited snapshot (REL-050);
//   - the nudged pull's snapshot verifies + applies at the advanced
//     generation, and the wire state.ack the app peer records carries it
//     (REL-054).
//
// The session rides the real production dialer (internal/relay/relayconn) —
// the nudge delivery, coalescing cell, and dispatcher under test are the
// shipped code, exactly as the in-process REL-056/061/090 drives exercise
// their own shipped subsystems directly.
func driveREL057(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-057")
	if !ok {
		rep.Fail("REL-057", contract, "case not found in frozen corpus")
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

	if err := feeder.StageSnapshot(42, false); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-42 snapshot: %v", err))
		return
	}

	// The connection's nudge handler: pull + verify + apply + ack — the
	// relay binary's own shape. Results ride a channel the drive waits on.
	type applyResult struct {
		applied desiredstate.Applied
		err     error
	}
	results := make(chan applyResult, 4)
	nudgeGens := make(chan int64, 4)
	frames := &frameLog{}
	var connMu sync.Mutex
	var conn *relayconn.Client

	onGeneration := func(gen int64) {
		nudgeGens <- gen
		connMu.Lock()
		cl := conn
		connMu.Unlock()
		reply, err := cl.Pull("", nil)
		if err != nil {
			results <- applyResult{err: err}
			return
		}
		body, raw, err := relayconn.SnapshotFromFrame(reply)
		if err != nil {
			results <- applyResult{err: err}
			return
		}
		applied, err := desiredstate.VerifyAndApply(store, body, raw)
		if err == nil {
			err = cl.SendStateAck(reply.ID, reply.TraceID,
				wire.StateAckBody{AppliedGeneration: applied.Generation})
		}
		results <- applyResult{applied: applied, err: err}
	}

	dialed, err := relayconn.Dial(relayconn.Config{
		URL: feeder.EnrollBaseURL(), Store: store, Declaration: driverDeclaration,
		OnGenerationAdvance: onGeneration,
		ObserveFrame:        frames.observe,
	})
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("dial persistent connection: %v", err))
		return
	}
	defer dialed.Close()
	connMu.Lock()
	conn = dialed
	connMu.Unlock()

	// Establish the case's precondition on THIS connection: applied
	// generation 42 (input.relay_last_applied).
	reply, err := dialed.Pull("", nil)
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
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply baseline gen-42: %v", err))
		return
	}

	// The staged advance + the server-initiated nudge under test.
	if err := feeder.StageSnapshot(43, false); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("stage gen-43 snapshot: %v", err))
		return
	}
	feeder.NotifyGenerationAdvance()

	var diffs []report.Diff

	// nudge_generation_delivered: the nudge announced exactly the advanced
	// generation (generation-only payload, REL-057).
	wantNudgeGen, present := expectInt(c, "nudge_generation_delivered")
	if !present {
		diffs = append(diffs, report.Diff{Field: "nudge_generation_delivered", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	}
	select {
	case gen := <-nudgeGens:
		if present && gen != wantNudgeGen {
			diffs = append(diffs, report.Diff{Field: "nudge_generation_delivered", Expected: wantNudgeGen, Actual: gen})
		}
	case <-time.After(ackWaitTimeout):
		rep.Fail(c.CaseID, contract, "no state.changed nudge reached the relay within the wait budget (REL-057)")
		return
	}

	// applied_generation: the nudge-triggered pull verified + applied the
	// advanced generation.
	select {
	case res := <-results:
		if res.err != nil {
			diffs = append(diffs, report.Diff{Field: "nudge-triggered pull/apply", Expected: "applied", Actual: res.err.Error()})
		} else if wantGen, present := expectInt(c, "applied_generation"); !present {
			diffs = append(diffs, report.Diff{Field: "applied_generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
		} else if res.applied.Generation != wantGen {
			diffs = append(diffs, report.Diff{Field: "applied_generation", Expected: wantGen, Actual: res.applied.Generation})
		}
	case <-time.After(ackWaitTimeout):
		rep.Fail(c.CaseID, contract, "the nudge-triggered pull never completed within the wait budget")
		return
	}

	// relay_responds_with_state_pull + unsolicited_snapshot_sent: on the
	// wire, every received state.snapshot must correlate to a state.pull
	// this relay sent (REL-050 — the nudge itself moves no desired state).
	sentPulls := map[string]bool{}
	for _, f := range frames.Sent() {
		if f.Type == wire.FrameTypeStatePull {
			sentPulls[f.ID] = true
		}
	}
	if want := c.ExpectBool("relay_responds_with_state_pull"); want && len(sentPulls) < 2 {
		diffs = append(diffs, report.Diff{Field: "relay_responds_with_state_pull", Expected: true, Actual: fmt.Sprintf("%d state.pull frame(s) sent — the nudge did not draw the relay's own pull", len(sentPulls))})
	}
	for _, f := range frames.Received() {
		if f.Type == wire.FrameTypeStateSnapshot && !sentPulls[f.ID] {
			diffs = append(diffs, report.Diff{Field: "unsolicited_snapshot_sent", Expected: false, Actual: fmt.Sprintf("state.snapshot id %q correlates to no state.pull this relay sent (REL-050)", f.ID)})
		}
	}

	// state_ack.body.applied_generation: the WIRE acknowledgment recorded at
	// the app peer (REL-054).
	if wantAckGen, present := expectInt(c, "state_ack.body.applied_generation"); !present {
		diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if diff, ok := awaitWireAck(feeder, relayIdent.RelayID, func(body wire.StateAckBody) bool {
		return body.AppliedGeneration == wantAckGen && body.Error == nil
	}); !ok {
		diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation (wire, REL-054)", Expected: wantAckGen, Actual: diff})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "state.changed nudge diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"relay_id / frame ids: runtime-issued (the enrolled identity and fresh correlation ids), asserted self-consistent on the wire rather than byte-equal to the corpus's static fixtures; generations 42/43 are staged live and match the corpus values exactly.")
}

// frameLog records the wire exchange off the client's test-only
// frame-observation seam, mirroring internal/feeder/relayconn's own e2e
// recorder.
type frameLog struct {
	mu       sync.Mutex
	sent     []wire.Frame
	received []wire.Frame
}

func (l *frameLog) observe(sent bool, f wire.Frame) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if sent {
		l.sent = append(l.sent, f)
	} else {
		l.received = append(l.received, f)
	}
}

func (l *frameLog) Sent() []wire.Frame     { return l.snapshotOf(&l.sent) }
func (l *frameLog) Received() []wire.Frame { return l.snapshotOf(&l.received) }

func (l *frameLog) snapshotOf(log *[]wire.Frame) []wire.Frame {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]wire.Frame, len(*log))
	copy(out, *log)
	return out
}

// awaitWireAck polls the feeder's recorded last state.ack for relayID until
// pred accepts its decoded body or the wait budget elapses (the ack lands on
// the app peer's read loop asynchronously). On timeout it returns a
// description of what WAS observed, for the diff.
func awaitWireAck(feeder Feeder, relayID string, pred func(wire.StateAckBody) bool) (observed string, ok bool) {
	deadline := time.Now().Add(ackWaitTimeout)
	for time.Now().Before(deadline) {
		if f, present := feeder.LastStateAck(relayID); present {
			var body wire.StateAckBody
			if f.DecodeBody(&body) == nil && pred(body) {
				return "", true
			}
			observed = fmt.Sprintf("last wire state.ack body: %s", string(f.Body))
		} else {
			observed = "no wire state.ack recorded at the app peer"
		}
		time.Sleep(5 * time.Millisecond)
	}
	return observed, false
}
